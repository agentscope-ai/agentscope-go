package tierrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

var (
	// ErrChatOperationRequired rejects a Chat call without a trusted operation ID.
	ErrChatOperationRequired = errors.New("tierrouter: chat operation required")
	// ErrChatUnsupported rejects tools, multimodal content, nested retries, and other unsupported calls.
	ErrChatUnsupported = errors.New("tierrouter: unsupported chat operation")
	// ErrChatInvalidResponse rejects a partial or protocol-invalid response without fallback.
	ErrChatInvalidResponse = errors.New("tierrouter: invalid or incomplete chat response")
	// ErrResponseUnavailable prevents redispatch when a completed response is unavailable in this process.
	ErrResponseUnavailable = errors.New("tierrouter: completed chat response unavailable")
)

// ChatDeployment binds one tier to a single-attempt ChatModel.
type ChatDeployment struct {
	Tier  Tier
	Model model.ChatModel
}

// ChatRouterConfig registers the policy, current epoch source, deployments, and unknown-window fallback.
type ChatRouterConfig struct {
	Policy          Policy
	Epoch           func() int64
	Deployments     []ChatDeployment
	ContextFallback int
}

// ChatAttempt records safe evidence for one dispatch decision.
type ChatAttempt struct {
	Seq        int              `json:"seq"`
	Tier       Tier             `json:"tier"`
	Reason     string           `json:"reason"`
	Outcome    AttemptOutcome   `json:"outcome"`
	Err        string           `json:"error,omitempty"`
	RecordedAt time.Time        `json:"recorded_at"`
	Settled    bool             `json:"settled"`
	Usage      *model.ChatUsage `json:"usage,omitempty"`
}

// ChatRecord is a frozen evidence snapshot; it never contains message or response bodies.
type ChatRecord struct {
	ID                string        `json:"id"`
	RequestHash       string        `json:"request_hash"`
	TaskEpoch         int64         `json:"task_epoch"`
	State             OpState       `json:"state"`
	StopReason        string        `json:"stop_reason,omitempty"`
	Attempts          []ChatAttempt `json:"attempts,omitempty"`
	SubmittedAt       time.Time     `json:"submitted_at"`
	ResponseAvailable bool          `json:"response_available"`
}

func (r *ChatRecord) clone() *ChatRecord {
	if r == nil {
		return nil
	}
	out := *r
	out.Attempts = append([]ChatAttempt(nil), r.Attempts...)
	for i := range out.Attempts {
		out.Attempts[i].Usage = cloneChatUsage(out.Attempts[i].Usage)
	}
	return &out
}

type chatOperation struct {
	op       *Operation
	record   *ChatRecord
	response *model.ChatResponse
}

// ChatRouter routes buffered plain-text Chat calls and keeps operation evidence and responses in memory.
type ChatRouter struct {
	mu              sync.Mutex
	policy          Policy
	epoch           func() int64
	now             func() time.Time
	deployments     map[Tier]model.ChatModel
	contextFallback int
	operations      map[string]*chatOperation
}

var _ model.ChatModel = (*ChatRouter)(nil)

// NewChatRouter validates and snapshots deployment registration without calling a model.
func NewChatRouter(cfg ChatRouterConfig) (*ChatRouter, error) {
	if cfg.Policy == nil || cfg.Epoch == nil || len(cfg.Deployments) == 0 || cfg.ContextFallback <= 0 {
		return nil, errors.New("tierrouter: invalid chat router configuration")
	}
	r := &ChatRouter{
		policy:          cfg.Policy,
		epoch:           cfg.Epoch,
		now:             time.Now,
		deployments:     make(map[Tier]model.ChatModel, len(cfg.Deployments)),
		contextFallback: cfg.ContextFallback,
		operations:      make(map[string]*chatOperation),
	}
	for _, deployment := range cfg.Deployments {
		if deployment.Tier == "" || nilChatModel(deployment.Model) || r.deployments[deployment.Tier] != nil {
			return nil, errors.New("tierrouter: invalid or duplicate chat deployment")
		}
		switch deployment.Model.(type) {
		case *ChatRouter, *model.FallbackChatModel, *model.ConnectivityAwareModel:
			return nil, fmt.Errorf("tierrouter: nested retry/fallback deployment on tier %s", deployment.Tier)
		}
		r.deployments[deployment.Tier] = deployment.Model
	}
	return r, nil
}

