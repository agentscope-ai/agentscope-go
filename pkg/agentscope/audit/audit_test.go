package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInMemoryLogger(t *testing.T) {
	l := NewInMemoryLogger()

	entry := &Entry{
		Timestamp: time.Now(),
		Action:    ActionToolExecute,
		ToolName:  "Bash",
		Input:     "ls -la",
		Decision:  "allowed",
	}

	if err := l.Log(context.Background(), entry); err != nil {
		t.Fatalf("Log error: %v", err)
	}
	if l.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", l.Len())
	}
	entries := l.Entries()
	if entries[0].ToolName != "Bash" {
		t.Fatalf("expected ToolName=Bash, got %q", entries[0].ToolName)
	}
}

func TestInMemoryLogger_Concurrent(t *testing.T) {
	l := NewInMemoryLogger()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Log(context.Background(), &Entry{Action: ActionBashExec})
		}()
	}
	wg.Wait()
	if l.Len() != 100 {
		t.Fatalf("expected 100 entries, got %d", l.Len())
	}
}

func TestFileLogger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	l, err := NewFileLogger(path)
	if err != nil {
		t.Fatalf("NewFileLogger error: %v", err)
	}

	entry := &Entry{
		Timestamp: time.Now(),
		Action:    ActionToolExecute,
		ToolName:  "Write",
		Input:     "/workspace/hello.txt",
		Decision:  "allowed",
	}
	if err := l.Log(context.Background(), entry); err != nil {
		t.Fatalf("Log error: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `"tool_name":"Write"`) {
		t.Fatalf("expected JSON with tool_name=Write, got:\n%s", content)
	}
	if !strings.HasSuffix(strings.TrimSpace(content), "}") {
		t.Fatalf("expected JSON line, got:\n%s", content)
	}
}

func TestMultiLogger(t *testing.T) {
	l1 := NewInMemoryLogger()
	l2 := NewInMemoryLogger()
	multi := NewMultiLogger(l1, l2)

	if err := multi.Log(context.Background(), &Entry{Action: ActionPolicyDenied}); err != nil {
		t.Fatalf("Log error: %v", err)
	}
	if l1.Len() != 1 {
		t.Fatalf("l1: expected 1, got %d", l1.Len())
	}
	if l2.Len() != 1 {
		t.Fatalf("l2: expected 1, got %d", l2.Len())
	}
}

func TestNopLogger(t *testing.T) {
	l := NopLogger{}
	if err := l.Log(context.Background(), &Entry{Action: ActionToolExecute}); err != nil {
		t.Fatalf("NopLogger.Log error: %v", err)
	}
}

func TestGetLogger_Default(t *testing.T) {
	l := GetLogger(context.Background())
	// Default should be NopLogger.
	if _, ok := l.(NopLogger); !ok {
		t.Fatalf("expected NopLogger, got %T", l)
	}
}

func TestGetLogger_WithLogger(t *testing.T) {
	mem := NewInMemoryLogger()
	ctx := WithLogger(context.Background(), mem)
	l := GetLogger(ctx)
	if l != mem {
		t.Fatalf("expected same InMemoryLogger, got %T", l)
	}
}
