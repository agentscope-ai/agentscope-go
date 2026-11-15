package agent

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// A user shell command runs outside the model loop: start/output/end events
// stream on the conversation event channel and nothing enters model context.
func TestConversationRunUserShellCommand(t *testing.T) {
	a := NewUnifiedAgent("shell-agent", "helpful", &mockChatModel{})
	conv, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()

	cmdID, err := conv.RunUserShellCommand("echo user-shell-marker", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cmdID == "" {
		t.Fatal("expected a command id")
	}

	var output strings.Builder
	var endSeen bool
	var exitCode any
	deadline := time.After(15 * time.Second)
	for !endSeen {
		select {
		case evt, ok := <-conv.Events():
			if !ok {
				t.Fatal("events closed before user_shell_end")
			}
			ce, ok := evt.(event.CustomEvent)
			if !ok {
				continue
			}
			switch ce.Name {
			case ConversationEventUserShellOutput:
				output.WriteString(ce.Value["data"].(string))
			case ConversationEventUserShellEnd:
				endSeen = true
				exitCode = ce.Value["exit_code"]
				if _, hasErr := ce.Value["error"]; hasErr {
					t.Fatalf("command failed: %v", ce.Value["error"])
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for user_shell_end")
		}
	}
	if !strings.Contains(output.String(), "user-shell-marker") {
		t.Errorf("output missing command stdout: %q", output.String())
	}
	if exitCode != 0 {
		t.Errorf("exit_code = %v, want 0", exitCode)
	}

	// The command must not enter model context.
	a.mu.Lock()
	ctxLen := len(a.state.Context)
	a.mu.Unlock()
	if ctxLen != 0 {
		t.Errorf("user shell command leaked into model context: %d messages", ctxLen)
	}
}

func TestConversationRunUserShellCommand_Validation(t *testing.T) {
	a := NewUnifiedAgent("shell-agent", "helpful", &mockChatModel{})
	conv, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conv.RunUserShellCommand("   ", time.Second); err == nil {
		t.Error("empty command must fail")
	}
	if err = conv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = conv.RunUserShellCommand("echo x", time.Second); err != ErrConversationClosed {
		t.Errorf("closed conversation: got %v", err)
	}
}

// Closing a conversation while a user shell command is in flight must not
// panic (send on closed channel) and must return in bounded time: Close
// cancels the command and waits for its goroutine before closing events.
func TestConversationRunUserShellCommand_CloseDuringRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a Unix shell")
	}
	a := NewUnifiedAgent("shell-agent", "helpful", &mockChatModel{})
	conv, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conv.RunUserShellCommand("sleep 30", 0); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_ = conv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked past the process-kill bound")
	}
}
