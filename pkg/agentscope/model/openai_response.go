package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OpenAIResponseConfig configures the OpenAI Responses API model.
type OpenAIResponseConfig struct {
	APIKey          string
	SecretAPIKey    SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	Model           string
	BaseURL         string // default: https://api.openai.com
	MaxOutputTokens int
	ReasoningEffort string // "low", "medium", "high"
	HTTPClient      *http.Client
	ClientOptions   *ClientOptions
}

type openaiResponseModel struct {
	cfg            OpenAIResponseConfig
	httpClient     *http.Client
	defaultHeaders map[string]string
}

// NewOpenAIResponseModel creates a ChatModel that uses the OpenAI Responses API.
func NewOpenAIResponseModel(cfg *OpenAIResponseConfig) (ChatModel, error) {
	cfg.APIKey = ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openai response: api key is required")
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4.1"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com"
	}
	var defHeaders map[string]string
	opts := cfg.ClientOptions
	if opts == nil {
		opts = &ClientOptions{Timeout: 5 * time.Minute}
	} else if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &openaiResponseModel{
		cfg:            *cfg,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, opts),
		defaultHeaders: defHeaders,
	}, nil
}

func (m *openaiResponseModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	o := CallOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	body := m.buildRequestBody(msgs, &o, false)

	respBody, err := m.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	defer respBody.Close()

	var raw map[string]any
	if err := json.NewDecoder(respBody).Decode(&raw); err != nil {
		return nil, fmt.Errorf("openai response: decode failed: %w", err)
	}

	return m.parseResponse(raw)
}

func (m *openaiResponseModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	o := CallOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	body := m.buildRequestBody(msgs, &o, true)

	respBody, err := m.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	ch := make(chan ChatResponse, 32)
	go m.processStream(ctx, respBody, ch)
	return ch, nil
}

func (m *openaiResponseModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

func (m *openaiResponseModel) ContextSize() int  { return 200000 }
func (m *openaiResponseModel) ModelName() string { return m.cfg.Model }

func (m *openaiResponseModel) buildRequestBody(msgs []*message.Msg, opts *CallOptions, stream bool) map[string]any {
	body := map[string]any{
		"model":  m.cfg.Model,
		"stream": stream,
	}

	// Convert messages to input items
	var input []map[string]any
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if msg.Role == message.RoleSystem {
			if txt := msg.GetTextContent("\n"); txt != nil {
				body["instructions"] = *txt
			}
			continue
		}
		input = append(input, m.formatInputItems(msg)...)
	}
	body["input"] = input

	if m.cfg.MaxOutputTokens > 0 {
		body["max_output_tokens"] = m.cfg.MaxOutputTokens
	}
	if opts.MaxTokens != nil {
		body["max_output_tokens"] = *opts.MaxTokens
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if m.cfg.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": m.cfg.ReasoningEffort}
	}

	if len(opts.Tools) > 0 {
		var tools []map[string]any
		for _, t := range opts.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  json.RawMessage(t.Function.Parameters),
			})
		}
		body["tools"] = tools
	}
	if opts.ToolChoice != nil {
		body["tool_choice"] = opts.ToolChoice.Mode
	}

	return body
}

