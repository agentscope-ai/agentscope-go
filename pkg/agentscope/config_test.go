package agentscope

import (
	"testing"
)

// Basic sanity check for Init, ConfigSnapshot, Logger and Log.
func TestInitAndConfigSnapshot(t *testing.T) {
	Init(
		WithProject("TestProject"),
		WithName("TestName"),
		WithRunID("test-run-id"),
		WithLogging("", "debug"),
	)

	cfg := ConfigSnapshot()
	if cfg.Project != "TestProject" {
		t.Fatalf("unexpected project: %s", cfg.Project)
	}
	if cfg.Name != "TestName" {
		t.Fatalf("unexpected name: %s", cfg.Name)
	}
	if cfg.RunID != "test-run-id" {
		t.Fatalf("unexpected run id: %s", cfg.RunID)
	}

	if Logger() == nil {
		t.Fatalf("Logger() returned nil")
	}
	if Log() == nil {
		t.Fatalf("Log() returned nil")
	}

	// Ensure loggers are usable without panic.
	Logger().Println("test logger line")
	Log().Debug("test logrus line")
}

// When log path is invalid, setupLogger should fall back to stdout without panic.
func TestInitWithInvalidLogPath(t *testing.T) {
	Init(
		WithProject("BadPathProject"),
		WithLogging("/root/__no_permission__/agentscope.log", "info"),
	)

	Logger().Println("log after invalid path")
	Log().Info("logrus after invalid path")
}
