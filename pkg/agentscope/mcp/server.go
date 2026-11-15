package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Server exposes local tools as MCP endpoints via HTTP.
// It implements the server side of the MCP gateway protocol,
// complementing the GatewayClient on the host side.
type Server struct {
	mu    sync.RWMutex
	tools map[string]tool.Tool
	token string
	mux   *http.ServeMux
}

// ServerConfig configures an MCP server.
type ServerConfig struct {
	Token string // auth token for bearer authentication
}

// NewServer creates an MCP server that exposes the given tools.
func NewServer(tools []tool.Tool, cfg ServerConfig) *Server {
	s := &Server{
		tools: make(map[string]tool.Tool),
		token: cfg.Token,
		mux:   http.NewServeMux(),
	}
	for _, t := range tools {
		s.tools[t.Name()] = t
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /tools", s.handleListTools)
	s.mux.HandleFunc("POST /tools/{name}", s.handleCallTool)
	return s
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	if s.token == "" {
		return s.mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+s.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

// AddTool dynamically adds a tool to the server.
func (s *Server) AddTool(t tool.Tool) {
	s.mu.Lock()
	s.tools[t.Name()] = t
	s.mu.Unlock()
}

// RemoveTool removes a tool from the server.
func (s *Server) RemoveTool(name string) {
	s.mu.Lock()
	delete(s.tools, name)
	s.mu.Unlock()
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type mcpToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (s *Server) handleListTools(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tools := make([]mcpToolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, mcpToolInfo{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tools)
}

func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	s.mu.RLock()
	t, ok := s.tools[name]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("tool %q not found", name), http.StatusNotFound)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := t.Execute(context.Background(), input)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":  err.Error(),
			"is_error": true,
		})
		return
	}

	var content string
	isError := false
	if resp != nil {
		for _, b := range resp.Content {
			if tb, ok := b.(message.TextBlock); ok {
				content += tb.Text
			}
		}
		if content == "" {
			data, _ := json.Marshal(resp.Content)
			content = string(data)
		}
		isError = resp.State == message.ToolResultError
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"content":  content,
		"is_error": isError,
	})
}
