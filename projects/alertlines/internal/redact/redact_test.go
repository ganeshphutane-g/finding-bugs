package redact

import (
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	r, _ := New(true, nil)
	for _, c := range [][2]string{
		{"ERROR payment failed: card declined (token=tok_live_abc123)", "ERROR payment failed: card declined (token=[redacted])"},
		{`login failed user=bob password="correct horse battery" ip=10.0.0.1`, `login failed user=bob password="[redacted]" ip=10.0.0.1`},
		{`{"api_key": "abcd1234", "user": "x"}`, `{"api_key": "[redacted]", "user": "x"}`},
		{`{"pin": 1, "db_password": 12345}`, `{"pin": 1, "db_password": [redacted]}`},
		{`"body":"{\"password\":\"hunter2\",\"user\":\"bob\"}"`, `"body":"{\"password\":\"[redacted]\",\"user\":\"bob\"}"`},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig12345678", "Authorization: [redacted]"},
		{"authorization: SSWS 00abcdefghijklmnop", "authorization: [redacted]"},
		{"Cookie: _ga=1; sid=abc; PHPSESSID=xyz", "Cookie: [redacted]"},
		{"connect postgres://app:s3cret@db:5432/app failed", "connect postgres://app:[redacted]@db:5432/app failed"},
		{"connect postgres://app:p@ss@db/app", "connect postgres://app:[redacted]@db/app"},
		{"dial app:s3cret@tcp(db:3306)/shop", "dial app:[redacted]@tcp(db:3306)/shop"},
		{"pushed with ghp_abcdefghijklmnopqrstuvwxyz0123", "pushed with [redacted]"},
		{"keys AKIAIOSFODNN7EXAMPLE ASIAIOSFODNN7EXAMPLE", "keys [redacted] [redacted]"},
		{"GET /maps?key=AIzaSyA1234567890abcdefghijklmnopqrst", "GET /maps?key=[redacted]"},
		{"GET /blob?sv=2021&sig=abc%2Fdef&se=x", "GET /blob?sv=2021&sig=[redacted]&se=x"},
		{"posting to https://hooks.slack.com/services/T000/B000/XXXX", "posting to https://[redacted]"},
		{"jwt eyJhbGciOiJub25lIn0.eyJzdWIiOiIxMjM0In0.", "jwt [redacted]"},
		{"-----BEGIN RSA PRIVATE KEY----- MIIEowIBAAKCAQEA abc -----END RSA PRIVATE KEY----- loaded", "[redacted] loaded"},
		{`{"private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----\n", "x": 1}`, `{"private_key": "[redacted]", "x": 1}`},
		{"session_id=abc; theme=dark", "session_id=[redacted]; theme=dark"},
		{"password: hunter2 rejected", "password: [redacted] rejected"},
		// Left alone: words that only contain a secret name, and prose.
		{"GET /orders/17 200 12ms", "GET /orders/17 200 12ms"},
		{"ERROR auth: connection refused", "ERROR auth: connection refused"},
		{`msg="token: expired"`, `msg="token: expired"`},
		{"tokens_used=1200 max_tokens=4096 author=ann auth_method=oidc password_reset_sent=true cookie_consent=yes",
			"tokens_used=1200 max_tokens=4096 author=ann auth_method=oidc password_reset_sent=true cookie_consent=yes"},
	} {
		if got := r.Line(c[0]); got != c[1] {
			t.Errorf("%s\n got %s\nwant %s", c[0], got, c[1])
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
	if strings.Contains(Mask, " ") {
		t.Fatal("the mask must not break the line into fields")
	}
}