func nilChatModel(m model.ChatModel) bool {
	if m == nil {
		return true
	}
	v := reflect.ValueOf(m)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

type chatOperationContextKey struct{}

// WithChatOperation attaches a trusted host operation to a Chat call context.
func WithChatOperation(ctx context.Context, op *Operation) context.Context {
	return context.WithValue(ctx, chatOperationContextKey{}, copyOperation(op))
}

func chatOperationFromContext(ctx context.Context) (*Operation, bool) {
	if ctx == nil {
		return nil, false
	}
	op, ok := ctx.Value(chatOperationContextKey{}).(*Operation)
	if !ok || op == nil {
		return nil, false
	}
	return copyOperation(op), true
}

type chatRequest struct {
	messages []*message.Msg
	options  model.CallOptions
	hash     string
}

func freezeChatRequest(op *Operation, msgs []*message.Msg, opts []model.CallOption) (*chatRequest, error) {
	var options model.CallOptions
	for _, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: nil call option", ErrChatUnsupported)
		}
		opt(&options)
	}
	if !validUTF8Strings(op.ID, op.Kind, op.Content) || !validChatOptionStrings(&options) {
		return nil, fmt.Errorf("%w: invalid UTF-8 in operation or options", ErrChatUnsupported)
	}
	options, err := cloneCallOptions(&options)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChatUnsupported, err)
	}
	if len(options.Tools) > 0 || options.Voice != nil || options.MaxRetries != 0 ||
		(options.ToolChoice != nil && options.ToolChoice.Mode != "" && options.ToolChoice.Mode != "none") {
		return nil, fmt.Errorf("%w: tools, voice, and nested retries are disabled", ErrChatUnsupported)
	}
	messages, err := cloneTextMessages(msgs)
	if err != nil {
		return nil, err
	}
	payload := struct {
		Domain    string            `json:"domain"`
		Kind      string            `json:"kind"`
		Content   string            `json:"content"`
		LocalOnly bool              `json:"local_only"`
		TaskEpoch int64             `json:"task_epoch"`
		TTL       time.Duration     `json:"ttl"`
		Messages  []*message.Msg    `json:"messages"`
		Options   model.CallOptions `json:"options"`
	}{"tierrouter.chat.v1", op.Kind, op.Content, op.LocalOnly, op.TaskEpoch, op.TTL, messages, options}
	wire, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: request identity: %v", ErrChatUnsupported, err)
	}
	sum := sha256.Sum256(wire)
	return &chatRequest{messages: messages, options: options, hash: hex.EncodeToString(sum[:])}, nil
}

