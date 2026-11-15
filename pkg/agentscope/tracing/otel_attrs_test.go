package tracing

import "testing"

// TestOTELTracer_ImplementsAttributedTracer ensures OTELTracer satisfies the
// AttributedTracer interface so GenAI span attributes (model/tokens/cost) are
// actually recorded rather than silently dropped.
func TestOTELTracer_ImplementsAttributedTracer(t *testing.T) {
	var _ AttributedTracer = OTELTracer{}
}

func TestToOTELAttrs_Types(t *testing.T) {
	kvs := toOTELAttrs([]SpanAttribute{
		{Key: "model", Value: "gpt-4"},
		{Key: "input_tokens", Value: 123},
		{Key: "cost", Value: 0.0025},
		{Key: "cached", Value: true},
	})
	if len(kvs) != 4 {
		t.Fatalf("expected 4 attrs, got %d", len(kvs))
	}
	if string(kvs[0].Key) != "model" || kvs[0].Value.AsString() != "gpt-4" {
		t.Errorf("string attr wrong: %+v", kvs[0])
	}
	if kvs[1].Value.AsInt64() != 123 {
		t.Errorf("int attr wrong: %+v", kvs[1])
	}
	if kvs[3].Value.AsBool() != true {
		t.Errorf("bool attr wrong: %+v", kvs[3])
	}
}

// TestOTELTracer_NilTracerSafe ensures a zero-value tracer does not panic.
func TestOTELTracer_NilTracerSafe(t *testing.T) {
	tr := OTELTracer{}
	ctx, end := tr.StartSpanWithAttrs(nil, "x", SpanAttribute{Key: "k", Value: "v"}) //nolint:staticcheck
	_ = ctx
	end()
}