// formatInputItems converts one Msg into zero or more Responses API input
// items. Unlike a one-item-per-message mapping, this preserves multi-block
// turns faithfully (upstream #2426 and the #2389 family):
//   - a ThinkingBlock carrying Extra["responses_item"] replays the original
//     reasoning item (including encrypted_content) so stateless reasoning
//     continues across turns;
//   - every ToolCallBlock becomes its own function_call item (previously
//     only the first survived);
//   - every ToolResultBlock becomes its own function_call_output item;
//   - accumulated text is flushed as a message item before the tool calls
//     that follow it, matching the API's segment ordering.
func (m *openaiResponseModel) formatInputItems(msg *message.Msg) []map[string]any {
	role := string(msg.Role)

	blocks := msg.GetContentBlocks()
	if len(blocks) == 0 {
		return []map[string]any{{"role": role, "content": ""}}
	}

	var items []map[string]any
	var content []map[string]any

	flushContent := func() {
		if len(content) == 0 {
			return
		}
		if len(content) == 1 {
			if text, ok := content[0]["text"].(string); ok && content[0]["type"] == "input_text" {
				items = append(items, map[string]any{"role": role, "content": text})
			} else {
				items = append(items, map[string]any{"role": role, "content": content})
			}
		} else {
			items = append(items, map[string]any{"role": role, "content": content})
		}
		content = nil
	}

	for _, b := range blocks {
		switch blk := b.(type) {
		case message.ThinkingBlock:
			raw, ok := blk.Extra["responses_item"].(map[string]any)
			if !ok || len(raw) == 0 {
				// A plain thinking block cannot be replayed, and an empty
				// map would serialize to "{}" and get the whole request
				// rejected by the API.
				continue
			}
			flushContent() // reasoning starts a new output segment
			items = append(items, stripJSONNullsMap(raw))
		case message.TextBlock:
			content = append(content, map[string]any{
				"type": "input_text",
				"text": blk.Text,
			})
		case message.DataBlock:
			if part := responsesInputImage(blk); part != nil {
				content = append(content, part)
			}
		case message.HintBlock:
			// Hints are user input, including when attached to an assistant
			// turn. Keep their position between the surrounding segments.
			flushContent()
			switch hint := blk.Hint.(type) {
			case string:
				items = append(items, map[string]any{"role": "user", "content": hint})
			case []message.ContentBlock:
				var parts []map[string]any
				for _, sub := range hint {
					switch h := sub.(type) {
					case message.TextBlock:
						parts = append(parts, map[string]any{"type": "input_text", "text": h.Text})
					case message.DataBlock:
						if part := responsesInputImage(h); part != nil {
							parts = append(parts, part)
						}
					}
				}
				if len(parts) > 0 {
					items = append(items, map[string]any{"role": "user", "content": parts})
				}
			}
		case message.ToolCallBlock:
			flushContent()
			items = append(items, map[string]any{
				"type":      "function_call",
				"id":        blk.ID,
				"call_id":   blk.ID,
				"name":      blk.Name,
				"arguments": blk.Input,
			})
		case message.ToolResultBlock:
			flushContent()
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": blk.ID,
				"output":  formatToolResultOutput(&blk),
			})
		}
	}
	flushContent()
	if len(items) == 0 {
		// Every block was of a type the Responses API cannot carry here
		// (audio, unsupported sources, ...).
		// Emitting nothing would silently drop the turn from the
		// conversation; the old one-item-per-message mapping produced
		// {"role":...,"content":null} and the API rejected it loudly. Keep
		// the turn visible with an explicit note instead of losing it.
		return []map[string]any{{
			"role":    role,
			"content": "[content omitted: this turn carried only block types the Responses API cannot replay]",
		}}
	}
	return items
}

// responsesInputImage preserves image sources without fetching or decoding them.
// Other media keep the adapter's existing omission policy.
func responsesInputImage(block message.DataBlock) map[string]any {
	if !strings.HasPrefix(block.GetMediaType(), "image/") {
		return nil
	}
	var url string
	switch src := block.Source.(type) {
	case message.URLSource:
		url = src.URL
	case message.Base64Source:
		url = "data:" + src.MediaType + ";base64," + src.Data
	default:
		return nil
	}
	return map[string]any{"type": "input_image", "image_url": url}
}

