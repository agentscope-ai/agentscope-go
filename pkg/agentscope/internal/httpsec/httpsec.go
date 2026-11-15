// Package httpsec provides shared hardening for the framework's HTTP servers.
package httpsec

import (
	"net/http"
	"time"
)

// Defaults for HTTP server hardening.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultMaxHeaderBytes    = 1 << 20  // 1 MiB
	DefaultMaxBodyBytes      = 10 << 20 // 10 MiB
)

// Harden sets protective defaults on an http.Server without overriding values the
// caller already set. ReadHeaderTimeout closes the Slowloris hole (a client that
// dribbles headers forever); IdleTimeout bounds keep-alive connections;
// MaxHeaderBytes caps header size.
func Harden(srv *http.Server) {
	if srv.ReadHeaderTimeout == 0 {
		srv.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if srv.IdleTimeout == 0 {
		srv.IdleTimeout = DefaultIdleTimeout
	}
	if srv.MaxHeaderBytes == 0 {
		srv.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
}

// LimitBody wraps a handler so each request body is capped at max bytes (0 uses
// DefaultMaxBodyBytes). Reads past the limit return an error the handler can turn
// into a 413.
func LimitBody(next http.Handler, max int64) http.Handler {
	if max <= 0 {
		max = DefaultMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}
