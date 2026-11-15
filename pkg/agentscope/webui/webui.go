// Package webui provides an embedded web interface for AgentScope agents.
//
// It embeds a single-page application (SPA) that communicates with the
// agent [service.Service] REST/SSE API. The UI displays:
//   - Session management (create, list, delete)
//   - Streaming chat with thinking blocks, tool calls, and tool results
//   - Human-in-the-loop confirmation for tool calls
//   - Model information
//
// Usage:
//
//	svc := service.New(cfg, cm, factory)
//	webui.Mount(svc, webui.Options{})
//	svc.ListenAndServe()
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Options configures the web UI.
type Options struct {
	// PathPrefix is the URL prefix for the web UI (default "/").
	PathPrefix string

	// Title overrides the page title (default "AgentScope Studio").
	Title string
}

// Mountable is the interface that service.Service must implement for
// the web UI to attach its routes. It is satisfied by *service.Service
// because we add a MountHandler method.
type Mountable interface {
	Handler() http.Handler
}

// Handler returns an http.Handler that serves the embedded web UI.
// It serves static files from the embedded filesystem and falls back
// to index.html for SPA routing.
//
// The handler strips the "/static/" URL prefix when looking up files
// in the embedded FS so that HTML can reference assets as
// "/static/style.css" while the embed directive uses "static/".
func Handler(opts Options) http.Handler {
	if opts.PathPrefix == "" {
		opts.PathPrefix = "/"
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("webui: embedded static fs: " + err.Error())
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Strip optional path prefix
		if opts.PathPrefix != "/" {
			path = strings.TrimPrefix(path, strings.TrimSuffix(opts.PathPrefix, "/"))
			if path == "" {
				path = "/"
			}
		}

		// Strip "/static/" prefix so the file server can resolve
		// e.g. "/static/style.css" -> "style.css" in the embedded FS.
		if cleaned := strings.TrimPrefix(path, "/static/"); cleaned != path {
			if f, openErr := sub.Open(cleaned); openErr == nil {
				_ = f.Close()
				r2 := new(http.Request)
				*r2 = *r
				r2.URL = new(url.URL)
				*r2.URL = *r.URL
				r2.URL.Path = "/" + cleaned
				fileServer.ServeHTTP(w, r2)
				return
			}
		}

		// Try to serve the file directly (non-prefixed path)
		if path != "/" {
			clean := strings.TrimPrefix(path, "/")
			if f, openErr := sub.Open(clean); openErr == nil {
				_ = f.Close()
				r2 := new(http.Request)
				*r2 = *r
				r2.URL = new(url.URL)
				*r2.URL = *r.URL
				r2.URL.Path = "/" + clean
				fileServer.ServeHTTP(w, r2)
				return
			}
		}

		// SPA fallback: serve index.html
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// MuxMount registers the web UI handler on an existing *http.ServeMux.
func MuxMount(mux *http.ServeMux, opts Options) {
	if opts.PathPrefix == "" {
		opts.PathPrefix = "/"
	}
	handler := Handler(opts)

	// Register both exact and prefix patterns
	prefix := strings.TrimSuffix(opts.PathPrefix, "/")
	if prefix == "" {
		mux.Handle("/", handler)
	} else {
		mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	}
}
