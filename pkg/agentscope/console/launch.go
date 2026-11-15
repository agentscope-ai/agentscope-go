package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// Agent is the minimal surface Launch needs — *agent.UnifiedAgent
// satisfies it.
type Agent interface {
	ReplyStream(ctx context.Context, input string) (<-chan event.Event, error)
	SubmitUserConfirm(result *event.UserConfirmResultEvent)
}

// LaunchOption configures Launch.
type LaunchOption func(*launchConfig)

type launchConfig struct {
	userName           string
	verbosity          Verbosity
	maxToolResultLines int
	in                 io.Reader
	out                io.Writer
}

// WithUserName sets the name attached to user input and the prompt label
// (default "user").
func WithUserName(name string) LaunchOption {
	return func(c *launchConfig) {
		if name != "" {
			c.userName = name
		}
	}
}

// WithLaunchVerbosity sets the renderer verbosity for Launch.
func WithLaunchVerbosity(v Verbosity) LaunchOption {
	return func(c *launchConfig) { c.verbosity = v }
}

// WithLaunchMaxToolResultLines sets the renderer's tool-result truncation
// for Launch (default 20; <= 0 disables).
func WithLaunchMaxToolResultLines(n int) LaunchOption {
	return func(c *launchConfig) { c.maxToolResultLines = n }
}

// WithInput overrides stdin (used by tests and embedded frontends).
func WithInput(r io.Reader) LaunchOption {
	return func(c *launchConfig) { c.in = r }
}

// WithOutput overrides stdout (used by tests and embedded frontends).
func WithOutput(w io.Writer) LaunchOption {
	return func(c *launchConfig) { c.out = w }
}

