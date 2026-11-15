// Package channel connects agents to IM platforms (DingTalk first).
//
// A Channel keeps the platform connection, normalises inbound platform
// payloads into Event / ConfirmationEvent values, and renders the agent's
// reply event stream back to the platform. The stateless Gateway
// orchestrates each inbound event: route it to a session, run the agent,
// feed the reply stream to the channel, and round-trip tool-call
// confirmations.
//
// This is the Go port of Python agentscope's app.channel package, scoped
// to the connection + gateway core; storage-backed channel CRUD, the
// lifecycle dispatcher and multi-node heartbeats belong to the app layer
// and are not ported here.
package channel

import (
	"context"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// ChatKind is a chat's audience shape.
type ChatKind string

const (
	ChatKindGroup   ChatKind = "group"
	ChatKindPrivate ChatKind = "private"
)

// StatusState is a channel connection's lifecycle state.
type StatusState string

const (
	StatusStopped    StatusState = "stopped"
	StatusConnecting StatusState = "connecting"
	StatusConnected  StatusState = "connected"
	StatusRetrying   StatusState = "retrying"
	StatusFailed     StatusState = "failed"
)

// Status is the live connection state of a channel adapter.
type Status struct {
	State     StatusState
	LastError string
}

// Capability declares what a platform can render (send direction).
type Capability struct {
	Text        bool
	Markdown    bool
	Image       bool
	File        bool
	Interactive bool // can present an interactive confirmation UI
	Streaming   bool // can update one reply message in place

	// MaxMessageLength bounds one outbound message; longer text is split.
	MaxMessageLength int
}

// Event is a normalised inbound message from a platform.
type Event struct {
	ChannelID        string
	ChannelUserID    string
	ChannelUserName  string
	ChatID           string
	ChatName         string
	ChannelMessageID string
	Kind             ChatKind
	Text             string
	Metadata         map[string]any
	ReceivedAt       time.Time
}

// ConfirmationEvent is a user's decision on a pending tool approval,
// delivered inbound (e.g. a card button click). It carries only lookup
// keys — the authoritative pending tool call is read from the session's
// parked confirmations, never trusted from this round-tripped payload.
type ConfirmationEvent struct {
	ChannelID     string
	ChatID        string
	ChannelUserID string
	SessionID     string // empty when the channel cannot know it
	ToolCallID    string // correlation key
	Approved      bool
	WithRules     bool // accept suggested rules too ("always")
	Actor         string
}

// Inbound carries one normalised inbound payload — either a message or a
// confirmation decision.
type Inbound struct {
	Message      *Event
	Confirmation *ConfirmationEvent
}

// EmitFunc is the gateway entry point handed to a channel; channels
// dispatch normalised inbound events through it and must not reach any
// other gateway state.
type EmitFunc func(ctx context.Context, in Inbound) error

// Response is one reply handed to a channel for rendering: the reply's
// event stream plus routing context. The channel owns the stream until
// it is closed (and must drain it), emitting platform messages as it
// goes (e.g. a confirmation prompt when the reply parks).
type Response struct {
	ReplyID string
	ChatID  string
	Kind    ChatKind
	Events  <-chan event.Event
}

// Channel is a platform adapter. StartListening must return once the
// listener is launched (not when it exits); the connection runs until
// Close or ctx cancellation.
type Channel interface {
	// Type is the platform type id (e.g. "dingtalk").
	Type() string
	// ChannelID is this channel instance's unique identifier.
	ChannelID() string
	// Capabilities describes the send direction.
	Capabilities() Capability
	// StartListening launches the inbound connection and routes
	// normalised events to emit.
	StartListening(ctx context.Context, emit EmitFunc) error
	// SendResponse renders one reply stream back to the platform.
	SendResponse(ctx context.Context, r Response) error
	// Status reports the live connection state.
	Status() Status
	// Close tears the channel down.
	Close() error
}

// SplitText splits text into chunks of at most maxLen runes, preferring
// line boundaries. maxLen <= 0 disables splitting.
func SplitText(text string, maxLen int) []string {
	if maxLen <= 0 || len([]rune(text)) <= maxLen {
		return []string{text}
	}
	var chunks []string
	current := ""
	for _, line := range splitLines(text) {
		// A single line longer than maxLen is hard-split.
		for len([]rune(line)) > maxLen {
			runes := []rune(line)
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			chunks = append(chunks, string(runes[:maxLen]))
			line = string(runes[maxLen:])
		}
		// The "\n" joiner counts against the bound (HARNESS R7-M3).
		joined := len([]rune(line))
		if current != "" {
			joined += len([]rune(current)) + 1
		}
		if joined > maxLen && current != "" {
			chunks = append(chunks, current)
			current = line
		} else {
			if current != "" {
				current += "\n"
			}
			current += line
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	runes := []rune(text)
	for i, r := range runes {
		if r == '\n' {
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		}
	}
	lines = append(lines, string(runes[start:]))
	return lines
}
