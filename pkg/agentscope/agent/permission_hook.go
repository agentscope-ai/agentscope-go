package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// checkToolPermission keeps model I/O and host hooks outside the engine lock.
// Eligibility is explicit, so context replacement by middleware cannot enable
// permission hooks on restored, resumed or parallel work.
func (a *UnifiedAgent) checkToolPermission(ctx context.Context, t tool.Tool, tc *message.ToolCallBlock, input map[string]any, hooks bool) (permission.Decision, error) {
	engine := func() (permission.Decision, error) {
		if pctx := tool.BackendPermissionContext(ctx, a.engine.Context); pctx != nil {
			return a.engine.CheckPermissionInContext(t, input, pctx)
		}
		return a.engine.CheckPermission(t, input)
	}
	if !hooks || len(a.middlewares) == 0 {
		return engine()
	}
	if err := ctx.Err(); err != nil {
		return permission.Decision{}, err
	}
	// JSON snapshots isolate nested arguments and provider metadata. The core
	// always authorizes the original invocation, not middleware replacements.
	original := middleware.CheckPermissionInput{AgentName: a.name, ToolCall: *tc, ToolInput: input}
	encoded, err := json.Marshal(original)
	if err != nil {
		return permission.Decision{}, fmt.Errorf("permission hook input cannot be copied")
	}
	var snapshot middleware.CheckPermissionInput
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return permission.Decision{}, err
	}
	// Preserve ParseInput's numeric types in the independently parsed arguments;
	// opaque provider metadata keeps exact JSON number tokens instead.
	snapshot.ToolInput, err = tc.ParseInput()
	if err != nil {
		return permission.Decision{}, err
	}
	// Normalize key ordering after decoding while preserving exact numeric tokens.
	encoded, err = json.Marshal(snapshot)
	if err != nil {
		return permission.Decision{}, err
	}
	unchanged := func(v *middleware.CheckPermissionInput) bool {
		b, e := json.Marshal(v)
		return e == nil && bytes.Equal(b, encoded)
	}
	core := func(c context.Context, in *middleware.CheckPermissionInput) (*permission.Decision, error) {
		if err := c.Err(); err != nil {
			return nil, err
		}
		if !unchanged(in) {
			return nil, fmt.Errorf("permission hooks cannot rewrite the invocation")
		}
		d, e := engine()
		return &d, e
	}
	d, err := middleware.BuildCheckPermissionChain(a.middlewares, core)(ctx, &snapshot)
	if err != nil {
		return permission.Decision{}, err
	}
	if err := ctx.Err(); err != nil {
		return permission.Decision{}, err
	}
	if !unchanged(&snapshot) {
		return permission.Decision{}, fmt.Errorf("permission hooks cannot rewrite the invocation")
	}
	if d == nil {
		return permission.Decision{}, fmt.Errorf("permission hook returned a nil decision")
	}
	switch d.Behavior {
	case permission.BehaviorAllow, permission.BehaviorDeny, permission.BehaviorAsk, permission.BehaviorPassthrough:
		return *d, nil
	default:
		return permission.Decision{}, fmt.Errorf("permission hook returned an unknown behavior")
	}
}
