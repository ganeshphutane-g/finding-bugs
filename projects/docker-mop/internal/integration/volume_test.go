package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/volume"
)

// trickyTar builds a volume's worth of awkward content under "seed/".
func trickyTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	big := make([]byte, 5<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	add := func(h *tar.Header, body []byte) {
		h.ModTime = mtime
		h.Format = tar.FormatPAX
		if body != nil {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != nil {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(&tar.Header{Name: "seed/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 1000, Gid: 1000}, nil)
	add(&tar.Header{Name: "seed/app/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000}, nil)
	add(&tar.Header{Name: "seed/app/config.json", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1000, Gid: 2000}, []byte(`{"ok":true}`))
	add(&tar.Header{Name: "seed/app/run.sh", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0}, []byte("#!/bin/sh\n"))
	add(&tar.Header{Name: "seed/app/current", Typeflag: tar.TypeSymlink, Linkname: "config.json", Mode: 0o777, Uid: 1000, Gid: 1000}, nil)
	add(&tar.Header{Name: "seed/app/dangling", Typeflag: tar.TypeSymlink, Linkname: "/does/not/exist", Mode: 0o777}, nil)
	add(&tar.Header{Name: "seed/app/hardlink.json", Typeflag: tar.TypeLink, Linkname: "seed/app/config.json"}, nil)
	add(&tar.Header{Name: "seed/empty dir/", Typeflag: tar.TypeDir, Mode: 0o1777, Uid: 33, Gid: 33}, nil)
	add(&tar.Header{Name: "seed/ünïcode name.txt", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 65534, Gid: 65534}, []byte("hello"))
	add(&tar.Header{Name: "seed/empty.txt", Typeflag: tar.TypeReg, Mode: 0o644}, []byte{})
	add(&tar.Header{Name: "seed/data/big.bin", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 999, Gid: 999}, big)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedVolume creates a labelled volume and fills it with content.
func seedVolume(t *testing.T, run, name string, content []byte) {
	t.Helper()
	repo := "mop-" + run
	buildImage(t, run, repo+":seed", "seed")
	docker(t, "volume", "create", "--label", testLabel+"="+run, "--label", "app=demo", name)
	id := docker(t, "create", "--label", testLabel+"="+run, "-v", name+":/seed", repo+":seed")
	defer dockerQuiet("rm", "-f", id)
	cmd := exec.Command("docker", "cp", "-", id+":/")
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding %s: %v\n%s", name, err, out)
	}
}

// listing returns "path mode uid:gid size target" lines for a volume, read
// independently of docker-mop's own verification.
func listing(t *testing.T, run, name string) string {
	t.Helper()
	id := docker(t, "create", "--label", testLabel+"="+run, "-v", name+":/seed", "mop-"+run+":seed")
	defer dockerQuiet("rm", "-f", id)
	out, err := exec.Command("docker", "cp", id+":/seed", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var lines []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		lines = append(lines, fmt.Sprintf("%s %c %04o %d:%d %d %s %s",
			h.Name, h.Typeflag, h.Mode&0o7777, h.Uid, h.Gid, len(body), h.Linkname, h.ModTime.UTC().Format(time.RFC3339)))
	}
	return strings.Join(lines, "\n")
}

func volumeLabels(t *testing.T, name string) string {
	t.Helper()
	return docker(t, "volume", "inspect", "-f", "{{json .Labels}}", name)
}

func TestVolumeCopyIsExact(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	src, dst := "mop-"+run+"-src", "mop-"+run+"-dst"
	seedVolume(t, run, src, trickyTar(t))

	rep, err := volume.Copy(context.Background(), c, src, dst, volume.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 5 {
		t.Errorf("expected 5 regular files, got %d", rep.Files)
	}
	if rep.Bytes < 5<<20 {
		t.Errorf("expected at least 5 MiB copied, got %d", rep.Bytes)
	}

	want, got := listing(t, run, src), listing(t, run, dst)
	// The archive names the mount point after the source's mount path, which
	// is the same here, so the listings must be identical.
	if want != got {
		t.Fatalf("copy differs from source:\n--- source\n%s\n--- copy\n%s", want, got)
	}
	if !strings.Contains(want, "seed/app/run.sh 0 4755 0:0") {
		t.Errorf("setuid bit not present in the source listing; seeding went wrong:\n%s", want)
	}
	if volumeLabels(t, src) != volumeLabels(t, dst) {
		t.Errorf("labels differ: %s vs %s", volumeLabels(t, src), volumeLabels(t, dst))
	}
	assertNoHelpers(t)
}

func TestVolumeRename(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	src := "mop-" + run + "-old"
	dst := "mop-" + run + "proj_data"
	seedVolume(t, run, src, trickyTar(t))
	before := listing(t, run, src)

	_, err := volume.Rename(context.Background(), c, src, dst, volume.Options{ComposeProject: "mop-" + run + "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "volume", "inspect", src).CombinedOutput(); err == nil {
		t.Fatalf("old volume still exists: %s", out)
	}
	if after := listing(t, run, dst); after != before {
		t.Fatalf("renamed volume differs:\n--- before\n%s\n--- after\n%s", before, after)
	}
	labels := volumeLabels(t, dst)
	if !strings.Contains(labels, `"com.docker.compose.project":"mop-`+run+`proj"`) || !strings.Contains(labels, `"com.docker.compose.volume":"data"`) || !strings.Contains(labels, `"app":"demo"`) {
		t.Fatalf("unexpected labels: %s", labels)
	}
	assertNoHelpers(t)
}

func TestVolumeRenameRefusesWhenInUse(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	src, dst := "mop-"+run+"-busy", "mop-"+run+"-new"
	seedVolume(t, run, src, trickyTar(t))
	docker(t, "create", "--name", "mop-"+run+"-user", "--label", testLabel+"="+run, "-v", src+":/data", "mop-"+run+":seed")

	_, err := volume.Rename(context.Background(), c, src, dst, volume.Options{})
	if err == nil || !strings.Contains(err.Error(), "mop-"+run+"-user") {
		t.Fatalf("expected a refusal naming the container, got %v", err)
	}
	if out, err := exec.Command("docker", "volume", "inspect", dst).CombinedOutput(); err == nil {
		t.Fatalf("target volume was created anyway: %s", out)
	}
	// Copying is fine while the user is stopped...
	if _, err := volume.Copy(context.Background(), c, src, dst, volume.Options{}); err != nil {
		t.Fatalf("copy with a stopped user: %v", err)
	}
	// ...but not while it runs.
	running := docker(t, "run", "-d", "--label", testLabel+"="+run, "-v", src+":/data", "mop-"+run+":seed", "sleep")
	defer dockerQuiet("rm", "-f", running)
	if _, err := volume.Copy(context.Background(), c, src, "mop-"+run+"-other", volume.Options{}); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected a refusal for a running user, got %v", err)
	}
}

func TestVolumeCopyRefusals(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	a, b := "mop-"+run+"-a", "mop-"+run+"-b"
	docker(t, "volume", "create", "--label", testLabel+"="+run, a)
	docker(t, "volume", "create", "--label", testLabel+"="+run, b)
	withOpts := "mop-" + run + "-tmpfs"
	docker(t, "volume", "create", "--label", testLabel+"="+run, "--opt", "type=tmpfs", "--opt", "device=tmpfs", withOpts)

	cases := map[string]struct {
		src, dst, want string
	}{
		"target exists":   {a, b, "already exists"},
		"missing source":  {"mop-" + run + "-missing", "mop-" + run + "-x", "not found"},
		"same volume":     {a, a, "same volume"},
		"driver options":  {withOpts, "mop-" + run + "-y", "driver options"},
		"compose project": {a, "mop-" + run + "-z", "must look like"},
	}
	for name, tc := range cases {
		opts := volume.Options{}
		if name == "compose project" {
			opts.ComposeProject = "other"
		}
		_, err := volume.Copy(context.Background(), c, tc.src, tc.dst, opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected error containing %q, got %v", name, tc.want, err)
		}
	}
	for _, v := range []string{"mop-" + run + "-x", "mop-" + run + "-y", "mop-" + run + "-z"} {
		if out, err := exec.Command("docker", "volume", "inspect", v).CombinedOutput(); err == nil {
			t.Errorf("volume %s was created by a refused copy: %s", v, out)
		}
	}
}

func assertNoHelpers(t *testing.T) {
	t.Helper()
	if out := docker(t, "ps", "-aq", "--filter", "label="+volume.HelperLabel); out != "" {
		t.Fatalf("helper containers left behind: %s", out)
	}
}

func TestVolumeComposeLabelsDroppedWithoutProject(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	src, dst := "mop-"+run+"_data", "mop-"+run+"-copy"
	docker(t, "volume", "create", "--label", testLabel+"="+run,
		"--label", "com.docker.compose.project=mop-"+run, "--label", "com.docker.compose.volume=data", src)

	var warnings []string
	_, err := volume.Copy(context.Background(), c, src, dst, volume.Options{Warn: func(s string) { warnings = append(warnings, s) }})
	if err != nil {
		t.Fatal(err)
	}
	labels := volumeLabels(t, dst)
	if strings.Contains(labels, "com.docker.compose") || !strings.Contains(labels, testLabel) {
		t.Fatalf("unexpected labels on the copy: %s", labels)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "--compose-project") {
		t.Fatalf("expected one warning about --compose-project, got %v", warnings)
	}
}

func TestVolumeRenameReportsLeftoverHelper(t *testing.T) {
	c := requireDocker(t)
	run := newRunID(t)
	cleanupRun(t, run)
	src := "mop-" + run + "-src"
	docker(t, "volume", "create", "--label", testLabel+"="+run, src)
	buildImage(t, run, "mop-"+run+":seed", "seed")
	// Simulate a helper left behind by an interrupted run.
	helper := docker(t, "create", "--label", volume.HelperLabel+"=1", "--label", testLabel+"="+run,
		"-v", src+":/volume", "mop-"+run+":seed")
	defer dockerQuiet("rm", "-f", helper)

	_, err := volume.Rename(context.Background(), c, src, "mop-"+run+"-dst", volume.Options{})
	if err == nil || !strings.Contains(err.Error(), "left over from an interrupted run") {
		t.Fatalf("expected the leftover helper to be reported, got %v", err)
	}
}
