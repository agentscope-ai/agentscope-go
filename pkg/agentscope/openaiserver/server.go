// Package openaiserver serves a text-only subset of OpenAI Chat Completions
// through request-owned UnifiedAgents. It is Experimental. Hosts supply
// authentication/authorization middleware and deployment-owned tools.
package openaiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Config configures one handler. Zero limits select 4 MiB, 32 concurrent
// requests and a five-minute reply timeout. Negative limits are invalid.
// Models and model implementations must support concurrent calls. The map is
// copied at construction; tool instances/middleware are owned by the host.
type Config struct {
	Models map[string]model.ChatModel
	// AgentFactory must return r.NewAgent(...), called exactly once, and
	// honor r.Request.Context(). Nil selects a tool-free default agent.
	AgentFactory  func(r *RequestInfo) (*agent.UnifiedAgent, error)
	MaxBodyBytes  int64
	MaxConcurrent int
	ReplyTimeout  time.Duration
}

// AgentConfig is the supported request-owned agent configuration. Automatic
// compression, background precompression, recovery and arbitrary AgentOptions
// are intentionally excluded from this stateless v1 surface.
type AgentConfig struct {
	Toolkit           *tool.Toolkit
	PermissionContext *permission.Context
	// MaxIters defaults to 20; negative values are invalid.
	MaxIters     int
	StopOnReject bool
	// BufferedModel selects Chat instead of ChatStream. Agent middleware also
	// uses buffered model calls under the existing UnifiedAgent contract.
	BufferedModel bool
	// Middlewares are trusted host code and must preserve the request context
	// and normal model/permission pipeline. Do not start detached work.
	Middlewares []middleware.Middleware
}

// RequestInfo contains validated, request-local construction inputs. Use
// Request.Context to access host identity and cancellation. Do not retain it.
type RequestInfo struct {
	Request      *http.Request
	ModelID      string
	SystemPrompt string
	// ModelConfig controls retries and fallback. NewAgent wraps its fallback
	// in the same request policy so it cannot bypass request restrictions.
	ModelConfig     agent.ModelConfig
	policy          *requestPolicy
	state           *agent.AgentState
	model           model.ChatModel
	created         *agent.UnifiedAgent
	constructions   int
	constructionErr error
}

// NewAgent creates this request's agent with server-owned state. Each request must create
// exactly one agent, and must not run it from the factory or retain it afterward.
// Like agent.NewUnifiedAgent, an empty name is a programmer error and panics.
func (r *RequestInfo) NewAgent(name string, cfg AgentConfig) *agent.UnifiedAgent {
	modelCfg := r.ModelConfig
	if modelCfg.FallbackModel != nil {
		if nilModel(modelCfg.FallbackModel) {
			r.constructionErr = errors.New("openaiserver: fallback model must not be nil")
			modelCfg.FallbackModel = nil
		} else {
			modelCfg.FallbackModel = &requestModel{inner: modelCfg.FallbackModel, policy: r.policy}
		}
	}
	if cfg.MaxIters < 0 {
		r.constructionErr = errors.New("openaiserver: iteration limit must not be negative")
	}
	maxIters := cfg.MaxIters
	if maxIters == 0 {
		maxIters = 20
	}
	opts := []agent.AgentOption{agent.WithState(r.state), agent.WithModelConfig(modelCfg), agent.WithReactConfig(agent.ReactConfig{MaxIters: maxIters, StopOnReject: cfg.StopOnReject}), agent.WithModelStreaming(!cfg.BufferedModel)}
	if cfg.Toolkit != nil {
		opts = append(opts, agent.WithToolkit(cfg.Toolkit))
	}
	if cfg.PermissionContext != nil {
		opts = append(opts, agent.WithPermissionContext(cfg.PermissionContext))
	}
	if len(cfg.Middlewares) > 0 {
		opts = append(opts, agent.WithMiddlewares(cfg.Middlewares...))
	}
	r.created = agent.NewUnifiedAgent(name, r.SystemPrompt, &requestModel{inner: r.model, policy: r.policy}, opts...)
	r.constructions++
	return r.created
}

type server struct {
	cfg     Config
	slots   chan struct{}
	created int64
}

// New validates configuration and returns a mountable http.Handler exposing
// POST /v1/chat/completions and GET /v1/models. It starts no server/goroutines.
func New(cfg Config) (http.Handler, error) {
	if len(cfg.Models) == 0 {
		return nil, errors.New("openaiserver: at least one model is required")
	}
	if cfg.MaxBodyBytes < 0 || cfg.MaxConcurrent < 0 || cfg.ReplyTimeout < 0 {
		return nil, errors.New("openaiserver: limits must not be negative")
	}
	models := make(map[string]model.ChatModel, len(cfg.Models))
	for name, m := range cfg.Models {
		if strings.TrimSpace(name) == "" || nilModel(m) {
			return nil, errors.New("openaiserver: models require nonempty names and non-nil implementations")
		}
		models[name] = m
	}
	cfg.Models = models
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 32
	}
	if cfg.ReplyTimeout == 0 {
		cfg.ReplyTimeout = 5 * time.Minute
	}
	return &server{cfg: cfg, slots: make(chan struct{}, cfg.MaxConcurrent), created: time.Now().Unix()}, nil
}

