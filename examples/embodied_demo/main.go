package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/embodied"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tierrouter"
)

type scenarioConfig struct {
	Dir                   string
	UseCloud              bool
	CloudAvailable        bool
	LocalAlternative      string
	ApprovedAlternatives  []string
	LoseACK               bool
	ReviseSiteDuringCloud bool
	StaleObservation      bool
	InvalidAgentReply     bool
	Reopen                bool
}

type scenarioReport struct {
	Mode             string        `json:"mode"`
	Planned          int           `json:"planned"`
	Completed        int           `json:"completed"`
	Interventions    int           `json:"interventions"`
	RejectedStale    int           `json:"rejected_stale"`
	DuplicateEffects int           `json:"duplicate_effects"`
	PlanningRequests int           `json:"planning_requests"`
	CloudRequests    int           `json:"cloud_requests"`
	ActionEffects    int           `json:"action_effects"`
	Latency          time.Duration `json:"latency"`
	Error            string        `json:"error,omitempty"`
}

type scenario struct {
	cfg        scenarioConfig
	now        time.Time
	gateway    *embodied.Gateway
	controller *embodied.SimController
	agent      *agent.UnifiedAgent
	models     map[tierrouter.Tier]*tierrouter.FakeChatModel
	cloud      *tierrouter.FakeChatModel
	world      embodied.WorldState
	feedback   []taskenvelope.Record
}

var (
	errStaleProposal        = errors.New("stale alternative")
	errForbiddenDestination = errors.New("forbidden destination")
	errNoApprovedRoute      = errors.New("no approved route while cloud is unavailable")
)

