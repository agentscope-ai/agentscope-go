package tts

import "context"

// Response holds the result of a TTS synthesis call.
type Response struct {
	Content   []byte // audio data (e.g. raw PCM or WAV)
	MediaType string // e.g. "audio/wav", "audio/pcm"
	IsLast    bool   // true for the final chunk in a stream
	Usage     *Usage
}

// Usage tracks resource consumption for a TTS call.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Time         float64 // elapsed seconds
}

// Model abstracts a TTS engine that converts text into audio.
type Model interface {
	// Synthesize converts text to audio and returns the complete result.
	Synthesize(ctx context.Context, text string) (*Response, error)

	// SynthesizeStream converts text to audio and returns a channel of
	// incremental audio chunks. Returns ErrStreamNotSupported if the
	// implementation does not support streaming.
	SynthesizeStream(ctx context.Context, text string) (<-chan Response, error)

	// ModelName returns the model identifier.
	ModelName() string
}

// ErrStreamNotSupported is returned by SynthesizeStream when the model does
// not support streaming output.
var ErrStreamNotSupported = &streamNotSupportedError{}

type streamNotSupportedError struct{}

func (e *streamNotSupportedError) Error() string { return "tts: streaming not supported" }

// DummyModel is a placeholder that returns empty audio. Useful for tests.
type DummyModel struct{}

func (DummyModel) Synthesize(_ context.Context, _ string) (*Response, error) {
	return &Response{Content: []byte{}, MediaType: "audio/wav", IsLast: true}, nil
}

func (DummyModel) SynthesizeStream(_ context.Context, _ string) (<-chan Response, error) {
	return nil, ErrStreamNotSupported
}

func (DummyModel) ModelName() string { return "dummy" }
