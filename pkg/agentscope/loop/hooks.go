// pkg/agentscope/loop/hooks.go
package loop

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"

// Hook receives notifications at key points in the loop lifecycle.
type Hook interface {
	BeforeModelCall(state protocol.LoopState, iter int)
	AfterModelCall(state protocol.LoopState, iter int, err error)
	BeforeToolExec(state protocol.LoopState, iter int, toolName string)
	AfterToolExec(state protocol.LoopState, iter int, toolName string, err error)
	OnStateTransition(from, to protocol.LoopState, iter int)
	OnLoopStart()
	OnLoopEnd(err error)
}

// HookRunner dispatches notifications to a chain of hooks.
type HookRunner struct {
	hooks []Hook
}

// NewHookRunner creates a HookRunner with the given hooks.
func NewHookRunner(hooks ...Hook) *HookRunner {
	return &HookRunner{hooks: hooks}
}

// BeforeModelCall dispatches the BeforeModelCall notification to all hooks.
func (r *HookRunner) BeforeModelCall(state protocol.LoopState, iter int) {
	for _, h := range r.hooks {
		h.BeforeModelCall(state, iter)
	}
}

// AfterModelCall dispatches the AfterModelCall notification to all hooks.
func (r *HookRunner) AfterModelCall(state protocol.LoopState, iter int, err error) {
	for _, h := range r.hooks {
		h.AfterModelCall(state, iter, err)
	}
}

// BeforeToolExec dispatches the BeforeToolExec notification to all hooks.
func (r *HookRunner) BeforeToolExec(state protocol.LoopState, iter int, toolName string) {
	for _, h := range r.hooks {
		h.BeforeToolExec(state, iter, toolName)
	}
}

// AfterToolExec dispatches the AfterToolExec notification to all hooks.
func (r *HookRunner) AfterToolExec(state protocol.LoopState, iter int, toolName string, err error) {
	for _, h := range r.hooks {
		h.AfterToolExec(state, iter, toolName, err)
	}
}

// OnStateTransition dispatches the OnStateTransition notification to all hooks.
func (r *HookRunner) OnStateTransition(from, to protocol.LoopState, iter int) {
	for _, h := range r.hooks {
		h.OnStateTransition(from, to, iter)
	}
}

// OnLoopStart dispatches the OnLoopStart notification to all hooks.
func (r *HookRunner) OnLoopStart() {
	for _, h := range r.hooks {
		h.OnLoopStart()
	}
}

// OnLoopEnd dispatches the OnLoopEnd notification to all hooks.
func (r *HookRunner) OnLoopEnd(err error) {
	for _, h := range r.hooks {
		h.OnLoopEnd(err)
	}
}