// Chat routes one plain-text operation and accepts only responses current under the original epoch and TTL.
func (r *ChatRouter) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	op, ok := chatOperationFromContext(ctx)
	if !ok || op.ID == "" {
		return nil, ErrChatOperationRequired
	}
	req, err := freezeChatRequest(op, msgs, opts)
	if err != nil {
		return nil, err
	}
	if op.SubmittedAt.IsZero() {
		op.SubmittedAt = r.now()
	}

	r.mu.Lock()
	if existing, ok := r.operations[op.ID]; ok {
		defer r.mu.Unlock()
		if existing.record.RequestHash != req.hash {
			return nil, fmt.Errorf("%w: %s", ErrIDConflict, op.ID)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := r.chatGuardLocked(existing.op); err != nil {
			return nil, err
		}
		if existing.record.State == StateCompleted {
			if existing.response == nil {
				return nil, fmt.Errorf("%w: %s", ErrResponseUnavailable, op.ID)
			}
			resp, err := cloneChatResponse(existing.response)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrResponseUnavailable, err)
			}
			return resp, nil
		}
		return nil, replayChatError(existing.record)
	}
	if err := ctx.Err(); err != nil {
		r.rejectChatLocked(op, req.hash, stopCanceled)
		r.mu.Unlock()
		return nil, err
	}
	if code, err := r.chatGuardLocked(op); err != nil {
		r.rejectChatLocked(op, req.hash, code)
		r.mu.Unlock()
		return nil, err
	}
	candidates := r.chatCandidates(op)
	if len(candidates) == 0 {
		err := fmt.Errorf("%w: %s", ErrNoRoute, op.ID)
		r.rejectChatLocked(op, req.hash, rejectNoRoute)
		r.mu.Unlock()
		return nil, err
	}
	rec := &ChatRecord{ID: op.ID, RequestHash: req.hash, TaskEpoch: op.TaskEpoch, State: StatePending, SubmittedAt: op.SubmittedAt}
	r.operations[op.ID] = &chatOperation{op: op, record: rec}
	r.mu.Unlock()

	return r.executeChat(ctx, op, rec, req, candidates)
}

func (r *ChatRouter) rejectChatLocked(op *Operation, hash, code string) {
	rec := &ChatRecord{ID: op.ID, RequestHash: hash, TaskEpoch: op.TaskEpoch, State: StateRejected, StopReason: code, SubmittedAt: op.SubmittedAt}
	r.operations[op.ID] = &chatOperation{op: op, record: rec}
}

func (r *ChatRouter) chatCandidates(op *Operation) []Tier {
	tiers := r.policy.Prefer(copyOperation(op))
	if !op.LocalOnly {
		return append([]Tier(nil), tiers...)
	}
	out := make([]Tier, 0, 1)
	for _, tier := range tiers {
		if tier == TierDevice {
			out = append(out, tier)
		}
	}
	return out
}

func (r *ChatRouter) chatGuardLocked(op *Operation) (string, error) {
	if current := r.epoch(); op.TaskEpoch != current {
		return rejectStaleEpoch, fmt.Errorf("%w: %s (op epoch %d, current %d)", ErrStaleEpoch, op.ID, op.TaskEpoch, current)
	}
	if deadline, ok := op.Deadline(); ok && !r.now().Before(deadline) {
		return rejectExpired, fmt.Errorf("%w: %s", ErrExpired, op.ID)
	}
	return "", nil
}

func (r *ChatRouter) executeChat(ctx context.Context, op *Operation, rec *ChatRecord, req *chatRequest, candidates []Tier) (*model.ChatResponse, error) {
	for i, tier := range candidates {
		reason := "policy-preferred"
		if i > 0 {
			reason = "fallback-after-failure:" + string(candidates[i-1])
		}
		deployment, attached := r.deployments[tier]
		if !attached {
			if err := r.commitChatAttempt(ctx, op, rec, tier, reason, true); err != nil {
				return nil, err
			}
			continue
		}
		if err := r.commitChatAttempt(ctx, op, rec, tier, reason, false); err != nil {
			return nil, err
		}
		resp, callErr := deployment.Chat(ctx, req.messages, frozenChatOption(&req.options))
		if ctx.Err() != nil {
			callErr = ctx.Err()
		}
		var responseErr error
		if callErr == nil {
			resp, responseErr = validateChatResponse(resp)
		}
		accepted, stop, err := r.settleChatAttempt(op, rec, resp, callErr, responseErr)
		if stop || err != nil {
			return accepted, err
		}
	}
	r.mu.Lock()
	rec.State = StateExhausted
	r.mu.Unlock()
	return nil, fmt.Errorf("%w: %s", ErrExhausted, op.ID)
}