func main() {
	reports, err := runDemo()
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

func runDemo() ([]scenarioReport, error) {
	reports := make([]scenarioReport, 0, 2)
	for _, cfg := range []scenarioConfig{
		{UseCloud: false, CloudAvailable: false, LocalAlternative: "B", ApprovedAlternatives: []string{"B"}},
		{UseCloud: true, CloudAvailable: true, ApprovedAlternatives: []string{"B"}},
	} {
		dir, err := os.MkdirTemp("", "embodied-demo-")
		if err != nil {
			return nil, err
		}
		cfg.Dir = dir
		report, runErr := runScenario(cfg)
		removeErr := os.RemoveAll(dir)
		if runErr != nil {
			return nil, runErr
		}
		if removeErr != nil {
			return nil, removeErr
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func runScenario(cfg scenarioConfig) (scenarioReport, error) {
	s, err := newScenario(cfg)
	if err != nil {
		return scenarioReport{Mode: modeForConfig(cfg), Error: err.Error()}, err
	}
	return s.run(context.Background())
}

func newScenario(cfg scenarioConfig) (*scenario, error) {
	if cfg.Dir == "" {
		return nil, errors.New("scenario directory required")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	controller, err := embodied.OpenSimController(cfg.Dir+"/controller.json", nil)
	if err != nil {
		return nil, err
	}
	controller.LoseACK = cfg.LoseACK
	skills := []embodied.Skill{
		{Name: "pick", Version: "v1", Destinations: []string{"A"}, Preconditions: []string{"clear"}},
		{Name: "deliver", Version: "v1", Destinations: []string{"A", "B"}, Preconditions: []string{"clear"}},
	}
	gateway, err := embodied.NewGateway(&embodied.GatewayConfig{
		LedgerPath: cfg.Dir + "/ledger.json", MetadataPath: cfg.Dir + "/metadata.json",
		Controller: controller, Skills: skills, Now: func() time.Time { return now },
	})
	if err != nil {
		return nil, err
	}
	if !cfg.Reopen {
		if err := gateway.RegisterRoot("root-1", 10); err != nil {
			return nil, err
		}
		if _, err := gateway.AdvanceEpoch("root-1"); err != nil {
			return nil, err
		}
	}
	models := map[tierrouter.Tier]*tierrouter.FakeChatModel{
		tierrouter.TierDevice: tierrouter.NewFakeChatModel(tierrouter.TierDevice),
		tierrouter.TierEdge:   tierrouter.NewFakeChatModel(tierrouter.TierEdge),
		tierrouter.TierCloud:  tierrouter.NewFakeChatModel(tierrouter.TierCloud),
	}
	for _, fake := range models {
		fake.Reply = "replan"
		if cfg.InvalidAgentReply {
			fake.Reply = "unrelated"
		}
	}
	preferred := []tierrouter.Tier{tierrouter.TierEdge, tierrouter.TierDevice}
	if cfg.UseCloud && cfg.CloudAvailable {
		preferred = []tierrouter.Tier{tierrouter.TierCloud, tierrouter.TierEdge, tierrouter.TierDevice}
	}
	router, err := tierrouter.NewChatRouter(tierrouter.ChatRouterConfig{
		Policy: tierrouter.StaticPolicy{Default: preferred}, Epoch: func() int64 { return 1 },
		Deployments: []tierrouter.ChatDeployment{
			{Tier: tierrouter.TierDevice, Model: models[tierrouter.TierDevice]},
			{Tier: tierrouter.TierEdge, Model: models[tierrouter.TierEdge]},
			{Tier: tierrouter.TierCloud, Model: models[tierrouter.TierCloud]},
		}, ContextFallback: 4096,
	})
	if err != nil {
		return nil, err
	}
	cloud := tierrouter.NewFakeChatModel(tierrouter.TierCloud)
	cloud.Reply = "B"
	world := embodied.WorldState{
		RootTaskID: "root-1", TaskEpoch: 1, Frame: "map", Units: "m", CalibrationVersion: "cal-1", MapVersion: "map-1", SiteStateVersion: 1,
		Conditions: map[string]bool{"clear": true, "packing-unavailable": true},
	}
	_ = gateway.SetWorldState(&world)
	if cfg.Reopen {
		if err := gateway.Recover(context.Background()); err != nil {
			return nil, err
		}
	}
	s := &scenario{cfg: cfg, now: now, gateway: gateway, controller: controller, models: models, cloud: cloud, world: world}
	s.agent = agent.NewUnifiedAgent("device-agent", "Plan only within approved skills.", router,
		agent.WithModelStreaming(false), agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}))
	return s, nil
}

func (s *scenario) run(ctx context.Context) (report scenarioReport, err error) {
	started := time.Now()
	report = scenarioReport{Mode: modeForConfig(s.cfg), Planned: 1, Interventions: 1}
	defer s.completeMetrics(&report, started)
	obs := embodied.Observation{
		RootTaskID: "root-1", TaskEpoch: 1, ID: "obs-1", Sequence: 1,
		CapturedAt: s.now.Add(-time.Second), ClockDomain: "host", MaxAge: 5 * time.Second,
		Frame: "map", Units: "m", CalibrationVersion: "cal-1", MapVersion: "map-1", SiteStateVersion: s.world.SiteStateVersion,
		Summary: "blue tray at A; usual packing station unavailable", MediaRef: "opaque://frames/1", Content: []byte(`{"tray":"blue","station":"unavailable"}`),
		SummaryTiers: []tierrouter.Tier{tierrouter.TierDevice, tierrouter.TierEdge},
	}
	if s.cfg.StaleObservation {
		obs.CapturedAt = s.now.Add(-time.Hour)
	}
	if err = s.gateway.RegisterObservation(&obs); err != nil {
		report.Error = err.Error()
		return report, err
	}
	projected, err := obs.Project(tierrouter.TierEdge)
	if err != nil {
		report.Error = err.Error()
		return report, err
	}
	ctx = tierrouter.WithChatOperation(ctx, &tierrouter.Operation{ID: "device-plan", TaskEpoch: 1, Kind: "plan", TTL: time.Minute})
	reply, err := s.agent.Reply(ctx, projected.Summary)
	if err != nil {
		report.Error = err.Error()
		return report, err
	}
	if decision := reply.GetTextContent("\n"); decision == nil || *decision != "replan" {
		err = errors.New("device planner returned an unsupported decision")
		report.Error = err.Error()
		return report, err
	}
	plan, err := s.plan(&obs)
	if err != nil {
		report.Error = err.Error()
		if errors.Is(err, errStaleProposal) {
			report.RejectedStale = 1
		}
		return report, err
	}
	for i := range plan.Steps {
		action := &embodied.Action{ID: fmt.Sprintf("%s/%d", plan.ID, i), Plan: *plan, StepIndex: i, Observation: obs}
		rec, actionErr := s.gateway.Execute(ctx, action)
		if rec != nil {
			s.feedback = append(s.feedback, *rec.Clone())
		}
		if actionErr != nil || rec == nil || rec.State != taskenvelope.StateCompleted {
			if errors.Is(actionErr, embodied.ErrStaleObservation) {
				report.RejectedStale++
			}
			if rec == nil {
				err = fmt.Errorf("action %d rejected without a record: %w", i, actionErr)
			} else {
				err = fmt.Errorf("action %d did not complete: state=%s err=%w", i, rec.State, actionErr)
			}
			report.Error = err.Error()
			return report, err
		}
	}
	report.Completed = 1
	report.Interventions = 0
	return report, nil
}

func (s *scenario) completeMetrics(report *scenarioReport, started time.Time) {
	report.Latency = time.Since(started)
	report.CloudRequests = len(s.cloud.Requests())
	report.ActionEffects = len(s.controller.Commands())
	counts := make(map[string]int)
	for _, command := range s.controller.Commands() {
		id := command.AttemptID
		var action embodied.Action
		if err := json.Unmarshal([]byte(command.Command), &action); err == nil && action.ID != "" {
			id = action.ID
		}
		counts[id]++
	}
	for _, count := range counts {
		if count > 1 {
			report.DuplicateEffects += count - 1
		}
	}
	for _, fake := range s.models {
		report.PlanningRequests += len(fake.Requests())
	}
}

func (s *scenario) plan(obs *embodied.Observation) (*embodied.Plan, error) {
	alternative := s.cfg.LocalAlternative
	if s.cfg.UseCloud && s.cfg.CloudAvailable {
		if s.cfg.ReviseSiteDuringCloud {
			s.cloud.BeforeReturn = func() { s.world.SiteStateVersion++ }
		}
		summary := fmt.Sprintf("site_version=%d; %s", s.world.SiteStateVersion, obs.Summary)
		proposal, err := s.cloud.Chat(context.Background(), []*message.Msg{message.UserMsg("edge", summary)})
		if err != nil {
			return nil, err
		}
		alternative = proposal.GetTextContent()
	}
	if alternative == "" {
		return nil, errNoApprovedRoute
	}
	if s.world.SiteStateVersion != obs.SiteStateVersion {
		return nil, errStaleProposal
	}
	if !s.approved(alternative) {
		return nil, errForbiddenDestination
	}
	return &embodied.Plan{
		RootTaskID: "root-1", TaskEpoch: 1, ID: "plan-1", ObservationID: obs.ID, ObservationSequence: obs.Sequence,
		SiteStateVersion: s.world.SiteStateVersion, ValidUntil: s.now.Add(time.Minute),
		Steps: []embodied.PlanStep{
			{ID: "pick", Skill: "pick", SkillVersion: "v1", Destination: "A", Arguments: []byte(`{"tray":"blue"}`), Preconditions: []string{"clear"}},
			{ID: "deliver", Skill: "deliver", SkillVersion: "v1", Destination: alternative, Arguments: []byte(`{"tray":"blue"}`), Preconditions: []string{"clear"}},
		},
	}, nil
}

func (s *scenario) approved(destination string) bool {
	for _, approved := range s.cfg.ApprovedAlternatives {
		if destination == approved {
			return true
		}
	}
	return false
}

func modeForConfig(cfg scenarioConfig) string {
	if cfg.UseCloud {
		return "edge+cloud"
	}
	return "edge-only"
}
