package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
)

const (
	defaultReMeBaseURL       = "http://localhost:2333"
	defaultReMeSearchTimeout = 30 * time.Second
	defaultReMeAddTimeout    = 120 * time.Second // auto_memory runs a server-side LLM call
)

// ReMeConfig holds the configuration for connecting to a ReMe HTTP service.
type ReMeConfig struct {
	BaseURL string // Base URL of the ReMe service. Default: http://localhost:2333
	// HTTPClient is an optional custom HTTP client. Its Timeout, when set,
	// caps every call on top of SearchTimeout/AddTimeout.
	HTTPClient *http.Client

	SearchTimeout time.Duration // Per-search deadline. Default: 30s
	AddTimeout    time.Duration // Per-add deadline. Default: 120s (auto_memory runs a server-side LLM call)

	// FilterBySession scopes search to memories written with the same
	// session_id (userID, or userID:agentID). It requires the server to run
	// with include_frontmatter_in_metadata; without that server flag the
	// filter silently matches nothing. Note the middleware composes session
	// IDs asymmetrically: Add always includes AgentID, Search only when
	// middleware Config.ScopeSearchByAgent is set — with a non-empty
	// AgentID, enable both or every search silently matches nothing.
	FilterBySession bool
}

// ReMeStore implements MemoryStore by calling a ReMe HTTP service.
//
// ReMe reports business errors as HTTP 200 with {"success": false, "answer":
// ...}, so the response envelope is parsed on every call instead of relying on
// status codes. Writes are LLM-distilled on the server and not guaranteed to
// persist: a successful auto_memory response with metadata.created=false is
// not an error. List and Delete are not supported (ErrStoreUnsupported);
// ReMe has no per-user listing or per-memory deletion endpoint.
type ReMeStore struct {
	baseURL         string
	client          *http.Client
	searchTimeout   time.Duration
	addTimeout      time.Duration
	filterBySession bool
}

// NewReMeStore creates a ReMe-backed memory store.
func NewReMeStore(cfg ReMeConfig) (*ReMeStore, error) {
	if cfg.SearchTimeout < 0 || cfg.AddTimeout < 0 {
		return nil, fmt.Errorf("reme: timeouts must not be negative")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultReMeBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		// Per-call deadlines come from the request context, so the client
		// itself carries no whole-request timeout.
		client = &http.Client{}
	}
	searchTimeout := cfg.SearchTimeout
	if searchTimeout <= 0 {
		searchTimeout = defaultReMeSearchTimeout
	}
	addTimeout := cfg.AddTimeout
	if addTimeout <= 0 {
		addTimeout = defaultReMeAddTimeout
	}
	return &ReMeStore{
		baseURL:         base,
		client:          client,
		searchTimeout:   searchTimeout,
		addTimeout:      addTimeout,
		filterBySession: cfg.FilterBySession,
	}, nil
}

// reMeSessionID combines userID and agentID into the ReMe session scope.
func reMeSessionID(userID, agentID string) string {
	if agentID != "" {
		return userID + ":" + agentID
	}
	return userID
}

// reMeEnvelope is the response envelope shared by all ReMe endpoints.
type reMeEnvelope struct {
	Answer   string          `json:"answer"`
	Success  bool            `json:"success"`
	Metadata json.RawMessage `json:"metadata"`
}

func reMeEnvelopeError(op string, env *reMeEnvelope) error {
	if env.Answer == "" {
		return fmt.Errorf("reme: %s failed (no details)", op)
	}
	return fmt.Errorf("reme: %s failed: %s", op, env.Answer)
}

type reMeSearchRequest struct {
	Query        string            `json:"query"`
	Limit        int               `json:"limit,omitempty"`
	MinScore     float64           `json:"min_score,omitempty"`
	SearchFilter *reMeSearchFilter `json:"search_filter,omitempty"`
}

