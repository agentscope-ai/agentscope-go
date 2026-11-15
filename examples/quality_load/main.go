// quality_load runs an offline quality/load contract fixture by default.
// -live uses an explicitly configured OpenAI-compatible service instead.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay/evalkit"
)

//go:embed workload.json
var workload []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	live := flag.Bool("live", false, "call a configured inference service (may incur costs)")
	revision := flag.String("revision", "working-tree", "source revision to record")
	manifestPath := flag.String("manifest", "", "optional version-1 JSON workload")
	flag.Parse()
	raw := workload
	var err error
	if *manifestPath != "" {
		raw, err = os.ReadFile(*manifestPath)
		if err != nil {
			return err
		}
	}
	var manifest evalkit.WorkloadManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	manifest.RunID = fmt.Sprintf("quality-%d", time.Now().UnixNano())
	manifest.SourceRevision = *revision
	if manifest.Metadata == nil {
		manifest.Metadata = map[string]string{}
	}
	manifest.Metadata["mode"] = "offline-fixture"
	cfg := &evalkit.LoadConfig{MaxInFlight: 2, TaskTimeout: 30 * time.Second, DrainTimeout: 35 * time.Second, MaxPendingScores: 100, MaxScorers: 2, ScoreTimeout: time.Second, ScorePhaseTimeout: 10 * time.Second,
		NewModel: func(_ context.Context, t evalkit.TaskSpec) (model.ChatModel, error) {
			answers := map[string]string{"arithmetic": "4", "go-keyword": "go"}
			answer, ok := answers[t.ID]
			if !ok {
				return nil, fmt.Errorf("no offline response for task %q", t.ID)
			}
			return fixtureModel{answer: answer}, nil
		},
	}
	if *live {
		endpoint, name, key := os.Getenv("OPENAI_BASE_URL"), os.Getenv("MODEL_NAME"), os.Getenv("OPENAI_API_KEY")
		if endpoint == "" || name == "" || key == "" || *revision == "working-tree" {
			return fmt.Errorf("-live requires OPENAI_BASE_URL, MODEL_NAME, OPENAI_API_KEY and -revision")
		}
		manifest.Metadata["mode"], manifest.Metadata["model"] = "live", name
		cfg.NewModel = func(context.Context, evalkit.TaskSpec) (model.ChatModel, error) {
			return model.NewOpenAIChatModel(model.OpenAIConfig{BaseURL: endpoint, Model: name, APIKey: key})
		}
	}
	report, runErr := (&evalkit.Runner{}).RunLoad(context.Background(), context.Background(), &manifest, cfg)
	if report != nil {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	}
	return runErr
}

type fixtureModel struct{ answer string }

func (m fixtureModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: m.answer}}, IsLast: true}, nil
}
func (fixtureModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (fixtureModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }
