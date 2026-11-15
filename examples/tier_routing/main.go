package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

type task struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Input string `json:"input"`
}

type taskResult struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Latency  time.Duration     `json:"latency"`
	Attempts int               `json:"attempts"`
	Tiers    []tierrouter.Tier `json:"tiers"`
}

type groupReport struct {
	Group                string                  `json:"group"`
	Planned              int                     `json:"planned"`
	Completed            int                     `json:"completed"`
	Failed               int                     `json:"failed"`
	Requests             int                     `json:"requests"`
	UnknownUsageAttempts int                     `json:"unknown_usage_attempts"`
	TotalLatency         time.Duration           `json:"total_latency"`
	Results              []taskResult            `json:"results"`
	TierRequests         map[tierrouter.Tier]int `json:"tier_requests"`
}

var tasks = []task{
	{ID: "status", Kind: "local", Input: "Report current device status."},
	{ID: "scene", Kind: "scene", Input: "Describe the approved synthetic scene."},
	{ID: "plan", Kind: "plan", Input: "Plan the next synthetic step."},
	{ID: "second-plan", Kind: "plan", Input: "Revise the remaining synthetic step."},
}

func main() {
	reports, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(reports); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() ([]groupReport, error) {
	groups := []struct {
		name   string
		routed bool
	}{
		{name: "fixed-device"},
		{name: "fixed-edge"},
		{name: "fixed-cloud"},
		{name: "routed", routed: true},
	}
	reports := make([]groupReport, 0, len(groups))
	for _, group := range groups {
		report, err := runGroup(group.name, group.routed)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func runGroup(name string, routed bool) (groupReport, error) {
	device := tierrouter.NewFakeChatModel(tierrouter.TierDevice)
	device.Reply = "device answer"
	device.Delay = time.Millisecond
	edge := tierrouter.NewFakeChatModel(tierrouter.TierEdge)
	edge.Reply = "edge answer"
	edge.Delay = 2 * time.Millisecond
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.Reply = "cloud answer"
	cloud.Delay = 3 * time.Millisecond
	cloud.FailNext = 1
	models := map[tierrouter.Tier]*tierrouter.FakeChatModel{
		tierrouter.TierDevice: device,
		tierrouter.TierEdge:   edge,
		tierrouter.TierCloud:  cloud,
	}

	var chatModel model.ChatModel
	switch name {
	case "fixed-device":
		chatModel = device
	case "fixed-edge":
		chatModel = edge
	case "fixed-cloud":
		chatModel = cloud
	default:
		router, err := tierrouter.NewChatRouter(tierrouter.ChatRouterConfig{
			Policy: tierrouter.StaticPolicy{
				Rules: map[string][]tierrouter.Tier{
					"local": {tierrouter.TierDevice},
					"scene": {tierrouter.TierEdge, tierrouter.TierDevice},
					"plan":  {tierrouter.TierCloud, tierrouter.TierEdge, tierrouter.TierDevice},
				},
				Default: []tierrouter.Tier{tierrouter.TierDevice},
			},
			Epoch: func() int64 { return 1 },
			Deployments: []tierrouter.ChatDeployment{
				{Tier: tierrouter.TierDevice, Model: device},
				{Tier: tierrouter.TierEdge, Model: edge},
				{Tier: tierrouter.TierCloud, Model: cloud},
			},
			ContextFallback: 1024,
		})
		if err != nil {
			return groupReport{}, err
		}
		chatModel = router
	}

	replyAgent := agent.NewUnifiedAgent("device-agent", "Answer synthetic tasks briefly.", chatModel,
		agent.WithModelStreaming(false), agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}))
	report := groupReport{Group: name, Planned: len(tasks), TierRequests: map[tierrouter.Tier]int{}}
	for _, task := range tasks {
		ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{ID: name + "/" + task.ID, TaskEpoch: 1, Kind: task.Kind})
		started := time.Now()
		_, err := replyAgent.Reply(ctx, task.Input)
		result := taskResult{ID: task.ID, Latency: time.Since(started), Status: "completed", Attempts: 1}
		report.Completed++
		if err != nil {
			result.Status = "failed"
			report.Completed--
			report.Failed++
			if !routed {
				report.UnknownUsageAttempts++
			}
		}
		if routed {
			router, ok := chatModel.(*tierrouter.ChatRouter)
			if !ok {
				return groupReport{}, fmt.Errorf("routed group has wrong model type")
			}
			record, found := router.ChatSnapshot(name + "/" + task.ID)
			if !found {
				return groupReport{}, fmt.Errorf("missing route record for %s", task.ID)
			}
			result.Attempts = len(record.Attempts)
			seenTiers := make(map[tierrouter.Tier]bool, len(record.Attempts))
			for _, attempt := range record.Attempts {
				if !seenTiers[attempt.Tier] {
					result.Tiers = append(result.Tiers, attempt.Tier)
					seenTiers[attempt.Tier] = true
				}
				if attempt.Usage == nil {
					report.UnknownUsageAttempts++
				}
			}
		}
		report.Results = append(report.Results, result)
		report.TotalLatency += result.Latency
	}
	for tier, chatModel := range models {
		requests := len(chatModel.Requests())
		report.Requests += requests
		report.TierRequests[tier] = requests
	}
	return report, nil
}
