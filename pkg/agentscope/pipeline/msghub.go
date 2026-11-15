package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// MsgHub manages message routing between multiple agents, conceptually similar to Python's MsgHub.
type MsgHub struct {
	mu     sync.RWMutex
	agents map[string]agent.Agent
}

func NewMsgHub() *MsgHub {
	return &MsgHub{
		agents: make(map[string]agent.Agent),
	}
}

func (h *MsgHub) Register(name string, a agent.Agent) {
	if a == nil || name == "" {
		return
	}
	h.mu.Lock()
	h.agents[name] = a
	h.mu.Unlock()
}

func (h *MsgHub) Get(name string) agent.Agent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[name]
}

// Broadcast sends the message to all registered agents via their Observe method.
func (h *MsgHub) Broadcast(ctx context.Context, msg *message.Msg) error {
	// Snapshot under the read lock so Observe (which may be slow or re-enter the
	// hub) runs without holding it.
	h.mu.RLock()
	agents := make([]agent.Agent, 0, len(h.agents))
	for _, a := range h.agents {
		agents = append(agents, a)
	}
	h.mu.RUnlock()

	for _, a := range agents {
		if err := a.Observe(ctx, []*message.Msg{msg}); err != nil {
			return err
		}
	}
	return nil
}

// RequestReply sends a message from one logical agent to another agent's Reply method and returns the reply.
func (h *MsgHub) RequestReply(
	ctx context.Context,
	from string,
	to string,
	msg *message.Msg,
) (*message.Msg, error) {
	h.mu.RLock()
	src := h.agents[from]
	dst := h.agents[to]
	h.mu.RUnlock()
	if dst == nil {
		return nil, fmt.Errorf("msghub: agent %q not found", to)
	}
	_ = src // Source is not used for now; kept for future extension.
	return dst.Reply(ctx, msg)
}
