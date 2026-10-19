// Package loki reads log lines from Loki.
package loki

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Loki.
type Client struct {
	URL                string
	Username, Password string
	BearerToken        string
	HTTP               *http.Client
}

// New makes a client.
func New(base string, insecure bool) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure}
	return &Client{URL: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Transport: t}}
}

// Line is one log line.
type Line struct {
	Time   time.Time
	Labels map[string]string
	Text   string
}

// Lines returns up to limit lines of a log query between start and end,
// newest first, as tenant (if set).
func (c *Client) Lines(ctx context.Context, query string, start, end time.Time, limit int, tenant string) ([]Line, error) {
	q := url.Values{"query": {query}, "start": {strconv.FormatInt(start.UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)},
		"limit": {strconv.Itoa(limit)}, "direction": {"backward"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if tenant != "" {
		req.Header.Set("X-Scope-OrgID", tenant)
	}
	switch {
	case c.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	case c.Username != "":
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // (the URL holds the query, not secrets, but is long)
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, fmt.Errorf("Loki answered %s: %s", resp.Status, msg)
	}
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("an unreadable answer from Loki: %v", err)
	}
	if r.Data.ResultType != "streams" {
		return nil, fmt.Errorf("the query returns %s, not log lines", r.Data.ResultType)
	}
	var out []Line
	for _, s := range r.Data.Result {
		for _, v := range s.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			out = append(out, Line{Time: time.Unix(0, ns), Labels: s.Stream, Text: v[1]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
