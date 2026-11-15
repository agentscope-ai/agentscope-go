package logging

import (
	"io"
	"log/slog"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestLogrusToSlogHook(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	h := &capturingHandler{}
	SetDefault(slog.New(h))

	l := logrus.New()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.TraceLevel)
	l.AddHook(&LogrusToSlogHook{})

	l.WithField("key", "val").Info("test msg")

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", r.Level, slog.LevelInfo)
	}
	if r.Message != "test msg" {
		t.Errorf("message = %q, want %q", r.Message, "test msg")
	}

	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "key" && a.Value.String() == "val" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Errorf("attr key=val not found in record")
	}
}

func TestLogrusToSlogHookLevels(t *testing.T) {
	cases := map[logrus.Level]slog.Level{
		logrus.TraceLevel: slog.LevelDebug,
		logrus.DebugLevel: slog.LevelDebug,
		logrus.InfoLevel:  slog.LevelInfo,
		logrus.WarnLevel:  slog.LevelWarn,
		logrus.ErrorLevel: slog.LevelError,
		logrus.FatalLevel: slog.LevelError,
		logrus.PanicLevel: slog.LevelError,
	}
	for in, want := range cases {
		if got := logrusLevelToSlog(in); got != want {
			t.Errorf("logrusLevelToSlog(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestNoDuplicateOutput(t *testing.T) {
	orig := Default()
	defer SetDefault(orig)

	h := &capturingHandler{}
	SetDefault(slog.New(h))

	l := logrus.New()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.TraceLevel)
	l.AddHook(&LogrusToSlogHook{})

	l.Info("once")

	if recs := h.snapshot(); len(recs) != 1 {
		t.Fatalf("got %d records, want exactly 1", len(recs))
	}
}
