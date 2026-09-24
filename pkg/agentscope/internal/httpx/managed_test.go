package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
)

func managedHTTPContext(t *testing.T, url string) (context.Context, func(error), *inference.Deployment) {
	t.Helper()
	c := inference.NewController(inference.NewLedger(10))
	t.Cleanup(c.Close)
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: url}, PoolID: "p", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := d.Start(inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"}), "reasoning")
	return ctx, finish, d
}
func TestManagedJSONAndSSELifecycle(t *testing.T) {
	for _, mode := range []string{"json", "sse", "unterminated-event", "failure"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch mode {
				case "json":
					w.Write([]byte(`{"value":1}`))
				case "sse":
					w.Write([]byte("data: [DONE]\n\n"))
				case "unterminated-event":
					w.Write([]byte("data: [DONE]"))
				case "failure":
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			ctx, finish, d := managedHTTPContext(t, server.URL)
			if mode == "json" {
				var out map[string]int
				err := DoJSONRequest(ctx, server.Client(), "POST", server.URL, struct{}{}, &out, nil)
				finish(err)
				if err != nil || out["value"] != 1 {
					t.Fatalf("managed JSON: %v %v", out, err)
				}
				return
			}
			events, err := DoSSERequest(ctx, server.Client(), "POST", server.URL, struct{}{}, nil)
			if mode == "failure" {
				finish(err)
				if err == nil || d.Stats().Active != 0 {
					t.Fatal("setup failure leaked permit")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var data string
			for e := range events {
				data = e.Data
			}
			if data != "[DONE]" || inference.CurrentOperation(ctx).StreamError() != nil || d.Stats().Active != 1 {
				t.Fatal("raw stream evidence or consumer ownership lost")
			}
			finish(nil)
			if d.Stats().Active != 0 {
				t.Fatal("stream finish did not release")
			}
		})
	}
}
