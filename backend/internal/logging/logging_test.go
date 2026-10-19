package logging

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"Authorization: Bearer abc.def-ghi":               "abc.def-ghi",
		"dial https://user:hunter2@example.com/x":         "hunter2",
		"GET https://api.example.com/v1?token=s3cr3t&a=1": "s3cr3t",
		"https://x.example/cb?api_key=K123":               "K123",
	}
	for in, secret := range cases {
		out := Redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("Redact(%q) = %q still contains %q", in, out, secret)
		}
	}
	if got := Redact("plain message"); got != "plain message" {
		t.Errorf("modified harmless text: %q", got)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	for _, k := range []string{"Authorization", "X-Api-Key", "client_secret", "DB_PASSWORD"} {
		if !IsSensitiveKey(k) {
			t.Errorf("%s should be sensitive", k)
		}
	}
	if IsSensitiveKey("Content-Type") {
		t.Error("Content-Type is not sensitive")
	}
}
