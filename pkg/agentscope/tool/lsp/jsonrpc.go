package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

// Conn wraps an LSP server's stdin/stdout with Content-Length framing.
type Conn struct {
	mu     sync.Mutex
	writer io.Writer
	nextID int

	pendingMu sync.Mutex
	pending   map[int]chan *Response

	done   chan struct{}
	closer io.Closer
}

// NewConn creates a connection. stdin is what we write to (the server's stdin),
// stdout is what we read from (the server's stdout). The read loop runs in a
// background goroutine.
func NewConn(stdin io.WriteCloser, stdout io.Reader) *Conn {
	c := &Conn{
		writer:  stdin,
		closer:  stdin,
		pending: make(map[int]chan *Response),
		done:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(stdout))
	return c
}

func (c *Conn) readLoop(reader *bufio.Reader) {
	defer close(c.done)
	for {
		resp, err := readMessage(reader)
		if err != nil {
			return
		}
		if resp.ID == 0 {
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func readRawMessage(reader *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLength, _ = strconv.Atoi(val)
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("invalid content length: %d", contentLength)
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

func readMessage(reader *bufio.Reader) (*Response, error) {
	body, err := readRawMessage(reader)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Call sends a JSON-RPC request and waits for the response.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return err
	}

	ch := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	err = writeMessage(c.writer, data)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed")
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	req := Request{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return writeMessage(c.writer, data)
}

// Close shuts down the connection.
func (c *Conn) Close() error {
	err := c.closer.Close()
	<-c.done
	return err
}

func writeMessage(w io.Writer, data []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	_, err := fmt.Fprintf(w, "%s%s", header, data)
	return err
}
