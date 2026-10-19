// Package logging configures structured JSON logging and provides helpers for
// keeping sensitive values out of log output.
package logging

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// New returns a JSON slog logger tagged with the component name.
func New(level, component string) *slog.Logger {
	return NewWithWriter(os.Stdout, level, component)
}

func NewWithWriter(w io.Writer, level, component string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l})
	return slog.New(h).With("component", component)
}

// sensitiveKeys are header/field names whose values must never be logged.
var sensitiveKeys = []string{
	"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key",
	"api-key", "apikey", "token", "secret", "password", "passwd", "private-key",
	"access_token", "refresh_token", "client_secret",
}

// IsSensitiveKey reports whether a header or field name looks like it carries
// a credential.
func IsSensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

var (
	bearerRe   = regexp.MustCompile(`(?i)(bearer|basic)\s+[A-Za-z0-9\-._~+/=]+`)
	urlCredRe  = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)
	queryKeyRe = regexp.MustCompile(`(?i)([?&](?:token|key|apikey|api_key|secret|signature|sig|password)=)[^&\s]+`)
)

// Redact masks credential-looking substrings in free text (error messages,
// URLs) before they are logged or persisted.
func Redact(s string) string {
	s = bearerRe.ReplaceAllString(s, "$1 [REDACTED]")
	s = urlCredRe.ReplaceAllString(s, "://[REDACTED]@")
	s = queryKeyRe.ReplaceAllString(s, "${1}[REDACTED]")
	return s
}
