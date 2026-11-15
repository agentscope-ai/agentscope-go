package tool

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// isDisallowedIP reports whether ip is in a range that must not be reachable via
// the WebFetch tool: loopback, private (RFC1918 / ULA), link-local (incl. the
// 169.254.169.254 cloud-metadata endpoint), or the unspecified address. This is
// the core SSRF guard.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}

// ssrfSafeDialContext resolves the target host and refuses to connect if any
// resolved address is disallowed. It dials the vetted IP directly to avoid a
// DNS-rebinding time-of-check/time-of-use gap. Applied per-connection, so it
// also guards redirects.
func ssrfSafeDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ipa := range ips {
			if isDisallowedIP(ipa.IP) {
				return nil, fmt.Errorf("ssrf blocked: %s resolves to disallowed address %s", host, ipa.IP)
			}
		}
		// Dial the first vetted IP directly (avoids re-resolution / rebinding).
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// newSSRFSafeClient returns an http.Client whose dialer blocks SSRF targets.
func newSSRFSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         ssrfSafeDialContext(dialer),
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        10,
		},
	}
}