func nilModel(m model.ChatModel) bool {
	if m == nil {
		return true
	}
	v := reflect.ValueOf(m)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

type apiError struct {
	status  int
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func (e *apiError) Error() string { return e.Message }
func serverError(status int, code, msg string) *apiError {
	return &apiError{status: status, Message: msg, Type: "server_error", Code: code}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.status, struct {
		Error *apiError `json:"error"`
	}{err})
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			writeError(w, &apiError{status: 405, Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
			return
		}
		s.models(w)
	case "/v1/chat/completions":
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			writeError(w, &apiError{status: 405, Message: "method not allowed", Type: "invalid_request_error", Code: "method_not_allowed"})
			return
		}
		s.completion(w, r)
	default:
		writeError(w, &apiError{status: 404, Message: "unknown endpoint", Type: "invalid_request_error", Code: "not_found"})
	}
}
func (s *server) models(w http.ResponseWriter) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	names := make([]string, 0, len(s.cfg.Models))
	for name := range s.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]entry, 0, len(names))
	for _, name := range names {
		data = append(data, entry{name, "model", s.created, "agentscope"})
	}
	writeJSON(w, 200, struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{"list", data})
}

func (s *server) completion(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req *chatRequest
	err := dec.Decode(&req)
	if err == nil {
		var extra any
		if trailingErr := dec.Decode(&extra); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("trailing JSON")
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, &apiError{status: 413, Message: "request body exceeds size limit", Type: "invalid_request_error", Code: "body_too_large"})
		} else {
			writeError(w, badRequest("invalid JSON request or unsupported field"))
		}
		return
	}
	if req == nil {
		writeError(w, badRequest("request must be a JSON object"))
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, err.(*apiError))
		return
	}
	m, ok := s.cfg.Models[req.Model]
	if !ok {
		writeError(w, &apiError{status: 404, Message: "unknown model", Type: "invalid_request_error", Code: "model_not_found"})
		return
	}
	prompt, state, input, err := req.hydrate()
	if err != nil {
		writeError(w, err.(*apiError))
		return
	}
	if req.Stream && !canFlush(w) {
		writeError(w, serverError(500, "streaming_unavailable", "response writer does not support streaming"))
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, &apiError{status: 429, Message: "concurrent reply limit exceeded", Type: "rate_limit_error", Code: "rate_limit_exceeded"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ReplyTimeout)
	ctx, scope := runctx.New(ctx)
	// ReplyStream's public forwarder may close before a canceled core exits.
	// Hold admission until all owned cores finish, even for a slow tool.
	defer func() { cancel(); scope.Close() }()
	policy := &requestPolicy{req: req}
	info := &RequestInfo{Request: r.WithContext(ctx), ModelID: req.Model, SystemPrompt: prompt, policy: policy, state: state, model: m}
	var a *agent.UnifiedAgent
	if s.cfg.AgentFactory == nil {
		a = info.NewAgent("assistant", AgentConfig{})
	} else {
		a, err = s.cfg.AgentFactory(info)
	}
	if ctx.Err() != nil {
		writeError(w, serverError(504, "reply_timeout", "reply canceled or timed out"))
		return
	}
	if err != nil {
		writeError(w, serverError(500, "agent_factory_failed", "agent construction failed"))
		return
	}
	if a == nil || a != info.created || info.constructions != 1 || info.constructionErr != nil {
		writeError(w, serverError(500, "invalid_agent_factory", "factory must return exactly one request-owned RequestInfo.NewAgent result"))
		return
	}
	events, err := a.ReplyStream(ctx, input)
	if err != nil {
		writeError(w, serverError(500, "reply_failed", "agent could not start reply"))
		return
	}
	output := newOutput(w, req)
	var end *event.ReplyEndEvent
	outputs := 0
	var failure *apiError
drain:
	for {
		select {
		case <-ctx.Done():
			failure = serverError(504, "reply_timeout", "reply canceled or timed out")
			break drain
		case ev, ok := <-events:
			if !ok {
				break drain
			}
			switch e := ev.(type) {
			case event.TextBlockDeltaEvent:
				if err := output.text(e.Delta); err != nil {
					cancel()
					return
				}
			case event.CustomEvent:
				if e.Name == "turn_stats" {
					outputs, _ = e.Value["output_tokens"].(int)
				}
			case event.ReplyEndEvent:
				end = &e
			case event.RequireUserConfirmEvent, event.RequireExternalExecutionEvent:
				failure = badRequest("interactive approval and client-executed tools are not supported by this endpoint")
				break drain
			}
		}
	}
	if failure == nil {
		switch {
		case ctx.Err() != nil:
			failure = serverError(504, "reply_timeout", "reply canceled or timed out")
		case end == nil:
			failure = serverError(502, "incomplete_reply", "agent stream ended without a reply result")
		case policy.failure() != nil:
			failure = replyFailure(policy, end)
		case end.FinishedReason == types.ReplyError:
			failure = replyFailure(policy, end)
		case end.FinishedReason != types.ReplyCompleted && end.FinishedReason != types.ReplyExceedMaxIters:
			failure = serverError(502, "reply_interrupted", "agent reply was interrupted")
		}
	}
	if failure != nil {
		_ = output.fail(failure)
		return
	}
	_ = output.finish(policy.usage(outputs))
}

func replyFailure(p *requestPolicy, end *event.ReplyEndEvent) *apiError {
	err := p.failure()
	var invalid *apiError
	if errors.As(err, &invalid) {
		return invalid
	}
	if model.IsRetryableError(err) || (end.Error != nil && (end.Error.Type == types.ErrorRateLimit || end.Error.Type == types.ErrorConnection || end.Error.Type == types.ErrorUpstream)) {
		return serverError(503, "upstream_unavailable", "model provider is temporarily unavailable")
	}
	return serverError(502, "upstream_error", "agent could not complete the model reply")
}

func canFlush(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}
