package console

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiItalic  = "\x1b[3m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
)

// defaultMaxToolResultLines truncates printed tool results; mirrors the
// Python ConsoleRenderer default.
const defaultMaxToolResultLines = 20

// Renderer turns agent events into line-based terminal output, so that
// consuming ReplyStream in a terminal is a one-liner:
//
//	r := console.NewRenderer()
//	for evt := range ch {
//		r.Render(evt)
//	}
//
// Text/thinking deltas print as they arrive; concurrently streamed tool
// calls/results are buffered via message.Msg.AppendEvent and printed as
// whole blocks on their end events. The accumulated reply message is
// available via LastMsg.
//
// Renderer is NOT safe for concurrent Render calls — it is meant for a
// single consumer of one event stream (Launch consumes it that way).
type Renderer struct {
	verbosity          Verbosity
	maxToolResultLines int // <= 0 means unlimited
	w                  io.Writer
	color              bool
	colorConfigured    bool

	msg         *message.Msg
	midStream   bool
	sawReplyEnd bool
}

// RendererOption configures NewRenderer.
type RendererOption func(*Renderer)

// WithVerbosity sets how much of the stream is printed.
func WithVerbosity(v Verbosity) RendererOption {
	return func(r *Renderer) { r.verbosity = v }
}

// WithMaxToolResultLines truncates printed tool results to n lines
// (default 20). Values <= 0 disable truncation.
func WithMaxToolResultLines(n int) RendererOption {
	return func(r *Renderer) { r.maxToolResultLines = n }
}

// WithWriter redirects output (default os.Stdout).
func WithWriter(w io.Writer) RendererOption {
	return func(r *Renderer) { r.w = w }
}

// WithColor forces ANSI styling on or off. By default color is used only
// when the writer is an interactive terminal and NO_COLOR is unset.
func WithColor(enabled bool) RendererOption {
	return func(r *Renderer) {
		r.color = enabled
		r.colorConfigured = true
	}
}

// NewRenderer creates a Renderer with defaults (default verbosity, 20-line
// tool-result truncation, stdout, auto-detected color).
func NewRenderer(opts ...RendererOption) *Renderer {
	r := &Renderer{
		verbosity:          VerbosityDefault,
		maxToolResultLines: defaultMaxToolResultLines,
		w:                  os.Stdout,
	}
	for _, opt := range opts {
		opt(r)
	}
	if !r.colorConfigured {
		if f, ok := r.w.(*os.File); ok {
			r.color = colorAutoDetect(f)
		}
	}
	return r
}

// LastMsg returns the reply message accumulated from rendered events.
func (r *Renderer) LastMsg() *message.Msg { return r.msg }

// SawReplyEnd reports whether the current (or last) reply's ReplyEndEvent
// was rendered; callers use it to avoid double-printing interruption
// notices when the stream dies without the end event.
func (r *Renderer) SawReplyEnd() bool { return r.sawReplyEnd }

