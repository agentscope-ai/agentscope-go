package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// StdinInputProvider reads user input line-by-line from os.Stdin, printing a
// prompt before each read.
type StdinInputProvider struct {
	Prompt  string // e.g. "> " or "user: "
	scanner *bufio.Scanner
}

// NewStdinInputProvider creates a StdinInputProvider with the given prompt.
func NewStdinInputProvider(prompt string) *StdinInputProvider {
	return &StdinInputProvider{
		Prompt:  prompt,
		scanner: bufio.NewScanner(os.Stdin),
	}
}

// ReadInput prints the prompt and reads one line from stdin. It returns io.EOF
// when stdin is closed, and respects ctx cancellation while blocked on the read.
func (p *StdinInputProvider) ReadInput(ctx context.Context) (string, error) {
	if p.scanner == nil {
		p.scanner = bufio.NewScanner(os.Stdin)
	}
	if p.Prompt != "" {
		fmt.Print(p.Prompt)
	}

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if p.scanner.Scan() {
			done <- result{line: p.scanner.Text()}
			return
		}
		if err := p.scanner.Err(); err != nil {
			done <- result{err: err}
			return
		}
		done <- result{err: io.EOF}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		return strings.TrimSpace(r.line), r.err
	}
}

// OnInterrupt aborts the current reply on interrupt.
func (p *StdinInputProvider) OnInterrupt() InterruptAction {
	return InterruptAbort
}