// reasoningItemSummaryText extracts the concatenated summary_text of a raw
// Responses API reasoning item.
func reasoningItemSummaryText(item map[string]any) string {
	summary, ok := item["summary"].([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, part := range summary {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := pm["text"].(string); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// buildStreamContent assembles the final content blocks of a streamed
// Responses reply.
//
// Order matters and must match parseResponse (the non-streaming path), which
// walks the response's output array in order: reasoning items, then text, then
// tool calls. formatInputItems replays history in the order the blocks appear,
// so emitting text first would replay as message(text) -> reasoning: a
// reasoning item with nothing after it. That is exactly the shape upstream
// #2426 exists to prevent, and it broke encrypted-reasoning replay because the
// agent's default path is streaming.
//
// toolCallOrder keys toolCalls so multiple calls in one reply keep the order
// the API produced them in; ranging the map directly is non-deterministic.
func buildStreamContent(
	accText string,
	accThinking string,
	reasoningItems []map[string]any,
	toolCalls map[string]*message.ToolCallBlock,
	toolCallOrder []string,
) []message.ContentBlock {
	var content []message.ContentBlock

	if len(reasoningItems) > 0 {
		for _, item := range reasoningItems {
			tb := message.ThinkingBlock{
				Type:     "thinking",
				Thinking: reasoningItemSummaryText(item),
				Extra:    map[string]any{"responses_item": item},
			}
			if id, ok := item["id"].(string); ok {
				tb.ID = id
			}
			content = append(content, tb)
		}
	} else if accThinking != "" {
		content = append(content, message.ThinkingBlock{Type: "thinking", Thinking: accThinking})
	}

	if accText != "" {
		content = append(content, message.TextBlock{Type: "text", Text: accText})
	}

	for _, id := range toolCallOrder {
		if tc, ok := toolCalls[id]; ok {
			content = append(content, *tc)
		}
	}
	return content
}

// stripJSONNullsMap is stripJSONNulls for a known map root.
func stripJSONNullsMap(m map[string]any) map[string]any {
	out, _ := stripJSONNulls(m).(map[string]any)
	return out
}

// stripJSONNulls recursively removes nil-valued map entries so replayed
// items do not carry explicit nulls (upstream #2426: exclude_none — some
// Responses-compatible APIs reject null fields on input items).
func stripJSONNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if val == nil {
				continue
			}
			out[k] = stripJSONNulls(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, stripJSONNulls(item))
		}
		return out
	default:
		return v
	}
}

func (m *openaiResponseModel) doRequest(ctx context.Context, body map[string]any) (io.ReadCloser, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai response: marshal request: %w", err)
	}

	url := strings.TrimRight(m.cfg.BaseURL, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	for k, v := range mergeHeaders(map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + m.cfg.APIKey,
	}, m.defaultHeaders) {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai response: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai response: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

func (m *openaiResponseModel) parseResponse(raw map[string]any) (*ChatResponse, error) {
	resp := &ChatResponse{IsLast: true, ModelName: m.cfg.Model}

	if id, ok := raw["id"].(string); ok {
		resp.ID = id
	}

	if output, ok := raw["output"].([]any); ok {
		for _, item := range output {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch obj["type"] {
			case "message":
				if content, ok := obj["content"].([]any); ok {
					for _, c := range content {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						if cm["type"] == "output_text" {
							if text, ok := cm["text"].(string); ok {
								resp.Content = append(resp.Content, message.TextBlock{Type: "text", Text: text})
							}
						}
					}
				}
			case "reasoning":
				// Upstream #2426: preserve the whole reasoning item (including
				// encrypted_content) so multi-turn history replay can hand it
				// back to the Responses API; summary text becomes the visible
				// thinking content.
				tb := message.ThinkingBlock{
					Type:     "thinking",
					Thinking: reasoningItemSummaryText(obj),
					Extra:    map[string]any{"responses_item": stripJSONNullsMap(obj)},
				}
				if id, ok := obj["id"].(string); ok {
					tb.ID = id
				}
				resp.Content = append(resp.Content, tb)
			case "function_call":
				name, _ := obj["name"].(string)
				args, _ := obj["arguments"].(string)
				id, _ := obj["id"].(string)
				resp.Content = append(resp.Content, message.ToolCallBlock{
					Type:  "tool_call",
					ID:    id,
					Name:  name,
					Input: args,
				})
			}
		}
	}

	if usage, ok := raw["usage"].(map[string]any); ok {
		resp.Usage = &ChatUsage{}
		if v, ok := usage["input_tokens"].(float64); ok {
			resp.Usage.InputTokens = int(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			resp.Usage.OutputTokens = int(v)
		}
	}
	// A response cut at max_output_tokens (or one that failed) still
	// carries its partial output; keep it, but report the status so callers
	// can tell it apart from a complete answer.
	resp.Error = responseStatusError(raw, "")

	return resp, nil
}

// responseStatusError reports a Responses API response whose status is not
// "completed", naming the status and, when present, the incomplete_details
// reason or the error code and message. A response without a status is
// accepted as before. eventStatus is the status a streamed terminal event
// names ("incomplete" for response.incomplete, "failed" for
// response.failed); it applies when the object omits its status or reports
// "completed", so a failure event never reads as a complete answer.
// Buffered responses pass "".
func responseStatusError(obj map[string]any, eventStatus string) error {
	status, _ := obj["status"].(string)
	if status == "" || status == "completed" {
		status = eventStatus
	}
	if status == "" || status == "completed" {
		return nil
	}
	var reason string
	if details, ok := obj["incomplete_details"].(map[string]any); ok {
		reason, _ = details["reason"].(string)
	}
	if e, ok := obj["error"].(map[string]any); ok && reason == "" {
		// Keep the code: the agent's context-overflow recovery matches
		// context_length_exceeded, which the message does not contain.
		code, _ := e["code"].(string)
		msg, _ := e["message"].(string)
		switch {
		case code != "" && msg != "":
			reason = code + ": " + msg
		case code != "":
			reason = code
		default:
			reason = msg
		}
	}
	if reason != "" {
		return fmt.Errorf("openai response: response %s: %s", status, reason)
	}
	return fmt.Errorf("openai response: response %s", status)
}

// processStream consumes the Responses SSE stream. Contract: exactly one
// IsLast response is emitted on every path EXCEPT ctx cancellation
// (abandoned consumer), where the channel simply closes — consumers looping
// until IsLast must also handle channel closure (upstream #2349).
func (m *openaiResponseModel) processStream(ctx context.Context, body io.ReadCloser, ch chan<- ChatResponse) {
	defer close(ch)
	defer body.Close()

	// Upstream #2349 class: every send is ctx-selected so an abandoned
	// stream can never wedge the producer goroutine on a full buffer.
	send := func(resp ChatResponse) bool {
		select {
		case ch <- resp:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var accText string
	var accThinking string
	var completedFinal bool
	var reasoningItems []map[string]any
	toolCalls := make(map[string]*message.ToolCallBlock)
	// Insertion order of toolCalls: ranging a map is random, and a reply with
	// two tool calls must replay them in the order the model produced them.
	var toolCallOrder []string

scanLoop:
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			accText += delta
			if !send(ChatResponse{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: delta}},
			}) {
				return
			}

		case "response.reasoning_summary_text.delta":
			delta, _ := event["delta"].(string)
			accThinking += delta
			if !send(ChatResponse{
				Content: []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Thinking: delta}},
			}) {
				return
			}

		case "response.output_item.done":
			// Upstream #2426: a completed reasoning item carries the
			// encrypted_content needed to replay reasoning across turns.
			if item, ok := event["item"].(map[string]any); ok {
				if itemType, _ := item["type"].(string); itemType == "reasoning" {
					reasoningItems = append(reasoningItems, stripJSONNullsMap(item))
				}
			}

		case "response.output_item.added":
			if item, ok := event["item"].(map[string]any); ok {
				if itemType, _ := item["type"].(string); itemType == "function_call" {
					id, _ := item["id"].(string)
					name, _ := item["name"].(string)
					if _, seen := toolCalls[id]; !seen {
						toolCallOrder = append(toolCallOrder, id)
					}
					toolCalls[id] = &message.ToolCallBlock{
						Type: "tool_call", ID: id, Name: name,
					}
				}
			}

		case "response.function_call_arguments.delta":
			itemID, _ := event["item_id"].(string)
			delta, _ := event["delta"].(string)
			if tc, ok := toolCalls[itemID]; ok {
				tc.Input += delta
			}

		case "response.completed", "response.incomplete", "response.failed":
			// Terminal events. The latter two (and a completed event whose
			// response reports another status) keep the accumulated content
			// and usage and report the status on Error.
			resp := &ChatResponse{IsLast: true, ModelName: m.cfg.Model}
			resp.Content = buildStreamContent(accText, accThinking, reasoningItems, toolCalls, toolCallOrder)

			respObj, ok := event["response"].(map[string]any)
			if ok {
				if id, ok := respObj["id"].(string); ok {
					resp.ID = id
				}
				if usage, ok := respObj["usage"].(map[string]any); ok {
					resp.Usage = &ChatUsage{}
					if v, ok := usage["input_tokens"].(float64); ok {
						resp.Usage.InputTokens = int(v)
					}
					if v, ok := usage["output_tokens"].(float64); ok {
						resp.Usage.OutputTokens = int(v)
					}
				}
			}
			resp.Error = responseStatusError(respObj, strings.TrimPrefix(eventType, "response."))

			if !send(*resp) {
				return
			}
			completedFinal = true
			break scanLoop
		}
	}

	if ctx.Err() != nil || completedFinal {
		return
	}

	// Upstream #2349: never end silently. A scan error or a stream that
	// terminates without a terminal event (response.completed,
	// response.incomplete or response.failed) still delivers a final IsLast
	// response so consumers can distinguish completion from truncation.
	final := ChatResponse{IsLast: true, ModelName: m.cfg.Model}
	final.Content = buildStreamContent(accText, accThinking, reasoningItems, toolCalls, toolCallOrder)
	if err := scanner.Err(); err != nil {
		logrus.WithError(err).Error("openai response: stream scan error")
		final.Error = err
	} else {
		// Upstream #2349/#2350 class: the SSE stream ended cleanly but never
		// delivered a terminal event, so the reply is truncated (proxy
		// reset, upstream abort). Reporting it on ChatResponse.Error is what
		// lets consumers distinguish a complete answer from a cut-off one;
		// ending silently made the two indistinguishable. Cancellation and
		// normal completion both returned above, so this is only truncation.
		final.Error = fmt.Errorf("openai response: stream ended without response.completed (truncated)")
	}
	send(final)
}

