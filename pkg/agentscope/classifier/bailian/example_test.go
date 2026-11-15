package bailian_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/bailian"
)

// Constructing a client does not contact the service. For live use, supply your
// API key and full workspace/region endpoint; the model is explicitly chosen.
func ExampleNew() {
	cl, err := bailian.New(bailian.Config{
		APIKey:   "example-key",
		Model:    "decision-model-preview",
		Endpoint: "https://workspace.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/systemone",
	})
	if err != nil {
		panic(err)
	}
	_, model := cl.Target()
	fmt.Println(model)
	// Output: decision-model-preview
}

// This example uses a synthetic HTTP transport and makes no network requests.
func ExampleClient_Classify() {
	cl, err := bailian.New(bailian.Config{
		APIKey: "example-key", Model: "decision-model-preview",
		Endpoint:   "https://fixture.invalid/compatible-mode/v1/systemone",
		HTTPClient: &http.Client{Transport: exampleTransport{}},
	})
	if err != nil {
		panic(err)
	}
	out, err := cl.Classify(context.Background(), c.Request{
		OperationID: "ticket-1", State: json.RawMessage(`"My payment failed."`),
		Questions: map[string]c.Question{"refund": &c.BinaryQuestion{Instructions: "Does this request a refund?"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Answers["refund"].(*c.BinaryAnswer).Probability)
	fmt.Println(len(out.Attempts), out.Attempts[0].Usage == nil)
	// Output:
	// 0.8
	// 1 true
}

type exampleTransport struct{}

func (exampleTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"model":"decision-model-preview","answers":{"refund":{"type":"noul","noul":0.8}}}`))}, nil
}
