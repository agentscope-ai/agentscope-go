package bailian_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/bailian"
)

// TestLiveContract checks protocol compatibility, not quality or cost savings.
func TestLiveContract(t *testing.T) {
	if os.Getenv("BAILIAN_LIVE_TEST") != "1" {
		t.Skip("set BAILIAN_LIVE_TEST=1 to enable one real, billable Bailian request")
	}
	key := os.Getenv("DASHSCOPE_API_KEY")
	if key == "" {
		t.Skip("DASHSCOPE_API_KEY is missing; live contract not tested")
	}
	endpoint, model := os.Getenv("BAILIAN_LIVE_ENDPOINT"), os.Getenv("BAILIAN_LIVE_MODEL")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Path != "/compatible-mode/v1/systemone" || !(strings.HasSuffix(u.Hostname(), ".cn-beijing.maas.aliyuncs.com") || strings.HasSuffix(u.Hostname(), ".ap-southeast-1.maas.aliyuncs.com")) {
		t.Fatal("BAILIAN_LIVE_ENDPOINT must be a full HTTPS workspace System One URL in Beijing or Singapore")
	}
	if model == "" {
		t.Fatal("BAILIAN_LIVE_MODEL must be explicitly configured (for example decision-model-preview)")
	}
	cl := client(t, bailian.Config{APIKey: key, Model: model, Endpoint: endpoint})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := request()
	out, err := cl.Classify(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ValidateResponse(r, out); err != nil {
		t.Fatal(err)
	}
	if len(out.Attempts) != 1 || out.Attempts[0].Kind != c.PhysicalAttempt || out.Attempts[0].RequestedModel != model {
		t.Fatal("unexpected live attempt record")
	}
	t.Logf("live protocol checked: requested=%s returned=%s; no quality or savings claim", model, out.Model)
}
