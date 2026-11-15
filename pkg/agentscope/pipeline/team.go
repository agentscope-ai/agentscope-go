package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// TeamAssignToolName is the reserved leader delegation tool.
const TeamAssignToolName = "TeamAssign"

// ErrTeamBusy means a pipeline still owns a previous run.
var ErrTeamBusy = errors.New("pipeline: team run is active")

// ErrTeamClosed means a run no longer accepts host input.
var ErrTeamClosed = errors.New("pipeline: team run is closed or canceled")

// ErrTeamInput means a response does not match a pending participant request.
var ErrTeamInput = errors.New("pipeline: invalid team input")

// ErrTeamOutput means a normally completed member produced no supported text.
var ErrTeamOutput = errors.New("pipeline: member returned no final textual answer")

// TeamAgentFactory creates a fresh, idle, exclusively owned member per run.
type TeamAgentFactory func(context.Context) (*agent.UnifiedAgent, error)

// TeamLeaderFactory installs assign in a fresh leader's exclusively owned toolkit.
type TeamLeaderFactory func(context.Context, tool.Tool) (*agent.UnifiedAgent, error)

// TeamMember describes a named specialist and its per-run factory.
type TeamMember struct {
	Name, Description string
	Factory           TeamAgentFactory
}

// TeamPipeline runs serial named-member delegation with participant interactions.
type TeamPipeline struct {
	leader  TeamLeaderFactory
	members []TeamMember
	busy    atomic.Bool
}

// NewTeamPipeline constructs an experimental, run-local serial team.
// Factories must construct idle agents with their own PermissionContext and may
// not share agents across runs or with other callers. Models and tool backends
// remain caller-owned; this is context isolation, not a filesystem sandbox.
func NewTeamPipeline(leader TeamLeaderFactory, members ...TeamMember) (*TeamPipeline, error) {
	if leader == nil || len(members) == 0 {
		return nil, fmt.Errorf("pipeline: leader factory and at least one member required")
	}
	seen := map[string]bool{}
	for _, m := range members {
		if strings.TrimSpace(m.Name) == "" || m.Factory == nil || seen[m.Name] {
			return nil, fmt.Errorf("pipeline: members require unique names and non-nil factories")
		}
		seen[m.Name] = true
	}
	return &TeamPipeline{leader: leader, members: append([]TeamMember(nil), members...)}, nil
}

// Start begins a run. Callers must drain Events or cancel the run.
//
// The leader factory must install the supplied TeamAssign unchanged in its
// exclusively owned toolkit; NewTeamLeaderToolkit checks registration conflicts.
// The pipeline cannot inspect a private toolkit or detect external agent use.
func (p *TeamPipeline) Start(ctx context.Context, input string) (*TeamRun, error) {
	if p == nil || p.leader == nil || len(p.members) == 0 || ctx == nil || strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("pipeline: configured team, context and nonempty input required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.busy.CompareAndSwap(false, true) {
		return nil, ErrTeamBusy
	}
	release, accepted := runctx.Start(ctx)
	if !accepted {
		p.busy.Store(false)
		return nil, ErrTeamClosed
	}
	workCtx, cancel := context.WithCancel(ctx)
	started := false
	defer func() {
		if !started {
			cancel()
			release()
			p.busy.Store(false)
		}
	}()
	assign := newTeamAssign(p.members)
	leader, err := p.leader(workCtx, assign)
	if err != nil {
		return nil, fmt.Errorf("pipeline: leader factory: %w", err)
	}
	if leader == nil || leader.PermissionEngine() == nil {
		return nil, fmt.Errorf("pipeline: leader factory must provide an agent with a PermissionContext")
	}
	workers := make(map[string]*agent.UnifiedAgent, len(p.members))
	identities := map[*agent.UnifiedAgent]bool{leader: true}
	for _, member := range p.members {
		if leader.Name() == member.Name {
			return nil, fmt.Errorf("pipeline: leader/member name collision %q", member.Name)
		}
		worker, factoryErr := member.Factory(workCtx)
		if factoryErr != nil {
			return nil, fmt.Errorf("pipeline: member %s factory: %w", member.Name, factoryErr)
		}
		if worker == nil || worker.PermissionEngine() == nil || identities[worker] || worker.Name() != member.Name {
			return nil, fmt.Errorf("pipeline: member %q requires a distinct named agent with a PermissionContext", member.Name)
		}
		identities[worker] = true
		workers[member.Name] = worker
	}
	if err := workCtx.Err(); err != nil {
		return nil, err
	}
	run := newTeamRun(workCtx, cancel, workers)
	started = true
	go func() {
		out := run.participant(workCtx, leader, input, true)
		run.finish(out.err)
		release()
		p.busy.Store(false)
		close(run.done)
	}()
	return run, nil
}

// NewTeamLeaderToolkit installs the supplied TeamAssign without overwriting
// caller tools. A factory may construct its own toolkit with the same contract.
func NewTeamLeaderToolkit(assign tool.Tool, others ...tool.Tool) (*tool.Toolkit, error) {
	if teamNil(assign) || assign.Name() != TeamAssignToolName || !assign.IsExternalTool() {
		return nil, fmt.Errorf("pipeline: the supplied TeamAssign external tool is required")
	}
	seen := map[string]bool{TeamAssignToolName: true}
	for _, t := range others {
		if teamNil(t) || strings.TrimSpace(t.Name()) == "" || seen[t.Name()] {
			return nil, fmt.Errorf("pipeline: nil, duplicate or reserved leader tool name")
		}
		seen[t.Name()] = true
	}
	return tool.NewToolkit(append([]tool.Tool{assign}, others...)...), nil
}

type teamAssign struct{ tool.BaseTool }

func (*teamAssign) IsExternalTool() bool { return true }
func newTeamAssign(members []TeamMember) *teamAssign {
	names := make([]string, 0, len(members))
	var description strings.Builder
	description.WriteString("Assign a task to a named member and receive its final textual answer. Members cannot see your context; include all necessary information in prompt.\n")
	for _, m := range members {
		names = append(names, m.Name)
		fmt.Fprintf(&description, "- %s: %s\n", m.Name, m.Description)
	}
	// This schema consists solely of JSON-native strings/maps/slices.
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
		"member": map[string]any{"type": "string", "enum": names},
		"prompt": map[string]any{"type": "string", "minLength": 1},
	}, "required": []string{"member", "prompt"}, "additionalProperties": false})
	return &teamAssign{tool.BaseTool{ToolName: TeamAssignToolName, ToolDescription: description.String(), ToolSchema: schema}}
}