func (r *ChatRouter) commitChatAttempt(ctx context.Context, op *Operation, rec *ChatRecord, tier Tier, reason string, missed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return r.supersedeChatLocked(rec, stopCanceled, err)
	}
	if code, err := r.chatGuardLocked(op); err != nil {
		return r.supersedeChatLocked(rec, code, err)
	}
	attempt := ChatAttempt{Seq: len(rec.Attempts) + 1, Tier: tier, Reason: reason, RecordedAt: r.now()}
	if missed {
		attempt.Outcome = AttemptFailed
		attempt.Err = "provider not attached"
		attempt.Settled = true
	}
	rec.Attempts = append(rec.Attempts, attempt)
	return nil
}

func (r *ChatRouter) settleChatAttempt(op *Operation, rec *ChatRecord, resp *model.ChatResponse, callErr, responseErr error) (*model.ChatResponse, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := &rec.Attempts[len(rec.Attempts)-1]
	at.Settled = true
	switch {
	case callErr != nil && (unknownOutcome(callErr) || errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)):
		at.Outcome = AttemptUnknown
		at.Err = callErr.Error()
		rec.State = StateUncertain
		return nil, true, fmt.Errorf("%w: %v", ErrUncertain, callErr)
	case callErr != nil:
		at.Outcome = AttemptFailed
		at.Err = callErr.Error()
		return nil, false, nil
	case responseErr != nil:
		at.Outcome = AttemptUnknown
		at.Err = responseErr.Error()
		rec.State = StateUncertain
		return nil, true, responseErr
	default:
		at.Outcome = AttemptCompleted
		at.Usage = cloneChatUsage(resp.Usage)
		if code, err := r.chatGuardLocked(op); err != nil {
			return nil, true, r.supersedeChatLocked(rec, code, err)
		}
		rec.State = StateCompleted
		rec.ResponseAvailable = true
		operation := r.operations[op.ID]
		operation.response = resp
		accepted, err := cloneChatResponse(resp)
		if err != nil {
			return nil, true, fmt.Errorf("%w: %v", ErrResponseUnavailable, err)
		}
		return accepted, true, nil
	}
}

func (r *ChatRouter) supersedeChatLocked(rec *ChatRecord, code string, cause error) error {
	rec.State = StateSuperseded
	rec.StopReason = code
	rec.ResponseAvailable = false
	return cause
}

func replayChatError(rec *ChatRecord) error {
	switch rec.State {
	case StatePending:
		return fmt.Errorf("%w: %s", ErrInFlight, rec.ID)
	case StateRejected:
		if rec.StopReason == stopCanceled {
			return context.Canceled
		}
		if rec.StopReason == rejectExpired {
			return fmt.Errorf("%w: %s", ErrExpired, rec.ID)
		}
		if rec.StopReason == rejectStaleEpoch {
			return fmt.Errorf("%w: %s", ErrStaleEpoch, rec.ID)
		}
		return fmt.Errorf("%w: %s", ErrNoRoute, rec.ID)
	case StateExhausted:
		return fmt.Errorf("%w: %s", ErrExhausted, rec.ID)
	case StateSuperseded:
		if rec.StopReason == stopCanceled {
			return context.Canceled
		}
		if rec.StopReason == rejectExpired {
			return fmt.Errorf("%w: %s", ErrExpired, rec.ID)
		}
		return fmt.Errorf("%w: %s", ErrStaleEpoch, rec.ID)
	default:
		return fmt.Errorf("%w: %s", ErrUncertain, rec.ID)
	}
}

const stopCanceled = "canceled"

func frozenChatOption(snapshot *model.CallOptions) model.CallOption {
	wire, err := json.Marshal(snapshot)
	if err != nil {
		panic("tierrouter: validated options became unserializable")
	}
	return func(out *model.CallOptions) {
		if err := json.Unmarshal(wire, out); err != nil {
			panic("tierrouter: validated options became undecodable")
		}
	}
}

func cloneCallOptions(in *model.CallOptions) (model.CallOptions, error) {
	wire, err := json.Marshal(in)
	if err != nil {
		return model.CallOptions{}, err
	}
	var out model.CallOptions
	if err := json.Unmarshal(wire, &out); err != nil {
		return model.CallOptions{}, err
	}
	return out, nil
}

