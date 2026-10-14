package plan

import (
	"reflect"
	"testing"
)

func TestCustomHBA(t *testing.T) {
	rules := []string{
		"local all all trust",
		"host all all 127.0.0.1/32 trust",
		"host all all ::1/128 trust",
		"local replication all trust",
		"host replication all 127.0.0.1/32 trust",
		"host all all all scram-sha-256",
		"hostssl app app 10.0.0.0/8 scram-sha-256",
	}
	if got := customHBA(rules); !reflect.DeepEqual(got, []string{"hostssl app app 10.0.0.0/8 scram-sha-256"}) {
		t.Fatalf("got %q", got)
	}
}

func TestParseDF(t *testing.T) {
	out := "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda 263174212 22000000 18000000 55% /var/lib/postgresql/data\n"
	if got := parseDF(out); got != 18000000*1024 {
		t.Fatalf("got %d", got)
	}
}

func TestNewNames(t *testing.T) {
	if got := NewVolumeName("app_pgdata", 18); got != "app_pgdata_pg18" {
		t.Error(got)
	}
	if got := NewBindPath("/srv/app/db/", 18); got != "/srv/app/db-pg18" {
		t.Error(got)
	}
}
