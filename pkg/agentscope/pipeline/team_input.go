package pipeline

import (
	"context"
	"fmt"
	"reflect"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type teamResponse struct {
	confirm  *event.UserConfirmResultEvent
	external *event.ExternalExecutionResultEvent
}
type teamPending struct {
	request  string
	reply    string
	confirm  bool
	calls    map[string]string
	used     bool
	response chan teamResponse
}

// SubmitUserConfirm copies and queues one complete pending confirmation.
// requestID is the original RequireUserConfirmEvent.GetEventID(), not a newly
// generated response ID: the same tool call may ask again for a directory grant.
// Unknown/stale/duplicate IDs, edited tool names and mixed-owner responses fail
// before agent submission. Do not mutate input during this call; after return,
// supported nested values belong to an independent copy.
func (r *TeamRun) SubmitUserConfirm(requestID string, input *event.UserConfirmResultEvent) error {
	if input == nil {
		return ErrTeamInput
	}
	cp, err := cloneTeamInput(*input)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for i := range cp.ConfirmResults {
		result := &cp.ConfirmResults[i]
		if _, exists := ids[result.ToolCall.ID]; exists {
			return ErrTeamInput
		}
		ids[result.ToolCall.ID] = result.ToolCall.Name
	}
	return r.submit(requestID, cp.ReplyID, true, ids, teamResponse{confirm: &cp})
}

// SubmitExternalResult copies and queues one complete pending external result.
// requestID is the original RequireExternalExecutionEvent.GetEventID().
// Standard exported data/block/rule values retain their concrete Go types;
// cyclic/deep values, non-string map keys and opaque mutable values are rejected.
func (r *TeamRun) SubmitExternalResult(requestID string, input *event.ExternalExecutionResultEvent) error {
	if input == nil {
		return ErrTeamInput
	}
	cp, err := cloneTeamInput(*input)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, result := range cp.ExecutionResults {
		if _, exists := ids[result.ID]; exists {
			return ErrTeamInput
		}
		switch result.State {
		case message.ToolResultSuccess, message.ToolResultError, message.ToolResultDenied, message.ToolResultInterrupted:
		default:
			return ErrTeamInput
		}
		ids[result.ID] = result.Name
	}
	return r.submit(requestID, cp.ReplyID, false, ids, teamResponse{external: &cp})
}

func (r *TeamRun) submit(request, reply string, confirm bool, ids map[string]string, value teamResponse) error {
	if r == nil || r.ctx == nil {
		return ErrTeamClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.ctx.Err() != nil {
		return ErrTeamClosed
	}
	p := r.pending
	if p == nil || p.used || p.confirm != confirm || request == "" || p.request != request || reply == "" || p.reply != reply || len(ids) == 0 || len(ids) != len(p.calls) {
		return ErrTeamInput
	}
	for id, name := range ids {
		if expected, ok := p.calls[id]; !ok || name != expected {
			return ErrTeamInput
		}
	}
	p.used = true
	p.response <- value // capacity one, accepted once under mu; never blocks
	return nil
}

func (r *TeamRun) hostResponse(ctx context.Context, a *agent.UnifiedAgent, ev event.Event) error {
	p := &teamPending{request: ev.GetEventID(), reply: ev.GetReplyID(), calls: map[string]string{}, response: make(chan teamResponse, 1)}
	if p.request == "" {
		return ErrTeamInput
	}
	var calls []message.ToolCallBlock
	var forward event.Event
	switch e := ev.(type) {
	case event.RequireUserConfirmEvent:
		cp, err := cloneTeamInput(e)
		if err != nil {
			return err
		}
		forward = cp
		calls = cp.ToolCalls
		p.confirm = true
	case event.RequireExternalExecutionEvent:
		cp, err := cloneTeamInput(e)
		if err != nil {
			return err
		}
		forward = cp
		calls = cp.ToolCalls
	default:
		return ErrTeamInput
	}
	for _, call := range calls {
		if call.ID == "" || call.Name == "" {
			return ErrTeamInput
		}
		if _, ok := p.calls[call.ID]; ok {
			return ErrTeamInput
		}
		p.calls[call.ID] = call.Name
	}
	if len(p.calls) == 0 {
		return ErrTeamInput
	}
	r.mu.Lock()
	r.pending = p
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.pending == p {
			r.pending = nil
		}
		r.mu.Unlock()
	}()
	if !r.emit(ctx, forward) {
		return ctx.Err()
	}
	var response teamResponse
	select {
	case response = <-p.response:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.pending == p {
		r.pending = nil
	}
	r.mu.Unlock()
	// Only this coordinator submits to these exclusively owned agents. A
	// request is consumed once, so its capacity-one agent channel is available;
	// no unbounded or detached sender goroutine is needed.
	if response.confirm != nil {
		a.SubmitUserConfirm(response.confirm)
	} else {
		a.SubmitExternalResult(response.external)
	}
	return nil
}

func teamNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

// Preserve typed permission.Rule and ContentBlock values. A JSON round trip
// changes those types and can silently discard approved permission rules.
func cloneTeamInput[T any](value T) (T, error) {
	var zero T
	nodes := 100000
	v, err := copyTeamValue(reflect.ValueOf(value), 0, &nodes)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrTeamInput, err)
	}
	return v.Interface().(T), nil
}

func copyTeamValue(v reflect.Value, depth int, nodes *int) (reflect.Value, error) {
	*nodes--
	if depth > 64 || *nodes < 0 {
		return reflect.Value{}, fmt.Errorf("submission nesting/size limit exceeded")
	}
	if !v.IsValid() {
		return v, nil
	}
	copyChild := func(child reflect.Value) (reflect.Value, error) { return copyTeamValue(child, depth+1, nodes) }
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		child, err := copyChild(v.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		if v.Kind() == reflect.Pointer {
			out := reflect.New(v.Type().Elem())
			out.Elem().Set(child)
			return out, nil
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(child)
		return out, nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, fmt.Errorf("submission maps need string keys")
		}
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		if v.Len() > *nodes {
			return reflect.Value{}, fmt.Errorf("submission map too large")
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			child, err := copyChild(it.Value())
			if err != nil {
				return reflect.Value{}, err
			}
			out.SetMapIndex(it.Key(), child)
		}
		return out, nil
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		if v.Len() > *nodes {
			return reflect.Value{}, fmt.Errorf("submission slice too large")
		}
		var out reflect.Value
		if v.Kind() == reflect.Slice {
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		} else {
			out = reflect.New(v.Type()).Elem()
		}
		for i := 0; i < v.Len(); i++ {
			child, err := copyChild(v.Index(i))
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(child)
		}
		return out, nil
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanInterface() {
				return reflect.Value{}, fmt.Errorf("opaque struct values are unsupported")
			}
			child, err := copyChild(v.Field(i))
			if err != nil {
				return reflect.Value{}, err
			}
			out.Field(i).Set(child)
		}
		return out, nil
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return v, nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported submission value %s", v.Kind())
	}
}
