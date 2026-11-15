package logging

import (
	"io"
	"log/slog"
	"os"
)

type InitOption func(*initConfig)

type initConfig struct {
	level   slog.Level
	json    bool
	writer  io.Writer
	handler slog.Handler
}

func WithLevel(level slog.Level) InitOption {
	return func(c *initConfig) { c.level = level }
}

func WithJSON() InitOption {
	return func(c *initConfig) { c.json = true }
}

func WithWriter(w io.Writer) InitOption {
	return func(c *initConfig) { c.writer = w }
}

func withHandler(h slog.Handler) InitOption {
	return func(c *initConfig) { c.handler = h }
}

func Init(opts ...InitOption) {
	cfg := &initConfig{
		level:  slog.LevelInfo,
		writer: os.Stderr,
	}
	for _, o := range opts {
		o(cfg)
	}

	var h slog.Handler
	if cfg.handler != nil {
		h = cfg.handler
	} else if cfg.json {
		h = slog.NewJSONHandler(cfg.writer, &slog.HandlerOptions{Level: cfg.level})
	} else {
		h = slog.NewTextHandler(cfg.writer, &slog.HandlerOptions{Level: cfg.level})
	}

	SetDefault(slog.New(h))
}
