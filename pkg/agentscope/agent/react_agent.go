package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/memory"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

const (
	// defaultMaxIters is the default cap on the ReAct reasoning-acting loop iterations.
	defaultMaxIters = 4
	// defaultTopK is the default number of documents retrieved per knowledge base query.
	defaultTopK = 3
)

// Deprecated: ReActAgent is the v1 agent using JSON-based tool calling.
// Use [UnifiedAgent] with [WithToolkit] for new code.
//
// ReActAgent is a simplified Go implementation of a ReAct-style agent,
// conceptually aligned with the Python ReActAgent: system prompt, model, tools,
// memory, and a reasoning-acting loop with a max iteration count.
type ReActAgent struct {
	AgentBase

	Name      string
	SysPrompt string

	Model   model.ChatModel
	Toolkit *tool.Toolkit
	Memory  memory.Store

	Knowledge   []rag.KnowledgeBase
	Compression *memory.CompressionConfig

	MaxIters int
}

// Deprecated: NewReActAgent constructs a legacy ReActAgent.
// Use [NewUnifiedAgent] with [WithToolkit] for new code.
func NewReActAgent(
	name string,
	sysPrompt string,
	m model.ChatModel,
	tk *tool.Toolkit,
	mem memory.Store,
) *ReActAgent {
	if tk == nil {
		tk = tool.NewToolkit()
	}
	if mem == nil {
		mem = memory.NewInMemoryStore()
	}
	return &ReActAgent{
		AgentBase: NewAgentBase(),
		Name:      name,
		SysPrompt: sysPrompt,
		Model:     m,
		Toolkit:   tk,
		Memory:    mem,
		MaxIters:  defaultMaxIters,
	}
}

// WithKnowledge attaches one or more knowledge bases (for RAG) to the agent.
func (a *ReActAgent) WithKnowledge(bases ...rag.KnowledgeBase) *ReActAgent {
	a.Knowledge = bases
	return a
}

// WithCompression configures a basic conversation compression strategy.
func (a *ReActAgent) WithCompression(cfg *memory.CompressionConfig) *ReActAgent {
	a.Compression = cfg
	return a
}

