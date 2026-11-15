package tracing

import (
	"context"
	"log"
	"time"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
)

// Tracer is a minimal tracing interface that can be backed by OpenTelemetry or any other implementation.
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, func())
}

// SpanAttribute is a key-value pair attached to a span.
type SpanAttribute struct {
	Key   string
	Value any // string, int, float64, bool
}

// LateAttributer is an optional extension of Tracer that supports adding
// attributes to an already-started span (identified by the ctx returned
// from StartSpan). Reply IDs are only known after the reply loop starts,
// so long-lived spans (e.g. invoke_agent) need late attributes.
type LateAttributer interface {
	AddSpanAttr(ctx context.Context, attr SpanAttribute)
}

// AttributedTracer is an optional extension of Tracer that supports
// attaching attributes to spans. Implementations that support richer
// tracing (like OTEL) should implement this interface.
type AttributedTracer interface {
	Tracer
	// StartSpanWithAttrs creates a span with initial attributes.
	StartSpanWithAttrs(ctx context.Context, name string, attrs ...SpanAttribute) (context.Context, func())
}

// NoopTracer is the default implementation that performs no tracing and only keeps the interface wired.
type NoopTracer struct{}

// StartSpan returns the context unchanged and a no-op end function.
func (NoopTracer) StartSpan(ctx context.Context, _ string) (context.Context, func()) {
	return ctx, func() {}
}

var tracer Tracer = NoopTracer{}

// LoggerTracer is a reference implementation that logs span start/end events to a logger.
type LoggerTracer struct {
	Logger *log.Logger
}

// StartSpan logs the span start and returns an end function that logs the span duration.
func (l LoggerTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	if l.Logger == nil {
		l.Logger = as.Logger()
	}
	start := time.Now()
	l.Logger.Printf("[trace] start span name=%s at=%s", name, start.Format(time.RFC3339Nano))
	return ctx, func() {
		end := time.Now()
		l.Logger.Printf("[trace] end span name=%s duration=%s", name, end.Sub(start))
	}
}

// SetupTracing installs a custom Tracer, for example one backed by OpenTelemetry.
func SetupTracing(t Tracer) {
	if t == nil {
		tracer = NoopTracer{}
		return
	}
	tracer = t
	cfg := as.ConfigSnapshot()
	cfg.TraceEnabled = true
	as.Logger().Printf("tracing enabled at %s", time.Now().Format(time.RFC3339Nano))
}

// TracerInstance returns the currently installed global Tracer.
func TracerInstance() Tracer {
	return tracer
}
