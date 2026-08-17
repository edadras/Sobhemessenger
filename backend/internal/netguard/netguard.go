// Package netguard decides whether the server may fetch a URL a user supplied.
//
// Two features need this: bot webhooks and link previews. Both take a URL from
// an untrusted party and make the server request it, which is server-side
// request forgery unless something stops it. Without a guard, "https://…" is
// enough to make SOBH probe its own database, the cloud metadata endpoint, or
// anything else reachable from inside the network but not from outside it.
//
// The check is on the resolved address rather than the hostname, because a
// hostname is under the attacker's control: a name that resolves to a public
// address at validation time can resolve to 169.254.169.254 a moment later.
// Callers therefore validate and then dial with the address they validated.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var (
	ErrSchemeNotAllowed  = errors.New("netguard: only http and https are allowed")
	ErrPrivateAddress    = errors.New("netguard: that address is not publicly routable")
	ErrHostNotResolvable = errors.New("netguard: that host does not resolve")
	ErrPortNotAllowed    = errors.New("netguard: only the standard web ports are allowed")
)

// allowedPorts keeps the fetcher off every other listening service on the
// network. A webhook on port 22 is not a webhook.
var allowedPorts = map[string]bool{"": true, "80": true, "443": true, "8080": true, "8443": true}

// ValidateURL parses a user-supplied URL and rejects anything the server must
// not fetch.
func ValidateURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("netguard: parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, ErrSchemeNotAllowed
	}
	if parsed.Host == "" {
		return nil, ErrHostNotResolvable
	}
	if !allowedPorts[parsed.Port()] {
		return nil, ErrPortNotAllowed
	}

	host := parsed.Hostname()

	// A literal address needs no lookup, and must be checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublic(ip) {
			return nil, ErrPrivateAddress
		}
		return parsed, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrHostNotResolvable
	}
	// Every resolved address must be public. One private answer among several
	// is enough to make the request unsafe, because the dialler may pick it.
	for _, address := range addresses {
		if !IsPublic(address.IP) {
			return nil, ErrPrivateAddress
		}
	}
	return parsed, nil
}

// IsPublic reports whether an address is one the server may talk to.
//
// Everything that is not globally routable is refused: loopback, link-local
// (which covers the cloud metadata endpoint at 169.254.169.254), the private
// ranges, carrier-grade NAT, and the unique-local IPv6 block.
func IsPublic(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return false
	}

	// An IPv4-mapped IPv6 address must be judged on the IPv4 it carries.
	if mapped := ip.To4(); mapped != nil {
		ip = mapped
	}

	for _, blocked := range blockedRanges {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}

// blockedRanges covers what the standard-library predicates do not.
var blockedRanges = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved
		"fc00::/7",        // unique local
		"2001:db8::/32",   // documentation
	}
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			networks = append(networks, network)
		}
	}
	return networks
}()

// SafeClient returns an HTTP client that refuses to connect to a private
// address even if DNS changes between validation and the dial.
//
// This is the half that closes the rebinding window: Control runs after
// resolution and before the connection, on the address actually being dialled.
func SafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !IsPublic(ip) {
				return ErrPrivateAddress
			}
			return nil
		},
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Redirects are followed sparingly and re-validated, because a
			// public URL that redirects to a private one would otherwise walk
			// straight past the first check.
			if len(via) >= 3 {
				return errors.New("netguard: too many redirects")
			}
			if _, err := ValidateURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}
