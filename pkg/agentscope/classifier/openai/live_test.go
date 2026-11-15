package openai_test

import (
	"context"
	"os"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/openai"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
)

// TestLiveContract is opt-in and makes one potentially billable request to the
// default OpenAI endpoint. It checks wire compatibility, not answer quality.
func TestLiveContract(t *testing.T) {
	if os.Getenv("OPENAI_DECISIONS_LIVE_TEST") != "1" {
		t.Skip("set OPENAI_DECISIONS_LIVE_TEST=1 to enable a real, billable OpenAI request")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY is missing; live contract not tested")
	}
	model := os.Getenv("OPENAI_DECISIONS_LIVE_MODEL")
	if model == "" {
		t.Fatal("OPENAI_DECISIONS_LIVE_MODEL must explicitly name a supported model")
	}
	cl, err := openai.New(openai.Config{Credential: &credential.OpenAICredential{Key: key}, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := request()
	r.State = []byte(`"Export fails in one browser; another browser works."`)
	out, err := cl.Classify(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ValidateResponse(r, out); err != nil {
		t.Fatal(err)
	}
	if len(out.Attempts) != 1 || out.Attempts[0].Kind != c.PhysicalAttempt || out.Attempts[0].Model != out.Model || out.Attempts[0].RequestedModel != model {
		t.Fatal("unexpected attempt accounting")
	}
	t.Logf("live protocol verified: requested=%s returned=%s; complete distributions checked, no quality or cost claim", model, out.Model)
}
