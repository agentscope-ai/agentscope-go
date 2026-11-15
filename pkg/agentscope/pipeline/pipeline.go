package pipeline

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/memory"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/session"
)

// Context holds the runtime context for a pipeline execution.
type Context struct {
	Ctx context.Context

	Model   model.ChatModel
	Session session.Session
	Memory  memory.Store

	Agents map[string]agent.Agent

	Messages []*message.Msg
}

// Step represents a single step in the pipeline.
type Step func(*Context) error

// Pipeline represents a simple sequential pipeline.
type Pipeline struct {
	steps []Step
}

// New creates a new Pipeline.
func New(steps ...Step) *Pipeline {
	return &Pipeline{steps: steps}
}

// Then appends a new step to the pipeline in a fluent style.
func (p *Pipeline) Then(step Step) *Pipeline {
	p.steps = append(p.steps, step)
	return p
}

// Run executes all steps in order.
func (p *Pipeline) Run(ctx *Context) error {
	for _, step := range p.steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// If conditionally executes a step based on the given predicate.
func (p *Pipeline) If(cond func(*Context) bool, step Step) *Pipeline {
	wrapped := func(c *Context) error {
		if cond(c) {
			return step(c)
		}
		return nil
	}
	p.steps = append(p.steps, wrapped)
	return p
}
