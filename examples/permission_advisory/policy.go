package main

import (
	"fmt"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// All policy/environment updates in this controlled example use this owner.
// There is no lock held while a classifier or human confirmation is awaited.
// A real host must account for every policy writer, not expose these maps.
type hostPolicy struct {
	mu                 sync.RWMutex
	engine             *permission.Engine
	checker            permission.Checker
	generation         uint64
	workspace, backend string
}
type policyStamp struct {
	Generation         uint64
	Workspace, Backend string
	Context            *permission.Context
}

func newPolicy(checker permission.Checker) *hostPolicy {
	return &hostPolicy{checker: checker, generation: 1, workspace: "synthetic-workspace-v1", backend: "synthetic-tool-v1"}
}
func (p *hostPolicy) capture(input map[string]any) (*policyStamp, permission.Decision, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.engine == nil {
		return nil, permission.Decision{}, fmt.Errorf("policy is not bound")
	}
	d, err := p.engine.CheckPermission(p.checker, input)
	return &policyStamp{p.generation, p.workspace, p.backend, p.engine.SnapshotContext()}, d, err
}
func (p *hostPolicy) addRule(rule permission.Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.engine.AddRule(rule)
	p.generation++
}
func (p *hostPolicy) changeEnvironment(workspace, backend string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspace, p.backend = workspace, backend
	p.generation++
}
