package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// spaCSP matches the policy nginx sets in the multi-service deployment:
// scripts, styles, fonts and images only from this origin.
const spaCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// isAPIPath reports whether a path belongs to the API rather than the SPA.
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/api" || p == "/metrics" || p == "/healthz" || p == "/readyz"
}

// staticHandler serves the built frontend from STATIC_DIR, falling back to
// index.html for client-side routes. It returns nil when STATIC_DIR is unset
// (the nginx-fronted deployment). Paths are resolved inside an fs.FS rooted
// at the directory, so ".." cannot escape it.
func (s *Server) staticHandler() http.Handler {
	if s.cfg.StaticDir == "" {
		return nil
	}
	root := os.DirFS(s.cfg.StaticDir)
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.", nil)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", spaCSP)
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && fs.ValidPath(name) {
			if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite fingerprints asset names, so they never change.
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					h.Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				fail(w, r, http.StatusInternalServerError, "internal_error", "Could not read file.", nil)
				return
			}
		}
		// Unknown path: let the SPA router handle it.
		h.Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, root, "index.html")
	})
}
