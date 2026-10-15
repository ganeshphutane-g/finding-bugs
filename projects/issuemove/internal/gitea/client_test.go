package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectToAnotherPortDropsTheToken(t *testing.T) {
	var got atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("Sudo"))
		w.Write([]byte("data"))
	}))
	defer other.Close()
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}))
	defer home.Close()
	c := New(home.URL, "secret").As("alice")
	data, err := c.DownloadAttachment(context.Background(), "u")
	if err != nil || string(data) != "data" {
		t.Fatalf("%q %v", data, err)
	}
	if v := got.Load(); v != "|" {
		t.Fatalf("headers sent to the other origin: %q", v)
	}
}

func TestReadsAreRetried(t *testing.T) {
	retries = []time.Duration{time.Millisecond, time.Millisecond}
	var calls, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"login":"root"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	u, err := c.Me(context.Background())
	if err != nil || u.Login != "root" || calls.Load() != 3 {
		t.Fatalf("%+v %v after %d calls", u, err, calls.Load())
	}
	if _, err := c.CreateComment(context.Background(), "o", "r", 1, "x"); err == nil || posts.Load() != 1 {
		t.Fatalf("a write was retried: %d posts, %v", posts.Load(), err)
	}
}

func TestRefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"user does not exist [uid: 0, name: ]"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "bad").Me(context.Background())
	if err == nil || !IsStatus(err, 401) || !strings.Contains(err.Error(), "token was refused") {
		t.Fatalf("%v", err)
	}
}
