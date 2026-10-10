package jev_test

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

// This opt-in test checks protocol compatibility, not classification quality.
func TestLiveContract(t *testing.T) {
	if os.Getenv("JEV_LIVE_TEST") != "1" {
		t.Skip("set JEV_LIVE_TEST=1 to enable a real, billable TypeSafe request")
	}
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY is missing; live contract not tested")
	}
	model := os.Getenv("JEV_LIVE_MODEL")
	if !regexp.MustCompile(`^jev-[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(model) {
		t.Fatal("JEV_LIVE_MODEL must be a pinned version, for example jev-1.13.0")
	}
	cl, err := jev.New(jev.Config{APIKey: key, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := cl.Classify(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ValidateResponse(request(), out); err != nil {
		t.Fatal(err)
	}
	if len(out.Attempts) != 1 || out.Attempts[0].Kind != c.PhysicalAttempt || out.Model != model {
		t.Fatal("unexpected attempt count or resolved model")
	}
	t.Logf("live protocol verified: requested=%s returned=%s; no quality or cost claim", model, out.Model)
}
