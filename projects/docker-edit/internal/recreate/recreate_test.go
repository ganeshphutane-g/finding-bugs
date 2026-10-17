package recreate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-edit/internal/engine"
)

var ctx = context.Background()

func client(t *testing.T) *engine.Client {
	t.Helper()
	if os.Getenv("DOCKEREDIT_INTEGRATION") == "" {
		t.Skip("set DOCKEREDIT_INTEGRATION=1 to run tests against the Docker daemon")
	}
	ep, err := engine.ResolveEndpoint(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := engine.New(ep)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// unique names test objects so parallel runs and leftovers do not clash.
func unique(t *testing.T, base string) string {
	return fmt.Sprintf("de-%s-%d", base, time.Now().UnixNano()%1e9)
}

// comparable is what must not change when a container is made again: its
// settings, mounts and network connections (not its ID, times, state, its
// automatic hostname or the addresses it was given).
func comparable(t *testing.T, d Doc) map[string]any {
	t.Helper()
	cfg := deep(d["Config"]).(Doc)
	id := str(d["Id"])
	if str(cfg["Hostname"]) == id[:12] {
		delete(cfg, "Hostname")
	}
	host := deep(d["HostConfig"]).(Doc)
	// Anonymous volumes are named in the new container's host config; the
	// mounts below compare what matters.
	var keep []any
	for _, m := range list(host["Mounts"]) {
		if md := asDoc(m); !(str(md["Type"]) == "volume" && anonymousName.MatchString(str(md["Source"]))) {
			keep = append(keep, m)
		}
	}
	host["Mounts"] = keep
	var mounts []string
	for _, m := range list(d["Mounts"]) {
		md := asDoc(m)
		mounts = append(mounts, fmt.Sprintf("%s %s %s->%s rw=%v", md["Type"], md["Name"], md["Source"], md["Destination"], md["RW"]))
	}
	sort.Strings(mounts)
	nets := map[string]any{}
	for n, e := range asDoc(asDoc(d["NetworkSettings"])["Networks"]) {
		ed := asDoc(e)
		var aliases []string
		for _, a := range list(ed["Aliases"]) {
			if s := str(a); s != id[:12] {
				aliases = append(aliases, s)
			}
		}
		sort.Strings(aliases)
		// (An empty setting is reported as {} or null depending on how the
		// network was joined.)
		emptyNil := func(v any) any {
			if d, ok := v.(Doc); ok && len(nonEmpty(d)) == 0 {
				return nil
			}
			return v
		}
		nets[n] = map[string]any{"aliases": aliases, "ipam": emptyNil(ed["IPAMConfig"]), "links": ed["Links"], "driverOpts": emptyNil(ed["DriverOpts"])}
	}
	return map[string]any{"name": d["Name"], "image": d["Image"], "path": d["Path"], "args": d["Args"],
		"config": cfg, "host": host, "mounts": mounts, "networks": nets, "running": asDoc(d["State"])["Running"]}
}

func diff(a, b map[string]any) string {
	ja, _ := json.MarshalIndent(a, "", " ")
	jb, _ := json.MarshalIndent(b, "", " ")
	if string(ja) == string(jb) {
		return ""
	}
	la, lb := strings.Split(string(ja), "\n"), strings.Split(string(jb), "\n")
	var out []string
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("- %s\n+ %s", x, y))
			if len(out) > 12 {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// TestRecreateIsExact makes containers of many shapes again without a change
// and checks nothing that matters differs.
func TestRecreateIsExact(t *testing.T) {
	c := client(t)
	docker(t, "pull", "-q", "alpine:3")
	docker(t, "pull", "-q", "nginx:alpine")
	n1, n2, vol := unique(t, "net1"), unique(t, "net2"), unique(t, "vol")
	docker(t, "network", "create", "--subnet", "172.31.250.0/24", n1)
	docker(t, "network", "create", n2)
	docker(t, "volume", "create", vol)
	bind := t.TempDir()
	t.Cleanup(func() {
		exec.Command("docker", "network", "rm", n1, n2).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
	})

	shapes := map[string][]string{
		"minimal": {"alpine:3", "sleep", "3600"},
		"web": {"-p", "18081:80", "-p", "127.0.0.1:18443:443/tcp", "-e", "FOO=bar", "-e", "WITH=equals=sign", "-l", "app=web",
			"-v", vol + ":/data", "-v", bind + ":/bind:ro", "-v", "/anon", "--tmpfs", "/run/x:size=1m",
			"--restart", "unless-stopped", "--network", n1, "--ip", "172.31.250.10", "--network-alias", "web",
			"--memory", "64m", "--cap-add", "NET_ADMIN", "--add-host", "db.local:10.0.0.5", "--dns", "9.9.9.9",
			"--health-cmd", "true", "--health-interval", "5s", "--log-opt", "max-size=1m", "nginx:alpine"},
		"process": {"--init", "--user", "nobody", "--workdir", "/tmp", "--entrypoint", "/bin/sh", "--hostname", "custom-host",
			"--cap-drop", "ALL", "--read-only", "--ulimit", "nofile=1024:2048", "--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
			"--security-opt", "no-new-privileges", "--stop-signal", "SIGTERM", "--stop-timeout", "3",
			"alpine:3", "-c", "sleep 3600"},
		"isolated": {"--network", "none", "--device", "/dev/null:/dev/xnull", "--pids-limit", "50", "alpine:3", "sleep", "3600"},
		"host-net": {"--network", "host", "alpine:3", "sleep", "3600"},
	}
	for shape, args := range shapes {
		t.Run(shape, func(t *testing.T) {
			name := unique(t, shape)
			docker(t, append([]string{"run", "-d", "--name", name}, args...)...)
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
			if shape == "web" {
				docker(t, "network", "connect", "--alias", "w2", n2, name)
				docker(t, "exec", name, "sh", "-c", "echo kept > /anon/file")
			}
			before, err := c.InspectContainer(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := FromInspect(before)
			if err != nil {
				t.Fatal(err)
			}
			newID, err := Swap(ctx, c, before, spec, SwapOptions{})
			if err != nil {
				t.Fatal(err)
			}
			after, err := c.InspectContainer(ctx, newID)
			if err != nil {
				t.Fatal(err)
			}
			if d := diff(comparable(t, before), comparable(t, after)); d != "" {
				t.Fatalf("the new container differs:\n%s", d)
			}
			if shape == "web" {
				if got := docker(t, "exec", name, "cat", "/anon/file"); got != "kept" {
					t.Fatalf("the anonymous volume's data: %q", got)
				}
			}
			if _, err := c.InspectContainer(ctx, str(before["Id"])); !engine.IsNotFound(err) {
				t.Fatalf("the old container is still there: %v", err)
			}
		})
	}
}

// TestStoppedStaysStopped: a stopped container is made again, not started.
func TestStoppedStaysStopped(t *testing.T) {
	c := client(t)
	name := unique(t, "stopped")
	docker(t, "create", "--name", name, "alpine:3", "true")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	before, _ := c.InspectContainer(ctx, name)
	spec, _ := FromInspect(before)
	newID, err := Swap(ctx, c, before, spec, SwapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := c.InspectContainer(ctx, newID)
	if asDoc(after["State"])["Running"] == true || str(after["Name"]) != "/"+name {
		t.Fatalf("after: %v %v", after["State"], after["Name"])
	}
}

// TestFailedStartPutsTheOldOneBack breaks the new container on purpose.
func TestFailedStartPutsTheOldOneBack(t *testing.T) {
	c := client(t)
	name := unique(t, "rollback")
	docker(t, "run", "-d", "--name", name, "alpine:3", "sleep", "3600")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	before, _ := c.InspectContainer(ctx, name)
	spec, _ := FromInspect(before)
	spec.Config["Cmd"] = []any{"/no/such/program"}
	if _, err := Swap(ctx, c, before, spec, SwapOptions{}); err == nil {
		t.Fatal("the broken container started")
	}
	back, err := c.InspectContainer(ctx, name)
	if err != nil || str(back["Id"]) != str(before["Id"]) || asDoc(back["State"])["Running"] != true {
		t.Fatalf("the old container is not back: %v", err)
	}
	all, _ := c.ListContainers(ctx)
	for _, x := range all {
		for _, n := range list(x["Names"]) {
			if strings.Contains(str(n), name+".old") {
				t.Fatalf("a backup is left: %v", n)
			}
		}
	}
}

// TestInterruptPutsTheOldOneBack cancels the swap (as Ctrl-C does) at each
// step and checks the old container is back as it was.
func TestInterruptPutsTheOldOneBack(t *testing.T) {
	c := client(t)
	for _, at := range []string{"stopping", "creating", "starting", "checking"} {
		t.Run(at, func(t *testing.T) {
			name := unique(t, "interrupt")
			docker(t, "run", "-d", "--name", name, "alpine:3", "sleep", "3600")
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
			before, _ := c.InspectContainer(ctx, name)
			spec, _ := FromInspect(before)
			spec.Config["Env"] = append(list(spec.Config["Env"]), "CHANGED=1")
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, err := Swap(cctx, c, before, spec, SwapOptions{
				Log: func(s string) {
					if strings.HasPrefix(s, at) {
						cancel()
					}
				},
				Started: func(string) error {
					if at == "checking" {
						cancel()
					}
					<-cctx.Done()
					return cctx.Err()
				},
			})
			if err == nil {
				t.Fatal("the swap went through although it was cancelled")
			}
			back, err := c.InspectContainer(ctx, name)
			if err != nil || str(back["Id"]) != str(before["Id"]) || asDoc(back["State"])["Running"] != true {
				t.Fatalf("the old container is not back: %v", err)
			}
			all, _ := c.ListContainers(ctx)
			for _, x := range all {
				for _, n := range list(x["Names"]) {
					if strings.Contains(str(n), name) && str(x["Id"]) != str(before["Id"]) {
						t.Fatalf("left over: %v", n)
					}
				}
			}
		})
	}
}