func cloneTextMessages(msgs []*message.Msg) ([]*message.Msg, error) {
	out := make([]*message.Msg, len(msgs))
	for i, msg := range msgs {
		if msg == nil {
			return nil, fmt.Errorf("%w: nil message", ErrChatUnsupported)
		}
		if !validUTF8Strings(msg.ID, msg.Name, string(msg.Role), msg.Timestamp, msg.FinishedAt) {
			return nil, fmt.Errorf("%w: invalid UTF-8 in message", ErrChatUnsupported)
		}
		cp := *msg
		cp.Content = make([]message.ContentBlock, len(msg.Content))
		for j, block := range msg.Content {
			text, ok := block.(message.TextBlock)
			if !ok {
				return nil, fmt.Errorf("%w: only text messages are supported", ErrChatUnsupported)
			}
			if !validUTF8Strings(text.Type, text.ID, text.Text) {
				return nil, fmt.Errorf("%w: invalid UTF-8 in text block", ErrChatUnsupported)
			}
			cp.Content[j] = text
		}
		var metadata types.JSONObject
		if msg.Metadata != nil {
			metadata = make(types.JSONObject, len(msg.Metadata))
		}
		for key, value := range msg.Metadata {
			if !utf8.ValidString(key) {
				return nil, fmt.Errorf("%w: invalid UTF-8 in metadata key", ErrChatUnsupported)
			}
			cloned, err := cloneJSONValue(value)
			if err != nil {
				return nil, fmt.Errorf("%w: message metadata: %v", ErrChatUnsupported, err)
			}
			metadata[key] = cloned
		}
		cp.Metadata = metadata
		if msg.Usage != nil {
			usage := *msg.Usage
			cp.Usage = &usage
		}
		out[i] = &cp
	}
	return out, nil
}

func validateChatResponse(resp *model.ChatResponse) (*model.ChatResponse, error) {
	if resp == nil || resp.Error != nil || !resp.IsLast || len(resp.Content) == 0 ||
		resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter || resp.StopReason == model.StopReasonToolCalls {
		return nil, ErrChatInvalidResponse
	}
	if !validUTF8Strings(resp.ID, resp.CreatedAt, resp.ModelName, resp.StopReason) {
		return nil, ErrChatInvalidResponse
	}
	for _, block := range resp.Content {
		text, ok := block.(message.TextBlock)
		if !ok || !validUTF8Strings(text.Type, text.ID, text.Text) {
			return nil, ErrChatInvalidResponse
		}
	}
	cloned, err := cloneChatResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChatInvalidResponse, err)
	}
	return cloned, nil
}

func cloneChatResponse(resp *model.ChatResponse) (*model.ChatResponse, error) {
	if resp == nil {
		return nil, ErrChatInvalidResponse
	}
	out := *resp
	out.Content = append([]message.ContentBlock(nil), resp.Content...)
	metadata, err := cloneJSONMap(resp.Metadata)
	if err != nil {
		return nil, err
	}
	out.Metadata = metadata
	out.Usage = cloneChatUsage(resp.Usage)
	return &out, nil
}

func cloneChatUsage(usage *model.ChatUsage) *model.ChatUsage {
	if usage == nil {
		return nil
	}
	out := *usage
	return &out
}

type jsonVisit struct {
	kind reflect.Kind
	ptr  uintptr
	len  int
}

func cloneJSONMap(in map[string]any) (map[string]any, error) {
	return cloneJSONMapSeen(in, make(map[jsonVisit]bool))
}

