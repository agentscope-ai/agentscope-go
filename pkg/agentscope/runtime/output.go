package runtime

import (
	"fmt"
	"io"
	"os"
)

// TerminalOutputSink writes agent output to an io.Writer (os.Stdout by default)
// with simple text formatting.
type TerminalOutputSink struct {
	Writer io.Writer
}

// NewTerminalOutputSink creates a TerminalOutputSink writing to os.Stdout.
func NewTerminalOutputSink() *TerminalOutputSink {
	return &TerminalOutputSink{Writer: os.Stdout}
}

func (s *TerminalOutputSink) w() io.Writer {
	if s.Writer == nil {
		return os.Stdout
	}
	return s.Writer
}

// WriteText prints text directly.
func (s *TerminalOutputSink) WriteText(text string) error {
	_, err := fmt.Fprint(s.w(), text)
	return err
}

// WriteThinking prints text with a "[thinking] " prefix.
func (s *TerminalOutputSink) WriteThinking(text string) error {
	_, err := fmt.Fprintf(s.w(), "[thinking] %s", text)
	return err
}

// WriteToolCall prints the tool invocation.
func (s *TerminalOutputSink) WriteToolCall(name string, input map[string]any) error {
	_, err := fmt.Fprintf(s.w(), "\nTool: %s(%v)\n", name, input)
	return err
}

// WriteToolResult prints the tool result.
func (s *TerminalOutputSink) WriteToolResult(name string, output string, state string) error {
	_, err := fmt.Fprintf(s.w(), "Result[%s] %s: %s\n", state, name, output)
	return err
}

// WriteError prints an error with an "Error: " prefix.
func (s *TerminalOutputSink) WriteError(err error) error {
	_, werr := fmt.Fprintf(s.w(), "Error: %v\n", err)
	return werr
}

// Flush writes a trailing newline to terminate the current line.
func (s *TerminalOutputSink) Flush() error {
	_, err := fmt.Fprintln(s.w())
	return err
}
