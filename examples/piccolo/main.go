// Piccolo text decisions through the existing Jev adapter. The default uses a
// synthetic HTTP fixture; -live requires an already-running local server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/internal/systemoneconfig"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

func main() {
	live := flag.Bool("live", false, "send synthetic questions to an explicitly configured local System One server")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, *live, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type summary struct {
	Evidence            string  `json:"evidence"`
	Model               string  `json:"model"`
	ReadOnlyProbability float64 `json:"read_only_probability"`
	Action              string  `json:"action"`
	RiskScore           float64 `json:"risk_score"`
	Attempts            int     `json:"attempts"`
	InputTokens         *int64  `json:"input_tokens"`
	OutputTokens        *int64  `json:"output_tokens"`
}

func run(ctx context.Context, live bool, getenv func(string) string, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var cfg *jev.Config
	evidence := "fixture"
	if live {
		var err error
		cfg, err = systemoneconfig.Local(getenv)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("-live requires SYSTEMONE_LOCAL_ENDPOINT and SYSTEMONE_LOCAL_MODEL")
		}
		evidence = "live"
	} else {
		server := fixtureServer()
		defer server.Close()
		cfg = &jev.Config{Endpoint: server.URL + "/v1/systemone", Model: "Piccolo-fixture", APIKey: "local-fixture"}
	}
	client, err := jev.New(*cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req := classifier.Request{OperationID: "piccolo-example", State: json.RawMessage(`"The user asks for the total in a CSV file. No file changes are requested."`), Questions: map[string]classifier.Question{
		"read_only": &classifier.BinaryQuestion{Instructions: "Is the requested task read-only?"},
		"action":    &classifier.ChoiceQuestion{Instructions: "Select the next action to propose; do not execute it.", Choices: map[string]string{"read": "Read the CSV to calculate the total", "ask": "Ask the user for clarification"}},
		"risk":      &classifier.ScoreQuestion{Instructions: "Rate the risk of changing files for this request.", Levels: []string{"low", "high"}},
	}}
	out, err := client.Classify(ctx, req)
	if err != nil {
		return err
	}
	if err := classifier.ValidateResponse(req, out); err != nil {
		return err
	}
	// Some runtimes ignore a request's model field. Check actual identity before
	// displaying answers; a successful HTTP status alone cannot establish it.
	if out.Model != cfg.Model || len(out.Attempts) != 1 || out.Attempts[0].Kind != classifier.PhysicalAttempt {
		return fmt.Errorf("unexpected model identity or attempt accounting")
	}
	result := summary{Evidence: evidence, Model: out.Model, ReadOnlyProbability: out.Answers["read_only"].(*classifier.BinaryAnswer).Probability, Action: out.Answers["action"].(*classifier.ChoiceAnswer).Choice, RiskScore: out.Answers["risk"].(*classifier.ScoreAnswer).Score, Attempts: len(out.Attempts)}
	if usage := out.Attempts[0].Usage; usage != nil {
		result.InputTokens, result.OutputTokens = usage.InputTokens, usage.OutputTokens
	}
	// These are model judgments, not tool execution, permission approval or a
	// calibrated safety policy. Unknown usage remains null, never an estimate.
	return json.NewEncoder(w).Encode(result)
}

func fixtureServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"Piccolo-fixture","answers":{"read_only":{"type":"noul","noul":0.9},"action":{"type":"choice","choice":"read","confidence":0.8,"probabilities":{"read":0.9,"ask":0.1}},"risk":{"type":"score","score":0.1,"confidence":0.8,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.9,"1":0.1}}},"usage":{"input_tokens":12,"output_tokens":0}}`)
	}))
}
