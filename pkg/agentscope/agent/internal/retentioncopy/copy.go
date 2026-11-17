package retentioncopy

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"unsafe"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Copy copies only the documented closed set of types. It returns no partial
// value on failure. Cycles are refused by finite depth/node budgets (ErrLimit),
// not by invoking reflection or preserving shared graph identity. Raw JSON is
// copied byte-for-byte, not decoded. The caller must keep input immutable until
// this method returns, including while cancellation is being observed.
func (c *Copier) Copy(ctx context.Context, input any) (any, Stats, error) {
	if c == nil || !c.valid {
		return nil, Stats{}, ErrInvalidLimits
	}
	if ctx == nil {
		return nil, Stats{}, ErrInvalidContext
	}
	b := &budget{ctx: ctx, limits: c.limits}
	out, err := b.value(input, 0)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, b.stats, err
	}
	return out, b.stats, nil
}

func (b *budget) value(input any, depth int) (any, error) {
	if err := b.enter(depth); err != nil {
		return nil, err
	}
	if err := b.alloc(16); err != nil {
		return nil, err
	}
	switch v := input.(type) {
	case nil:
		return nil, nil
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, nil
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, ErrInvalidNumber
		}
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, ErrInvalidNumber
		}
		return v, nil
	case string:
		return v, b.payload(int64(len(v)))
	case message.Role:
		return v, b.payload(int64(len(v)))
	case message.ToolCallState:
		return v, b.payload(int64(len(v)))
	case message.ToolResultState:
		return v, b.payload(int64(len(v)))
	case message.ContentBlockType:
		return v, b.payload(int64(len(v)))
	case json.Number:
		if err := b.payload(int64(len(v))); err != nil {
			return nil, err
		}
		if len(v) == 0 || (v[0] != '-' && (v[0] < '0' || v[0] > '9')) {
			return nil, ErrInvalidNumber
		}
		// Charge the temporary byte conversion before the standard syntax checker.
		if err := b.alloc(int64(len(v))); err != nil {
			return nil, err
		}
		if strings.ContainsAny(string(v), " \t\r\n") || !json.Valid([]byte(v)) {
			return nil, ErrInvalidNumber
		}
		return v, nil
	case []byte:
		return copyBytes(b, v)
	case json.RawMessage:
		out, err := copyBytes(b, []byte(v))
		return json.RawMessage(out), err
	case []any:
		return copySlice(b, v, depth)
	case []string:
		return copySlice(b, v, depth)
	case []message.ContentBlock:
		return copySlice(b, v, depth)
	case []*message.Msg:
		return copySlice(b, v, depth)
	case map[string]any:
		return copyMap(b, v, depth)
	case map[string]string:
		return copyMap(b, v, depth)
	case types.JSONObject:
		return copyObject(b, v, depth)
	case *message.Msg:
		return copyPointer(b, v, depth)
	case message.Msg:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Messages, b.limits.MaxMessages, 1, "messages"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Name, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Role, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Content, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Metadata, depth); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Timestamp, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.FinishedAt, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Usage, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.Usage:
		return copyPointer(b, v, depth)
	case message.Usage:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.InputTokens, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.OutputTokens, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.CacheCreationInputTokens, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.CacheInputTokens, depth+1); err != nil {
			return nil, err
		}
		return out, nil
	case *message.TextBlock:
		return copyPointer(b, v, depth)
	case message.TextBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Text, depth+1); err != nil {
			return nil, err
		}
		return out, nil
	case *message.ThinkingBlock:
		return copyPointer(b, v, depth)
	case message.ThinkingBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Thinking, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Extra, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.RedactedThinkingBlock:
		return copyPointer(b, v, depth)
	case message.RedactedThinkingBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Data, depth+1); err != nil {
			return nil, err
		}
		return out, nil
	case *message.ToolCallBlock:
		return copyPointer(b, v, depth)
	case message.ToolCallBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Name, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Input, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.State, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.SuggestedRules, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Extra, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.ToolResultBlock:
		return copyPointer(b, v, depth)
	case message.ToolResultBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Name, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Output, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.State, depth); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Metadata, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.HintBlock:
		return copyPointer(b, v, depth)
	case message.HintBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Source, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Hint, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.DataBlock:
		return copyPointer(b, v, depth)
	case message.DataBlock:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		if err := reserve(&b.stats.Blocks, b.limits.MaxBlocks, 1, "blocks"); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.ID, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Name, depth+1); err != nil {
			return nil, err
		}
		if err := copyField(b, &out.Source, depth); err != nil {
			return nil, err
		}
		return out, nil
	case *message.Base64Source:
		return copyPointer(b, v, depth)
	case message.Base64Source:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.Data, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.MediaType, depth+1); err != nil {
			return nil, err
		}
		return out, nil
	case *message.URLSource:
		return copyPointer(b, v, depth)
	case message.URLSource:
		if err := b.alloc(int64(unsafe.Sizeof(v))); err != nil {
			return nil, err
		}
		out := v
		if _, err := b.value(v.Type, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.URL, depth+1); err != nil {
			return nil, err
		}
		if _, err := b.value(v.MediaType, depth+1); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, ErrUnsupported
	}
}