// formatToolResultOutput renders a tool result for the Responses API
// (upstream #2389): when the result carries image data blocks, the output
// becomes native content parts (input_text / input_image) instead of being
// flattened to text; plain results stay a single string.
func formatToolResultOutput(blk *message.ToolResultBlock) any {
	list, ok := blk.Output.([]message.ContentBlock)
	if !ok {
		return formatter.ConvertToolResultToString(blk.Output)
	}
	hasImage := false
	for _, sub := range list {
		if db, ok := sub.(message.DataBlock); ok && strings.HasPrefix(db.GetMediaType(), "image/") {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return formatter.ConvertToolResultToString(blk.Output)
	}
	parts := make([]map[string]any, 0, len(list))
	for _, sub := range list {
		switch b := sub.(type) {
		case message.TextBlock:
			parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
		case message.DataBlock:
			if !strings.HasPrefix(b.GetMediaType(), "image/") {
				parts = append(parts, map[string]any{"type": "input_text", "text": formatter.ConvertToolResultToString([]message.ContentBlock{b})})
				continue
			}
			switch src := b.Source.(type) {
			case message.URLSource:
				parts = append(parts, map[string]any{"type": "input_image", "image_url": src.URL})
			case message.Base64Source:
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": "data:" + src.MediaType + ";base64," + src.Data,
				})
			default:
				parts = append(parts, map[string]any{"type": "input_text", "text": formatter.ConvertToolResultToString([]message.ContentBlock{b})})
			}
		}
	}
	if len(parts) == 0 {
		return formatter.ConvertToolResultToString(blk.Output)
	}
	return parts
}