type reMeSearchFilter struct {
	SessionID string `json:"session_id"`
}

type reMeSearchMetadata struct {
	Results []reMeSearchChunk `json:"results"`
}

type reMeSearchChunk struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Search queries ReMe for relevant memories. opts.TopK maps to the request
// limit (unset uses the server default); opts.Threshold maps to min_score,
// which is ReMe's RRF fusion score, not a cosine similarity. Returned memories
// carry only ID and Text: ReMe results do not include the user or a creation
// timestamp, so UserID stays empty and CreatedAt stays zero.
func (s *ReMeStore) Search(ctx context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error) {
	req := reMeSearchRequest{Query: query}
	var agentID string
	if opts != nil {
		if opts.TopK > 0 {
			req.Limit = opts.TopK
		}
		if opts.Threshold > 0 {
			req.MinScore = opts.Threshold
		}
		agentID = opts.AgentID
	}
	if s.filterBySession {
		req.SearchFilter = &reMeSearchFilter{SessionID: reMeSessionID(userID, agentID)}
	}

	ctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
	defer cancel()

	var env reMeEnvelope
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodPost, s.baseURL+"/search", req, &env,
		map[string]string{"Content-Type": "application/json"}); err != nil {
		return nil, fmt.Errorf("reme: search: %w", err)
	}
	if !env.Success {
		return nil, reMeEnvelopeError("search", &env)
	}
	if len(env.Metadata) == 0 {
		return nil, nil
	}
	var meta reMeSearchMetadata
	if err := json.Unmarshal(env.Metadata, &meta); err != nil {
		return nil, fmt.Errorf("reme: search: decode metadata: %w", err)
	}
	memories := make([]Memory, 0, len(meta.Results))
	for _, r := range meta.Results {
		memories = append(memories, Memory{ID: r.ID, Text: r.Text})
	}
	return memories, nil
}

// Add asks ReMe to distill and store a memory from text via auto_memory. The
// write is an LLM decision on the server: success with metadata.created=false
// (nothing worth remembering) is reported at debug level, not as an error.
func (s *ReMeStore) Add(ctx context.Context, text string, userID string, agentID string) error {
	if text == "" {
		return nil
	}

	req := reMeAddRequest{
		// ReMe requires the message name field; omitting it is rejected by
		// the server's request validation.
		Messages:  []reMeMessage{{Name: userID, Role: "user", Content: text}},
		SessionID: reMeSessionID(userID, agentID),
	}

	ctx, cancel := context.WithTimeout(ctx, s.addTimeout)
	defer cancel()

	var env reMeEnvelope
	if err := httpx.DoJSONRequest(ctx, s.client, http.MethodPost, s.baseURL+"/auto_memory", req, &env,
		map[string]string{"Content-Type": "application/json"}); err != nil {
		return fmt.Errorf("reme: add memory: %w", err)
	}
	if !env.Success {
		return reMeEnvelopeError("add memory", &env)
	}
	if len(env.Metadata) > 0 {
		var meta reMeAddMetadata
		if err := json.Unmarshal(env.Metadata, &meta); err == nil && !meta.Created {
			logrus.Debug("reme: auto_memory succeeded but did not persist (created=false)")
		}
	}
	return nil
}

type reMeAddRequest struct {
	Messages  []reMeMessage `json:"messages"`
	SessionID string        `json:"session_id"`
}

type reMeMessage struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type reMeAddMetadata struct {
	Created bool `json:"created"`
}

// List is not supported: ReMe has no per-user memory listing endpoint.
func (s *ReMeStore) List(context.Context, string) ([]Memory, error) {
	return nil, fmt.Errorf("reme: list: %w", ErrStoreUnsupported)
}

// Delete is not supported: ReMe has no per-memory deletion endpoint.
func (s *ReMeStore) Delete(context.Context, string) error {
	return fmt.Errorf("reme: delete: %w", ErrStoreUnsupported)
}