// Render prints the event to the console. Unknown event types are skipped
// silently (or shown as a dim line under debug verbosity), so the renderer
// keeps working when new event types are introduced.
func (r *Renderer) Render(e event.Event) {
	r.accumulate(e)

	switch evt := e.(type) {
	case event.ReplyStartEvent:
		if r.show(1) {
			r.breakLine(false)
			r.printStyled(ansiDim, strings.Repeat("─", 3)+" "+evt.Name+" "+strings.Repeat("─", 3))
			r.printRaw("\n")
		}
	case event.ThinkingBlockStartEvent:
		if r.show(1) {
			r.breakLine(false)
			r.printStyled(ansiDim+ansiItalic, "✻ Thinking…")
			r.printRaw("\n")
		}
	case event.ThinkingBlockDeltaEvent:
		r.stream(evt.Delta, ansiDim, false)
	case event.ThinkingBlockEndEvent:
		if r.show(1) {
			r.breakLine(false)
			r.printRaw("\n")
		}
	case event.TextBlockDeltaEvent:
		r.stream(evt.Delta, "", true)
	case event.TextBlockEndEvent:
		r.breakLine(true)
	case event.ToolCallEndEvent:
		r.renderToolCall(evt.ToolCallID)
	case event.ToolResultEndEvent:
		r.renderToolResult(evt.ToolCallID, evt.State)
	case event.DataBlockEndEvent:
		r.renderDataBlock(evt.BlockID)
	case event.ModelCallStartEvent:
		r.debugLine("model call → " + evt.ModelName)
	case event.ModelCallEndEvent:
		r.renderModelCallEnd(&evt)
	case event.HintBlockEvent:
		r.renderHint(&evt)
	case event.RequireUserConfirmEvent:
		r.renderHITL(evt.ToolCalls, "Tool calls awaiting user confirmation:")
	case event.RequireExternalExecutionEvent:
		r.renderHITL(evt.ToolCalls, "Tool calls awaiting external execution:")
	case event.ExceedMaxItersEvent:
		if r.show(1) {
			r.breakLine(false)
			r.printStyled(ansiYellow, "⚠ Exceeded the maximum reasoning-acting iterations.")
			r.printRaw("\n")
		}
	case event.ReplyEndEvent:
		r.breakLine(true)
		r.sawReplyEnd = true
		if evt.Error != nil && evt.Error.Message != "" {
			r.printStyled(ansiRed+ansiBold, "✗ Error ("+string(evt.Error.Type)+"): "+evt.Error.Message)
			r.printRaw("\n")
		} else if evt.FinishedReason == types.ReplyInterrupted && r.show(1) {
			r.printStyled(ansiYellow, "⚠ Reply interrupted by the user.")
			r.printRaw("\n")
		}
	case event.CustomEvent:
		r.renderCustom(&evt)
	default:
		t := string(e.GetEventType())
		// Delta events are noise even under debug — their content renders
		// as a whole block on the corresponding end event.
		if !strings.HasSuffix(t, "_delta") {
			r.debugLine(t)
		}
	}
}

// --- accumulation -------------------------------------------------------

func (r *Renderer) accumulate(e event.Event) {
	replyID := e.GetReplyID()
	if replyID == "" {
		return
	}
	if _, ok := e.(event.ReplyStartEvent); ok {
		name := ""
		if rs, ok := e.(event.ReplyStartEvent); ok {
			name = rs.Name
		}
		m := message.AssistantMsg(name, []message.ContentBlock{})
		m.ID = replyID
		r.msg = m
		r.sawReplyEnd = false
	} else if r.msg == nil || r.msg.ID != replyID {
		// A continuation (e.g. after resume) without a ReplyStartEvent.
		m := message.AssistantMsg("agent", []message.ContentBlock{})
		m.ID = replyID
		r.msg = m
	}
	r.msg.AppendEvent(e)
}

// --- helpers -------------------------------------------------------------

func (r *Renderer) show(level int) bool { return verbosityLevel(r.verbosity) >= level }

func (r *Renderer) printRaw(s string) { _, _ = io.WriteString(r.w, s) }

func (r *Renderer) printStyled(style, s string) {
	if !r.color || style == "" {
		r.printRaw(s)
		return
	}
	r.printRaw(style + s + ansiReset)
}

func (r *Renderer) stream(delta, style string, quietOK bool) {
	if !quietOK && !r.show(1) {
		return
	}
	if r.color && style != "" {
		r.printRaw(style + delta + ansiReset)
	} else {
		r.printRaw(delta)
	}
	r.midStream = true
}

func (r *Renderer) breakLine(quietOK bool) {
	if !quietOK && !r.show(1) {
		return
	}
	if r.midStream {
		r.printRaw("\n")
		r.midStream = false
	}
}

func (r *Renderer) debugLine(text string) {
	if !r.show(2) {
		return
	}
	r.breakLine(false)
	r.printStyled(ansiDim, "· "+text)
	r.printRaw("\n")
}

func (r *Renderer) findBlock(blockType message.ContentBlockType, blockID string) message.ContentBlock {
	if r.msg == nil {
		return nil
	}
	for _, b := range r.msg.Content {
		if b.GetType() == blockType && b.GetID() == blockID {
			return b
		}
	}
	return nil
}

