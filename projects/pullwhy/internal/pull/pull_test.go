package pull

import (
	"context"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
)

// script plays events at given offsets (in ms) from a fake clock.
type script struct {
	clock  *time.Time
	events []struct {
		ms int
		e  engine.Event
	}
	err error
}

func (s *script) Pull(_ context.Context, _, _, _, _ string, fn func(engine.Event)) error {
	start := *s.clock
	for _, ev := range s.events {
		e := ev.e
		e.At = start.Add(time.Duration(ev.ms) * time.Millisecond)
		*s.clock = e.At
		fn(e)
	}
	return s.err
}

func ev(ms int, status, id string, cur, total int64) struct {
	ms int
	e  engine.Event
} {
	e := engine.Event{Status: status, ID: id}
	e.ProgressDetail.Current, e.ProgressDetail.Total = cur, total
	return struct {
		ms int
		e  engine.Event
	}{ms, e}
}

func TestTimeline(t *testing.T) {
	clock := time.Unix(1000, 0)
	s := &script{clock: &clock}
	s.events = append(s.events,
		ev(10, "Pulling from library/nginx", "alpine", 0, 0),
		// 31 s of nothing before the layers: a dead mirror.
		ev(31000, "Pulling fs layer", "big", 0, 0),
		ev(31000, "Already exists", "base", 0, 0),
		ev(31100, "Downloading", "big", 1<<20, 20<<20),
		ev(43100, "Downloading", "big", 2<<20, 20<<20), // a 12 s stall
		ev(51100, "Download complete", "big", 0, 0),
		ev(51200, "Extracting", "big", 0, 0),
		ev(52000, "Pull complete", "big", 0, 0),
		ev(52000, "Digest: sha256:abc", "", 0, 0),
		ev(52000, "Status: Downloaded newer image for nginx:alpine", "", 0, 0),
	)
	now := func() time.Time { return clock }
	tl := Run(context.Background(), s, "n", "t", "", "", 10*time.Second, now)
	if tl.Error != "" || tl.Digest != "sha256:abc" || tl.Status != "Downloaded newer image for nginx:alpine" {
		t.Fatalf("%+v", tl)
	}
	if tl.FirstLayer != 31*time.Second || tl.Took != 52*time.Second {
		t.Fatalf("first %s took %s", tl.FirstLayer, tl.Took)
	}
	if len(tl.Gaps) != 2 || tl.Gaps[0].Phase != "before the first layer" || tl.Gaps[0].Took < 30*time.Second ||
		tl.Gaps[1].Phase != "downloading" || tl.Gaps[1].Took != 12*time.Second {
		t.Fatalf("%+v", tl.Gaps)
	}
	big := tl.Layers[0]
	if big.ID != "big" || big.Total != 20<<20 || big.Longest != 12*time.Second || big.Rate() < 1<<20 || big.Rate() > 1<<21 {
		t.Fatalf("%+v rate %f", big, big.Rate())
	}
	if !tl.Layers[1].Cached || tl.Bytes() != 20<<20 || tl.Slowest() != big {
		t.Fatalf("%+v", tl.Layers[1])
	}
}

func TestFailure(t *testing.T) {
	clock := time.Unix(1000, 0)
	s := &script{clock: &clock, err: &engine.DaemonError{Status: 200, Message: "toomanyrequests: You have reached your unauthenticated pull rate limit."}}
	tl := Run(context.Background(), s, "n", "t", "", "", 0, func() time.Time { return clock })
	if tl.Kind != probe.KindRateLimited || tl.Error != "toomanyrequests: You have reached your unauthenticated pull rate limit." {
		t.Fatalf("%+v", tl)
	}
}

func TestClassify(t *testing.T) {
	for msg, kind := range map[string]string{
		`failed to resolve reference "192.0.2.2:5443/t/alpine:3": failed to do request: Head "https://192.0.2.2:5443/v2/t/alpine/manifests/3": tls: failed to verify certificate: x509: certificate signed by unknown authority`: probe.KindTLSAuthority,
		`Get "https://reg:5000/v2/": http: server gave HTTP response to HTTPS client`:                                                                                     probe.KindHTTPOnHTTPS,
		`no matching manifest for linux/s390x in the manifest list entries`:                                                                                               KindPlatform,
		`pull access denied for nope, repository does not exist or may require 'docker login': denied: requested access to the resource is denied`:                        probe.KindNotFound,
		`Head "https://reg/v2/x/manifests/1": unauthorized: authentication required`:                                                                                      probe.KindUnauthorized,
		`failed to do request: Head "https://registry-1.docker.io/v2/library/alpine/manifests/3": proxyconnect tcp: dial tcp 127.0.0.1:3129: connect: connection refused`: probe.KindProxy,
		`dial tcp: lookup reg.example on 127.0.0.53:53: no such host`:                                                                                                     probe.KindDNS,
		`dial tcp 10.0.0.9:5000: connect: connection refused`:                                                                                                             probe.KindRefused,
		`Get "https://10.255.255.1:5000/v2/": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`:                                                 probe.KindTimeout,
		`x509: certificate has expired or is not yet valid`:                                                                                                               probe.KindTLSExpired,
		`x509: certificate is valid for a.example, not b.example`:                                                                                                         probe.KindTLSName,
		`manifest unknown`:        probe.KindNotFound,
		`something else entirely`: probe.KindOther,
	} {
		if got := Classify(msg); got != kind {
			t.Errorf("%s\n got %s want %s", msg, got, kind)
		}
	}
}
