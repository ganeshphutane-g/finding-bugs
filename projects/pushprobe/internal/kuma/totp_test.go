package kuma

import (
	"testing"
	"time"
)

// RFC 6238 test vector (SHA-1, secret "12345678901234567890"), last 6 digits.
func TestTOTP(t *testing.T) {
	for at, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924"} {
		got, err := totp("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(at, 0))
		if err != nil || got != want {
			t.Errorf("totp at %d = %q, %v; want %q", at, got, err, want)
		}
	}
	if _, err := totp("not base32!", time.Now()); err == nil {
		t.Error("bad secret accepted")
	}
}
