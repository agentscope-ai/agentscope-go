package replay

import (
	"encoding/json"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Mode determines whether the middleware records or replays.
type Mode int

const (
	ModeRecord Mode = iota
	ModeReplay
)

// Entry represents one recorded model call (request + response pair).
type Entry struct {
	Index      int              `json:"index"`
	AgentName  string           `json:"agent_name"`
	ModelName  string           `json:"model_name"`
	ReplyID    string           `json:"reply_id,omitempty"`
	Messages   json.RawMessage  `json:"messages"`
	Tools      json.RawMessage  `json:"tools,omitempty"`
	Response   json.RawMessage  `json:"response"`
	Usage      *model.ChatUsage `json:"usage,omitempty"`
	Error      string           `json:"error,omitempty"`
	Timestamp  time.Time        `json:"timestamp"`
	DurationMs int64            `json:"duration_ms"`
}

// Tape holds a sequence of recorded entries.
type Tape struct {
	Version  string            `json:"version"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Entries  []Entry           `json:"entries"`
}

// NewTape creates a new empty tape with version metadata.
func NewTape() *Tape {
	return &Tape{
		Version: "1.0",
		Entries: []Entry{},
	}
}
