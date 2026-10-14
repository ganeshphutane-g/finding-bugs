package upgrade

import (
	"reflect"
	"testing"
)

func TestSettingValue(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"work_mem", "8MB"}:                                             "'8MB'",
		{"search_path", `"$user", public, extra`}:                       `'$user', 'public', 'extra'`,
		{"shared_preload_libraries", "pg_stat_statements,auto_explain"}: `'pg_stat_statements', 'auto_explain'`,
		{"search_path", `"a""b", "with space"`}:                         `'a"b', 'with space'`,
		{"log_destination", "stderr,csvlog"}:                            `'stderr,csvlog'`,
		{"search_path", ""}:                                             `''`,
	} {
		if got := settingValue(in[0], in[1]); got != want {
			t.Errorf("settingValue(%q, %q) = %s, want %s", in[0], in[1], got, want)
		}
	}
	if got := splitList(`"$user", public`); !reflect.DeepEqual(got, []string{"$user", "public"}) {
		t.Errorf("splitList: %q", got)
	}
}
