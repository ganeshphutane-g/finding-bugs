package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/bucketmove/internal/probe"
)

// TestProbeTargets checks the probe against what is known about each server,
// and that it leaves no scratch buckets behind.
func TestProbeTargets(t *testing.T) {
	_, targets := servers(t)
	for _, s := range targets {
		t.Run(s.name, func(t *testing.T) {
			caps, err := probe.Run(context.Background(), s.c)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{probe.Metadata, probe.Multipart, probe.OddKeys, probe.Lifecycle} {
				// SeaweedFS drops Content-Encoding in unversioned buckets.
				if s.name == "seaweedfs" && f == probe.Metadata {
					if caps.Supports(f) || !strings.Contains(caps[f].Detail, "Content-Encoding") {
						t.Errorf("seaweedfs: expected Content-Encoding to be reported as not kept, got %+v", caps[f])
					}
					continue
				}
				if !caps.Supports(f) {
					t.Errorf("%s should support %s: %s", s.name, f, caps[f].Detail)
				}
			}
			switch s.name {
			case "garage":
				for _, f := range []string{probe.Versioning, probe.ObjectTags, probe.Policy, probe.ObjectLock} {
					if caps.Supports(f) {
						t.Errorf("garage is not expected to support %s; if it does now, update this test", f)
					}
				}
			default:
				for _, f := range []string{probe.Versioning, probe.DeleteMarker, probe.ObjectTags, probe.ObjectLock, probe.Retention, probe.LegalHold} {
					if !caps.Supports(f) {
						t.Errorf("%s should support %s: %s", s.name, f, caps[f].Detail)
					}
				}
			}
			buckets, err := s.c.ListBuckets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range buckets {
				if strings.HasPrefix(b.Name, "bucketmove-probe-") {
					t.Errorf("scratch bucket left behind: %s", b.Name)
				}
			}
		})
	}
}
