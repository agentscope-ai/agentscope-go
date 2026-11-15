package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var grepSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "Search pattern (regex supported)"
		},
		"path": {
			"type": "string",
			"description": "File or directory to search in (default: current directory)"
		},
		"include": {
			"type": "string",
			"description": "File glob filter (e.g. '*.go', '*.ts')"
		},
		"glob": {
			"type": "string",
			"description": "File glob filter (alternative to include)"
		},
		"type": {
			"type": "string",
			"description": "File type filter (e.g. 'go', 'py', 'js'). Maps to ripgrep --type."
		},
		"output_mode": {
			"type": "string",
			"description": "Output mode: 'content' (default, show matching lines), 'files_with_matches' (only file names), 'count' (match count per file)"
		},
		"context_lines": {
			"type": "integer",
			"description": "Number of context lines to show before and after each match (default 0)"
		},
		"before_context": {
			"type": "integer",
			"description": "Number of lines to show before each match (-B flag, default 0)"
		},
		"after_context": {
			"type": "integer",
			"description": "Number of lines to show after each match (-A flag, default 0)"
		},
		"combined_context": {
			"type": "integer",
			"description": "Number of lines to show around each match (-C flag, default 0). Overrides before_context/after_context."
		},
		"max_count": {
			"type": "integer",
			"description": "Maximum number of matches per file (-m flag)"
		},
		"case_sensitive": {
			"type": "boolean",
			"description": "If false, search case-insensitively (default true)"
		},
		"word_regexp": {
			"type": "boolean",
			"description": "If true, match only whole words (-w flag)"
		},
		"fixed_strings": {
			"type": "boolean",
			"description": "If true, treat pattern as a literal string, not a regex (-F flag)"
		},
		"page": {
			"type": "integer",
			"description": "Page number for paginated results (1-based, default 1)"
		},
		"page_size": {
			"type": "integer",
			"description": "Number of results per page (default 200)"
		}
	},
	"required": ["pattern"]
}`)

const defaultGrepPageSize = 200

type grepTool struct {
	BaseTool
}

// grepOptions holds parsed grep parameters.
type grepOptions struct {
	pattern         string
	searchPath      string
	include         string
	glob            string
	fileType        string
	outputMode      string // "content", "files_with_matches", "count"
	contextLines    int
	beforeContext   int
	afterContext    int
	combinedContext int
	maxCount        int
	caseSensitive   bool
	wordRegexp      bool
	fixedStrings    bool
	page            int
	pageSize        int
}

func parseGrepOptions(args map[string]any) (*grepOptions, error) {
	raw, ok := args["pattern"]
	if !ok {
		return nil, fmt.Errorf("pattern is required")
	}
	pattern, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("pattern must be a string")
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("pattern cannot be empty")
	}

	searchPath := "."
	if v, ok := args["path"].(string); ok && v != "" {
		searchPath = v
	}
	searchPath, err := filepath.Abs(searchPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	include, _ := args["include"].(string)

	globFilter, _ := args["glob"].(string)
	if include == "" && globFilter != "" {
		include = globFilter
	}

	fileType, _ := args["type"].(string)

	outputMode := "content"
	if v, ok := args["output_mode"].(string); ok && v != "" {
		switch v {
		case "content", "files_with_matches", "count":
			outputMode = v
		default:
			return nil, fmt.Errorf("invalid output_mode %q: must be content, files_with_matches, or count", v)
		}
	}

	contextLines := 0
	if v, ok := args["context_lines"]; ok {
		contextLines = toInt(v)
		if contextLines < 0 {
			contextLines = 0
		}
		if contextLines > 10 {
			contextLines = 10
		}
	}

	beforeContext := 0
	if v, ok := args["before_context"]; ok {
		beforeContext = toInt(v)
		if beforeContext < 0 {
			beforeContext = 0
		}
		if beforeContext > 10 {
			beforeContext = 10
		}
	}

	afterContext := 0
	if v, ok := args["after_context"]; ok {
		afterContext = toInt(v)
		if afterContext < 0 {
			afterContext = 0
		}
		if afterContext > 10 {
			afterContext = 10
		}
	}

	combinedContext := 0
	if v, ok := args["combined_context"]; ok {
		combinedContext = toInt(v)
		if combinedContext < 0 {
			combinedContext = 0
		}
		if combinedContext > 10 {
			combinedContext = 10
		}
	}

	maxCount := 0
	if v, ok := args["max_count"]; ok {
		maxCount = toInt(v)
		if maxCount < 0 {
			maxCount = 0
		}
	}

	caseSensitive := true
	if v, ok := args["case_sensitive"]; ok {
		if b, ok := v.(bool); ok {
			caseSensitive = b
		}
	}

	wordRegexp := false
	if v, ok := args["word_regexp"]; ok {
		if b, ok := v.(bool); ok {
			wordRegexp = b
		}
	}

	fixedStrings := false
	if v, ok := args["fixed_strings"]; ok {
		if b, ok := v.(bool); ok {
			fixedStrings = b
		}
	}

	page := 1
	if v, ok := args["page"]; ok {
		page = toInt(v)
		if page < 1 {
			page = 1
		}
	}

	pageSize := defaultGrepPageSize
	if v, ok := args["page_size"]; ok {
		n := toInt(v)
		if n > 0 {
			pageSize = n
		}
		if pageSize > 1000 {
			pageSize = 1000
		}
	}

	return &grepOptions{
		pattern:         pattern,
		searchPath:      searchPath,
		include:         include,
		glob:            globFilter,
		fileType:        fileType,
		outputMode:      outputMode,
		contextLines:    contextLines,
		beforeContext:   beforeContext,
		afterContext:    afterContext,
		combinedContext: combinedContext,
		maxCount:        maxCount,
		caseSensitive:   caseSensitive,
		wordRegexp:      wordRegexp,
		fixedStrings:    fixedStrings,
		page:            page,
		pageSize:        pageSize,
	}, nil
}

func (t *grepTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	opts, err := parseGrepOptions(args)
	if err != nil {
		return NewErrorResponse(err), nil
	}

	// Try ripgrep first (much faster for large codebases)
	if result, err := t.tryRipgrep(ctx, opts); err == nil {
		return result, nil
	}

	// Fallback to Go implementation
	return t.goGrep(opts)
}

func (t *grepTool) tryRipgrep(ctx context.Context, opts *grepOptions) (*ToolResponse, error) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		return nil, err
	}

	args := []string{
		"--no-heading",
		"--sort", "path",
		"--max-filesize", "1M",
	}

	// Case sensitivity
	if !opts.caseSensitive {
		args = append(args, "--ignore-case")
	}

	// Word regexp
	if opts.wordRegexp {
		args = append(args, "--word-regexp")
	}

	// Fixed strings
	if opts.fixedStrings {
		args = append(args, "--fixed-strings")
	}

	switch opts.outputMode {
	case "files_with_matches":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count")
	default:
		args = append(args, "--line-number")
		// Context lines: combined_context (-C) takes priority, then before/after, then context_lines
		if opts.combinedContext > 0 {
			args = append(args, fmt.Sprintf("-C%d", opts.combinedContext))
		} else if opts.beforeContext > 0 || opts.afterContext > 0 {
			if opts.beforeContext > 0 {
				args = append(args, fmt.Sprintf("-B%d", opts.beforeContext))
			}
			if opts.afterContext > 0 {
				args = append(args, fmt.Sprintf("-A%d", opts.afterContext))
			}
		} else if opts.contextLines > 0 {
			args = append(args, fmt.Sprintf("-C%d", opts.contextLines))
		}
		// Per-file max count
		if opts.maxCount > 0 {
			args = append(args, "--max-count", fmt.Sprintf("%d", opts.maxCount))
		} else {
			args = append(args, "--max-count", "5")
		}
	}

	if opts.include != "" {
		args = append(args, "--glob", opts.include)
	}

	// File type filter
	if opts.fileType != "" {
		args = append(args, "--type", opts.fileType)
	}

	args = append(args, opts.pattern, opts.searchPath)

	cmd := exec.CommandContext(ctx, rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return NewTextResponse("No matches found."), nil
		}
		return nil, fmt.Errorf("rg: %w", err)
	}

	result := string(out)
	lines := strings.Split(strings.TrimSpace(result), "\n")

	return t.paginateResults(lines, opts), nil
}

func (t *grepTool) goGrep(opts *grepOptions) (*ToolResponse, error) {
	patternStr := opts.pattern

	// Fixed strings: escape regex metacharacters
	if opts.fixedStrings {
		patternStr = regexp.QuoteMeta(patternStr)
	}

	// Word regexp: wrap in word boundary anchors
	if opts.wordRegexp {
		patternStr = `\b` + patternStr + `\b`
	}

	// Case sensitivity
	if !opts.caseSensitive {
		patternStr = "(?i)" + patternStr
	}

	re, err := regexp.Compile(patternStr)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("invalid regex: %w", err)), nil
	}

	var includeRe *regexp.Regexp
	if opts.include != "" {
		globRe := globToRegex(opts.include)
		includeRe, _ = regexp.Compile(globRe)
	}

	// Resolve effective context lines
	beforeCtx, afterCtx := opts.beforeContext, opts.afterContext
	if opts.combinedContext > 0 {
		beforeCtx = opts.combinedContext
		afterCtx = opts.combinedContext
	} else if beforeCtx == 0 && afterCtx == 0 && opts.contextLines > 0 {
		beforeCtx = opts.contextLines
		afterCtx = opts.contextLines
	}

	maxPerFile := opts.maxCount
	if maxPerFile <= 0 {
		maxPerFile = 5
	}

	// Collect more results than needed for pagination
	maxCollect := opts.page * opts.pageSize

	var results []string
	switch opts.outputMode {
	case "files_with_matches":
		results = t.goGrepFiles(re, includeRe, opts.searchPath, maxCollect)
	case "count":
		results = t.goGrepCount(re, includeRe, opts.searchPath, maxCollect)
	default:
		results = t.goGrepContent(re, includeRe, opts.searchPath, beforeCtx, afterCtx, maxPerFile, maxCollect)
	}

	if len(results) == 0 {
		return NewTextResponse("No matches found."), nil
	}

	return t.paginateResults(results, opts), nil
}

func (t *grepTool) goGrepContent(re *regexp.Regexp, includeRe *regexp.Regexp, searchPath string, beforeCtx, afterCtx, maxPerFile, maxResults int) []string {
	var results []string
	_ = filepath.Walk(searchPath, func(fpath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || len(results) >= maxResults {
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
			return nil
		}
		if includeRe != nil && !includeRe.MatchString(info.Name()) {
			return nil
		}
		if info.Size() > MaxFileSize {
			return nil
		}

		f, err := os.Open(fpath)
		if err != nil {
			return nil
		}
		defer f.Close() //nolint:errcheck

		scanner := bufio.NewScanner(f)
		var allLines []string
		for scanner.Scan() {
			allLines = append(allLines, scanner.Text())
		}

		matchesInFile := 0
		for lineNo, line := range allLines {
			if matchesInFile >= maxPerFile || len(results) >= maxResults {
				break
			}
			if re.MatchString(line) {
				if beforeCtx > 0 || afterCtx > 0 {
					// Add context lines
					start := lineNo - beforeCtx
					if start < 0 {
						start = 0
					}
					end := lineNo + afterCtx + 1
					if end > len(allLines) {
						end = len(allLines)
					}
					for i := start; i < end; i++ {
						prefix := "-"
						if i == lineNo {
							prefix = ":"
						}
						results = append(results, fmt.Sprintf("%s%s%d%s%s", fpath, prefix, i+1, prefix, allLines[i]))
					}
					if end < len(allLines) {
						results = append(results, "--")
					}
				} else {
					results = append(results, fmt.Sprintf("%s:%d:%s", fpath, lineNo+1, line))
				}
				matchesInFile++
			}
		}
		return nil
	})
	return results
}

func (t *grepTool) goGrepFiles(re *regexp.Regexp, includeRe *regexp.Regexp, searchPath string, maxResults int) []string {
	var results []string
	_ = filepath.Walk(searchPath, func(fpath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || len(results) >= maxResults {
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
			return nil
		}
		if includeRe != nil && !includeRe.MatchString(info.Name()) {
			return nil
		}
		if info.Size() > MaxFileSize {
			return nil
		}

		f, err := os.Open(fpath)
		if err != nil {
			return nil
		}
		defer f.Close() //nolint:errcheck

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if re.MatchString(scanner.Text()) {
				results = append(results, fpath)
				return nil
			}
		}
		return nil
	})
	return results
}

func (t *grepTool) goGrepCount(re *regexp.Regexp, includeRe *regexp.Regexp, searchPath string, maxResults int) []string {
	var results []string
	_ = filepath.Walk(searchPath, func(fpath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || len(results) >= maxResults {
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
			return nil
		}
		if includeRe != nil && !includeRe.MatchString(info.Name()) {
			return nil
		}
		if info.Size() > MaxFileSize {
			return nil
		}

		f, err := os.Open(fpath)
		if err != nil {
			return nil
		}
		defer f.Close() //nolint:errcheck

		count := 0
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if re.MatchString(scanner.Text()) {
				count++
			}
		}
		if count > 0 {
			results = append(results, fmt.Sprintf("%s:%d", fpath, count))
		}
		return nil
	})
	return results
}

func (t *grepTool) paginateResults(lines []string, opts *grepOptions) *ToolResponse {
	totalResults := len(lines)
	start := (opts.page - 1) * opts.pageSize
	if start >= totalResults {
		return NewTextResponse(fmt.Sprintf("No results on page %d. Total results: %d", opts.page, totalResults))
	}
	end := start + opts.pageSize
	if end > totalResults {
		end = totalResults
	}

	pageLines := lines[start:end]
	output := strings.Join(pageLines, "\n")

	if totalResults > end {
		output += fmt.Sprintf("\n... (showing %d-%d of %d results, use page=%d for more)", start+1, end, totalResults, opts.page+1)
	}

	return NewTextResponse(strings.TrimSpace(output))
}

func globToRegex(glob string) string {
	var b strings.Builder
	b.WriteString("(?i)")
	for _, c := range glob {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.':
			b.WriteString("\\.")
		default:
			b.WriteRune(c)
		}
	}
	b.WriteString("$")
	return b.String()
}

// GrepTool returns a tool that searches file contents using regex patterns.
// Prefers ripgrep (rg) if available, falls back to Go standard library.
func GrepTool() Tool {
	return &grepTool{
		BaseTool: BaseTool{
			ToolName:        "Grep",
			ToolDescription: "Search file contents for a regex pattern. Uses ripgrep if available. Returns matching lines with file path and line number.",
			ToolSchema:      grepSchema,
			ReadOnly:        true,
			ConcurrencySafe: true,
		},
	}
}
