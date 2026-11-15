package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

var multiEditSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {
			"type": "string",
			"description": "Path to the file to edit"
		},
		"edits": {
			"type": "array",
			"description": "A sequence of find/replace edits applied in order. All must apply or none are written (atomic).",
			"items": {
				"type": "object",
				"properties": {
					"old_string": {"type": "string", "description": "The exact text to find"},
					"new_string": {"type": "string", "description": "The replacement text"},
					"replace_all": {"type": "boolean", "description": "Replace all occurrences (default false)"}
				},
				"required": ["old_string", "new_string"]
			}
		}
	},
	"required": ["file_path", "edits"]
}`)

type multiEditTool struct {
	BaseTool
}

type multiEditOp struct {
	oldStr     string
	newStr     string
	replaceAll bool
}

// applyMultiEdit applies all edits in order to content, returning the final
// content and total replacement count. It is all-or-nothing: any failing edit
// aborts with an error and no partial result.
func applyMultiEdit(content string, ops []multiEditOp) (string, int, error) {
	total := 0
	for i, op := range ops {
		next, n, err := applyStringEdit(content, op.oldStr, op.newStr, op.replaceAll)
		if err != nil {
			return "", 0, fmt.Errorf("edit %d: %w", i+1, err)
		}
		content = next
		total += n
	}
	return content, total, nil
}

func parseMultiEdits(args map[string]any) ([]multiEditOp, error) {
	raw, ok := args["edits"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("edits is required and must be a non-empty array")
	}
	ops := make([]multiEditOp, 0, len(raw))
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edit %d: must be an object", i+1)
		}
		oldStr, ok := m["old_string"].(string)
		if !ok || oldStr == "" {
			return nil, fmt.Errorf("edit %d: old_string is required", i+1)
		}
		newStr, _ := m["new_string"].(string)
		if oldStr == newStr {
			return nil, fmt.Errorf("edit %d: old_string and new_string are identical", i+1)
		}
		replaceAll := false
		if v, ok := m["replace_all"].(bool); ok {
			replaceAll = v
		}
		ops = append(ops, multiEditOp{oldStr: oldStr, newStr: newStr, replaceAll: replaceAll})
	}
	return ops, nil
}

func (t *multiEditTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	path, ok := args["file_path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return NewErrorResponse(fmt.Errorf("file_path is required")), nil
	}
	path = strings.TrimSpace(path)

	ops, err := parseMultiEdits(args)
	if err != nil {
		return NewErrorResponse(err), nil
	}

	// Backend branch: edit inside the configured sandbox using the relative path.
	if b, ok := getBackendIfSet(ctx); ok {
		p := pathpkg.Clean(path)
		if rc := GetReadCache(ctx); rc != nil && !rc.HasBeenRead(p) {
			return NewErrorResponse(fmt.Errorf("you must read the file first before editing it")), nil
		}
		data, readErr := b.ReadFile(ctx, p)
		if readErr != nil {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		oldContent := string(data)
		newContent, count, editErr := applyMultiEdit(oldContent, ops)
		if editErr != nil {
			return NewErrorResponse(editErr), nil
		}
		if err := b.WriteFile(ctx, p, []byte(newContent)); err != nil {
			return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
		}
		if rc := GetReadCache(ctx); rc != nil && rc.RejectsStaleWrites() {
			rc.noteOwnEdit(p, data, []byte(newContent), backendMtime(ctx, b, p))
		}
		return multiEditResponse(p, oldContent, newContent, count, len(ops)), nil
	}

	abs, err := resolvePath(ctx, path)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid path: %w", err)), nil
	}
	if rc := GetReadCache(ctx); rc != nil && !rc.HasBeenRead(abs) {
		return NewErrorResponse(fmt.Errorf("you must read the file first before editing it")), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		return NewErrorResponse(fmt.Errorf("read file: %w", err)), nil
	}
	oldContent := string(data)
	newContent, count, editErr := applyMultiEdit(oldContent, ops)
	if editErr != nil {
		return NewErrorResponse(editErr), nil
	}
	if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
		return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
	}
	if rc := GetReadCache(ctx); rc != nil && rc.RejectsStaleWrites() {
		rc.noteOwnEdit(abs, data, []byte(newContent), nil)
	}
	return multiEditResponse(abs, oldContent, newContent, count, len(ops)), nil
}

func multiEditResponse(path, oldContent, newContent string, count, numEdits int) *ToolResponse {
	resp := NewTextResponse(fmt.Sprintf("Applied %d edit(s) (%d replacement(s)) to %s", numEdits, count, path))
	if diff := generateUnifiedDiff(path, oldContent, newContent); diff != "" {
		resp.Metadata = map[string]any{"diff": diff}
	}
	return resp
}

func (t *multiEditTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
	path, _ := input["file_path"].(string)
	if path == "" {
		return permission.Decision{Behavior: permission.BehaviorPassthrough}
	}
	if dangerous, reason := CheckDangerousPath(path); dangerous {
		return permission.Decision{Behavior: permission.BehaviorAsk, Message: reason, BypassImmune: true}
	}
	if ctx != nil && ctx.Mode == permission.ModeAcceptEdits {
		return permission.Decision{Behavior: permission.BehaviorAllow}
	}
	return permission.Decision{Behavior: permission.BehaviorPassthrough}
}

func (t *multiEditTool) MatchRule(ruleContent string, input map[string]any) bool {
	if ruleContent == "" {
		return true
	}
	path, _ := input["file_path"].(string)
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	if matched, _ := filepath.Match(ruleContent, abs); matched {
		return true
	}
	matched, _ := filepath.Match(ruleContent, filepath.Base(abs))
	return matched
}

// MultiEditTool returns a tool that applies multiple find/replace edits to a
// single file atomically (all edits succeed or none are written).
func MultiEditTool() Tool {
	return &multiEditTool{
		BaseTool: BaseTool{
			ToolName:        "MultiEdit",
			ToolDescription: "Apply a sequence of find/replace edits to a single file atomically. All edits must apply or none are written. Read the file first.",
			ToolSchema:      multiEditSchema,
		},
	}
}