func cloneJSONMapSeen(in map[string]any, seen map[jsonVisit]bool) (map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	visit := jsonVisit{kind: reflect.Map, ptr: reflect.ValueOf(in).Pointer()}
	if seen[visit] {
		return nil, errors.New("cyclic metadata map")
	}
	seen[visit] = true
	defer delete(seen, visit)
	out := make(map[string]any, len(in))
	for key, value := range in {
		if !utf8.ValidString(key) {
			return nil, errors.New("invalid UTF-8 in metadata key")
		}
		cloned, err := cloneJSONValueSeen(value, seen)
		if err != nil {
			return nil, err
		}
		out[key] = cloned
	}
	return out, nil
}

func cloneJSONValue(value any) (any, error) {
	return cloneJSONValueSeen(value, make(map[jsonVisit]bool))
}

func cloneJSONValueSeen(value any, seen map[jsonVisit]bool) (any, error) {
	switch value := value.(type) {
	case nil, bool:
		return value, nil
	case string:
		if !utf8.ValidString(value) {
			return nil, errors.New("invalid UTF-8 in metadata value")
		}
		return value, nil
	case json.Number:
		if _, err := json.Marshal(value); err != nil {
			return nil, err
		}
		return value, nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("non-finite number")
		}
		return value, nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("non-finite number")
		}
		return value, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return value, nil
	case []any:
		if value == nil {
			return []any(nil), nil
		}
		visit := jsonVisit{kind: reflect.Slice, ptr: reflect.ValueOf(value).Pointer(), len: len(value)}
		if seen[visit] {
			return nil, errors.New("cyclic metadata slice")
		}
		seen[visit] = true
		defer delete(seen, visit)
		out := make([]any, len(value))
		for i := range value {
			cloned, err := cloneJSONValueSeen(value[i], seen)
			if err != nil {
				return nil, err
			}
			out[i] = cloned
		}
		return out, nil
	case map[string]any:
		return cloneJSONMapSeen(value, seen)
	case types.JSONObject:
		if value == nil {
			return types.JSONObject(nil), nil
		}
		visit := jsonVisit{kind: reflect.Map, ptr: reflect.ValueOf(value).Pointer()}
		if seen[visit] {
			return nil, errors.New("cyclic metadata object")
		}
		seen[visit] = true
		defer delete(seen, visit)
		out := make(types.JSONObject, len(value))
		for key, item := range value {
			if !utf8.ValidString(key) {
				return nil, errors.New("invalid UTF-8 in metadata key")
			}
			cloned, err := cloneJSONValueSeen(item, seen)
			if err != nil {
				return nil, err
			}
			out[key] = cloned
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported metadata value %T", value)
	}
}

func validChatOptionStrings(options *model.CallOptions) bool {
	if options.ResponseFormat != nil && !validUTF8Strings(options.ResponseFormat.Type, options.ResponseFormat.Name) {
		return false
	}
	if options.ReasoningEffort != nil && !validUTF8Strings(*options.ReasoningEffort) {
		return false
	}
	if options.ToolChoice != nil {
		if !validUTF8Strings(options.ToolChoice.Mode) || !validUTF8Strings(options.ToolChoice.Tools...) {
			return false
		}
	}
	return true
}

func validUTF8Strings(values ...string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

// ChatSnapshot returns a deep copy of the operation's current evidence.
func (r *ChatRouter) ChatSnapshot(id string) (*ChatRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.operations[id]
	if !ok {
		return nil, false
	}
	return op.record.clone(), true
}

// ChatStream rejects streaming; this slice supports buffered Chat only.
func (r *ChatRouter) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

// CountTokens returns the maximum estimate across all possible deployments.
func (r *ChatRouter) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	maxTokens := 0
	for _, deployment := range r.deployments {
		maxTokens = max(maxTokens, deployment.CountTokens(msgs, tools))
	}
	return maxTokens
}

// ContextSize returns the minimum window across deployments, using the configured fallback for unknown windows.
func (r *ChatRouter) ContextSize() int {
	size := 0
	for _, deployment := range r.deployments {
		candidate := model.ResolveContextSize(deployment, r.contextFallback)
		if candidate <= 0 {
			candidate = r.contextFallback
		}
		if size == 0 || candidate < size {
			size = candidate
		}
	}
	return size
}