// Launch chats with the given agent interactively in the terminal.
//
// A lightweight try-out/debugging entry — no session management, no
// persistence: the conversation lives in the agent's state and ends with
// the process. It reads user messages from stdin, renders every streamed
// event, asks for tool-call confirmation (y/N/a) when the agent requires
// it, and turns Ctrl+C into a cancellation of the current reply. Type
// "exit"/"quit" or press Ctrl+D to leave.
//
// The reader goroutine feeding stdin lines may outlive Launch when ctx is
// canceled while it blocks on a read — harmless for a CLI process, but
// embedders passing pipes should close them to release the goroutine.
// Running two Launch calls in one process is unsupported: SIGINT is
// broadcast to both sessions.
func Launch(ctx context.Context, a Agent, opts ...LaunchOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := launchConfig{
		userName:           "user",
		verbosity:          VerbosityDefault,
		maxToolResultLines: defaultMaxToolResultLines,
		in:                 os.Stdin,
		out:                os.Stdout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	renderer := NewRenderer(
		WithVerbosity(cfg.verbosity),
		WithMaxToolResultLines(cfg.maxToolResultLines),
		WithWriter(cfg.out),
	)

	lines := make(chan lineResult)
	go func() {
		defer close(lines)
		br := bufio.NewReader(cfg.in)
		for {
			line, err := br.ReadString('\n')
			select {
			case lines <- lineResult{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	fmt.Fprintln(cfg.out, "Chat with the agent. Type 'exit' (or Ctrl+D) to quit.")

	// One persistent SIGINT handler for the whole session: during a reply
	// it interrupts just that reply (runReply's watcher owns sigCh); while
	// waiting at the prompt it exits the console. Ctrl+D/EOF exits too.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	for {
		fmt.Fprintf(cfg.out, "\n%s> ", cfg.userName)
		var lr lineResult
		var ok bool
		select {
		case lr, ok = <-lines:
			if !ok {
				return ctx.Err() // input exhausted, unless the caller canceled
			}
		case <-sigCh:
			fmt.Fprintln(cfg.out)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		query := strings.TrimSpace(lr.line)
		if lr.err != nil {
			// EOF / Ctrl+D: process a final non-empty line, then leave.
			if query == "" {
				return nil
			}
		}
		if query == "exit" || query == "quit" {
			return nil
		}
		if query == "" {
			continue
		}
		if err := runReply(ctx, a, renderer, lines, cfg.out, sigCh, query); err != nil {
			return err
		}
		if lr.err != nil {
			return nil
		}
	}
}

// runReply consumes one reply, rendering every event. Ctrl+C during the
// reply (mid-stream or at a confirmation question) cancels only this
// reply — the console returns to the prompt afterwards. Returns an error
// only for non-recoverable failures (parent ctx cancellation or
// ReplyStream setup).
func runReply(
	ctx context.Context,
	a Agent,
	renderer *Renderer,
	lines <-chan lineResult,
	out io.Writer,
	sigCh <-chan os.Signal,
	query string,
) error {
	replyCtx, cancelReply := context.WithCancel(ctx)
	defer cancelReply()

	// Own the session SIGINT channel for this reply's lifetime (Python
	// parity: SIGINT cancels the reply task). Without this watcher a
	// Ctrl+C mid-stream would sit in the buffer until the reply finished.
	go func() {
		select {
		case <-sigCh:
			cancelReply()
		case <-replyCtx.Done():
		}
	}()

	ch, err := a.ReplyStream(replyCtx, query)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

consume:
	for {
		var evt event.Event
		select {
		case <-replyCtx.Done():
			break consume
		case next, ok := <-ch:
			if !ok {
				break consume
			}
			evt = next
		}
		renderer.Render(evt)
		ce, ok := evt.(event.RequireUserConfirmEvent)
		if !ok {
			continue
		}
		confirmed, results := confirmInteractive(replyCtx, lines, out, &ce)
		if !confirmed {
			// EOF/interrupt while parked: abort the reply so the next
			// input starts clean (Go analog of Python's UserInterruptEvent).
			cancelReply()
			break
		}
		a.SubmitUserConfirm(&event.UserConfirmResultEvent{
			ReplyID:        ce.ReplyID,
			ConfirmResults: results,
		})
	}

	if replyCtx.Err() != nil && ctx.Err() == nil {
		// Canceled by SIGINT (watcher) or an aborted confirmation. The
		// renderer prints the notice itself when the interrupted
		// ReplyEndEvent made it through; cover the case where it didn't.
		if !renderer.SawReplyEnd() {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "⚠ Reply interrupted by the user.")
		}
		return nil
	}
	return ctx.Err()
}

type lineResult struct {
	line string
	err  error
}

// confirmInteractive asks the user to confirm each pending tool call via
// stdin. Answering "a" (always) also accepts the suggested permission
// rules, so matching calls will not ask again. Returns (false, nil) when
// input ends (EOF/closed channel) or the reply context is canceled
// mid-question (Ctrl+C — the runReply watcher cancels the ctx).
func confirmInteractive(
	ctx context.Context,
	lines <-chan lineResult,
	out io.Writer,
	pending *event.RequireUserConfirmEvent,
) (bool, []event.ConfirmResult) {
	var results []event.ConfirmResult
	for _, tc := range pending.ToolCalls {
		prompt := fmt.Sprintf("Allow '%s'? [y]es / [N]o", tc.Name)
		if len(tc.SuggestedRules) > 0 {
			prompt += " / [a]lways"
		}
		fmt.Fprintf(out, "\n%s ", prompt)

		var lr lineResult
		var ok bool
		select {
		case lr, ok = <-lines:
			if !ok || lr.err != nil {
				// Input exhausted (EOF/Ctrl+D): abort the parked reply.
				return false, nil
			}
		case <-ctx.Done():
			// Ctrl+C: the runReply watcher canceled the reply ctx.
			return false, nil
		}
		answer := strings.ToLower(strings.TrimSpace(lr.line))
		always := len(tc.SuggestedRules) > 0 && (answer == "a" || answer == "always")
		cr := event.ConfirmResult{
			Confirmed: always || answer == "y" || answer == "yes",
			ToolCall:  tc,
		}
		if always {
			cr.Rules = tc.SuggestedRules
		}
		results = append(results, cr)
	}
	return true, results
}
