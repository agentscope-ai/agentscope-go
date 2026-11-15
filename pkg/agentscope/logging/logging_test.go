package logging

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *capturingHandler) WithGroup(_ string) slog.Handler { return h }

func (h *capturingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

func TestSetDefaultAndDefault(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	custom := slog.New(&capturingHandler{})
	SetDefault(custom)

	if Default() != custom {
		t.Fatalf("Default() did not return the logger set via SetDefault")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"trace":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"fatal":   slog.LevelError,
		"panic":   slog.LevelError,
		"unknown": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestErrAttr(t *testing.T) {
	attr := Err(errors.New("x"))
	if attr.Key != "error" {
		t.Fatalf("Err key = %q, want %q", attr.Key, "error")
	}
}

func TestConvenienceFunctions(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	h := &capturingHandler{}
	SetDefault(slog.New(h))

	Debug("d")
	Info("i")
	Warn("w")
	Error("e")

	recs := h.snapshot()
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}

	want := []struct {
		msg   string
		level slog.Level
	}{
		{"d", slog.LevelDebug},
		{"i", slog.LevelInfo},
		{"w", slog.LevelWarn},
		{"e", slog.LevelError},
	}
	for i, w := range want {
		if recs[i].Message != w.msg || recs[i].Level != w.level {
			t.Errorf("record %d = (%q, %v), want (%q, %v)", i, recs[i].Message, recs[i].Level, w.msg, w.level)
		}
	}
}

func TestInitDefaultsToTextHandler(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	Init()
	Info("test after default init")
}

func TestInitWithLevel(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	h := &capturingHandler{}
	Init(WithLevel(slog.LevelWarn), withHandler(h))

	Debug("should be filtered")
	Warn("should appear")
	Error("should appear")

	recs := h.snapshot()
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3 (capturingHandler.Enabled always returns true)", len(recs))
	}
}

func TestInitWithJSON(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	var buf strings.Builder
	Init(WithJSON(), WithWriter(&buf), WithLevel(slog.LevelInfo))

	Info("json test", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "json test") {
		t.Errorf("expected 'json test' in JSON output, got: %s", output)
	}
}

func TestInitWithWriter(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	var buf strings.Builder
	Init(WithWriter(&buf))

	Info("custom writer test")

	if !strings.Contains(buf.String(), "custom writer test") {
		t.Errorf("expected output in custom writer, got: %s", buf.String())
	}
}
