package service

import (
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/webui"
)

// WebUIConfig configures the embedded web UI.
type WebUIConfig struct {
	// Enable activates the web UI (default: false).
	Enable bool

	// PathPrefix is the URL prefix for the web UI (default: "/").
	PathPrefix string
}

// HandlerWithWebUI returns an http.Handler that serves both the agent API
// and the embedded web UI. If webCfg.Enable is false, it returns the same
// handler as [Service.Handler].
func (s *Service) HandlerWithWebUI(webCfg WebUIConfig) http.Handler {
	apiHandler := s.Handler()

	if !webCfg.Enable {
		return apiHandler
	}

	prefix := webCfg.PathPrefix
	if prefix == "" {
		prefix = "/"
	}

	uiHandler := webui.Handler(webui.Options{
		PathPrefix: prefix,
	})

	mux := http.NewServeMux()

	// API routes take priority
	mux.Handle("/api/", apiHandler)
	mux.Handle("/healthz", apiHandler)
	mux.Handle("/readyz", apiHandler)

	// Web UI serves static files and SPA fallback
	mux.Handle("/static/", uiHandler)
	mux.Handle("/", uiHandler)

	if len(s.cfg.AllowedOrigins) > 0 {
		return s.corsMiddleware(mux)
	}
	return mux
}
