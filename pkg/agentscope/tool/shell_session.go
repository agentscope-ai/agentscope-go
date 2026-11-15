package tool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
)

// Shell session statuses returned in ShellExecResult.Status.
const (
	ShellStatusDone    = "done"
	ShellStatusRunning = "running"
)

const (
	defaultShellYield      = time.Second
	maxShellYield          = 30 * time.Second
	defaultShellMaxOutput  = 30 * 1024
	shellSentinelPrefix    = "__ASG_SHELL_DONE_"
	shellSentinelPrintfmt  = " ; printf '\\n" + shellSentinelPrefix + "%s_%%d__\\n' $?\n"
	defaultSessionName     = "main"
	shellReadBufferInitial = 64 * 1024
	shellReadBufferMax     = 1024 * 1024
)

// ShellExecResult is one observation of a persistent shell session.
type ShellExecResult struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // ShellStatusDone or ShellStatusRunning
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"` // head+tail truncated at the output budget
}

// ShellManager owns persistent shell sessions: long-running shell processes
// whose working directory, environment and foreground command state survive
// across tool calls (codex unified_exec parity). It requires a POSIX shell
// (bash/zsh/sh); NewShellManager fails on Windows shells.
//
// Cancellation semantics: canceling the context of an in-flight Exec or
// WriteStdin kills the whole session process (a pipe-fed shell cannot signal
// only its foreground command), and the session is dropped; the next Exec
// recreates it. Sessions otherwise outlive turns until Kill or Close.
type ShellManager struct {
	mu       sync.Mutex
	sessions map[string]*shellSession
	cwd      string
	shell    platform.Shell
	closed   bool
}

// ShellManagerOption configures a ShellManager.
type ShellManagerOption func(*ShellManager)

// WithShellManagerCwd sets the starting working directory for new sessions.
func WithShellManagerCwd(dir string) ShellManagerOption {
	return func(m *ShellManager) { m.cwd = dir }
}

// NewShellManager creates a manager. It fails when no POSIX shell is
// available (persistent sessions rely on POSIX stdin/printf semantics).
func NewShellManager(opts ...ShellManagerOption) (*ShellManager, error) {
	sh := platform.Detect()
	if !sh.IsUnix() {
		return nil, fmt.Errorf("tool: persistent shell sessions require a POSIX shell, detected %s", sh.Type)
	}
	m := &ShellManager{
		sessions: make(map[string]*shellSession),
		shell:    sh,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Exec runs command in the named session (created on first use), waiting up
// to yield for completion. A command still running past yield keeps running;
// poll it with WriteStdin.
//
// command must be single-line: the completion sentinel shares the command's
// final line, so multi-line input (e.g. a heredoc whose terminator owns a
// line) wedges the session until Kill. The Shell tool enforces this for
// model callers; direct Manager consumers must uphold it themselves.
func (m *ShellManager) Exec(ctx context.Context, session, command string, yield time.Duration, maxOutput int) (*ShellExecResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("tool: shell command cannot be empty")
	}
	if strings.Contains(command, "\n") {
		return nil, fmt.Errorf("tool: shell command must be single-line (the completion sentinel shares its final line)")
	}
	s, err := m.getOrCreate(session)
	if err != nil {
		return nil, err
	}
	return s.exec(ctx, command, normShellYield(yield), normShellMaxOutput(maxOutput))
}

// WriteStdin writes chars (e.g. "y\n") to the session's foreground command —
// or just polls its output when chars is empty — waiting up to yield.
func (m *ShellManager) WriteStdin(ctx context.Context, session, chars string, yield time.Duration, maxOutput int) (*ShellExecResult, error) {
	if session == "" {
		session = defaultSessionName
	}
	m.mu.Lock()
	s := m.sessions[session]
	m.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("tool: shell session %q does not exist", session)
	}
	return s.writeStdin(ctx, chars, normShellYield(yield), normShellMaxOutput(maxOutput))
}

// Kill terminates a session's process group and drops the session.
func (m *ShellManager) Kill(session string) error {
	m.mu.Lock()
	s := m.sessions[session]
	delete(m.sessions, session)
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	s.kill()
	return nil
}

// Close kills every session.
func (m *ShellManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*shellSession, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.kill()
	}
	return nil
}

func (m *ShellManager) getOrCreate(name string) (*shellSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("tool: shell manager closed")
	}
	if name == "" {
		name = defaultSessionName
	}
	if s := m.sessions[name]; s != nil && !s.dead() {
		return s, nil
	}
	s, err := startShellSession(name, m.shell, m.cwd)
	if err != nil {
		return nil, err
	}
	m.sessions[name] = s
	return s, nil
}

func normShellYield(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultShellYield
	}
	if d > maxShellYield {
		return maxShellYield
	}
	return d
}

