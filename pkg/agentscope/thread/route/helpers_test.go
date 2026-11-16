package route

import (
	"context"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
)

const (
	edgeDesc  = "Small local model on the device; holds device tools."
	cloudDesc = "Large hosted model with web tools."
)

func textBlocks(s string) []message.ContentBlock {
	return []message.ContentBlock{message.TextBlock{Type: "text", Text: s}}
}

func edgeCand(available bool, groups ...string) thread.RouteCandidate {
	return thread.RouteCandidate{
		Descriptor: thread.Descriptor{Name: "edge", Zone: thread.ZoneEdge, Description: edgeDesc, Window: 8192, ToolGroups: groups},
		Available:  available,
	}
}

func cloudCand(available bool, groups ...string) thread.RouteCandidate {
	return thread.RouteCandidate{
		Descriptor: thread.Descriptor{Name: "cloud", Zone: thread.ZoneCloud, Description: cloudDesc, Window: 200000, ToolGroups: groups},
		Available:  available,
	}
}

func namedCand(name string, zone thread.Zone, available bool) thread.RouteCandidate {
	return thread.RouteCandidate{Descriptor: thread.Descriptor{Name: name, Zone: zone}, Available: available}
}

// request returns a query with both participants available.
func request() thread.RouteRequest {
	return thread.RouteRequest{
		OperationID:  "op-1",
		Query:        "What is the weather like?",
		Participants: []thread.RouteCandidate{edgeCand(true, "device", "files"), cloudCand(true, "web", "files")},
	}
}

func newRouter(t *testing.T, cfg Config) *Router { //nolint:gocritic // test helper
	t.Helper()
	if cfg.Edge == "" {
		cfg.Edge = "edge"
	}
	if cfg.Cloud == "" {
		cfg.Cloud = "cloud"
	}
	if cfg.Mode == "" {
		cfg.Mode = Hybrid
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func i64(v int64) *int64 { return &v }

// choiceFake answers the owner question with choice at the given confidence,
// assuming the candidates are edge and cloud.
func choiceFake(choice string, confidence float64) *classifier.Fake {
	other := "cloud"
	if choice == "cloud" {
		other = "edge"
	}
	return classifier.NewFake(&classifier.FakeConfig{Response: classifier.Response{
		Model: "jev-test",
		Answers: map[string]classifier.Answer{
			QuestionID: &classifier.ChoiceAnswer{
				Choice:        choice,
				Confidence:    confidence,
				Probabilities: map[string]float64{choice: 0.75, other: 0.25},
			},
		},
		Attempts: []classifier.Attempt{{
			Kind:           classifier.SimulatedAttempt,
			RequestedModel: "jev-test",
			Usage:          &classifier.Usage{InputTokens: i64(120), OutputTokens: i64(3)},
		}},
	}})
}

// recorder wraps a classifier and records what it was asked.
type recorder struct {
	inner    classifier.DispatchClassifier
	endpoint string

	mu     sync.Mutex
	reqs   []classifier.Request
	limits []classifier.DispatchLimits
}

func (r *recorder) Classify(ctx context.Context, req classifier.Request) (*classifier.Response, error) {
	return r.ClassifyWithDispatch(ctx, req, classifier.DispatchLimits{}, nil)
}

func (r *recorder) Target() (endpoint, model string) {
	endpoint, model = r.inner.Target()
	if r.endpoint != "" {
		endpoint = r.endpoint
	}
	return endpoint, model
}

func (r *recorder) ClassifyWithDispatch(ctx context.Context, req classifier.Request, limits classifier.DispatchLimits, begin func(classifier.AttemptKind) error) (*classifier.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.limits = append(r.limits, limits)
	r.mu.Unlock()
	return r.inner.ClassifyWithDispatch(ctx, req, limits, begin)
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *recorder) last(t *testing.T) classifier.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("the classifier was not called")
	}
	return r.reqs[len(r.reqs)-1]
}

// stub returns a fixed response and error without validating anything.
type stub struct {
	endpoint string
	resp     *classifier.Response
	err      error
}

func (s *stub) Classify(ctx context.Context, req classifier.Request) (*classifier.Response, error) {
	return s.ClassifyWithDispatch(ctx, req, classifier.DispatchLimits{}, nil)
}

func (s *stub) Target() (endpoint, model string) { return s.endpoint, "stub-model" }

func (s *stub) ClassifyWithDispatch(_ context.Context, _ classifier.Request, _ classifier.DispatchLimits, _ func(classifier.AttemptKind) error) (*classifier.Response, error) {
	return s.resp, s.err
}