// --- per-event renderers --------------------------------------------------

func formatToolInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	var obj any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	if len(compact) <= 72 {
		return string(compact)
	}
	indented, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return string(compact)
	}
	return string(indented)
}

func (r *Renderer) renderToolCall(toolCallID string) {
	if !r.show(1) {
		return
	}
	b := r.findBlock(message.ContentBlockToolCall, toolCallID)
	tc, ok := b.(message.ToolCallBlock)
	if !ok {
		return
	}
	r.breakLine(false)
	args := formatToolInput(tc.Input)
	if strings.Contains(args, "\n") {
		r.printStyled(ansiCyan, "→ ")
		r.printStyled(ansiCyan+ansiBold, tc.Name)
		r.printRaw("\n")
		for _, line := range strings.Split(args, "\n") {
			r.printRaw("  " + line + "\n")
		}
	} else {
		r.printStyled(ansiCyan, "→ ")
		r.printStyled(ansiCyan+ansiBold, tc.Name)
		r.printRaw(" " + args + "\n")
	}
}

var resultStateIcons = map[message.ToolResultState]struct {
	icon  string
	style string
}{
	message.ToolResultSuccess:     {"✓", ansiGreen},
	message.ToolResultError:       {"✗", ansiRed},
	message.ToolResultDenied:      {"⊘", ansiYellow},
	message.ToolResultInterrupted: {"⚠", ansiYellow},
	message.ToolResultRunning:     {"…", ansiDim},
}

func humanSize(n int) string {
	size := float64(n)
	for _, unit := range []string{"B", "KB", "MB"} {
		if size < 1024 {
			return fmt.Sprintf("%.0f%s", size, unit)
		}
		size /= 1024
	}
	return fmt.Sprintf("%.1fGB", size)
}

func dataPlaceholder(b message.DataBlock) string {
	switch s := b.Source.(type) {
	case message.Base64Source:
		return fmt.Sprintf("[data: %s, ~%s]", s.MediaType, humanSize(len(s.Data)*3/4))
	case *message.Base64Source:
		return fmt.Sprintf("[data: %s, ~%s]", s.MediaType, humanSize(len(s.Data)*3/4))
	case message.URLSource:
		return fmt.Sprintf("[data: %s, %s]", s.MediaType, s.URL)
	case *message.URLSource:
		return fmt.Sprintf("[data: %s, %s]", s.MediaType, s.URL)
	default:
		return fmt.Sprintf("[data: %s]", b.GetMediaType())
	}
}

