package jev_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/internal/systemoneconfig"
	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

// This opt-in test checks protocol compatibility, not classification quality.
func TestLiveContract(t *testing.T) {
	if os.Getenv("JEV_LIVE_TEST") != "1" {
		t.Skip("set JEV_LIVE_TEST=1 to enable a real, billable TypeSafe request")
	}
	cfg, err := liveContractConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Skip("TYPESAFE_API_KEY is missing; live contract not tested")
	}
	model := cfg.Model
	cl, err := jev.New(*cfg)
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

// The default branch retains the hosted key/validation order. Local overrides
// are read only after TestLiveContract's explicit live opt-in.
func liveContractConfig(getenv func(string) string) (*jev.Config, error) {
	cfg, err := systemoneconfig.Local(getenv)
	if err != nil || cfg != nil {
		return cfg, err
	}
	key := getenv("TYPESAFE_API_KEY")
	if key == "" {
		return nil, nil
	}
	model := getenv("JEV_LIVE_MODEL")
	if !regexp.MustCompile(`^jev-[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(model) {
		return nil, fmt.Errorf("JEV_LIVE_MODEL must be a pinned version, for example jev-1.13.0")
	}
	return &jev.Config{APIKey: key, Model: model}, nil
}
