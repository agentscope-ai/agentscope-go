package agent

import (
	"context"
	"fmt"
	"sync"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Agent is the public interface implemented by all agents.
type Agent interface {
	ID() string

	Reply(ctx context.Context, args ...any) (*message.Msg, error)
	Observe(ctx context.Context, msgs []*message.Msg) error
	Interrupt(ctx context.Context, msg *message.Msg) error

	SetConsoleOutputEnabled(enabled bool)
}

// Hook type names, aligned with Python version.
const (
	HookPreReply    = "pre_reply"
	HookPostReply   = "post_reply"
	HookPrePrint    = "pre_print"
	HookPostPrint   = "post_print"
	HookPreObserve  = "pre_observe"
	HookPostObserve = "post_observe"
)

// Hook signatures.
type (
	PreReplyHook    func(ctx context.Context, a Agent, args []any) ([]any, error)
	PostReplyHook   func(ctx context.Context, a Agent, args []any, out *message.Msg) (*message.Msg, error)
	PrePrintHook    func(ctx context.Context, a Agent, msg *message.Msg) (*message.Msg, error)
	PostPrintHook   func(ctx context.Context, a Agent, msg *message.Msg) error
	PreObserveHook  func(ctx context.Context, a Agent, msgs []*message.Msg) ([]*message.Msg, error)
	PostObserveHook func(ctx context.Context, a Agent, msgs []*message.Msg) error
)

// AgentBase provides common functionality for concrete agents.
type AgentBase struct {
	id string

	disableConsoleOutput bool

	// subscribers maps msghub name to subscribed agents.
	subscribers map[string][]Agent

	// instance-level hooks
	preReplyHooks    map[string]PreReplyHook
	postReplyHooks   map[string]PostReplyHook
	prePrintHooks    map[string]PrePrintHook
	postPrintHooks   map[string]PostPrintHook
	preObserveHooks  map[string]PreObserveHook
	postObserveHooks map[string]PostObserveHook

	mu sync.RWMutex
}

// class-level hooks are shared across all instances of AgentBase-derived agents.
var (
	classHooksMu sync.RWMutex

	classPreReplyHooks    = map[string]PreReplyHook{}
	classPostReplyHooks   = map[string]PostReplyHook{}
	classPrePrintHooks    = map[string]PrePrintHook{}
	classPostPrintHooks   = map[string]PostPrintHook{}
	classPreObserveHooks  = map[string]PreObserveHook{}
	classPostObserveHooks = map[string]PostObserveHook{}
)

// NewAgentBase constructs an initialized AgentBase.
func NewAgentBase() AgentBase {
	return AgentBase{
		id:                   agentscope.GenerateID(),
		subscribers:          make(map[string][]Agent),
		preReplyHooks:        make(map[string]PreReplyHook),
		postReplyHooks:       make(map[string]PostReplyHook),
		prePrintHooks:        make(map[string]PrePrintHook),
		postPrintHooks:       make(map[string]PostPrintHook),
		preObserveHooks:      make(map[string]PreObserveHook),
		postObserveHooks:     make(map[string]PostObserveHook),
		disableConsoleOutput: false,
	}
}

// ID returns the agent identifier.
func (b *AgentBase) ID() string {
	return b.id
}

// SetConsoleOutputEnabled enables or disables printing to stdout.
func (b *AgentBase) SetConsoleOutputEnabled(enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disableConsoleOutput = !enabled
}

// Print prints a message to console after running hooks.
func (b *AgentBase) Print(ctx context.Context, msg *message.Msg) error {
	if msg == nil {
		return nil
	}

	// run pre-print hooks
	var err error
	msg, err = b.runPrePrintHooks(ctx, msg)
	if err != nil {
		return err
	}

	if !b.consoleOutputDisabled() {
		if t := msg.GetTextContent("\n"); t != nil {
			fmt.Printf("%s: %s\n", msg.Name, *t)
		} else if len(msg.Content) > 0 {
			fmt.Printf("%s: [%d content blocks]\n", msg.Name, len(msg.Content))
		}
	}

	// run post-print hooks
	return b.runPostPrintHooks(ctx, msg)
}

func (b *AgentBase) consoleOutputDisabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.disableConsoleOutput
}

// ResetSubscribers replaces subscribers for a given msghub name.
func (b *AgentBase) ResetSubscribers(msghubName string, subs []Agent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var filtered []Agent
	for _, a := range subs {
		if a == nil {
			continue
		}
		filtered = append(filtered, a)
	}
	b.subscribers[msghubName] = append([]Agent(nil), filtered...)
}

// RemoveSubscribers removes all subscribers under the given msghub name.
func (b *AgentBase) RemoveSubscribers(msghubName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, msghubName)
}

// Instance-level hook registration.

func (b *AgentBase) RegisterInstancePreReplyHook(name string, hook PreReplyHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.preReplyHooks[name] = hook
}

func (b *AgentBase) RegisterInstancePostReplyHook(name string, hook PostReplyHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.postReplyHooks[name] = hook
}

func (b *AgentBase) RegisterInstancePrePrintHook(name string, hook PrePrintHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prePrintHooks[name] = hook
}

func (b *AgentBase) RegisterInstancePostPrintHook(name string, hook PostPrintHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.postPrintHooks[name] = hook
}