func normShellMaxOutput(n int) int {
	if n <= 0 {
		return defaultShellMaxOutput
	}
	return n
}

// shellSession is one persistent shell process with a dedicated reader
// goroutine. Commands are serialized (one foreground command at a time).
type shellSession struct {
	id     string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	writeMu    sync.Mutex // serializes stdin writes (exec payload vs stdin chars)
	mu         sync.Mutex
	current    *runningShellCmd // non-nil while a command executes
	deadFlag   bool
	readerDone chan struct{}
}

// runningShellCmd accumulates one command's output until its sentinel line.
type runningShellCmd struct {
	token    string
	buf      *headTailBuffer
	done     chan struct{}
	exitCode int
}

func startShellSession(id string, sh platform.Shell, cwd string) (*shellSession, error) {
	cmd := exec.Command(sh.Path)
	if cwd != "" {
		cmd.Dir = cwd
	}
	configureProcGroup(cmd) // manual process-group kill via killProcessGroup
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("tool: shell session %s: stdin: %w", id, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("tool: shell session %s: stdout: %w", id, err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tool: shell session %s: start: %w", id, err)
	}
	s := &shellSession{
		id:         id,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		readerDone: make(chan struct{}),
	}
	go s.readLoop(stdout)
	return s, nil
}

// readLoop streams shell output into the current command buffer, detecting
// the per-command sentinel line. It uses ReadSlice rather than Scanner so an
// over-long line (minified bundles, base64 blobs) is streamed into the
// capped head/tail buffer instead of killing the session.
func (s *shellSession) readLoop(stdout io.Reader) {
	defer close(s.readerDone)
	defer func() { _ = s.cmd.Wait() }() // reap the process on EOF
	reader := bufio.NewReaderSize(stdout, shellReadBufferInitial)
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		if complete {
			s.handleLine(strings.TrimRight(string(line), "\n"))
			line = line[:0]
		} else if len(line) > shellReadBufferMax {
			// Far longer than any sentinel: stream the partial line out and
			// reset detection (a sentinel can never span this).
			s.handlePartial(line)
			line = line[:0]
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			break
		}
	}
	if len(line) > 0 {
		s.handlePartial(line)
	}
	// EOF: the shell died. Release any blocked caller.
	s.mu.Lock()
	s.deadFlag = true
	if cur := s.current; cur != nil {
		cur.buf.WriteString("\n[shell session terminated; its state (cwd/env) was reset]\n")
		cur.exitCode = -1
		close(cur.done)
		s.current = nil
	}
	s.mu.Unlock()
}

// handleLine processes one complete output line.
func (s *shellSession) handleLine(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.current
	if cur == nil {
		// Output arriving between commands (background jobs, shell noise) is
		// dropped: there is no buffer to attribute it to.
		return
	}
	if code, ok := parseShellSentinel(text, cur.token); ok {
		cur.exitCode = code
		close(cur.done)
		s.current = nil
		return
	}
	cur.buf.WriteString(text + "\n")
}

// handlePartial streams an incomplete line chunk into the current buffer.
func (s *shellSession) handlePartial(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.current; cur != nil {
		cur.buf.WriteString(string(chunk))
	}
}

func (s *shellSession) dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadFlag
}

// exec runs one command, serialized per session.
func (s *shellSession) exec(ctx context.Context, command string, yield time.Duration, maxOutput int) (*ShellExecResult, error) {
	s.mu.Lock()
	if s.deadFlag {
		s.mu.Unlock()
		return nil, fmt.Errorf("tool: shell session %q is dead", s.id)
	}
	if s.current != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("tool: shell session %q is busy with a running command; poll it with write_stdin or interrupt the turn", s.id)
	}
	cur := &runningShellCmd{
		token: strconv.FormatInt(time.Now().UnixNano(), 36),
		buf:   newHeadTailBuffer(maxOutput),
		done:  make(chan struct{}),
	}
	s.current = cur
	s.mu.Unlock()

	// The sentinel rides on the command's own line (separated by ";") rather
	// than a following line: a command that reads stdin (read, cat, ...) would
	// otherwise swallow the sentinel line as its input. Known limitation: a
	// command whose final line is a comment swallows the sentinel instead
	// (the command then looks stuck and is recoverable via Kill). The whole
	// payload is one write under writeMu so a concurrent ShellStdin write can
	// never interleave between the command and its sentinel.
	line := strings.TrimRight(command, "\n")
	s.writeMu.Lock()
	_, err := io.WriteString(s.stdin, line+fmt.Sprintf(shellSentinelPrintfmt, cur.token))
	s.writeMu.Unlock()
	if err != nil {
		s.finishCommand(cur)
		return nil, fmt.Errorf("tool: shell session %q: write: %w", s.id, err)
	}

	return s.waitCommand(ctx, cur, yield), nil
}