func copyField[T any](b *budget, dst *T, depth int) error {
	value, err := b.value(*dst, depth+1)
	if err != nil {
		return err
	}
	if value == nil {
		var zero T
		*dst = zero
	} else {
		*dst = value.(T)
	}
	return nil
}
func copyPointer[T any](b *budget, input *T, depth int) (*T, error) {
	if input == nil {
		return nil, nil
	}
	if err := b.alloc(int64(unsafe.Sizeof(*input))); err != nil {
		return nil, err
	}
	value, err := b.value(*input, depth+1)
	if err != nil {
		return nil, err
	}
	out := value.(T)
	return &out, nil
}
func copyBytes(b *budget, input []byte) ([]byte, error) {
	if err := b.payload(int64(len(input))); err != nil {
		return nil, err
	}
	if err := b.alloc(int64(unsafe.Sizeof(input))); err != nil {
		return nil, err
	}
	if err := b.slots(len(input), 1); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil
	}
	out := make([]byte, len(input))
	copy(out, input)
	return out, nil
}
func copySlice[T any](b *budget, input []T, depth int) ([]T, error) {
	if err := b.children(len(input), 1); err != nil {
		return nil, err
	}
	if err := b.alloc(int64(unsafe.Sizeof(input))); err != nil {
		return nil, err
	}
	var item T
	if err := b.slots(len(input), unsafe.Sizeof(item)); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil
	}
	out := make([]T, len(input))
	for i := range input {
		value, err := b.value(input[i], depth+1)
		if err != nil {
			return nil, err
		}
		if value != nil {
			out[i] = value.(T)
		}
	}
	return out, nil
}
func copyMap[T any](b *budget, input map[string]T, depth int) (map[string]T, error) {
	if err := b.children(len(input), 2); err != nil {
		return nil, err
	}
	if err := b.alloc(128); err != nil {
		return nil, err
	}
	if err := b.slots(len(input), 128); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, nil
	}
	out := make(map[string]T, len(input))
	for key, item := range input {
		if _, err := b.value(key, depth+1); err != nil {
			return nil, err
		}
		value, err := b.value(item, depth+1)
		if err != nil {
			return nil, err
		}
		var copied T
		if value != nil {
			copied = value.(T)
		}
		out[key] = copied
	}
	return out, nil
}
func copyObject(b *budget, input types.JSONObject, depth int) (types.JSONObject, error) {
	// This is a distinct named map with a distinct interface value type, not an
	// alias for map[string]any. Preserve it without a JSON round trip.
	out, err := copyMap(b, map[string]types.JSONValue(input), depth)
	return types.JSONObject(out), err
}
