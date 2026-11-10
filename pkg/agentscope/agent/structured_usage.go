package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/skill"
)

// StructuredResponse is an experimental report-generation result. Usage sums
// known usage at this helper's observable call boundary, including failures.
// UsageComplete does not cover retries hidden inside a model or transport.
type StructuredResponse struct {
	Output        json.RawMessage
	Usage         *model.ChatUsage
	UsageComplete bool
}

// GenerateStructuredResponseWithUsage generates a report without executing
// tools or changing conversation history. Call only between fully joined
// replies, with exclusive use of the agent; it does not acquire a session lease.
// Unlike the legacy helper, malformed/truncated candidates are not repaired.
// Domain validation (including schema validation) remains the caller's job.
// Errors after generation starts retain known usage but return no Output.
// Each invocation uses a fresh MiddleContext, shared by its compatibility
// requests. Typed reply-budget state attached to ctx remains shared.
func (a *UnifiedAgent) GenerateStructuredResponseWithUsage(ctx context.Context, schema json.RawMessage) (*StructuredResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("agent: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !json.Valid(schema) {
		return nil, fmt.Errorf("agent: invalid output schema")
	}
	schema = append(json.RawMessage(nil), schema...)
	a.mu.Lock()
	// This pre-existing guard checks the event stream only; a canceled
	// reply's core can outlive its stream. The newer between-replies seams
	// also check runningCores (see busyLocked).
	if a.activeConv != nil || a.activeReplies > 0 {
		a.mu.Unlock()
		return nil, fmt.Errorf("agent: structured generation requires an idle agent")
	}
	prompt := a.systemPrompt
	suffix := ""
	if a.stateAwareness {
		suffix = InjectStateAwareness("", a.state)
	}
	if text := skill.FormatSkillInstructions(a.skills); text != "" {
		suffix += "\n\n" + text
	}
	var msgs []*message.Msg
	if a.state.Summary != "" {
		msgs = append(msgs, message.UserMsg(a.name, "[Previous context summary]: "+a.state.Summary))
	}
	msgs = append(msgs, threadPromptMsgs(a.name, a.state.PeerSummary)...)
	if len(a.state.Plan) > 0 {
		msgs = append(msgs, planPromptMsg(a.name, a.state.Plan))
	}
	msgs = append(msgs, a.state.Context...)
	copied, err := copyStructuredValue(reflect.ValueOf(msgs), 0)
	replyID := a.state.ReplyID
	a.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("agent: structured snapshot: %w", err)
	}
	ctx = middleware.WithMiddleContext(ctx, middleware.MiddleContext{})
	if replyID != "" {
		middleware.GetMiddleContext(ctx).Set("agent", "reply_id", replyID)
	}
	result := &StructuredResponse{UsageComplete: true}
	sm := &structuredModel{agent: a, prompt: prompt, suffix: suffix, result: result}
	var cm model.ChatModel = sm
	if d, ok := a.model.(model.ThinkingDisabler); ok {
		cm = &structuredThinkingModel{structuredModel: sm, disabler: d}
	}
	output, _, err := model.GenerateStructuredOutputWithUsage(ctx, cm, copied.Interface().([]*message.Msg), schema)
	if result.Usage == nil {
		result.UsageComplete = false
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, err
	}
	result.Output = append(json.RawMessage(nil), output...)
	return result, nil
}

type structuredModel struct {
	agent          *UnifiedAgent
	prompt, suffix string
	result         *StructuredResponse
	providerErr    error
}

