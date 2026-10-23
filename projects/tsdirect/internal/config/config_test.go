package config

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	l, err := Parse("lan 192.168.1.0/24 gateway-mac 3C:7C:3F:12:34:56 probe 192.168.1.5:22", "t")
	if err != nil || l.String() != "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22" {
		t.Fatal(l, err)
	}
	if l, err := Parse("10.10.0.0/16 gateway 10.10.0.254 gateway-mac 00:11:22:33:44:55", "--lan"); err != nil || l.Gateway.String() != "10.10.0.254" {
		t.Fatal(l, err)
	}
	for line, want := range map[string]string{
		"lan":                "needs a prefix",
		"lan 192.168.1.0/24": "no fingerprint",
		"lan 192.168.1.1/24 gateway-mac 00:11:22:33:44:55":                                         "host bits",
		"lan 0.0.0.0/0 probe 1.2.3.4:5":                                                            "everything",
		"lan 192.168.1.0/24 probe 10.0.0.1:22":                                                     "with the address in",
		"lan 192.168.1.0/24 probe 192.168.1.1":                                                     "address:port",
		"lan 192.168.1.0/24 gateway-mac nope":                                                      "not a MAC",
		"lan 192.168.1.0/24 gateway 192.168.1.1 probe 192.168.1.1:22":                              "only used with gateway-mac",
		"lan 192.168.1.0/24 colour blue":                                                           "unknown",
		"lan 192.168.1.0/24 gateway-mac":                                                           "needs a value",
		"lan fd00:1::/64 probe [fd00:1::5]:22":                                                     "only IPv4",
		"lan 192.168.1.0/24 probe 192.168.1.5:22 probe 192.168.1.6:22":                             "probe given twice",
		"lan 192.168.1.0/24 gateway 192.168.1.1 gateway 192.168.1.2 gateway-mac 00:11:22:33:44:55": "gateway given twice",
	} {
		if _, err := Parse(line, "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", line, err)
		}
	}
}

func TestRead(t *testing.T) {
	lans, err := Read(strings.NewReader(`# home
lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56   # the router

lan 10.10.0.0/16 probe 10.10.0.5:22
`), "f")
	if err != nil || len(lans) != 2 || lans[1].Source != "f:4" {
		t.Fatal(lans, err)
	}
	if _, err := Read(strings.NewReader("lan 10.0.0.0/8 probe 10.0.0.1:22\nlan 10.1.0.0/16 probe 10.1.0.1:22\n"), "f"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal(err)
	}
	if lans, err := Read(strings.NewReader("lan\t10.0.0.0/8\tprobe 10.0.0.1:22\n"), "f"); err != nil || len(lans) != 1 {
		t.Fatal("a tab after lan:", lans, err)
	}
	if _, err := Read(strings.NewReader("route 10.0.0.0/8\n"), "f"); err == nil {
		t.Fatal("accepted")
	}
}