// writeStdin feeds the foreground command and waits for more output.
func (s *shellSession) writeStdin(ctx context.Context, chars string, yield time.Duration, maxOutput int) (*ShellExecResult, error) {
	s.mu.Lock()
	cur := s.current
	dead := s.deadFlag
	s.mu.Unlock()
	if dead {
		return nil, fmt.Errorf("tool: shell session %q is dead", s.id)
	}
	if cur == nil {
		return nil, fmt.Errorf("tool: shell session %q has no running command", s.id)
	}
	cur.buf.setMax(maxOutput) // a poll may widen or narrow the view
	if chars != "" {
		s.writeMu.Lock()
		_, err := io.WriteString(s.stdin, chars)
		s.writeMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("tool: shell session %q: write stdin: %w", s.id, err)
		}
	}
	return s.waitCommand(ctx, cur, yield), nil
}

// waitCommand blocks until the command's sentinel, the yield, or
// cancellation. Cancellation kills the session (see ShellManager docs).
func (s *shellSession) waitCommand(ctx context.Context, cur *runningShellCmd, yield time.Duration) *ShellExecResult {
	timer := time.NewTimer(yield)
	defer timer.Stop()
	select {
	case <-cur.done:
		return &ShellExecResult{
			SessionID: s.id,
			Status:    ShellStatusDone,
			ExitCode:  cur.exitCode,
			Output:    cur.buf.String(),
		}
	case <-timer.C:
		return &ShellExecResult{
			SessionID: s.id,
			Status:    ShellStatusRunning,
			ExitCode:  -1,
			Output:    cur.buf.String(),
		}
	case <-ctx.Done():
		s.kill()
		return &ShellExecResult{
			SessionID: s.id,
			Status:    ShellStatusDone,
			ExitCode:  -1,
			Output:    "Command interrupted; the shell session was terminated.\nPartial output:\n" + cur.buf.String(),
		}
	}
}

// finishCommand detaches an unfinished command after a local write failure.
func (s *shellSession) finishCommand(cur *runningShellCmd) {
	s.mu.Lock()
	if s.current == cur {
		s.current = nil
		select {
		case <-cur.done:
		default:
			close(cur.done)
		}
	}
	s.mu.Unlock()
}

// kill signals the process group and waits for the reader to drain; the
// readLoop EOF marks the session dead and releases any in-flight command.
// Daemonized grandchildren escape the process group but can hold the stdout
// pipe open (no EOF), so after a grace period the read end is force-closed
// to unblock the reader — kill must never hang, or Conversation.Interrupt
// and Close would hang with it.
func (s *shellSession) kill() {
	if s.cmd.Process != nil {
		killProcessGroup(s.cmd)
	}
	select {
	case <-s.readerDone:
	case <-time.After(5 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.stdout.Close() // force the reader out when a grandchild holds the pipe
		<-s.readerDone
	}
}

// parseShellSentinel matches the sentinel line "__ASG_SHELL_DONE_<token>_<code>__".
func parseShellSentinel(line, token string) (int, bool) {
	if !strings.HasPrefix(line, shellSentinelPrefix) || !strings.HasSuffix(line, "__") {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(line, shellSentinelPrefix), "__")
	tok, codeStr, ok := strings.Cut(rest, "_")
	if !ok || tok != token {
		return 0, false
	}
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return 0, false
	}
	return code, true
}

// headTailBuffer keeps the head and tail of a stream within a byte budget,
// eliding the middle with a marker (matching agent.TruncateToolResult).
type headTailBuffer struct {
	mu     sync.Mutex
	max    int
	head   bytes.Buffer
	tail   []byte
	total  int
	elided bool
}

func newHeadTailBuffer(max int) *headTailBuffer {
	return &headTailBuffer{max: max}
}

func (b *headTailBuffer) setMax(max int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.max = max
}

func (b *headTailBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(s)
	headCap := b.max / 2
	if b.head.Len() < headCap {
		keep := headCap - b.head.Len()
		if keep > len(s) {
			keep = len(s)
		}
		b.head.WriteString(s[:keep])
		s = s[keep:]
		if s == "" {
			return
		}
		b.elided = true
	}
	tailCap := b.max - b.max/2
	b.tail = append(b.tail, s...)
	if len(b.tail) > tailCap {
		b.tail = b.tail[len(b.tail)-tailCap:]
		b.elided = true
	}
}

func (b *headTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.elided {
		return b.head.String() + string(b.tail)
	}
	elided := b.total - b.head.Len() - len(b.tail)
	return fmt.Sprintf("%s\n<<<TRUNCATED: %d bytes elided>>>\n%s", b.head.String(), elided, b.tail)
}
