package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpsec"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ScopeResolver returns an opaque owner scope from the host-authenticated
// context. It must honor cancellation. An error or empty/whitespace-only scope
// rejects the request with a generic 401; the scope itself is never normalized.
// The service does not authenticate raw headers or client identity fields.
type ScopeResolver func(context.Context) (string, error)

// ContextAgentFactory constructs a fresh, idle agent exclusively for a session.
// The context is the creation request's context, not a session-lifetime context;
// do not retain it to govern later replies. The supplied model may be nil when
// the factory provides its own model, as with AgentFactory.
//
// The registry owns admitted agent references, not external-resource disposal.
// The factory/host owns resource lifetimes and cleanup on error or canceled
// construction, including a non-nil result discarded after cancellation. No
// generic Agent close API is implied. Errors and nil agents are not registered.
type ContextAgentFactory func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error)

// NewScoped creates an Experimental owner-scoped Agent Service. Both hooks are
// required; New and AgentFactory retain their existing unscoped behavior.
//
// Each session route resolves identity before reading or decoding the body or
// reporting a body-limit error. Authenticated JSON requests are capped at
// httpsec.DefaultMaxBodyBytes (10 MiB), including Handler use, and must contain
// one complete JSON document. Health/model routes and CORS preflight are not
// session routes; the host still owns authentication for the whole handler.
// Sessions and their owner scopes are in memory only. Persistent saver namespaces,
// tool permissions and external-resource lifetimes remain host responsibilities.
func NewScoped(cfg Config, cm model.ChatModel, resolver ScopeResolver, factory ContextAgentFactory) (*Service, error) {
	if resolver == nil {
		return nil, errors.New("service: scope resolver is required")
	}
	if factory == nil {
		return nil, errors.New("service: context agent factory is required")
	}
	s := New(cfg, cm, nil)
	s.scopeResolver = resolver
	s.contextFactory = factory
	return s, nil
}

func (s *Service) sessionRoute(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := ""
		if s.scopeResolver != nil {
			var err error
			scope, err = s.scopeResolver(r.Context())
			if requestCanceled(w, r) {
				return
			}
			if err != nil || strings.TrimSpace(scope) == "" {
				writeError(w, http.StatusUnauthorized, "session identity required")
				return
			}
			if r.ContentLength > httpsec.DefaultMaxBodyBytes {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			if r.Body == nil {
				r.Body = http.NoBody
			}
			r.Body = http.MaxBytesReader(w, r.Body, httpsec.DefaultMaxBodyBytes)
		}
		next(w, r, scope)
	}
}

func requestCanceled(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Err() == nil {
		return false
	}
	writeError(w, http.StatusRequestTimeout, "request canceled")
	return true
}

func (s *Service) decodeRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(dst)
	if err == nil && s.scopeResolver != nil {
		// Consume trailing whitespace through the bounded reader. A single Decode
		// would accept a small valid object followed by an unbounded body tail.
		var extra json.RawMessage
		err = decoder.Decode(&extra)
		if errors.Is(err, io.EOF) {
			return true
		}
		if err == nil {
			err = errors.New("multiple JSON documents")
		}
	}
	if err == nil {
		return true
	}
	var limit *http.MaxBytesError
	if s.scopeResolver != nil && errors.As(err, &limit) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	} else {
		writeError(w, http.StatusBadRequest, "invalid request body")
	}
	return false
}
