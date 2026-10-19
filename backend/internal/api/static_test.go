package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taskflow/taskflow/internal/api"
	"github.com/taskflow/taskflow/internal/config"
	"github.com/taskflow/taskflow/internal/metrics"
	. "github.com/taskflow/taskflow/internal/testutil"
)

func TestServesSPAWithFallbackAndStrictCSP(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>spa</html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app-123.js"), []byte("console.log(1)"), 0o644)
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	_ = os.WriteFile(secret, []byte("do-not-serve"), 0o644)
	t.Cleanup(func() { _ = os.Remove(secret) })

	cfg := &config.Config{MaxRequestBytes: 1 << 20, StaticDir: dir}
	s := api.New(api.Deps{Config: cfg, Log: Logger(), Registry: metrics.Registry()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	get := func(p string) (*http.Response, string) {
		t.Helper()
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r, string(b)
	}

	r, body := get("/jobs/123")
	if r.StatusCode != 200 || body != "<html>spa</html>" {
		t.Fatalf("client route should serve index.html: %d %q", r.StatusCode, body)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("SPA CSP missing: %q", r.Header.Get("Content-Security-Policy"))
	}
	r, body = get("/assets/app-123.js")
	if r.StatusCode != 200 || body != "console.log(1)" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q cache=%q", r.StatusCode, body, r.Header.Get("Cache-Control"))
	}
	for _, p := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/assets/../../secret.txt"} {
		if _, body := get(p); strings.Contains(body, "do-not-serve") {
			t.Fatalf("path traversal via %s", p)
		}
	}
	r, _ = get("/api/v1/nope")
	if r.StatusCode != 404 || !strings.Contains(r.Header.Get("Content-Type"), "json") {
		t.Fatalf("unknown API path must stay a JSON 404: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
}