// toolCall describes the simple tool invocation protocol used by ReActAgent.
type toolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Reply implements a simplified ReAct loop:
//  1. Load history from memory and append the current user question.
//  2. Call the model to get a reply.
//  3. If the reply is a tool call JSON {"tool": "...", "args": {...}}, execute the tool,
//     append the result to the context and call the model again.
//  4. Stop when no tool call is returned or MaxIters is reached.
func (a *ReActAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
	var userText string
	if len(args) > 0 {
		if s, ok := args[0].(string); ok {
			userText = s
		}
	}
	if userText == "" {
		return nil, fmt.Errorf("react agent: empty user input")
	}

	memKey := a.ID()
	history, _ := a.Memory.Load(ctx, memKey)

	userMsg := message.UserMsg(a.Name, userText)
	history = append(history, userMsg)

	// ReAct iteration loop.
	iters := 0
	for {
		if a.MaxIters > 0 && iters >= a.MaxIters {
			break
		}
		iters++

		// Optionally compress history.
		if a.Compression != nil {
			history = memory.CompressMessages(history, *a.Compression)
		}

		// Build system prompt and attach RAG context if available.
		sysPrompt := a.SysPrompt
		if len(a.Knowledge) > 0 {
			if ctxText := a.retrieveKnowledge(ctx, userText); ctxText != "" {
				sysPrompt = sysPrompt + "\n\n[KNOWLEDGE]\n" + ctxText
			}
		}

		systemMsg := message.SystemMsg(a.Name, sysPrompt)
		chatHistory := append([]*message.Msg{systemMsg}, history...)

		resp, err := a.Model.Chat(ctx, chatHistory)
		if err != nil {
			return nil, err
		}
		assistantMsg := message.AssistantMsg(a.Name, resp.Content)
		assistantMsg.ID = resp.ID
		if resp.Usage != nil {
			assistantMsg.Usage = &message.Usage{
				InputTokens:              resp.Usage.InputTokens,
				OutputTokens:             resp.Usage.OutputTokens,
				CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
				CacheInputTokens:         resp.Usage.CacheInputTokens,
			}
		}
		if assistantMsg == nil {
			return nil, fmt.Errorf("react agent: nil response message")
		}

		// Try to parse the reply as a tool call.
		if call, ok := tryParseToolCall(assistantMsg); ok && call.Tool != "" {
			// Execute tool.
			t := a.Toolkit.Get(call.Tool)
			if t == nil {
				// Tool not found; treat model reply as final answer.
				break
			}
			resp, err := t.Execute(ctx, call.Args)
			var resultText string
			if err != nil {
				resultText = fmt.Sprintf("tool %s error: %v", t.Name(), err)
			} else if resp != nil {
				for _, b := range resp.Content {
					if tb, ok := b.(message.TextBlock); ok {
						resultText += tb.Text
					}
				}
			}
			toolMsg := message.AssistantMsg(
				a.Name,
				fmt.Sprintf("TOOL[%s] RESULT: %s", t.Name(), resultText),
			)
			history = append(history, assistantMsg, toolMsg)
			continue
		}

		// Non-tool reply is treated as the final answer.
		if err := a.Memory.Save(ctx, memKey, userMsg); err != nil {
			logrus.WithError(err).Warn("react agent: failed to save user message to memory")
		}
		if err := a.Memory.Save(ctx, memKey, assistantMsg); err != nil {
			logrus.WithError(err).Warn("react agent: failed to save assistant message to memory")
		}

		// Print final reply.
		if err := a.Print(ctx, assistantMsg); err != nil {
			logrus.WithError(err).Warn("react agent: failed to print assistant message")
		}
		return assistantMsg, nil
	}

	// If the loop ends without a clear final answer, return the last message.
	if len(history) == 0 {
		return nil, fmt.Errorf("react agent: no messages in history")
	}
	last := history[len(history)-1]
	if err := a.Print(ctx, last); err != nil {
		logrus.WithError(err).Warn("react agent: failed to print last message")
	}
	return last, nil
}

// Observe is a no-op for now and can be extended as needed.
func (a *ReActAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
	_ = ctx
	_ = msgs
	return nil
}

// retrieveKnowledge queries configured knowledge bases and concatenates results into a text hint.
func (a *ReActAgent) retrieveKnowledge(ctx context.Context, query string) string {
	if len(a.Knowledge) == 0 || query == "" {
		return ""
	}
	type kbSnippet struct {
		name string
		text string
	}
	var snippets []kbSnippet
	for _, kb := range a.Knowledge {
		if kb == nil {
			continue
		}
		docs, err := kb.Query(ctx, query, defaultTopK)
		if err != nil || len(docs) == 0 {
			continue
		}
		var sb strings.Builder
		for i, d := range docs {
			if i >= defaultTopK {
				break
			}
			if sb.Len() > 0 {
				sb.WriteString("\n---\n")
			}
			sb.WriteString(d.Content)
		}
		if sb.Len() > 0 {
			name := kb.Name()
			if name == "" {
				name = "kb"
			}
			snippets = append(snippets, kbSnippet{name: name, text: sb.String()})
		}
	}
	if len(snippets) == 0 {
		return ""
	}
	var out strings.Builder
	for _, s := range snippets {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		fmt.Fprintf(&out, "[%s]\n%s", s.name, s.text)
	}
	return out.String()
}

// tryParseToolCall attempts to parse the assistant Msg content as a tool call JSON.
// Convention: if the assistant returns a JSON string like {"tool": "...", "args": {...}},
// it is treated as a tool invocation.
func tryParseToolCall(msg *message.Msg) (*toolCall, bool) {
	if msg == nil {
		return nil, false
	}
	textPtr := msg.GetTextContent("\n")
	if textPtr == nil {
		return nil, false
	}
	text := *textPtr
	var call toolCall
	if err := json.Unmarshal([]byte(text), &call); err != nil {
		return nil, false
	}
	return &call, true
}
