package tune

import "context"

// Job describes the basic information of a tuning job.
type Job struct {
	ID     string
	Status string
	Meta   map[string]any
}

// Tuner abstracts prompt/model tuning capabilities.
type Tuner interface {
	// Submit a tuning job and return its ID.
	Submit(ctx context.Context, spec map[string]any) (string, error)
	// Status queries the current status of a tuning job.
	Status(ctx context.Context, id string) (*Job, error)
}

// NoopTuner is a placeholder implementation that returns fixed values, useful when no real backend is configured.
type NoopTuner struct{}

func (NoopTuner) Submit(ctx context.Context, spec map[string]any) (string, error) {
	_ = ctx
	_ = spec
	return "noop", nil
}

func (NoopTuner) Status(ctx context.Context, id string) (*Job, error) {
	_ = ctx
	return &Job{ID: id, Status: "completed", Meta: map[string]any{}}, nil
}
