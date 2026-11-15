package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var webFetchSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"url": {
			"type": "string",
			"description": "The URL to fetch content from"
		},
		"timeout": {
			"type": "number",
			"description": "Timeout in seconds (default 30, max 120)"
		}
	},
	"required": ["url"]
}`)

type webFetchTool struct {
	BaseTool
	client *http.Client
}

const (
	defaultFetchTimeout = 30 * time.Second
	maxFetchTimeout     = 120 * time.Second
	maxResponseBytes    = 5 * 1024 * 1024 // 5MB
)

func (t *webFetchTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	urlStr, _ := args["url"].(string)
	if urlStr == "" {
		return NewErrorResponse(fmt.Errorf("url is required")), nil
	}

	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = "https://" + urlStr
	}

	// Upgrade http to https
	if strings.HasPrefix(urlStr, "http://") {
		urlStr = "https://" + urlStr[7:]
	}

	timeout := defaultFetchTimeout
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
		if timeout > maxFetchTimeout {
			timeout = maxFetchTimeout
		}
	}

	client := t.client
	if client == nil {
		// Default client blocks SSRF targets (loopback/private/link-local, incl.
		// the cloud-metadata endpoint) at dial time, across redirects.
		client = newSSRFSafeClient(timeout)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("create request: %w", err)), nil
	}
	req.Header.Set("User-Agent", "agentscope-go/2.0 (WebFetch tool)")
	req.Header.Set("Accept", "text/html, text/plain, application/json, */*")

	resp, err := client.Do(req)
	if err != nil {
		return NewErrorResponse(fmt.Errorf("fetch: %w", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return NewErrorResponse(fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)), nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return NewErrorResponse(fmt.Errorf("read body: %w", err)), nil
	}

	contentType := resp.Header.Get("Content-Type")
	content := string(body)

	if strings.Contains(contentType, "text/html") {
		content = htmlToText(content)
	}

	result := map[string]any{
		"url":          urlStr,
		"status":       resp.StatusCode,
		"content_type": contentType,
		"content":      content,
	}
	b, _ := json.Marshal(result)
	return NewTextResponse(string(b)), nil
}

var (
	htmlTagRe      = regexp.MustCompile(`<[^>]*>`)
	htmlScriptRe   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	htmlStyleRe    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	htmlNoscriptRe = regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`)
	htmlEntityRe   = regexp.MustCompile(`&[a-zA-Z]+;|&#[0-9]+;`)
	htmlSpaceRe    = regexp.MustCompile(`[ \t]+`)
	htmlNewline    = regexp.MustCompile(`\n{3,}`)
)

func htmlToText(html string) string {
	text := htmlScriptRe.ReplaceAllString(html, "")
	text = htmlStyleRe.ReplaceAllString(text, "")
	text = htmlNoscriptRe.ReplaceAllString(text, "")
	// Replace br and p/div tags with newlines
	text = strings.NewReplacer(
		"<br>", "\n", "<br/>", "\n", "<br />", "\n",
		"</p>", "\n\n", "</div>", "\n", "</li>", "\n",
		"</h1>", "\n\n", "</h2>", "\n\n", "</h3>", "\n\n",
		"</h4>", "\n", "</h5>", "\n", "</h6>", "\n",
		"</tr>", "\n",
	).Replace(text)
	// Strip remaining tags
	text = htmlTagRe.ReplaceAllString(text, "")
	// Decode common entities
	text = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'",
		"&nbsp;", " ",
	).Replace(text)
	text = htmlEntityRe.ReplaceAllString(text, "")
	// Normalize whitespace
	text = htmlSpaceRe.ReplaceAllString(text, " ")
	text = htmlNewline.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// WebFetchTool returns a tool that fetches content from a URL and converts
// HTML to plain text.
func WebFetchTool() Tool {
	return &webFetchTool{
		BaseTool: BaseTool{
			ToolName:        "WebFetch",
			ToolDescription: "Fetch content from a URL. HTML is automatically converted to plain text.",
			ToolSchema:      webFetchSchema,
		},
	}
}

var _ Tool = (*webFetchTool)(nil)
