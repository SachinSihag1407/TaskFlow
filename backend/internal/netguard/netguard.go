// Package netguard provides an HTTP client that refuses to connect to
// internal, loopback, link-local (cloud metadata) and other non-public
// addresses. The check runs on the IP address actually being dialled, after
// DNS resolution, so DNS-rebinding tricks cannot bypass it, and it applies
// equally to every redirect hop.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a destination is not allowed.
var ErrBlocked = errors.New("destination address is not allowed")

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",          // "this" network
	"10.0.0.0/8",         // RFC 1918
	"100.64.0.0/10",      // carrier-grade NAT
	"127.0.0.0/8",        // loopback
	"169.254.0.0/16",     // link-local, incl. 169.254.169.254 cloud metadata
	"172.16.0.0/12",      // RFC 1918
	"192.0.0.0/24",       // IETF protocol assignments
	"192.0.2.0/24",       // TEST-NET-1
	"192.88.99.0/24",     // 6to4 relay anycast
	"192.168.0.0/16",     // RFC 1918
	"198.18.0.0/15",      // benchmarking
	"198.51.100.0/24",    // TEST-NET-2
	"203.0.113.0/24",     // TEST-NET-3
	"224.0.0.0/4",        // multicast
	"240.0.0.0/4",        // reserved
	"255.255.255.255/32", // broadcast
	"::/128",             // unspecified
	"::1/128",            // loopback
	"::ffff:0:0/96",      // IPv4-mapped (checked again after unmapping)
	"64:ff9b::/96",       // NAT64 (could reach internal IPv4)
	"64:ff9b:1::/48",
	"100::/64",      // discard
	"2001::/23",     // IETF protocol assignments
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4 (can embed internal IPv4)
	"fc00::/7",      // unique local
	"fe80::/10",     // link-local
	"ff00::/8",      // multicast
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// IsPublic reports whether ip is a publicly routable unicast address.
func IsPublic(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast()
}

// Options configures the guarded client.
type Options struct {
	// AllowPrivate disables the address check. Only for tests and local
	// development; config refuses it in production.
	AllowPrivate bool
	Timeout      time.Duration
	MaxRedirects int
}

// control runs after DNS resolution, immediately before connect.
func control(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBlocked, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("%w: unparseable address %q", ErrBlocked, host)
		}
		if !IsPublic(ip) {
			return fmt.Errorf("%w: %s is a private, loopback, link-local or reserved address", ErrBlocked, ip)
		}
		return nil
	}
}

// NewClient returns an http.Client whose every connection is checked.
func NewClient(o Options) *http.Client {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = 3
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: control(o.AllowPrivate)}
	transport := &http.Transport{
		// Never route webhooks through an environment-configured proxy: the
		// proxy would make the connection on our behalf and bypass the check.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   o.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= o.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", o.MaxRedirects)
			}
			return ValidateURL(req.URL.String(), o.AllowPrivate)
		},
	}
}

// ValidateURL performs static checks on a webhook URL at configuration time:
// scheme, credentials, host, port and literal IP addresses. Hostnames are
// re-checked against their resolved IPs on every connection.
func ValidateURL(raw string, allowPrivate bool) error {
	if len(raw) > 2048 {
		return errors.New("URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL is not valid")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("URL scheme must be http or https")
	}
	if u.User != nil {
		return errors.New("URL must not contain credentials; use a header instead")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL must include a host")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("URL port is not valid")
		}
		// Restricting ports limits use of the webhook as a port scanner
		// against well-known internal services.
		if !allowPrivate && n != 80 && n != 443 && n < 1024 {
			return errors.New("URL port must be 80, 443 or above 1023")
		}
	}
	if allowPrivate {
		return nil
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".internal") || lower == "metadata.google.internal" {
		return fmt.Errorf("%w: %s", ErrBlocked, host)
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && !IsPublic(ip) {
		return fmt.Errorf("%w: %s", ErrBlocked, ip)
	}
	return nil
}
