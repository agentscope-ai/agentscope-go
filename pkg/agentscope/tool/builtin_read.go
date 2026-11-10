package tool

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

var readSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {
			"type": "string",
			"description": "Absolute or relative path to the file to read"
		},
		"offset": {
			"type": "integer",
			"description": "1-based line number to start reading from (default 1, i.e. the first line)"
		},
		"limit": {
			"type": "integer",
			"description": "Maximum number of lines to read (default 2000)"
		}
	},
	"required": ["file_path"]
}`)

const (
	defaultReadLimit    = 2000
	maxLineLengthChars  = 2000
	lineTruncatedSuffix = " [truncated]"
)

type readTool struct {
	BaseTool
}

func (t *readTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
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

	offset := 0
	if v, ok := args["offset"]; ok {
		// User provides 1-based offset; convert to 0-based internally.
		offset = toInt(v) - 1
	}
	if offset < 0 {
		offset = 0
	}

	limit := defaultReadLimit
	if v, ok := args["limit"]; ok {
		n := toInt(v)
		if n > 0 {
			limit = n
		}
	}

	// If a custom backend (Docker/E2B) is configured, read from it using the
	// caller-provided (workspace-relative) path.
	if b, ok := getBackendIfSet(ctx); ok {
		p := pathpkg.Clean(path)
		rc := GetReadCache(ctx)

		// Upstream #2092: a cached copy is only usable if freshness can be
		// judged by the backend's OWN filesystem — a host os.Stat of a
		// workspace-relative path is meaningless (or worse, matches an
		// unrelated host file). The stat is a container exec round-trip, so
		// it is paid only when there is an entry to validate, never just to
		// build a cache key.
		var rawLines []string
		// SetRejectStaleWrites: always read, so the version recorded is the
		// one this call returns.
		if rc != nil && !rc.RejectsStaleWrites() && rc.HasBeenRead(p) {
			if mt := backendMtime(ctx, b, p); mt != nil {
				if cached := rc.GetCacheWithMtime(p, mt); cached != nil {
					rawLines = cached.Lines
				}
			} else {
				// Entry exists but the backend can no longer vouch for its
				// freshness: drop it and re-read rather than serve possibly
				// stale content.
				rc.Remove(p)
			}
		}
		if rawLines != nil {
			return NewTextResponse(formatReadLines(rawLines, offset, limit)), nil
		}

		data, err := b.ReadFile(ctx, p)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		if int64(len(data)) > MaxFileSize {
			return NewErrorResponse(fmt.Errorf("file too large (%d bytes, max %d): %s", len(data), MaxFileSize, path)), nil
		}
		// Upstream #2114: image files come back as DataBlocks so multimodal
		// models can actually see them instead of receiving mojibake.
		if mt, isImage := imageMediaTypesByExt[strings.ToLower(pathpkg.Ext(p))]; isImage {
			if len(data) > MaxInlineImageBytes {
				return newOversizeImageResponse(path, pathpkg.Base(p), mt, len(data)), nil
			}
			return newImageDataResponse(pathpkg.Base(p), data, mt), nil
		}
		rawLines = splitReadLines(data)
		if rc != nil {
			// Cache only when the backend supplied a real mtime. Without one
			// there is no trustworthy freshness key, and falling back to a
			// host stat would key the entry on an unrelated file.
			if mt := backendMtime(ctx, b, p); mt != nil {
				rc.cacheFile(p, rawLines, mt, contentHash(data))
			} else if rc.RejectsStaleWrites() {
				// Without an mtime the copy is never served, but the version
				// still has to be known for the stale-write check.
				rc.cacheFile(p, rawLines, &time.Time{}, contentHash(data))
			}
		}
		return NewTextResponse(formatReadLines(rawLines, offset, limit)), nil
	}

	abs, err := resolvePath(ctx, path)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid path: %w", err)), nil
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResponse(fmt.Errorf("file not found: %s", path)), nil
		}
		return NewErrorResponse(fmt.Errorf("stat: %w", err)), nil
	}
	if info.IsDir() {
		return NewErrorResponse(fmt.Errorf("path is a directory: %s", path)), nil
	}
	if info.Size() > MaxFileSize {
		return NewErrorResponse(fmt.Errorf("file too large (%d bytes, max %d): %s", info.Size(), MaxFileSize, path)), nil
	}

	// Upstream #2114: image files come back as DataBlocks (host path).
	if mt, isImage := imageMediaTypesByExt[strings.ToLower(filepath.Ext(abs))]; isImage {
		if info.Size() > MaxInlineImageBytes {
			return newOversizeImageResponse(path, filepath.Base(abs), mt, int(info.Size())), nil
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			return NewErrorResponse(fmt.Errorf("read: %w", readErr)), nil
		}
		return newImageDataResponse(filepath.Base(abs), data, mt), nil
	}

	var rawLines []string
	rc := GetReadCache(ctx)

	if rc != nil && !rc.RejectsStaleWrites() {
		if cached := rc.GetCache(abs); cached != nil {
			rawLines = cached.Lines
		}
	}

	if rawLines == nil {
		f, err := os.Open(abs)
		if err != nil {
			return NewErrorResponse(fmt.Errorf("open: %w", err)), nil
		}
		defer f.Close() //nolint:errcheck

		// The hash of every byte scanned is the version this read returns.
		h := sha256.New()
		scanner := bufio.NewScanner(io.TeeReader(f, h))
		for scanner.Scan() {
			rawLines = append(rawLines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			return NewErrorResponse(fmt.Errorf("read: %w", err)), nil
		}

		if rc != nil {
			rc.cacheFile(abs, rawLines, nil, hex.EncodeToString(h.Sum(nil)))
		}
	}

	return NewTextResponse(formatReadLines(rawLines, offset, limit)), nil
}

// splitReadLines splits file bytes into lines the way the read tool expects,
// matching bufio.Scanner: a final trailing newline does not yield an extra empty
// line, and \r\n is normalized to \n.
func splitReadLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// formatReadLines renders numbered lines for [offset, offset+limit), truncating
// over-long lines to prevent output explosion.
func formatReadLines(rawLines []string, offset, limit int) string {
	end := offset + limit
	if end > len(rawLines) {
		end = len(rawLines)
	}
	var lines []string
	for i := offset; i < end; i++ {
		line := rawLines[i]
		if len(line) > maxLineLengthChars {
			line = line[:maxLineLengthChars] + lineTruncatedSuffix
		}
		lines = append(lines, fmt.Sprintf("%d\t%s", i+1, line))
	}
	return strings.Join(lines, "\n")
}

// CheckPermissions returns passthrough for read operations (reads are generally safe).
func (t *readTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorPassthrough}
}

// MatchRule checks whether a permission rule's glob pattern matches the file path.
func (t *readTool) MatchRule(ruleContent string, input map[string]any) bool {
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
	// Also try matching against just the base name
	matched, _ = filepath.Match(ruleContent, filepath.Base(abs))
	return matched
}

// ReadTool returns a tool that reads text files with optional line offset and limit.
func ReadTool() Tool {
	return &readTool{
		BaseTool: BaseTool{
			ToolName:        "Read",
			ToolDescription: "Read a file's contents with line numbers. Supports offset and limit for large files (max 1MB). Image files (png/jpg/jpeg/gif/webp/bmp/tiff/tif/ico, up to 256KB) are returned as an image the model can see.",
			ToolSchema:      readSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// imageMediaTypesByExt maps image file extensions to IANA media types
// (upstream #2114). PDF page rendering is intentionally NOT ported: it
// needs a rasterizer dependency, and Go's Read stays zero-dependency.
var imageMediaTypesByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".tiff": "image/tiff",
	".tif":  "image/tiff",
	".ico":  "image/x-icon",
}

// imagePlaceholder describes an image for consumers that cannot see pixels:
// text-only providers, logs, token counters, and the tool-result string the
// agent records. Without it the result would be an empty string and the model
// would conclude the file is empty.
func imagePlaceholder(name, mediaType string, size int) string {
	return fmt.Sprintf("[%s: %s, %d bytes]", name, mediaType, size)
}

// newImageDataResponse wraps raw image bytes in a base64 DataBlock result.
//
// The response always carries a leading TextBlock placeholder as well, so a
// pipeline that only understands text (or a model without image input) reports
// "this is an image of N bytes" instead of an empty tool result.
func newImageDataResponse(name string, data []byte, mediaType string) *ToolResponse {
	var idBuf [8]byte
	if _, err := rand.Read(idBuf[:]); err != nil {
		// crypto/rand never fails in practice; fall back to a name-based ID.
		copy(idBuf[:], name)
	}
	return &ToolResponse{
		Content: []message.ContentBlock{
			message.TextBlock{
				Type: "text",
				Text: imagePlaceholder(name, mediaType, len(data)),
			},
			message.DataBlock{
				Type: "data",
				ID:   "img_" + hex.EncodeToString(idBuf[:]),
				Name: name,
				Source: message.Base64Source{
					Type:      "base64",
					Data:      base64.StdEncoding.EncodeToString(data),
					MediaType: mediaType,
				},
			},
		},
		State: message.ToolResultSuccess,
	}
}

// newOversizeImageResponse reports an image that is too large to inline as a
// text-only result. A 1 MB PNG becomes ~1.37 MB of base64, which the token
// estimator reads as roughly 250k tokens: one screenshot would blow the
// context window and immediately trigger compression.
func newOversizeImageResponse(path, name, mediaType string, size int) *ToolResponse {
	return NewTextResponse(fmt.Sprintf(
		"%s (%s, %d bytes) is an image too large to inline (limit %d bytes). "+
			"It was not loaded into the context.",
		path, mediaType, size, MaxInlineImageBytes))
}
