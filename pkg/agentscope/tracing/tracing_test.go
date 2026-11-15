package tracing

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

func TestNoopTracer(t *testing.T) {
	tr := NoopTracer{}
	ctx, end := tr.StartSpan(context.Background(), "test_span")
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	end() // should not panic
}

func TestLoggerTracer(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	tr := LoggerTracer{Logger: logger}

	_, end := tr.StartSpan(context.Background(), "my_span")
	end()

	output := buf.String()
	if !strings.Contains(output, "start span name=my_span") {
		t.Errorf("expected start span log, got: %s", output)
	}
	if !strings.Contains(output, "end span name=my_span") {
		t.Errorf("expected end span log, got: %s", output)
	}
	if !strings.Contains(output, "duration=") {
		t.Errorf("expected duration in end span log, got: %s", output)
	}
}

func TestSetupTracing_Nil(t *testing.T) {
	SetupTracing(nil)
	if _, ok := TracerInstance().(NoopTracer); !ok {
		t.Error("expected NoopTracer after SetupTracing(nil)")
	}
}

func TestSetupTracing_Custom(t *testing.T) {
	custom := &LoggerTracer{Logger: log.New(&bytes.Buffer{}, "", 0)}
	SetupTracing(custom)
	if TracerInstance() != custom {
		t.Error("expected custom tracer to be installed")
	}
	// Reset
	SetupTracing(nil)
}

func TestAttributedTracer_Interface(t *testing.T) {
	// Verify that types implementing AttributedTracer also implement Tracer
	var _ Tracer = NoopTracer{}

	// SpanAttribute construction
	attr := SpanAttribute{Key: "model", Value: "gpt-4"}
	if attr.Key != "model" {
		t.Error("unexpected key")
	}
	if attr.Value != "gpt-4" {
		t.Error("unexpected value")
	}
}

func TestTracingHookCreatesSpans(t *testing.T) {
	recorder := &spanRecorder{}

	h := NewTracingHook(recorder)
	h.OnLoopStart()
	h.BeforeModelCall(protocol.StateReason, 0)
	h.AfterModelCall(protocol.StateReason, 0, nil)
	h.OnLoopEnd(nil)

	if len(recorder.spans) < 2 {
		t.Errorf("got %d spans, want at least 2 (loop + model_call)", len(recorder.spans))
	}

	names := make(map[string]bool)
	for _, s := range recorder.spans {
		names[s] = true
	}
	if !names["loop.run"] {
		t.Error("missing loop.run span")
	}
	if !names["loop.model_call"] {
		t.Error("missing loop.model_call span")
	}
}

func TestTracingHookToolSpans(t *testing.T) {
	recorder := &spanRecorder{}

	h := NewTracingHook(recorder)
	h.OnLoopStart()
	h.BeforeToolExec(protocol.StateAct, 0, "bash")
	h.AfterToolExec(protocol.StateAct, 0, "bash", nil)
	h.OnLoopEnd(nil)

	names := make(map[string]bool)
	for _, s := range recorder.spans {
		names[s] = true
	}
	if !names["loop.tool.bash"] {
		t.Error("missing loop.tool.bash span")
	}
}

func TestTracingHookNoopTracerDoesNotPanic(t *testing.T) {
	h := NewTracingHook(NoopTracer{})
	h.OnLoopStart()
	h.BeforeModelCall(protocol.StateReason, 0)
	h.AfterModelCall(protocol.StateReason, 0, nil)
	h.BeforeToolExec(protocol.StateAct, 0, "bash")
	h.AfterToolExec(protocol.StateAct, 0, "bash", nil)
	h.OnStateTransition(protocol.StateReason, protocol.StateInspect, 0)
	h.OnLoopEnd(nil)
}

type spanRecorder struct {
	mu    sync.Mutex
	spans []string
}

func (r *spanRecorder) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	r.mu.Lock()
	r.spans = append(r.spans, name)
	r.mu.Unlock()
	return ctx, func() {}
}
