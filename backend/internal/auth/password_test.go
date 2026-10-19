package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct-horse-9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected format %q", h)
	}
	if ok, _ := VerifyPassword("correct-horse-9", h); !ok {
		t.Fatal("correct password rejected")
	}
	if ok, _ := VerifyPassword("wrong-horse-9", h); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct-horse-9")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	if _, err := VerifyPassword("x", "$2a$10$bcrypt"); err == nil {
		t.Fatal("foreign hash formats must be rejected")
	}
}

func TestPasswordProblem(t *testing.T) {
	for _, bad := range []string{"short1", "onlyletterslong", "1234567890", strings.Repeat("a1", 70)} {
		if PasswordProblem(bad) == "" {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{"correct-horse", "abcdefghi1", "pass phrase with spaces"} {
		if msg := PasswordProblem(good); msg != "" {
			t.Errorf("rejected %q: %s", good, msg)
		}
	}
}

func TestTokens(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == b || len(a) < 40 {
		t.Fatalf("weak tokens %q %q", a, b)
	}
	if string(HashToken(a)) == a || len(HashToken(a)) != 32 {
		t.Fatal("token hash must be a SHA-256 digest")
	}
}
