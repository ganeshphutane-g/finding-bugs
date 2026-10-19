package redact

import "testing"

func TestBuiltin(t *testing.T) {
	r, _ := New(true, nil)
	for in, want := range map[string]string{
		"ERROR payment failed: card declined (token=tok_live_abc123)":            "ERROR payment failed: card declined (token=[redacted])",
		`login failed user=bob password="hunter2" ip=10.0.0.1`:                   `login failed user=bob password="[redacted]" ip=10.0.0.1`,
		`{"api_key": "abcd1234", "user": "x"}`:                                   `{"api_key": "[redacted]", "user": "x"}`,
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig12345678": "Authorization: Bearer [redacted]",
		"connect postgres://app:s3cret@db:5432/app failed":                       "connect postgres://app:[redacted]@db:5432/app failed",
		"pushed with ghp_abcdefghijklmnopqrstuvwxyz0123":                         "pushed with [redacted]",
		"key AKIAIOSFODNN7EXAMPLE used":                                          "key [redacted] used",
		"GET /orders/17 200 12ms":                                                "GET /orders/17 200 12ms",
		"session_id=abc; theme=dark":                                             "session_id=[redacted]; theme=dark",
	} {
		if got := r.Line(in); got != want {
			t.Errorf("%s\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestExtra(t *testing.T) {
	r, err := New(false, []string{`card \d{4}-\d{4}-\d{4}-(\d{4})`, `\b\d{3}-\d{2}-\d{4}\b`})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Line("card 1111-2222-3333-4444 for 123-45-6789 token=x"); got != "card 1111-2222-3333-[redacted] for [redacted] token=x" {
		t.Fatalf("%s", got)
	}
	if _, err := New(true, []string{"("}); err == nil {
		t.Fatal("a bad pattern was accepted")
	}
}
