package tts

import "context"

// RealtimeModel extends Model with streaming input support.
// Connect opens a persistent session. Push sends text chunks and receives
// audio chunks incrementally. Close terminates the session.
type RealtimeModel interface {
	Model

	Connect(ctx context.Context) error
	Push(ctx context.Context, text string) (*Response, error)
	Close() error
}

// IsRealtime returns true if the model implements RealtimeModel.
func IsRealtime(m Model) bool {
	_, ok := m.(RealtimeModel)
	return ok
}
