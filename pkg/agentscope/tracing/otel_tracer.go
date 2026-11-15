package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// OTELTracer is a Tracer implementation backed by OpenTelemetry's trace.Tracer.
// Users are expected to configure the OTEL TracerProvider and pass a trace.Tracer
// instance into this struct.
type OTELTracer struct {
	Tracer trace.Tracer
}

// StartSpan creates an OpenTelemetry span with the given name and returns a
// function that ends the span when called.
func (o OTELTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	if o.Tracer == nil {
		// Fallback to the global noop tracer if none provided.
		return ctx, func() {}
	}
	ctx, span := o.Tracer.Start(ctx, name)
	return ctx, func() {
		span.End()
	}
}

// StartSpanWithAttrs creates an OpenTelemetry span and attaches the given
// attributes (model, token counts, cost, etc.) so they are recorded by the
// exporter. Implements AttributedTracer.
func (o OTELTracer) StartSpanWithAttrs(ctx context.Context, name string, attrs ...SpanAttribute) (context.Context, func()) {
	if o.Tracer == nil {
		return ctx, func() {}
	}
	ctx, span := o.Tracer.Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(toOTELAttrs(attrs)...)
	}
	return ctx, func() {
		span.End()
	}
}

// AddSpanAttr attaches an attribute to the span stored in ctx (if any).
// Implements LateAttributer.
func (o OTELTracer) AddSpanAttr(ctx context.Context, attr SpanAttribute) {
	span := trace.SpanFromContext(ctx)
	if span == nil {
		return
	}
	span.SetAttributes(toOTELAttrs([]SpanAttribute{attr})...)
}

// toOTELAttrs converts framework SpanAttributes to OpenTelemetry key-values,
// preserving the underlying scalar type where possible.
func toOTELAttrs(attrs []SpanAttribute) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.Value.(type) {
		case string:
			out = append(out, attribute.String(a.Key, v))
		case bool:
			out = append(out, attribute.Bool(a.Key, v))
		case int:
			out = append(out, attribute.Int(a.Key, v))
		case int64:
			out = append(out, attribute.Int64(a.Key, v))
		case float64:
			out = append(out, attribute.Float64(a.Key, v))
		case float32:
			out = append(out, attribute.Float64(a.Key, float64(v)))
		default:
			out = append(out, attribute.String(a.Key, fmt.Sprintf("%v", v)))
		}
	}
	return out
}

// Compile-time check that OTELTracer implements AttributedTracer.
var _ AttributedTracer = OTELTracer{}
