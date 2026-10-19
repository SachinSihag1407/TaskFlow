package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "fe80::1", "fc00::1", "fd12::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "192.0.2.1",
	}
	for _, s := range blocked {
		if IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if !IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestValidateURL(t *testing.T) {
	bad := []string{
		"ftp://example.com", "file:///etc/passwd", "gopher://x", "http://", "https://user:pw@example.com/",
		"http://localhost/", "http://LOCALHOST./", "http://foo.localhost/", "http://127.0.0.1/",
		"http://169.254.169.254/latest/meta-data/", "http://[::1]/", "http://10.0.0.5:8080/",
		"http://metadata.google.internal/", "http://db.internal/", "https://example.com:22/",
		"https://example.com:99999/",
	}
	for _, u := range bad {
		if err := ValidateURL(u, false); err == nil {
			t.Errorf("ValidateURL(%q) accepted", u)
		}
	}
	good := []string{"https://example.com/hook", "http://example.com:8080/x?y=1", "https://93.184.216.34/"}
	for _, u := range good {
		if err := ValidateURL(u, false); err != nil {
			t.Errorf("ValidateURL(%q) = %v", u, err)
		}
	}
}

func TestClientBlocksPrivateDestinationsAtConnectTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The test server listens on 127.0.0.1: a guarded client must refuse it
	// even though the URL passed no static checks (hostname rebinding case).
	client := NewClient(Options{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := client.Do(req)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}

	open := NewClient(Options{AllowPrivate: true})
	resp, err := open.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-private client: %v", err)
	}
	resp.Body.Close()
}

func TestRedirectToPrivateAddressIsBlocked(t *testing.T) {
	// A public-looking endpoint that redirects to the metadata service must be
	// stopped at the redirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	client := NewClient(Options{AllowPrivate: false})
	// Bypass the connect check for the first hop only by validating the
	// redirect policy directly.
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err := client.CheckRedirect(req, []*http.Request{{}}); err == nil {
		t.Fatal("redirect to metadata address was allowed")
	}
}
