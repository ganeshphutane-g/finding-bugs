package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var known = map[string]map[string]bool{
	"up":   {"mkdir": true, "run": true},
	"down": {"down": true},
	"stop": {},
}

func TestParseArgs(t *testing.T) {
	// As Compose v5.6.0 called it in the lab.
	c, err := ParseArgs(strings.Fields("compose --project-name=demo up --mkdir=./a --mkdir=./b prep"), known)
	if err != nil || c.Project != "demo" || c.Command != "up" || c.Service != "prep" || strings.Join(c.Options["mkdir"], ",") != "./a,./b" {
		t.Fatalf("%+v %v", c, err)
	}
	// A value with spaces and "=" is one argument.
	c, err = ParseArgs([]string{"compose", "--project-name", "demo", "up", "--run=echo a=b c", "prep"}, known)
	if err != nil || c.Project != "demo" || c.Options["run"][0] != "echo a=b c" {
		t.Fatalf("%+v %v", c, err)
	}
	if v, err := c.One("run"); v != "echo a=b c" || err != nil {
		t.Errorf("One: %q %v", v, err)
	}
	if c, err := ParseArgs([]string{"compose", "metadata"}, known); err != nil || c.Command != "metadata" {
		t.Fatalf("%+v %v", c, err)
	}
	for args, want := range map[string]string{
		"up":                                        "Compose calls it",
		"compose":                                   "no command",
		"compose --project-name=d frob x":           `unknown command "frob"`,
		"compose --project-name=d up --nope=1 s":    `unknown option "nope"`,
		"compose --project-name=d up --run s":       `option "run" needs a value`,
		"compose --project-name=d up":               "no service name",
		"compose --project-name=d up a b":           `unexpected "b"`,
		"compose --project-name=d down --mkdir=x s": `unknown option "mkdir" for down`,
		"compose --zap up s":                        "unknown flag --zap",
	} {
		if _, err := ParseArgs(strings.Fields(args), known); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", args, err, want)
		}
	}
	c, _ = ParseArgs(strings.Fields("compose --project-name=d up --run=a --run=b s"), known)
	if _, err := c.One("run"); err == nil {
		t.Error("two values for one option")
	}
}

func TestOut(t *testing.T) {
	var b bytes.Buffer
	o := &Out{W: &b}
	o.Info("waiting for %s", "x")
	o.RawSetEnv("K", "v=1")
	o.Error(`bad "thing"`)
	var got []map[string]string
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var m map[string]string
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		got = append(got, m)
	}
	if got[0]["type"] != "info" || got[1]["message"] != "K=v=1" || got[2]["type"] != "error" || got[2]["message"] != `bad "thing"` {
		t.Errorf("%v", got)
	}
}

func TestKnown(t *testing.T) {
	m := Metadata{Up: Command{Parameters: []Param{{Name: "a"}}}, Stop: &Command{Parameters: []Param{{Name: "s"}}}}
	k := m.Known()
	if !k["up"]["a"] || !k["stop"]["s"] || k["down"]["a"] {
		t.Errorf("%v", k)
	}
}
