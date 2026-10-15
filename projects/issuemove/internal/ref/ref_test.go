package ref

import (
	"reflect"
	"testing"
)

func TestParseSource(t *testing.T) {
	for in, want := range map[string]Source{
		"me/app":           {Owner: "me", Repo: "app"},
		"me/app#12":        {Owner: "me", Repo: "app", Numbers: []int64{12}},
		"me/app#3,7-9,3":   {Owner: "me", Repo: "app", Numbers: []int64{3, 7, 8, 9}},
		"org.x/my-app#1-2": {Owner: "org.x", Repo: "my-app", Numbers: []int64{1, 2}},
	} {
		got, err := ParseSource(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseSource(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"app", "/app", "me/", "me/app#", "me/app#x", "me/app#5-2", "me/app#0", "a/b/c", "me/app#1-99999"} {
		if _, err := ParseSource(bad); err == nil {
			t.Errorf("ParseSource(%q) should fail", bad)
		}
	}
}
