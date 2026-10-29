package openaiserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools       []model.ToolSchema `json:"tools"`
	ToolChoice  json.RawMessage    `json:"tool_choice"`
	Temperature *float64           `json:"temperature"`
	TopP        *float64           `json:"top_p"`
	MaxTokens   *int               `json:"max_tokens"`
	Stop        json.RawMessage    `json:"stop"`
	N           *int               `json:"n"`
}

type wireMessage struct {
	Role       string          `json:"role"`
	Name       string          `json:"name"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []wireCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}
type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func badRequest(msg string) *apiError {
	return &apiError{status: 400, Message: msg, Type: "invalid_request_error", Code: "invalid_request"}
}

func (r *chatRequest) validate() error {
	if r.Model == "" || len(r.Messages) == 0 || r.Messages[len(r.Messages)-1].Role != "user" {
		return badRequest("model and messages ending in a user message are required")
	}
	if r.N != nil && *r.N != 1 {
		return badRequest("only n=1 is supported")
	}
	if r.StreamOptions != nil && !r.Stream {
		return badRequest("stream_options requires stream=true")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return badRequest("temperature must be between zero and two")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return badRequest("top_p must be between zero and one")
	}
	if r.MaxTokens != nil && *r.MaxTokens <= 0 {
		return badRequest("max_tokens must be positive")
	}
	if present(r.ToolChoice) {
		var mode string
		if json.Unmarshal(r.ToolChoice, &mode) != nil || (mode != "none" && mode != "auto") {
			return badRequest("tool_choice supports only none and auto")
		}
	}
	if present(r.Stop) {
		var one string
		if json.Unmarshal(r.Stop, &one) != nil {
			var many []string
			if json.Unmarshal(r.Stop, &many) != nil || len(many) == 0 || len(many) > 4 {
				return badRequest("stop must be a string or one to four strings")
			}
		}
	}
	seen := make(map[string]bool)
	for _, t := range r.Tools {
		if t.Type != "function" || t.Function.Name == "" || seen[t.Function.Name] {
			return badRequest("tools must contain unique named function definitions")
		}
		var schema map[string]any
		if present(t.Function.Parameters) && (json.Unmarshal(t.Function.Parameters, &schema) != nil || schema == nil) {
			return badRequest("tool parameters must be a JSON object")
		}
		seen[t.Function.Name] = true
	}
	return nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func textContent(raw json.RawMessage, allowNull bool) (string, error) {
	if !present(raw) {
		if allowNull {
			return "", nil
		}
		return "", badRequest("message content is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return "", badRequest("content must be a string or a nonempty array of text parts")
	}
	var out strings.Builder
	for _, p := range parts {
		if p.Type != "text" || p.Text == nil {
			return "", badRequest("only text content parts are supported; images, audio and files are not supported")
		}
		out.WriteString(*p.Text)
	}
	return out.String(), nil
}

// hydrate accepts complete historical exchanges only. It never restores a
// pending/allowed tool or any client-supplied permission/checkpoint state.
func (r *chatRequest) hydrate() (string, *agent.AgentState, string, error) {
	state := &agent.AgentState{SchemaVersion: agent.StateSchemaVersion, SessionID: agentscope.GenerateID()}
	var system []string
	pending := make(map[string]string)
	seen := make(map[string]bool)
	started := false
	for i, m := range r.Messages {
		text, err := textContent(m.Content, m.Role == "assistant" && len(m.ToolCalls) > 0)
		if err != nil {
			return "", nil, "", err
		}
		if m.Role != "assistant" && len(m.ToolCalls) > 0 {
			return "", nil, "", badRequest("tool_calls is only valid on assistant messages")
		}
		if m.Role != "tool" && m.ToolCallID != "" {
			return "", nil, "", badRequest("tool_call_id is only valid on tool messages")
		}
		if len(pending) > 0 && m.Role != "tool" {
			return "", nil, "", badRequest("each assistant tool call requires a matching tool result before the next message")
		}
		name := m.Name
		if name == "" {
			name = m.Role
		}
		switch m.Role {
		case "system", "developer":
			if started {
				return "", nil, "", badRequest("system and developer messages must precede conversation messages")
			}
			system = append(system, text)
		case "user":
			started = true
			if i == len(r.Messages)-1 {
				if text == "" {
					return "", nil, "", badRequest("final user input must not be empty")
				}
				return strings.Join(system, "\n\n"), state, text, nil
			}
			state.Context = append(state.Context, message.UserMsg(name, text))
		case "assistant":
			started = true
			var blocks []message.ContentBlock
			if text != "" {
				blocks = append(blocks, message.TextBlock{Type: "text", Text: text})
			}
			for _, call := range m.ToolCalls {
				var input map[string]any
				if call.ID == "" || seen[call.ID] || call.Type != "function" || call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
					return "", nil, "", badRequest("historical tool calls require unique ids, function names and JSON object arguments")
				}
				seen[call.ID] = true
				pending[call.ID] = call.Function.Name
				blocks = append(blocks, message.ToolCallBlock{Type: "tool_call", ID: call.ID, Name: call.Function.Name, Input: call.Function.Arguments, State: message.ToolCallFinished})
			}
			state.Context = append(state.Context, message.AssistantMsg(name, blocks))
		case "tool":
			toolName, ok := pending[m.ToolCallID]
			if !ok {
				return "", nil, "", badRequest("tool result has no matching pending historical call")
			}
			delete(pending, m.ToolCallID)
			state.Context = append(state.Context, message.AssistantMsg(name, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: m.ToolCallID, Name: toolName, Output: text, State: message.ToolResultSuccess}}))
		default:
			return "", nil, "", badRequest(fmt.Sprintf("unsupported message role %q", m.Role))
		}
	}
	return "", nil, "", badRequest("a final user message is required")
}
