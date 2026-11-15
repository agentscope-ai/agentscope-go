package lsp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type ServerConfig struct {
	Command string
	Args    []string
	RootDir string
}

type Server struct {
	cfg     ServerConfig
	cmd     *exec.Cmd
	conn    *Conn
	mu      sync.Mutex
	started bool
	opened  map[string]int
}

func NewServer(cfg ServerConfig) *Server {
	return &Server{
		cfg:    cfg,
		opened: make(map[string]int),
	}
}

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	cmd := exec.CommandContext(ctx, s.cfg.Command, s.cfg.Args...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.cfg.Command, err)
	}

	s.cmd = cmd
	s.conn = NewConn(stdin, stdout)

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rootURI := fileURI(s.cfg.RootDir)
	params := InitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   rootURI,
	}
	var result InitializeResult
	if err := s.conn.Call(initCtx, "initialize", params, &result); err != nil {
		_ = s.conn.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("initialize: %w", err)
	}

	if err := s.conn.Notify("initialized", struct{}{}); err != nil {
		_ = s.conn.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("initialized notify: %w", err)
	}

	s.started = true
	return nil
}

func (s *Server) EnsureOpen(ctx context.Context, filePath string) error {
	uri := fileURI(filePath)
	s.mu.Lock()
	_, already := s.opened[uri]
	if already {
		s.mu.Unlock()
		return nil
	}
	s.opened[uri] = 1
	s.mu.Unlock()

	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file for didOpen: %w", err)
	}

	langID := detectLanguageID(filePath)
	params := DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        uri,
			LanguageID: langID,
			Version:    1,
			Text:       string(content),
		},
	}
	return s.conn.Notify("textDocument/didOpen", params)
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.conn == nil {
		return nil
	}
	shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_ = s.conn.Call(shutCtx, "shutdown", nil, nil)
	_ = s.conn.Notify("exit", nil)
	_ = s.conn.Close()

	if s.cmd == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			logrus.WithError(err).Debug("lsp: server process exited with error")
		}
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	return nil
}

func fileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

func uriToFile(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	return u.Path
}

func detectLanguageID(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp"
	default:
		return "plaintext"
	}
}
