package grpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Message is the unit of communication between agents.
type Message struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Method    string          `json:"method"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
	IsStream  bool            `json:"is_stream,omitempty"`
	StreamEnd bool            `json:"stream_end,omitempty"`
}

// Transport provides bidirectional agent communication over TCP.
type Transport interface {
	Send(ctx context.Context, msg *Message) error
	Receive(ctx context.Context) (*Message, error)
	Close() error
}

// connTransport implements Transport over a single TCP connection using newline-delimited JSON.
type connTransport struct {
	conn      net.Conn
	scanner   *bufio.Scanner
	writeGate chan struct{} // serializes frame writes, with cancelable admission
}

func newConnTransport(conn net.Conn) *connTransport {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message
	return &connTransport{
		conn:      conn,
		scanner:   scanner,
		writeGate: make(chan struct{}, 1),
	}
}

func (t *connTransport) Send(ctx context.Context, msg *Message) error {
	select {
	case t.writeGate <- struct{}{}:
		defer func() { <-t.writeGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Serialize before touching the connection. Invalid local message data
	// must not interrupt unrelated requests using the same transport.
	frame, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("transport: marshal: %w", err)
	}
	frame = append(frame, '\n')
	restore := armContextDeadline(ctx, t.conn.SetWriteDeadline)
	defer restore()
	n, err := t.conn.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A failed write may leave a partial JSON frame on the wire. Further
		// requests cannot safely reuse that connection.
		_ = t.conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func (t *connTransport) Receive(ctx context.Context) (*Message, error) {
	restore := armContextDeadline(ctx, t.conn.SetReadDeadline)
	defer restore()
	if !t.scanner.Scan() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err := t.scanner.Err(); err != nil {
			return nil, fmt.Errorf("transport: read: %w", err)
		}
		return nil, errors.New("transport: connection closed")
	}
	var msg Message
	if err := json.Unmarshal(t.scanner.Bytes(), &msg); err != nil {
		return nil, fmt.Errorf("transport: unmarshal: %w", err)
	}
	return &msg, nil
}

func armContextDeadline(ctx context.Context, setDeadline func(time.Time) error) func() {
	if deadline, ok := ctx.Deadline(); ok {
		_ = setDeadline(deadline)
	} else {
		_ = setDeadline(time.Time{})
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = setDeadline(time.Now())
		close(callbackDone)
	})
	return func() {
		if !stop() {
			<-callbackDone
		}
		_ = setDeadline(time.Time{})
	}
}

func (t *connTransport) Close() error {
	return t.conn.Close()
}

// Server accepts incoming agent connections.
type Server struct {
	addr      string
	handler   func(msg *Message) *Message
	mu        sync.Mutex
	listener  net.Listener // guarded by mu for Addr(); Accept/Close are goroutine-safe
	conns     []net.Conn
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

// NewServer creates a new server bound to the given address.
// Pass ":0" to let the OS pick a free port.
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen: %w", err)
	}
	return &Server{
		addr:     addr,
		listener: ln,
		done:     make(chan struct{}),
	}, nil
}

// OnMessage registers a handler that processes incoming messages and returns a response.
func (s *Server) OnMessage(handler func(msg *Message) *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

// Addr returns the actual listener address (useful when using ":0").
func (s *Server) Addr() string {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	return ln.Addr().String()
}

// Listen starts accepting connections. Blocks until the context is canceled or Close is called.
func (s *Server) Listen(ctx context.Context) error {
	// Snapshot listener under the lock so concurrent Close doesn't race on
	// the struct field. net.Listener.Accept and Close are goroutine-safe.
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				return fmt.Errorf("server: accept: %w", err)
			}
		}
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		default:
		}
		s.conns = append(s.conns, conn)
		s.wg.Add(1)
		s.mu.Unlock()

		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	defer s.removeConn(conn)

	t := newConnTransport(conn)
	for {
		msg, err := t.Receive(context.Background())
		if err != nil {
			return // connection closed or error
		}

		s.mu.Lock()
		h := s.handler
		s.mu.Unlock()

		if h != nil {
			resp := h(msg)
			if resp != nil {
				if err := t.Send(context.Background(), resp); err != nil {
					return
				}
			}
		}
	}
}

func (s *Server) removeConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, tracked := range s.conns {
		if tracked == conn {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			return
		}
	}
}

// Close shuts down the server and all active connections.
// It is safe to call Close concurrently or multiple times.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)

		// Close listener — unblocks Accept in Listen.
		s.mu.Lock()
		ln := s.listener
		s.mu.Unlock()
		s.closeErr = ln.Close()

		// Closing active connections unblocks handlers waiting in Receive. Listen
		// checks s.done while holding this same lock before every wg.Add, so after
		// this snapshot no new handler can be registered.
		s.mu.Lock()
		conns := append([]net.Conn(nil), s.conns...)
		s.conns = nil
		s.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		s.wg.Wait()
	})
	return s.closeErr
}

// Client connects to a remote agent server.
type Client struct {
	addr       string
	conn       net.Conn
	t          *connTransport
	mu         sync.Mutex
	pending    map[string]chan *Message
	streams    map[string]*clientStream
	closed     chan struct{}
	readClosed bool
	closeOnce  sync.Once
	closeErr   error
	readWg     sync.WaitGroup
}

// NewClient connects to the remote server at addr.
func NewClient(addr string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("client: dial: %w", err)
	}
	c := &Client{
		addr:    addr,
		conn:    conn,
		t:       newConnTransport(conn),
		pending: make(map[string]chan *Message),
		streams: make(map[string]*clientStream),
		closed:  make(chan struct{}),
	}
	c.readWg.Add(1)
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer c.readWg.Done()
	defer c.closeResponseChannels()
	for {
		msg, err := c.t.Receive(context.Background())
		if err != nil {
			return
		}

		c.mu.Lock()
		stream, isStream := c.streams[msg.ID]
		if isStream {
			// Delivery never blocks. Keep removal and channel closure under
			// the registry lock so observing a closed channel also permits
			// a subsequent registration without a stale duplicate entry.
			if !stream.deliver(msg) {
				delete(c.streams, msg.ID)
			}
			c.mu.Unlock()
			continue
		}
		pendingCh, isPending := c.pending[msg.ID]
		// Claim this response while holding the registry lock. A canceled
		// request may be replaced as soon as the lock is released.
		if isPending {
			delete(c.pending, msg.ID)
		}
		c.mu.Unlock()

		if isPending {
			pendingCh <- msg
			close(pendingCh)
		}
	}
}

func (c *Client) closeResponseChannels() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readClosed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	for id, stream := range c.streams {
		stream.close()
		delete(c.streams, id)
	}
}

func (c *Client) removeStream(id string, stream *clientStream) {
	c.mu.Lock()
	if c.streams[id] == stream {
		delete(c.streams, id)
	}
	c.mu.Unlock()
}

// Send sends a request and waits for a single response (request-response pattern).
func (c *Client) Send(ctx context.Context, msg *Message) (*Message, error) {
	if msg == nil {
		return nil, errors.New("client: nil message")
	}
	ch := make(chan *Message, 1)

	c.mu.Lock()
	if c.readClosed {
		c.mu.Unlock()
		return nil, errors.New("client: connection closed")
	}
	if c.pending[msg.ID] != nil || c.streams[msg.ID] != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("client: duplicate request ID %q", msg.ID)
	}
	c.pending[msg.ID] = ch
	c.mu.Unlock()

	if err := c.t.Send(ctx, msg); err != nil {
		c.mu.Lock()
		if c.pending[msg.ID] == ch {
			delete(c.pending, msg.ID)
		}
		c.mu.Unlock()
		return nil, fmt.Errorf("client: send: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		if c.pending[msg.ID] == ch {
			delete(c.pending, msg.ID)
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("client: connection closed")
		}
		return resp, nil
	}
}

// Stream sends a message and returns a channel that receives streaming responses.
// The channel closes on StreamEnd, context cancellation, connection loss, or
// overflow of its 64-message buffer. Buffered messages remain readable after
// closure. Completion is successful only if the consumer receives StreamEnd;
// closure without it indicates interruption. Cancellation retires the local
// stream; this protocol has no remote cancellation frame.
func (c *Client) Stream(ctx context.Context, msg *Message) (<-chan *Message, error) {
	if msg == nil {
		return nil, errors.New("client: nil message")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream := &clientStream{messages: make(chan *Message, 64)}

	c.mu.Lock()
	if c.readClosed {
		c.mu.Unlock()
		return nil, errors.New("client: connection closed")
	}
	if c.streams[msg.ID] != nil || c.pending[msg.ID] != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("client: duplicate stream ID %q", msg.ID)
	}
	c.streams[msg.ID] = stream
	id := msg.ID
	// Install cancellation while holding the registry lock. The callback and
	// reader cannot reach the stream before its stop function is initialized.
	stream.stop = context.AfterFunc(ctx, func() {
		c.removeStream(id, stream)
		stream.close()
	})
	c.mu.Unlock()

	request := *msg
	request.IsStream = true
	if err := c.t.Send(ctx, &request); err != nil {
		c.removeStream(id, stream)
		stream.close()
		return nil, fmt.Errorf("client: stream send: %w", err)
	}

	return stream.messages, nil
}

// Close terminates the client connection.
// It is safe to call Close concurrently or multiple times.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = c.conn.Close()
		c.readWg.Wait()
	})
	return c.closeErr
}
