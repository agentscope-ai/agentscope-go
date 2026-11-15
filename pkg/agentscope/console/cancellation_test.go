package console

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestLaunchReturnsCallerErrorAtPrompt(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			err := Launch(ctx, &fakeConsoleAgent{}, WithInput(reader), WithOutput(io.Discard))
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("Launch = %v, want %v", err, ctx.Err())
			}
		})
	}
}

type unclosedConsoleAgent struct {
	started chan struct{}
	events  chan event.Event
}

func (a *unclosedConsoleAgent) ReplyStream(context.Context, string) (<-chan event.Event, error) {
	close(a.started)
	return a.events, nil
}

func (*unclosedConsoleAgent) SubmitUserConfirm(*event.UserConfirmResultEvent) {}

func TestLaunchCancellationDoesNotWaitForProducerClose(t *testing.T) {
	a := &unclosedConsoleAgent{started: make(chan struct{}), events: make(chan event.Event)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Launch(ctx, a, WithInput(strings.NewReader("hello\n")), WithOutput(io.Discard))
	}()
	select {
	case <-a.started:
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Launch = %v, want context.Canceled", err)
		}
		close(a.events)
	case <-time.After(time.Second):
		close(a.events)
		<-done
		t.Fatal("Launch waited for the producer to close its channel after cancellation")
	}
}

// promptSignal reports when Launch reaches a specific prompt without sharing
// a bytes.Buffer between the console goroutine and the test.
type promptSignal struct {
	marker  string
	reached chan struct{}
	once    sync.Once
}

func (w *promptSignal) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		w.once.Do(func() { close(w.reached) })
	}
	return len(p), nil
}

func TestLaunchActiveCallerCancellation(t *testing.T) {
	for _, phase := range []string{"prompt", "reply", "confirmation"} {
		for _, deadline := range []bool{false, true} {
			t.Run(phase+"/"+map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				}
				defer cancel()
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				a := &unclosedConsoleAgent{started: make(chan struct{}), events: make(chan event.Event, 1)}
				defer close(a.events)
				out := &promptSignal{marker: "user> ", reached: make(chan struct{})}
				var reached <-chan struct{} = out.reached
				if phase == "reply" {
					reached = a.started
				}
				if phase == "confirmation" {
					out.marker = "Allow '"
					a.events <- event.NewRequireUserConfirmEvent("reply", []message.ToolCallBlock{{Type: "tool_call", ID: "call", Name: "action"}})
				}
				done := make(chan error, 1)
				go func() { done <- Launch(ctx, a, WithInput(reader), WithOutput(out)) }()
				if phase != "prompt" {
					go func() { _, _ = io.WriteString(writer, "hello\n") }()
				}
				select {
				case <-reached:
					if ctx.Err() != nil {
						t.Fatal("caller context ended before the active phase")
					}
				case <-time.After(time.Second):
					t.Fatal("console did not reach " + phase)
				}
				if !deadline {
					cancel()
				}
				select {
				case err := <-done:
					want := context.Canceled
					if deadline {
						want = context.DeadlineExceeded
					}
					if !errors.Is(err, want) {
						t.Fatalf("Launch = %v, want %v", err, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Launch did not return on caller cancellation")
				}
			})
		}
	}
}