func (r *Renderer) flattenResultOutput(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case []message.ContentBlock:
		var parts []string
		for _, sub := range v {
			switch sb := sub.(type) {
			case message.TextBlock:
				parts = append(parts, sb.Text)
			case message.DataBlock:
				parts = append(parts, dataPlaceholder(sb))
			default:
				parts = append(parts, fmt.Sprint(sub))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(v)
	}
}

func (r *Renderer) renderToolResult(toolCallID string, state message.ToolResultState) {
	if !r.show(1) {
		return
	}
	b := r.findBlock(message.ContentBlockToolResult, toolCallID)
	tr, ok := b.(message.ToolResultBlock)
	if !ok {
		return
	}
	st := resultStateIcons[state]
	if st.icon == "" {
		st = struct {
			icon  string
			style string
		}{"•", ""}
	}

	r.breakLine(false)
	r.printStyled(st.style, st.icon+" "+tr.Name)
	r.printStyled(ansiDim, " · "+string(state))
	r.printRaw("\n")

	lines := strings.Split(r.flattenResultOutput(tr.Output), "\n")
	// Match Python's splitlines: a trailing newline does not add an empty
	// final line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	maxLines := r.maxToolResultLines
	truncated := 0
	if maxLines > 0 && len(lines) > maxLines {
		truncated = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	for _, line := range lines {
		r.printStyled(ansiDim, "  "+line)
		r.printRaw("\n")
	}
	if truncated > 0 {
		r.printStyled(ansiDim+ansiItalic, fmt.Sprintf("  … (+%d more lines)", truncated))
		r.printRaw("\n")
	}
}

func (r *Renderer) renderDataBlock(blockID string) {
	if !r.show(1) {
		return
	}
	b := r.findBlock(message.ContentBlockData, blockID)
	db, ok := b.(message.DataBlock)
	if !ok {
		return
	}
	r.breakLine(false)
	r.printStyled(ansiMagenta, dataPlaceholder(db))
	r.printRaw("\n")
}

func (r *Renderer) renderModelCallEnd(evt *event.ModelCallEndEvent) {
	if !r.show(1) {
		return
	}
	note := fmt.Sprintf("tokens: %d in / %d out", evt.InputTokens, evt.OutputTokens)
	if evt.CacheReadTokens > 0 || evt.CacheCreationTokens > 0 {
		note += fmt.Sprintf(" · cache: %d read / %d write", evt.CacheReadTokens, evt.CacheCreationTokens)
	}
	r.breakLine(false)
	r.printStyled(ansiDim, "· "+note)
	r.printRaw("\n")
}

func flattenHint(hint any) string {
	switch v := hint.(type) {
	case string:
		return v
	case []message.ContentBlock:
		var parts []string
		for _, sub := range v {
			switch sb := sub.(type) {
			case message.TextBlock:
				parts = append(parts, sb.Text)
			case message.DataBlock:
				parts = append(parts, dataPlaceholder(sb))
			default:
				parts = append(parts, fmt.Sprint(sub))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(v)
	}
}

func (r *Renderer) renderHint(evt *event.HintBlockEvent) {
	if !r.show(1) {
		return
	}
	text := flattenHint(evt.Hint)
	r.breakLine(false)
	title := "◈ hint"
	if evt.Source != "" {
		title += " from " + evt.Source
	}
	border := r.styleFor(ansiDim + ansiYellow)
	r.printRaw(border + "┌─ " + r.styleFor(ansiYellow) + title + r.styleFor(ansiReset+ansiDim+ansiYellow) + " ─" + ansiReset + "\n")
	for _, line := range strings.Split(text, "\n") {
		r.printRaw(border + "│ " + r.styleFor(ansiDim) + line + ansiReset + "\n")
	}
	r.printRaw(border + "└─" + ansiReset + "\n")
}

func (r *Renderer) styleFor(code string) string {
	if !r.color {
		return ""
	}
	return code
}

func (r *Renderer) renderHITL(toolCalls []message.ToolCallBlock, title string) {
	if !r.show(1) {
		return
	}
	r.breakLine(false)
	r.printStyled(ansiYellow+ansiBold, "⚠ "+title)
	r.printRaw("\n")
	for _, tc := range toolCalls {
		r.printStyled(ansiYellow, "  • ")
		r.printStyled(ansiYellow+ansiBold, tc.Name)
		r.printRaw(" " + formatToolInput(tc.Input) + "\n")
		for _, raw := range tc.SuggestedRules {
			rule, ok := raw.(permission.Rule)
			if !ok {
				continue
			}
			pattern := ""
			if rule.RuleContent != "" {
				pattern = "(" + rule.RuleContent + ")"
			}
			r.printStyled(ansiDim, fmt.Sprintf("    suggested rule: %s %s%s\n", rule.Behavior, rule.ToolName, pattern))
		}
	}
}

func (r *Renderer) renderCustom(evt *event.CustomEvent) {
	switch {
	case strings.Contains(evt.Name, "error"):
		detail := evt.Name
		if v, ok := evt.Value["error"]; ok {
			detail = fmt.Sprintf("%s: %v", evt.Name, v)
		}
		r.breakLine(true)
		r.printStyled(ansiRed+ansiBold, "✗ Error: "+detail)
		r.printRaw("\n")
	case evt.Name == "compacted":
		if r.show(1) {
			r.breakLine(false)
			r.printStyled(ansiDim, "· context compacted")
			r.printRaw("\n")
		}
	default:
		r.debugLine("custom: " + evt.Name)
	}
}