func (s *structuredModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	copied, err := copyStructuredValue(reflect.ValueOf(msgs), 0)
	if err != nil {
		return nil, err
	}
	prompt := middleware.ApplySystemPromptPipeline(ctx, s.agent.middlewares, s.agent.name, s.prompt) + s.suffix
	inputMsgs := append([]*message.Msg{message.SystemMsg(s.agent.name, prompt)}, copied.Interface().([]*message.Msg)...)
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	toolCopy, err := copyStructuredValue(reflect.ValueOf(o.Tools), 0)
	if err != nil {
		return nil, err
	}
	o.Tools = toolCopy.Interface().([]model.ToolSchema)
	if o.ToolChoice != nil {
		choice := *o.ToolChoice
		o.ToolChoice = &choice
	}
	input := &middleware.ModelCallInput{AgentName: s.agent.name, Messages: inputMsgs, Tools: o.Tools, ToolChoice: o.ToolChoice}
	if n, ok := s.agent.model.(model.ModelNamer); ok {
		input.ModelName = n.ModelName()
	}
	called := false
	s.providerErr = nil
	core := func(ctx context.Context, in *middleware.ModelCallInput) (*model.ChatResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		called = true
		actual := append([]model.CallOption(nil), opts...)
		actual = append(actual, model.WithTools(in.Tools), model.WithToolChoice(in.ToolChoice))
		resp, callErr := s.agent.model.Chat(ctx, in.Messages, actual...)
		s.providerErr = callErr
		s.addUsage(resp)
		return resp, callErr
	}
	resp, err := middleware.BuildModelCallChain(s.agent.middlewares, core)(ctx, input)
	if !called && resp != nil {
		s.addUsage(resp)
	}
	if err != nil {
		return resp, err
	}
	// Do not let the legacy helper repair an incomplete verdict into a pass.
	if resp == nil {
		return nil, fmt.Errorf("%w: no model response", agenterrors.ErrStructuredOutput)
	}
	if resp.Error != nil {
		return resp, resp.Error
	}
	if resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter {
		return resp, fmt.Errorf("%w: output stopped: %s", agenterrors.ErrStructuredOutput, resp.StopReason)
	}
	outputCalls := 0
	for _, b := range resp.Content {
		if tc, ok := b.(message.ToolCallBlock); ok {
			if tc.Name != "generate_structured_output" || !json.Valid([]byte(tc.Input)) {
				return resp, fmt.Errorf("%w: invalid output tool call", agenterrors.ErrStructuredOutput)
			}
			outputCalls++
		}
	}
	if outputCalls > 1 {
		return resp, fmt.Errorf("%w: multiple output calls", agenterrors.ErrStructuredOutput)
	}
	if outputCalls == 1 {
		return resp, nil
	}
	raw := strings.TrimSpace(resp.GetTextContent())
	if !json.Valid([]byte(raw)) {
		return resp, fmt.Errorf("%w: expected complete JSON", agenterrors.ErrStructuredOutput)
	}
	// Bridge the complete filtered value through the legacy extractor's tool
	// branch. Its text branch selects the first object inside arrays, which
	// could discard a contradictory verdict before domain validation.
	complete := *resp
	complete.Content = []message.ContentBlock{message.ToolCallBlock{
		Type: "tool_call", Name: "generate_structured_output", Input: raw,
	}}
	return &complete, nil
}

func (s *structuredModel) addUsage(resp *model.ChatResponse) {
	if resp == nil || resp.Usage == nil {
		s.result.UsageComplete = false
		return
	}
	if s.result.Usage == nil {
		s.result.Usage = &model.ChatUsage{}
	}
	u := s.result.Usage
	if u.TotalInputTokens > 0 || resp.Usage.TotalInputTokens > 0 {
		u.TotalInputTokens = u.EffectiveInputTokens() + resp.Usage.EffectiveInputTokens()
	}
	u.Time += resp.Usage.Time
	u.InputTokens += resp.Usage.InputTokens
	u.OutputTokens += resp.Usage.OutputTokens
	u.CacheCreationInputTokens += resp.Usage.CacheCreationInputTokens
	u.CacheInputTokens += resp.Usage.CacheInputTokens
}
func (s *structuredModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (s *structuredModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return s.agent.model.CountTokens(msgs, tools)
}
func (s *structuredModel) IsStructuredOutputFallbackError(err error) bool {
	// Only an error from the provider may advance compatibility strategies.
	// A guardrail error mentioning "thinking" must remain terminal.
	if s.providerErr == nil || !errors.Is(err, s.providerErr) {
		return false
	}
	if c, ok := s.agent.model.(model.StructuredFallbackClassifier); ok {
		return c.IsStructuredOutputFallbackError(s.providerErr)
	}
	text := strings.ToLower(s.providerErr.Error())
	return strings.Contains(text, "tool_choice") || strings.Contains(text, "thinking")
}

type structuredThinkingModel struct {
	*structuredModel
	disabler model.ThinkingDisabler
}

func (s *structuredThinkingModel) DisableThinkingOptions() []model.CallOption {
	return s.disabler.DisableThinkingOptions()
}

// Copy supported Go values without serializing polymorphic blocks or coercing
// integers in metadata to float64. Unsupported/cyclic custom data fails closed.
func copyStructuredValue(v reflect.Value, depth int) (reflect.Value, error) {
	if depth > 128 {
		return reflect.Value{}, fmt.Errorf("cyclic or deeply nested snapshot")
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		child, err := copyStructuredValue(v.Elem(), depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		out := reflect.New(v.Type()).Elem()
		if v.Kind() == reflect.Pointer {
			out.Set(reflect.New(v.Type().Elem()))
			out.Elem().Set(child)
		} else {
			out.Set(child)
		}
		return out, nil
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c, err := copyStructuredValue(v.Index(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(c)
		}
		return out, nil
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		if v.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, fmt.Errorf("unsupported snapshot map key: %s", v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c, err := copyStructuredValue(iter.Value(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.SetMapIndex(iter.Key(), c)
		}
		return out, nil
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				return reflect.Value{}, fmt.Errorf("unsupported private snapshot field: %s", v.Type())
			}
			c, err := copyStructuredValue(v.Field(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Field(i).Set(c)
		}
		return out, nil
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			c, err := copyStructuredValue(v.Index(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(c)
		}
		return out, nil
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return reflect.Value{}, fmt.Errorf("unsupported snapshot value: %s", v.Type())
	default:
		return v, nil
	}
}
