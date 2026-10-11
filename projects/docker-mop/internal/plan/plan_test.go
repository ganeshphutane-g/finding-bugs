package plan

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

var now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func mustFilters(t *testing.T, raw ...string) Filters {
	t.Helper()
	f, err := ParseFilters(raw)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseFiltersSplitsAtFirstEquals(t *testing.T) {
	f := mustFilters(t, "label!=a=b", "label=env", "until=24h")
	if got := f.Get("label!"); !reflect.DeepEqual(got, []string{"a=b"}) {
		t.Fatalf("label! = %v", got)
	}
	if got := f.Get("label"); !reflect.DeepEqual(got, []string{"env"}) {
		t.Fatalf("label = %v", got)
	}
	if _, err := ParseFilters([]string{"nokey"}); err == nil {
		t.Fatal("expected an error for a filter without '='")
	}
}

func TestMatchLabels(t *testing.T) {
	labels := map[string]string{"env": "prod", "team": "a"}
	cases := []struct {
		filters []string
		labels  map[string]string
		want    bool
	}{
		{nil, labels, true},
		{[]string{"label=env"}, labels, true},
		{[]string{"label=env=prod"}, labels, true},
		{[]string{"label=env=dev"}, labels, false},
		{[]string{"label=env", "label=missing"}, labels, false},
		{[]string{"label=env"}, nil, false},
		{[]string{"label!=env"}, labels, false},
		{[]string{"label!=env=dev"}, labels, true},
		// label! excludes only when all label! values match together.
		{[]string{"label!=env", "label!=missing"}, labels, true},
		{[]string{"label!=env", "label!=team"}, labels, false},
		{[]string{"label!=env"}, nil, true},
	}
	for _, tc := range cases {
		f := mustFilters(t, tc.filters...)
		if got := f.MatchLabels(tc.labels); got != tc.want {
			t.Errorf("filters %v on %v: got %v, want %v", tc.filters, tc.labels, got, tc.want)
		}
	}
}

func TestUntil(t *testing.T) {
	cases := []struct {
		value string
		want  time.Time
	}{
		{"24h", now.Add(-24 * time.Hour)},
		{"-1h", now.Add(time.Hour)},
		{"2026-10-01T10:00:00Z", time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
		{"2026-10-01T10:00:00+05:30", time.Date(2026, 10, 1, 4, 30, 0, 0, time.UTC)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)},
		{"1791748808", time.Unix(1791748808, 0)},
		{"1791748808.5", time.Unix(1791748808, 500000000)},
	}
	for _, tc := range cases {
		got, err := mustFilters(t, "until="+tc.value).Until(now)
		if err != nil {
			t.Errorf("%s: %v", tc.value, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.value, got, tc.want)
		}
	}
	for _, bad := range []string{"yesterday", "1e9", "NaN"} {
		if _, err := mustFilters(t, "until="+bad).Until(now); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
	if _, err := mustFilters(t, "until=1h", "until=2h").Until(now); err == nil {
		t.Error("expected an error for two until filters")
	}
}

func container(id, state, imageID string, created time.Time, labels map[string]string, volumes ...string) engine.Container {
	c := engine.Container{ID: id, Names: []string{"/" + id}, State: state, ImageID: imageID, Created: created.Unix(), Labels: labels, SizeRw: 100}
	for _, v := range volumes {
		c.Mounts = append(c.Mounts, struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		}{Type: "volume", Name: v})
	}
	return c
}

func itemIDs(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}

func TestContainersPlan(t *testing.T) {
	old := now.Add(-48 * time.Hour)
	st := State{Containers: []engine.Container{
		container("running", "running", "img", old, nil),
		container("paused", "paused", "img", old, nil),
		container("restarting", "restarting", "img", old, nil),
		container("exited", "exited", "img", old, map[string]string{"keep": "1"}),
		container("created", "created", "img", now, nil),
		container("dead", "dead", "img", old, nil),
	}}

	p, err := Compute(st, Options{Kind: Containers, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemIDs(p.Containers); !reflect.DeepEqual(got, []string{"created", "dead", "exited"}) {
		t.Fatalf("got %v", got)
	}

	p, _ = Compute(st, Options{Kind: Containers, Now: now, Filters: mustFilters(t, "until=24h")})
	if got := itemIDs(p.Containers); !reflect.DeepEqual(got, []string{"dead", "exited"}) {
		t.Fatalf("until: got %v", got)
	}

	p, _ = Compute(st, Options{Kind: Containers, Now: now, Filters: mustFilters(t, "label!=keep")})
	if got := itemIDs(p.Containers); !reflect.DeepEqual(got, []string{"created", "dead"}) {
		t.Fatalf("label!: got %v", got)
	}

	if _, err := Compute(st, Options{Kind: Containers, Now: now, Filters: mustFilters(t, "dangling=true")}); err == nil {
		t.Fatal("expected invalid filter error")
	}
}

func TestVolumesPlan(t *testing.T) {
	anon := map[string]string{anonymousVolumeLabel: ""}
	st := State{
		Containers: []engine.Container{
			container("stopped", "exited", "img", now, nil, "anon-used", "named-used"),
		},
		Volumes: []engine.Volume{
			{Name: "anon-used", Labels: anon},
			{Name: "anon-free", Labels: anon},
			{Name: "named-used"},
			{Name: "named-free", Labels: map[string]string{"env": "dev"}},
		},
	}
	p, _ := Compute(st, Options{Kind: Volumes, Now: now})
	if got := itemIDs(p.Volumes); !reflect.DeepEqual(got, []string{"anon-free"}) {
		t.Fatalf("default: got %v", got)
	}
	p, _ = Compute(st, Options{Kind: Volumes, Now: now, All: true})
	if got := itemIDs(p.Volumes); !reflect.DeepEqual(got, []string{"anon-free", "named-free"}) {
		t.Fatalf("all: got %v", got)
	}
	p, _ = Compute(st, Options{Kind: Volumes, Now: now, Filters: mustFilters(t, "all=true", "label=env")})
	if got := itemIDs(p.Volumes); !reflect.DeepEqual(got, []string{"named-free"}) {
		t.Fatalf("all filter + label: got %v", got)
	}
	if _, err := Compute(st, Options{Kind: Volumes, Now: now, Filters: mustFilters(t, "until=1h")}); err == nil {
		t.Fatal("volumes must reject until")
	}

	// System prune removes the stopped container first, so its anonymous
	// volume goes too; named volumes stay even with --all.
	p, _ = Compute(st, Options{Kind: System, Now: now, All: true, Volumes: true})
	if got := itemIDs(p.Volumes); !reflect.DeepEqual(got, []string{"anon-free", "anon-used"}) {
		t.Fatalf("system: got %v", got)
	}
	if _, err := Compute(st, Options{Kind: System, Now: now, Volumes: true, Filters: mustFilters(t, "until=1h")}); err == nil {
		t.Fatal("system --volumes must reject until")
	}
}

func TestNetworksPlan(t *testing.T) {
	stopped := container("web", "exited", "img", now, nil)
	stopped.NetworkSettings.Networks = map[string]struct {
		NetworkID string `json:"NetworkID"`
	}{"app": {NetworkID: "net-app"}}
	running := engine.Network{ID: "net-busy", Name: "busy", Created: now}
	running.Containers = map[string]struct {
		Name string `json:"Name"`
	}{"x": {Name: "db"}}

	st := State{
		Containers: []engine.Container{stopped},
		Networks: []engine.Network{
			{ID: "net-bridge", Name: "bridge"},
			{ID: "net-host", Name: "host"},
			{ID: "net-none", Name: "none"},
			{ID: "net-ingress", Name: "ingress", Scope: "swarm"},
			running,
			{ID: "net-app", Name: "app", Created: now.Add(-time.Hour)},
			{ID: "net-new", Name: "new", Created: now},
		},
	}
	p, _ := Compute(st, Options{Kind: Networks, Now: now})
	if got := itemIDs(p.Networks); !reflect.DeepEqual(got, []string{"net-app", "net-new"}) {
		t.Fatalf("got %v", got)
	}
	if len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "web") {
		t.Fatalf("expected a note about the stopped container, got %v", p.Notes)
	}
	p, _ = Compute(st, Options{Kind: Networks, Now: now, Filters: mustFilters(t, "until=30m")})
	if got := itemIDs(p.Networks); !reflect.DeepEqual(got, []string{"net-app"}) {
		t.Fatalf("until: got %v", got)
	}
}

func TestImagesPlan(t *testing.T) {
	st := State{
		Containers: []engine.Container{container("c", "exited", "used", now, nil)},
		Images: []engine.Image{
			{ID: "used", RepoTags: []string{"app:1"}, Size: 100},
			{ID: "tagged", RepoTags: []string{"app:2"}, Size: 100, SharedSize: 40},
			{ID: "dangling", RepoTags: []string{"<none>:<none>"}, Size: 50},
			{ID: "dangling-labelled", Size: 50, Labels: map[string]string{"keep": "1"}},
		},
		Dangling: map[string]bool{"dangling": true, "dangling-labelled": true},
	}
	p, _ := Compute(st, Options{Kind: Images, Now: now})
	if got := itemIDs(p.Images); !reflect.DeepEqual(got, []string{"dangling", "dangling-labelled"}) {
		t.Fatalf("dangling: got %v", got)
	}
	p, _ = Compute(st, Options{Kind: Images, Now: now, All: true, Filters: mustFilters(t, "label!=keep")})
	if got := itemIDs(p.Images); !reflect.DeepEqual(got, []string{"dangling", "tagged"}) {
		t.Fatalf("all: got %v", got)
	}
	for _, it := range p.Images {
		if it.ID == "tagged" && it.Size != 60 {
			t.Fatalf("tagged image should free its unshared 60 bytes, got %d", it.Size)
		}
	}
	p, _ = Compute(st, Options{Kind: Images, Now: now, Filters: mustFilters(t, "dangling=false")})
	if len(p.Images) != 3 {
		t.Fatalf("dangling=false should act like --all, got %v", itemIDs(p.Images))
	}
	if _, err := Compute(st, Options{Kind: Images, Now: now, Filters: mustFilters(t, "dangling=maybe")}); err == nil {
		t.Fatal("expected an error for dangling=maybe")
	}

	// System prune removes the stopped container first, which frees its image.
	p, _ = Compute(st, Options{Kind: System, Now: now, All: true})
	if got := itemIDs(p.Images); !reflect.DeepEqual(got, []string{"dangling", "dangling-labelled", "tagged", "used"}) {
		t.Fatalf("system: got %v", got)
	}
}

// Images built with the classic builder have untagged parent images. Docker
// removes those along with the image on top, and only untags a tagged image
// that other images are built on.
func TestImagesPlanClassicParents(t *testing.T) {
	st := State{
		Images: []engine.Image{
			{ID: "base", RepoTags: []string{"base:1"}, Size: 10},
			{ID: "layer1", ParentID: "base", Size: 20},
			{ID: "layer2", ParentID: "layer1", Size: 30},
			{ID: "app", ParentID: "layer2", RepoTags: []string{"app:1"}, Size: 40},
		},
	}
	p, _ := Compute(st, Options{Kind: Images, Now: now, All: true, Filters: mustFilters(t, "label!=x")})
	got := map[string]string{}
	for _, it := range p.Images {
		got[it.ID] = it.Note
	}
	want := map[string]string{
		"app":    "",
		"layer2": "untagged parent layer",
		"layer1": "untagged parent layer",
		"base":   "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// With app kept (used by a container), base is only untagged and the
	// layers stay.
	st.Containers = []engine.Container{container("c", "running", "app", now, nil)}
	p, _ = Compute(st, Options{Kind: Images, Now: now, All: true})
	if len(p.Images) != 1 || p.Images[0].ID != "base" || !strings.HasPrefix(p.Images[0].Note, "only untagged") || p.Images[0].Size != 0 {
		t.Fatalf("got %+v", p.Images)
	}
}

func TestTotal(t *testing.T) {
	p := Plan{
		Containers: []Item{{Size: 100}},
		Volumes:    []Item{{Size: -1}, {Size: 50}},
	}
	total, atLeast := p.Total()
	if total != 150 || !atLeast {
		t.Fatalf("got %d %v", total, atLeast)
	}
	p = Plan{Containers: []Item{{Size: 100}}}
	if total, atLeast := p.Total(); total != 100 || atLeast {
		t.Fatalf("got %d %v", total, atLeast)
	}
}

func TestParseKind(t *testing.T) {
	for in, want := range map[string]Kind{"image": Images, "VOLUMES": Volumes, "all": System, "network": Networks} {
		if got, err := ParseKind(in); err != nil || got != want {
			t.Errorf("%s: got %v %v", in, got, err)
		}
	}
	if _, err := ParseKind("builder"); err == nil {
		t.Error("expected an error for builder")
	}
}

// The containerd image store prunes each image on its own: parents of an
// image in use can still go, and untagged intermediates are ordinary dangling
// images.
func TestImagesPlanContainerdIgnoresParents(t *testing.T) {
	st := State{
		ContainerdStore: true,
		Containers:      []engine.Container{container("c", "running", "app", now, nil)},
		Images: []engine.Image{
			{ID: "base", RepoTags: []string{"base:1"}, Size: 10},
			{ID: "layer1", ParentID: "base", Size: 20},
			{ID: "app", ParentID: "layer1", RepoTags: []string{"app:1"}, Size: 40},
		},
		Dangling: map[string]bool{"layer1": true},
	}
	p, _ := Compute(st, Options{Kind: Images, Now: now, All: true})
	if got := itemIDs(p.Images); !reflect.DeepEqual(got, []string{"base", "layer1"}) {
		t.Fatalf("got %v", got)
	}
	for _, it := range p.Images {
		if it.Note != "" {
			t.Fatalf("unexpected note on %s: %q", it.ID, it.Note)
		}
	}
	p, _ = Compute(st, Options{Kind: Images, Now: now})
	if got := itemIDs(p.Images); !reflect.DeepEqual(got, []string{"layer1"}) {
		t.Fatalf("dangling: got %v", got)
	}
}
