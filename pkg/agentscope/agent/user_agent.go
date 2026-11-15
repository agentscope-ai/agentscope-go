package agent

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// InputProvider abstracts user input sources (terminal, web, GUI, etc.).
type InputProvider interface {
	Input(ctx context.Context, agentID, agentName string) (text string, structured map[string]any, err error)
}

// TerminalInputProvider reads a single line from stdin as the simplest implementation.
type TerminalInputProvider struct {
	Prompt string
}

func (p TerminalInputProvider) Input(_ context.Context, _, _ string) (string, map[string]any, error) {
	if p.Prompt == "" {
		p.Prompt = "User Input: "
	}
	var line string
	// Synchronously read one line of user input.
	fmt.Print(p.Prompt)
	_, err := scanln(&line)
	if err != nil {
		return "", nil, err
	}
	return line, nil, nil
}

// A minimal scanln helper is used to keep dependencies simple.
func scanln(dst *string) (int, error) {
	var s string
	_, err := fmt.Scanln(&s)
	if err != nil {
		return 0, err
	}
	*dst = s
	return 1, nil
}

// UserAgent aligns with the core behavior of Python's UserAgent:
// getting user input and converting it into Msg instances.
type UserAgent struct {
	AgentBase

	Name  string
	input InputProvider
}

var defaultInputProvider InputProvider = TerminalInputProvider{}

// NewUserAgent constructs a named UserAgent.
func NewUserAgent(name string, provider InputProvider) *UserAgent {
	if provider == nil {
		provider = defaultInputProvider
	}
	return &UserAgent{
		AgentBase: NewAgentBase(),
		Name:      name,
		input:     provider,
	}
}

// OverrideClassInputProvider sets the default global InputProvider
// (similar to Python's override_class_input_method).
func OverrideClassInputProvider(p InputProvider) {
	if p == nil {
		return
	}
	defaultInputProvider = p
}

// OverrideInstanceInputProvider sets the input provider for the current UserAgent instance
// (similar to override_instance_input_method).
func (u *UserAgent) OverrideInstanceInputProvider(p InputProvider) {
	if p == nil {
		return
	}
	u.input = p
}

// Reply pulls input once from the InputProvider and produces a Msg with role=user.
func (u *UserAgent) Reply(ctx context.Context, _ ...any) (*message.Msg, error) {
	text, structured, err := u.input.Input(ctx, u.ID(), u.Name)
	if err != nil {
		return nil, err
	}
	if text == "" {
		text = ""
	}
	msg := message.UserMsg(u.Name, text)
	if structured != nil {
		if msg.Metadata == nil {
			msg.Metadata = types.JSONObject{}
		}
		for k, v := range structured {
			msg.Metadata[k] = v
		}
	}
	// Print to terminal (controlled by AgentBase).
	_ = u.Print(ctx, msg)
	return msg, nil
}

// Observe is usually a no-op for UserAgent.
func (u *UserAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
	_ = ctx
	_ = msgs
	return nil
}
