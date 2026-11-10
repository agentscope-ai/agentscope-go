package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

var applyPatchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {"type": "string", "description": "Path to the file to patch"},
		"patch": {"type": "string", "description": "A unified diff (hunks starting with @@) to apply to the file"}
	},
	"required": ["file_path", "patch"]
}`)

type applyPatchTool struct {
	BaseTool
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

type patchOp struct {
	kind byte // ' ' context, '-' remove, '+' add
	text string
}

type patchHunk struct {
	oldStart int // 1-based line in the original
	ops      []patchOp
}

// parseHunks parses a unified diff into hunks, ignoring file headers
// (---/+++/diff/index) and "\ No newline at end of file" markers.
func parseHunks(patch string) ([]patchHunk, error) {
	var hunks []patchHunk
	var cur *patchHunk
	for _, line := range strings.Split(patch, "\n") {
		if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				hunks = append(hunks, *cur)
			}
			oldStart, _ := strconv.Atoi(m[1])
			cur = &patchHunk{oldStart: oldStart}
			continue
		}
		if cur == nil {
			continue // preamble / file headers before the first hunk
		}
		if line == "" {
			cur.ops = append(cur.ops, patchOp{kind: ' ', text: ""})
			continue
		}
		switch line[0] {
		case ' ':
			cur.ops = append(cur.ops, patchOp{kind: ' ', text: line[1:]})
		case '-':
			cur.ops = append(cur.ops, patchOp{kind: '-', text: line[1:]})
		case '+':
			cur.ops = append(cur.ops, patchOp{kind: '+', text: line[1:]})
		case '\\':
			// "\ No newline at end of file" — ignore.
		default:
			// Unknown line inside a hunk: treat as context to be tolerant.
			cur.ops = append(cur.ops, patchOp{kind: ' ', text: line})
		}
	}
	if cur != nil {
		hunks = append(hunks, *cur)
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("patch contains no hunks")
	}
	return hunks, nil
}

// applyUnifiedDiff applies a unified diff to original, returning the patched
// content. It verifies that context and removed lines match the original at each
// hunk position; any mismatch returns an error and no partial result.
func applyUnifiedDiff(original, patch string) (string, error) {
	hunks, err := parseHunks(patch)
	if err != nil {
		return "", err
	}
	lines := strings.Split(original, "\n")

	var result []string
	srcIdx := 0 // 0-based cursor into lines
	for _, h := range hunks {
		start := h.oldStart - 1
		if start < 0 {
			start = 0
		}
		if start < srcIdx {
			return "", fmt.Errorf("overlapping or out-of-order hunk at line %d", h.oldStart)
		}
		if start > len(lines) {
			return "", fmt.Errorf("hunk start %d is beyond end of file", h.oldStart)
		}
		result = append(result, lines[srcIdx:start]...)
		srcIdx = start

		for _, op := range h.ops {
			switch op.kind {
			case ' ':
				if srcIdx >= len(lines) || lines[srcIdx] != op.text {
					return "", fmt.Errorf("context mismatch at line %d (expected %q)", srcIdx+1, op.text)
				}
				result = append(result, lines[srcIdx])
				srcIdx++
			case '-':
				if srcIdx >= len(lines) || lines[srcIdx] != op.text {
					return "", fmt.Errorf("removed-line mismatch at line %d (expected %q)", srcIdx+1, op.text)
				}
				srcIdx++
			case '+':
				result = append(result, op.text)
			}
		}
	}
	result = append(result, lines[srcIdx:]...)
	return strings.Join(result, "\n"), nil
}

func (t *applyPatchTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	path, ok := args["file_path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return NewErrorResponse(fmt.Errorf("file_path is required")), nil
	}
	path = strings.TrimSpace(path)
	patch, ok := args["patch"].(string)
	if !ok || patch == "" {
		return NewErrorResponse(fmt.Errorf("patch is required")), nil
	}

	// Backend branch: patch inside the configured sandbox using the relative path.
	if b, ok := getBackendIfSet(ctx); ok {
		p := pathpkg.Clean(path)
		data, readErr := b.ReadFile(ctx, p)
		if readErr != nil {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		oldContent := string(data)
		newContent, err := applyUnifiedDiff(oldContent, patch)
		if err != nil {
			return NewErrorResponse(err), nil
		}
		if err := b.WriteFile(ctx, p, []byte(newContent)); err != nil {
			return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
		}
		if rc := GetReadCache(ctx); rc != nil && rc.RejectsStaleWrites() {
			rc.noteOwnEdit(p, data, []byte(newContent), backendMtime(ctx, b, p))
		}
		return patchResponse(p, oldContent, newContent), nil
	}

	abs, err := resolvePath(ctx, path)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid path: %w", err)), nil
	}
	if rc := GetReadCache(ctx); rc != nil && !rc.HasBeenRead(abs) {
		return NewErrorResponse(fmt.Errorf("you must read the file first before patching it")), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		return NewErrorResponse(fmt.Errorf("read file: %w", err)), nil
	}
	oldContent := string(data)
	newContent, err := applyUnifiedDiff(oldContent, patch)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
		return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
	}
	if rc := GetReadCache(ctx); rc != nil && rc.RejectsStaleWrites() {
		rc.noteOwnEdit(abs, data, []byte(newContent), hostMtime(abs))
	}
	return patchResponse(abs, oldContent, newContent), nil
}

func patchResponse(path, oldContent, newContent string) *ToolResponse {
	resp := NewTextResponse(fmt.Sprintf("Patched %s", path))
	if diff := generateUnifiedDiff(path, oldContent, newContent); diff != "" {
		resp.Metadata = map[string]any{"diff": diff}
	}
	return resp
}

func (t *applyPatchTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
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

func (t *applyPatchTool) MatchRule(ruleContent string, input map[string]any) bool {
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

// ApplyPatchTool returns a tool that applies a unified diff to a file. The patch
// is applied atomically: any context/removed-line mismatch rejects the whole
// patch with no write. Honors the workspace jail, read-before-write guard, and a
// configured Docker/E2B backend.
func ApplyPatchTool() Tool {
	return &applyPatchTool{
		BaseTool: BaseTool{
			ToolName:        "ApplyPatch",
			ToolDescription: "Apply a unified diff (hunks starting with @@) to a file. All hunks must apply cleanly or none are written. Read the file first.",
			ToolSchema:      applyPatchSchema,
		},
	}
}
