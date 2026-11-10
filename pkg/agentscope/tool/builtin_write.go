package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

const (
	// MaxWriteBytes is the maximum size of a single file write (10 MB).
	// This prevents agents from exhausting disk space via unbounded writes.
	MaxWriteBytes = 10 * 1024 * 1024
)

// executableExtensions lists file extensions that produce directly executable
// files. Writes to these paths receive elevated permission scrutiny.
var executableExtensions = map[string]bool{
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".csh": true,
	".py": true, ".rb": true, ".pl": true, ".php": true, ".lua": true,
	".js": true, ".mjs": true, ".cjs": true, ".ts": true,
	".exe": true, ".bat": true, ".cmd": true, ".ps1": true, ".psm1": true,
	".com": true, ".scr": true, ".msi": true,
	".so": true, ".dylib": true, ".dll": true,
	".elf": true, ".bin": true, ".run": true,
	".jar": true, ".war": true,
	".AppImage": true,
}

var writeSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {
			"type": "string",
			"description": "Absolute or relative path to the file to write"
		},
		"content": {
			"type": "string",
			"description": "The content to write to the file (complete overwrite)"
		}
	},
	"required": ["file_path", "content"]
}`)

type writeTool struct {
	BaseTool
}

func (t *writeTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	raw, ok := args["file_path"]
	if !ok {
		return NewErrorResponse(fmt.Errorf("file_path is required")), nil
	}
	path, ok := raw.(string)
	if !ok {
		return NewErrorResponse(fmt.Errorf("file_path must be a string")), nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return NewErrorResponse(fmt.Errorf("file_path cannot be empty")), nil
	}

	contentRaw, ok := args["content"]
	if !ok {
		return NewErrorResponse(fmt.Errorf("content is required")), nil
	}
	content, ok := contentRaw.(string)
	if !ok {
		return NewErrorResponse(fmt.Errorf("content must be a string")), nil
	}

	// Enforce maximum write size to prevent disk exhaustion.
	if len(content) > MaxWriteBytes {
		return NewErrorResponse(fmt.Errorf("content size %d bytes exceeds maximum %d bytes (%d MB)",
			len(content), MaxWriteBytes, MaxWriteBytes/(1024*1024))), nil
	}

	// If a custom backend (Docker/E2B) is configured, write inside it using the
	// caller-provided (workspace-relative) path.
	if b, ok := getBackendIfSet(ctx); ok {
		p := pathpkg.Clean(path)
		var oldContent string
		if existingData, readErr := b.ReadFile(ctx, p); readErr == nil {
			if rc := GetReadCache(ctx); rc != nil && !rc.HasBeenRead(p) {
				return NewErrorResponse(fmt.Errorf("file exists but has not been read yet; you must read the file first before writing to it")), nil
			}
			if staleWrite(ctx, p, existingData, content) {
				return NewErrorResponse(errors.New(staleWriteMessage)), nil
			}
			oldContent = string(existingData)
		}
		if err := b.WriteFile(ctx, p, []byte(content)); err != nil {
			return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
		}
		// Upstream #2092: drop any cached copy so the next Read/Edit sees
		// the new content — the host-side cache cannot stat backend files.
		if rc := GetReadCache(ctx); rc != nil {
			rc.Remove(p)
		}
		resp := NewTextResponse(fmt.Sprintf("Written %d bytes to %s", len(content), p))
		if oldContent != "" || content != "" {
			if diff := generateUnifiedDiff(p, oldContent, content); diff != "" {
				resp.Metadata = map[string]any{"diff": diff}
			}
		}
		return resp, nil
	}

	abs, err := resolvePath(ctx, path)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid path: %w", err)), nil
	}

	// Read old content for diff generation
	var oldContent string
	if existingData, statErr := os.ReadFile(abs); statErr == nil {
		// File exists, check read-before-write guard
		if rc := GetReadCache(ctx); rc != nil {
			if !rc.HasBeenRead(abs) {
				return NewErrorResponse(fmt.Errorf("file exists but has not been read yet; you must read the file first before writing to it")), nil
			}
		}
		if staleWrite(ctx, abs, existingData, content) {
			return NewErrorResponse(errors.New(staleWriteMessage)), nil
		}
		oldContent = string(existingData)
	}

	if err := fsutil.WriteFileAtomic(abs, []byte(content), 0o644); err != nil {
		return NewErrorResponse(fmt.Errorf("write file: %w", err)), nil
	}

	// Invalidate read cache if present
	if rc := GetReadCache(ctx); rc != nil {
		rc.CleanFileCache(nil) // Invalidate cache for the written file
	}

	resp := NewTextResponse(fmt.Sprintf("Written %d bytes to %s", len(content), abs))

	// Generate diff metadata
	if oldContent != "" || content != "" {
		diff := generateUnifiedDiff(abs, oldContent, content)
		if diff != "" {
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			resp.Metadata["diff"] = diff
		}
	}

	return resp, nil
}

// CheckPermissions checks for dangerous paths and defers to AcceptEdits mode.
func (t *writeTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
	path, _ := input["file_path"].(string)
	if path == "" {
		return permission.Decision{Behavior: permission.BehaviorPassthrough}
	}

	// Dangerous path -> bypass-immune ASK
	if dangerous, reason := CheckDangerousPath(path); dangerous {
		return permission.Decision{
			Behavior:     permission.BehaviorAsk,
			Message:      reason,
			BypassImmune: true,
		}
	}

	// Executable file extensions get elevated scrutiny: ASK even in
	// AcceptEdits mode because a written script could later be executed.
	ext := strings.ToLower(filepath.Ext(path))
	if executableExtensions[ext] {
		return permission.Decision{
			Behavior:     permission.BehaviorAsk,
			Message:      fmt.Sprintf("writing executable file (%s extension)", ext),
			BypassImmune: true,
		}
	}

	// AcceptEdits mode allows writes
	if ctx != nil && ctx.Mode == permission.ModeAcceptEdits {
		return permission.Decision{Behavior: permission.BehaviorAllow}
	}

	return permission.Decision{Behavior: permission.BehaviorPassthrough}
}

// MatchRule checks whether a permission rule's glob pattern matches the file path.
func (t *writeTool) MatchRule(ruleContent string, input map[string]any) bool {
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
	matched, _ := filepath.Match(ruleContent, abs)
	if matched {
		return true
	}
	matched, _ = filepath.Match(ruleContent, filepath.Base(abs))
	return matched
}

// GenerateSuggestions produces a permission rule for the parent directory.
func (t *writeTool) GenerateSuggestions(input map[string]any) []permission.Rule {
	path, _ := input["file_path"].(string)
	if path == "" {
		return []permission.Rule{{
			ToolName: t.ToolName,
			Behavior: permission.BehaviorAllow,
			Source:   "suggested",
		}}
	}

	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return []permission.Rule{{
			ToolName: t.ToolName,
			Behavior: permission.BehaviorAllow,
			Source:   "suggested",
		}}
	}

	parentDir := filepath.Dir(abs)
	return []permission.Rule{{
		ToolName:    t.ToolName,
		RuleContent: filepath.Join(parentDir, "**"),
		Behavior:    permission.BehaviorAllow,
		Source:      "suggested",
	}}
}

// WriteTool returns a tool that writes content to a file (complete overwrite).
func WriteTool() Tool {
	return &writeTool{
		BaseTool: BaseTool{
			ToolName:        "Write",
			ToolDescription: "Write content to a file, creating directories as needed. Completely overwrites existing content.",
			ToolSchema:      writeSchema,
		},
	}
}

// generateUnifiedDiff creates a simple unified diff between old and new content.
func generateUnifiedDiff(filePath, oldContent, newContent string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	if oldContent == newContent {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("--- a/%s\n", filepath.Base(filePath)))
	b.WriteString(fmt.Sprintf("+++ b/%s\n", filepath.Base(filePath)))

	// Simple diff: show removed and added lines
	// For a proper unified diff we'd need an LCS algorithm, but a simple
	// context-free diff is sufficient for metadata purposes.
	maxLines := len(oldLines)
	if len(newLines) > maxLines {
		maxLines = len(newLines)
	}

	// Limit output size
	const maxDiffLines = 100
	diffLines := 0

	b.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", 1, len(oldLines), 1, len(newLines)))

	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		if diffLines >= maxDiffLines {
			b.WriteString(fmt.Sprintf("... (%d more lines)\n", maxLines-diffLines))
			break
		}

		if i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j] {
			b.WriteString(" " + oldLines[i] + "\n")
			i++
			j++
		} else if i < len(oldLines) && (j >= len(newLines) || !containsAt(newLines, j, oldLines[i])) {
			b.WriteString("-" + oldLines[i] + "\n")
			i++
		} else {
			b.WriteString("+" + newLines[j] + "\n")
			j++
		}
		diffLines++
	}

	return b.String()
}

// containsAt checks if target appears at or after position start in lines.
// Used for simple diff heuristic to decide whether a line was removed vs added.
func containsAt(lines []string, start int, target string) bool {
	end := start + 5 // look ahead a few lines
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		if lines[i] == target {
			return true
		}
	}
	return false
}

// staleWrite reports whether replacing current with content would overwrite
// a version the agent has not seen (ReadCache.SetRejectStaleWrites). A write
// that leaves the content unchanged loses nothing.
func staleWrite(ctx context.Context, key string, current []byte, content string) bool {
	rc := GetReadCache(ctx)
	if rc == nil || !rc.RejectsStaleWrites() || string(current) == content {
		return false
	}
	return !rc.knowsVersion(key, current)
}
