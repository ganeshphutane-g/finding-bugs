package marker

import "testing"

func TestLast(t *testing.T) {
	text := "hello " + Make("issue", "h/repo-1#2 state=open") + "\n\n" + Make("issue", "h/repo-1#3 state=closed")
	if v, ok := Last(text, "issue"); !ok || v != "h/repo-1#3 state=closed" {
		t.Fatalf("Last = %q, %v", v, ok)
	}
	if _, ok := Last(text, "comment"); ok {
		t.Fatal("found a comment marker")
	}
	if _, ok := Last(Defuse(text), "issue"); ok {
		t.Fatal("a defused marker was found")
	}
	if _, ok := Last("<!-- issuemove:issue=a\nb -->", "issue"); ok {
		t.Fatal("a marker across lines was found")
	}
}
