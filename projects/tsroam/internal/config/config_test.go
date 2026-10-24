package config

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	l, err := Parse("trusted 192.168.1.0/24 gateway-mac 3C:7C:3F:12:34:56 probe 192.168.1.5:22", "t")
	if err != nil || l.String() != "trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22" {
		t.Fatal(l, err)
	}
	if l, err := Parse("10.10.0.0/16 gateway 10.10.0.254 gateway-mac 00:11:22:33:44:55", "--trusted"); err != nil || l.Gateway.String() != "10.10.0.254" {
		t.Fatal(l, err)
	}
	for line, want := range map[string]string{
		"trusted":                "needs a prefix",
		"trusted 192.168.1.0/24": "no fingerprint",
		"trusted 192.168.1.1/24 gateway-mac 00:11:22:33:44:55":                                         "host bits",
		"trusted 0.0.0.0/0 probe 1.2.3.4:5":                                                            "everything",
		"trusted 192.168.1.0/24 probe 10.0.0.1:22":                                                     "with the address in",
		"trusted 192.168.1.0/24 probe 192.168.1.1":                                                     "address:port",
		"trusted 192.168.1.0/24 gateway-mac nope":                                                      "not a MAC",
		"trusted 192.168.1.0/24 gateway 192.168.1.1 probe 192.168.1.1:22":                              "only used with gateway-mac",
		"trusted 192.168.1.0/24 colour blue":                                                           "unknown",
		"trusted 192.168.1.0/24 gateway-mac":                                                           "needs a value",
		"trusted fd00:1::/64 probe [fd00:1::5]:22":                                                     "only IPv4",
		"trusted 192.168.1.0/24 probe 192.168.1.5:22 probe 192.168.1.6:22":                             "probe given twice",
		"trusted 192.168.1.0/24 gateway 192.168.1.1 gateway 192.168.1.2 gateway-mac 00:11:22:33:44:55": "gateway given twice",
	} {
		if _, err := Parse(line, "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", line, err)
		}
	}
}

func TestRead(t *testing.T) {
	c, err := Read(strings.NewReader(`# home
exit-node 100.64.0.1
kill-switch on
allow-lan on
trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56   # the router

trusted	10.10.0.0/16 probe 10.10.0.5:22
`), "f")
	if err != nil || c.ExitNode != "100.64.0.1" || !c.KillSwitch || len(c.Trusted) != 2 || c.Trusted[1].Source != "f:7" || !c.AllowLAN {
		t.Fatal(c, err)
	}
	for text, want := range map[string]string{
		"trusted 10.0.0.0/8 probe 10.0.0.1:22\ntrusted 10.1.0.0/16 probe 10.1.0.1:22\n": "overlap",
		"route 10.0.0.0/8\n":         "unknown",
		"exit-node\n":                "takes one value",
		"exit-node a b\n":            "takes one value",
		"exit-node --socket=/x\n":    "is not a node",
		"exit-node a\nexit-node b\n": "given twice",
		"kill-switch yes\n":          "on or off",
		"trusted 192.168.1.0/24\n":   "no fingerprint",
	} {
		if _, err := Read(strings.NewReader(text), "f"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", text, err, want)
		}
	}
}

func TestReadFileMissing(t *testing.T) {
	c, err := ReadFile(t.TempDir() + "/none")
	if err != nil || c.ExitNode != "" || c.Trusted != nil {
		t.Fatal(c, err)
	}
}
