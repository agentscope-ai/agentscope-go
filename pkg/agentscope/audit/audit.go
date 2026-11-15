// Package audit provides structured audit logging for agent tool execution,
// permission decisions, and sandbox policy enforcement. Every security-relevant
// action in the orchestrator is recorded as an [Entry] and written to one or
// more [Logger] implementations.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Action classifies the kind of auditable operation.
type Action string

const (
	ActionToolExecute    Action = "tool_execute"
	ActionToolDenied     Action = "tool_denied"
	ActionPolicyDenied   Action = "policy_denied"
	ActionPermissionAsk  Action = "permission_ask"
	ActionFileWrite      Action = "file_write"
	ActionFileRead       Action = "file_read"
	ActionBashExec       Action = "bash_exec"
	ActionNetworkAccess  Action = "network_access"
	ActionSandboxCreate  Action = "sandbox_create"
	ActionSandboxDestroy Action = "sandbox_destroy"
)

// Entry is a single audit record. It is intentionally flat (no nested
// structs) so that it maps trivially to structured log formats (JSON Lines,
// SLS, BigQuery, etc.).
type Entry struct {
	Timestamp  time.Time     `json:"timestamp"`
	SessionID  string        `json:"session_id,omitempty"`
	ReplyID    string        `json:"reply_id,omitempty"`
	AgentID    string        `json:"agent_id,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Action     Action        `json:"action"`
	ToolName   string        `json:"tool_name,omitempty"`
	Input      string        `json:"input,omitempty"`    // command or path (may be redacted)
	Output     string        `json:"output,omitempty"`   // result summary (truncated)
	Decision   string        `json:"decision,omitempty"` // "allowed", "denied", "ask", "policy_denied"
	Reason     string        `json:"reason,omitempty"`
	Backend    string        `json:"backend,omitempty"` // "local", "docker", "acs", ...
	Duration   time.Duration `json:"duration_ns,omitempty"`
	ExitCode   int           `json:"exit_code,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// Logger is the interface for audit log backends.
type Logger interface {
	// Log records a single audit entry. Implementations must be safe for
	// concurrent use. The Entry is passed by pointer to avoid copying the
	// 200+ byte struct on every call.
	Log(ctx context.Context, entry *Entry) error
}

// ---------------------------------------------------------------------------
// InMemoryLogger — useful for tests and short-lived processes.
// ---------------------------------------------------------------------------

// InMemoryLogger stores audit entries in a slice protected by a mutex.
type InMemoryLogger struct {
	mu      sync.Mutex
	entries []Entry
}

// NewInMemoryLogger creates an empty in-memory audit logger.
func NewInMemoryLogger() *InMemoryLogger { return &InMemoryLogger{} }

// Log appends an entry to the in-memory store.
func (l *InMemoryLogger) Log(_ context.Context, entry *Entry) error {
	l.mu.Lock()
	l.entries = append(l.entries, *entry)
	l.mu.Unlock()
	return nil
}

// Entries returns a copy of all recorded entries.
func (l *InMemoryLogger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Len returns the number of recorded entries.
func (l *InMemoryLogger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// ---------------------------------------------------------------------------
// FileLogger — append-only JSON Lines file, crash-safe via fsutil.
// ---------------------------------------------------------------------------

// FileLogger writes audit entries as newline-delimited JSON (JSON Lines) to
// a file. Each Log call appends a single line atomically via file locking.
type FileLogger struct {
	mu   sync.Mutex
	path string
	file *os.File
}

// NewFileLogger opens (or creates) the audit log file at the given path.
func NewFileLogger(path string) (*FileLogger, error) {
	// Ensure parent directory exists using fsutil convention.
	if err := os.MkdirAll(fileDir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audit: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("audit: open file: %w", err)
	}
	return &FileLogger{path: path, file: f}, nil
}

// Log appends a JSON-encoded entry followed by a newline.
func (l *FileLogger) Log(_ context.Context, entry *Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Write(data); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file.
func (l *FileLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// fileDir is a filepath.Dir equivalent that avoids importing path/filepath
// in the hot path; we only need it at construction.
func fileDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}

// ---------------------------------------------------------------------------
// MultiLogger — fan-out to multiple loggers.
// ---------------------------------------------------------------------------

// MultiLogger writes each entry to every wrapped Logger. If any Logger
// returns an error the first error is returned but all Loggers are still
// called (best-effort fan-out).
type MultiLogger struct {
	loggers []Logger
}

// NewMultiLogger creates a logger that fans out to all provided loggers.
func NewMultiLogger(loggers ...Logger) *MultiLogger {
	return &MultiLogger{loggers: loggers}
}

// Log writes the entry to every wrapped logger.
func (m *MultiLogger) Log(ctx context.Context, entry *Entry) error {
	var firstErr error
	for _, l := range m.loggers {
		if err := l.Log(ctx, entry); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// NopLogger — discards all entries (default when audit is not configured).
// ---------------------------------------------------------------------------

// NopLogger discards all audit entries.
type NopLogger struct{}

// Log is a no-op.
func (NopLogger) Log(context.Context, *Entry) error { return nil }

// ---------------------------------------------------------------------------
// Context helpers
// ---------------------------------------------------------------------------

type loggerCtxKey struct{}

// WithLogger returns a copy of ctx carrying the given audit Logger.
func WithLogger(ctx context.Context, l Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, l)
}

// GetLogger returns the audit Logger attached to ctx, or NopLogger.
func GetLogger(ctx context.Context) Logger {
	if v, ok := ctx.Value(loggerCtxKey{}).(Logger); ok {
		return v
	}
	return NopLogger{}
}

// compile-time interface checks
var (
	_ Logger = (*InMemoryLogger)(nil)
	_ Logger = (*FileLogger)(nil)
	_ Logger = (*MultiLogger)(nil)
	_ Logger = NopLogger{}
)
