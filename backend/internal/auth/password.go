// Package auth provides password hashing, session token generation and
// role-based authorization helpers.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP-recommended minimums: m=19 MiB, t=2, p=1;
// we use 64 MiB / t=1 / p=4, the RFC 9106 "second recommended" profile).
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a PHC argon2id hash in constant time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("malformed argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a login names an unknown user so the
// response time does not reveal whether the account exists.
var dummyHash, _ = HashPassword("timing-equalisation-placeholder")

// VerifyDummy burns the same CPU as a real verification.
func VerifyDummy(password string) { _, _ = VerifyPassword(password, dummyHash) }

// PasswordProblem checks the password policy and returns a user-facing
// explanation of what is wrong, or "" if the password is acceptable: 10-128
// characters with at least one letter and one digit, symbol or space. Length
// matters more than composition rules, so the bar on composition is kept low.
func PasswordProblem(pw string) string {
	n := utf8.RuneCountInString(pw)
	if n < 10 {
		return "Password must be at least 10 characters."
	}
	if n > 128 {
		return "Password must be at most 128 characters."
	}
	var letter, other bool
	for _, r := range pw {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsDigit(r), unicode.IsPunct(r), unicode.IsSymbol(r), unicode.IsSpace(r):
			other = true
		}
	}
	if !letter || !other {
		return "Password must contain a letter and a number or symbol."
	}
	return ""
}

// NewToken returns a URL-safe random token with 256 bits of entropy.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the SHA-256 of a session token. Only the hash is stored,
// so a database leak does not yield usable session cookies.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