func (b *AgentBase) RegisterInstancePreObserveHook(name string, hook PreObserveHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.preObserveHooks[name] = hook
}

func (b *AgentBase) RegisterInstancePostObserveHook(name string, hook PostObserveHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.postObserveHooks[name] = hook
}

// ClearInstanceHooks clears hooks on this instance. If hookType is empty,
// all instance-level hooks are cleared.
func (b *AgentBase) ClearInstanceHooks(hookType string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch hookType {
	case HookPreReply:
		b.preReplyHooks = map[string]PreReplyHook{}
	case HookPostReply:
		b.postReplyHooks = map[string]PostReplyHook{}
	case HookPrePrint:
		b.prePrintHooks = map[string]PrePrintHook{}
	case HookPostPrint:
		b.postPrintHooks = map[string]PostPrintHook{}
	case HookPreObserve:
		b.preObserveHooks = map[string]PreObserveHook{}
	case HookPostObserve:
		b.postObserveHooks = map[string]PostObserveHook{}
	case "":
		b.preReplyHooks = map[string]PreReplyHook{}
		b.postReplyHooks = map[string]PostReplyHook{}
		b.prePrintHooks = map[string]PrePrintHook{}
		b.postPrintHooks = map[string]PostPrintHook{}
		b.preObserveHooks = map[string]PreObserveHook{}
		b.postObserveHooks = map[string]PostObserveHook{}
	}
}

// Class-level hook registration.

func RegisterClassPreReplyHook(name string, hook PreReplyHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPreReplyHooks[name] = hook
}

func RegisterClassPostReplyHook(name string, hook PostReplyHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPostReplyHooks[name] = hook
}

func RegisterClassPrePrintHook(name string, hook PrePrintHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPrePrintHooks[name] = hook
}

func RegisterClassPostPrintHook(name string, hook PostPrintHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPostPrintHooks[name] = hook
}

func RegisterClassPreObserveHook(name string, hook PreObserveHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPreObserveHooks[name] = hook
}

func RegisterClassPostObserveHook(name string, hook PostObserveHook) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()
	classPostObserveHooks[name] = hook
}

// ClearClassHooks clears class-level hooks. If hookType is empty, clears all.
func ClearClassHooks(hookType string) {
	classHooksMu.Lock()
	defer classHooksMu.Unlock()

	switch hookType {
	case HookPreReply:
		classPreReplyHooks = map[string]PreReplyHook{}
	case HookPostReply:
		classPostReplyHooks = map[string]PostReplyHook{}
	case HookPrePrint:
		classPrePrintHooks = map[string]PrePrintHook{}
	case HookPostPrint:
		classPostPrintHooks = map[string]PostPrintHook{}
	case HookPreObserve:
		classPreObserveHooks = map[string]PreObserveHook{}
	case HookPostObserve:
		classPostObserveHooks = map[string]PostObserveHook{}
	case "":
		classPreReplyHooks = map[string]PreReplyHook{}
		classPostReplyHooks = map[string]PostReplyHook{}
		classPrePrintHooks = map[string]PrePrintHook{}
		classPostPrintHooks = map[string]PostPrintHook{}
		classPreObserveHooks = map[string]PreObserveHook{}
		classPostObserveHooks = map[string]PostObserveHook{}
	}
}

// Internal helpers to run hooks in defined order: class-level then instance-level.

func (b *AgentBase) runPrePrintHooks(ctx context.Context, msg *message.Msg) (*message.Msg, error) {
	classHooksMu.RLock()
	for _, h := range classPrePrintHooks {
		var err error
		msg, err = h(ctx, b, msg)
		if err != nil {
			classHooksMu.RUnlock()
			return nil, err
		}
	}
	classHooksMu.RUnlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, h := range b.prePrintHooks {
		var err error
		msg, err = h(ctx, b, msg)
		if err != nil {
			return nil, err
		}
	}
	return msg, nil
}

func (b *AgentBase) runPostPrintHooks(ctx context.Context, msg *message.Msg) error {
	classHooksMu.RLock()
	for _, h := range classPostPrintHooks {
		if err := h(ctx, b, msg); err != nil {
			classHooksMu.RUnlock()
			return err
		}
	}
	classHooksMu.RUnlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, h := range b.postPrintHooks {
		if err := h(ctx, b, msg); err != nil {
			return err
		}
	}
	return nil
}

// Ensure AgentBase implements basic methods required by Agent when embedded.
var _ fmt.Stringer = (*AgentBase)(nil)

func (b *AgentBase) String() string {
	return fmt.Sprintf("AgentBase{id=%s}", b.id)
}

// Default implementations for Agent methods so that concrete agents can
// optionally embed AgentBase and override only what they need.

// Reply must be implemented by concrete agents.
func (b *AgentBase) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	return nil, fmt.Errorf("Reply not implemented for %T", b)
}

// Observe must be implemented by concrete agents.
func (b *AgentBase) Observe(ctx context.Context, msgs []*message.Msg) error {
	return fmt.Errorf("Observe not implemented for %T", b)
}

// Interrupt provides a no-op default implementation.
func (b *AgentBase) Interrupt(ctx context.Context, msg *message.Msg) error {
	return nil
}
