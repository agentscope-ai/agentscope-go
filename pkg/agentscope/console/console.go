// Package console provides terminal viewing and interactive trial of
// agents: a passive event Renderer that turns an agent event stream into
// line-based terminal output, and Launch, an interactive chat loop bound
// to a single agent with tool-call confirmation and Ctrl+C interruption.
//
// It is the Go port of Python agentscope's console module
// (ConsoleRenderer + launch_console), adapted to Go's streaming model:
// confirmations are submitted out-of-band via SubmitUserConfirm while the
// reply stream stays open, and interruption is context cancellation.
//
// Deliberate deltas from the Python version:
//   - A final unterminated input line at EOF is processed as a real reply
//     before exiting (Python's input() discards it).
//   - WithUserName labels the prompt only; the user message stored in the
//     agent context carries the agent's own name (Python attaches the
//     user name to the UserMsg).
//   - Reply-level errors/interruption/max-iterations surface via
//     CustomEvent / ExceedMaxItersEvent — Go's ReplyEndEvent carries no
//     error or finished-reason fields.
package console

import (
	"os"
)

// Verbosity controls how much of the event stream the Renderer prints.
type Verbosity string

const (
	// VerbosityQuiet prints only the streamed reply text and errors.
	VerbosityQuiet Verbosity = "quiet"
	// VerbosityDefault adds thinking, tool calls/results, hints, token
	// usage, and human-in-the-loop notices.
	VerbosityDefault Verbosity = "default"
	// VerbosityDebug adds lifecycle events and other events invisible at
	// default verbosity.
	VerbosityDebug Verbosity = "debug"
)

func verbosityLevel(v Verbosity) int {
	switch v {
	case VerbosityQuiet:
		return 0
	case VerbosityDebug:
		return 2
	default:
		return 1
	}
}

// colorEnabled reports whether ANSI styling should be used for w: only on
// an interactive terminal, and never when NO_COLOR is set. Explicit
// WithColor overrides this detection.
func colorAutoDetect(w interface{ Stat() (os.FileInfo, error) }) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := w.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
