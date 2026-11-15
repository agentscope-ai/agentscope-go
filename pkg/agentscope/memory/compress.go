package memory

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// CompressionConfig controls a simple conversation compression strategy.
// It currently uses message count instead of precise token counts; this can be
// swapped for a token-based implementation later without changing the interface.
type CompressionConfig struct {
	Enable          bool
	TriggerMessages int // Number of messages after which compression is triggered.
	KeepRecent      int // Number of most recent messages to keep.
}

// CompressMessages compresses history according to the config.
// The current implementation simply keeps the most recent N messages.
func CompressMessages(history []*message.Msg, cfg CompressionConfig) []*message.Msg {
	if !cfg.Enable || cfg.TriggerMessages <= 0 {
		return history
	}
	if len(history) <= cfg.TriggerMessages {
		return history
	}
	if cfg.KeepRecent <= 0 || cfg.KeepRecent >= len(history) {
		return history
	}
	start := len(history) - cfg.KeepRecent
	return history[start:]
}
