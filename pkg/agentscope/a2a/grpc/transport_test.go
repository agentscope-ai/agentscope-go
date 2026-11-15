package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func startTestServer(t *testing.T, handler func(msg *Message) *Message) *Server {
	t.Helper()
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.OnMessage(handler)

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		_ = srv.Listen(context.Background())
	}()
	// Give the server a moment to start accepting.
	time.Sleep(20 * time.Millisecond)

	t.Cleanup(func() {
		_ = srv.Close()
		<-listenDone // wait for Listen goroutine to exit
	})

	return srv
}

func TestServerStartsAndAccepts(t *testing.T) {
	srv := startTestServer(t, func(msg *Message) *Message {
		return &Message{ID: msg.ID, From: "server", Method: "ack"}
	})

	if srv.Addr() == "" {
		t.Fatal("expected non-empty address")
	}

	client, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_ = client.Close()
}

func TestRequestResponse(t *testing.T) {
	srv := startTestServer(t, func(msg *Message) *Message {
		return &Message{
			ID:      msg.ID,
			From:    "server",
			To:      msg.From,
			Method:  "agent.Reply",
			Payload: json.RawMessage(`{"echo":"hello"}`),
		}
	})

	client, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := client.Send(ctx, &Message{
		ID:        "req-1",
		From:      "agent-a",
		To:        "agent-b",
		Method:    "agent.Ask",
		Payload:   json.RawMessage(`{"question":"hi"}`),
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.ID != "req-1" {
		t.Errorf("expected response ID req-1, got %s", resp.ID)
	}
	if resp.From != "server" {
		t.Errorf("expected From=server, got %s", resp.From)
	}
}

func TestClientSendServerReceives(t *testing.T) {
	received := make(chan *Message, 1)
	srv := startTestServer(t, func(msg *Message) *Message {
		received <- msg
		return &Message{ID: msg.ID, From: "server", Method: "ack"}
	})

	client, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sent := &Message{
		ID:     "msg-42",
		From:   "client-x",
		To:     "agent-y",
		Method: "agent.Process",
	}
	_, err = client.Send(ctx, sent)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case msg := <-received:
		if msg.ID != "msg-42" {
			t.Errorf("server received wrong ID: %s", msg.ID)
		}
		if msg.From != "client-x" {
			t.Errorf("server received wrong From: %s", msg.From)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive message")
	}
}

func TestMultipleConcurrentClients(t *testing.T) {
	srv := startTestServer(t, func(msg *Message) *Message {
		return &Message{
			ID:      msg.ID,
			From:    "server",
			Method:  "reply",
			Payload: msg.Payload,
		}
	})

	const numClients = 10
	var wg sync.WaitGroup
	errs := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			client, err := NewClient(srv.Addr())
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", idx, err)
				return
			}
			defer func() { _ = client.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			id := fmt.Sprintf("concurrent-%d", idx)
			resp, err := client.Send(ctx, &Message{
				ID:      id,
				From:    fmt.Sprintf("agent-%d", idx),
				Method:  "agent.Ping",
				Payload: json.RawMessage(fmt.Sprintf(`{"idx":%d}`, idx)),
			})
			if err != nil {
				errs <- fmt.Errorf("client %d send: %w", idx, err)
				return
			}
			if resp.ID != id {
				errs <- fmt.Errorf("client %d: expected ID %s, got %s", idx, id, resp.ID)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCleanClose(t *testing.T) {
	srv := startTestServer(t, func(msg *Message) *Message {
		return &Message{ID: msg.ID, Method: "ack"}
	})

	client, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Send one message to confirm connection works
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = client.Send(ctx, &Message{ID: "close-test", Method: "ping"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Close should not error
	if err := client.Close(); err != nil {
		t.Fatalf("client.Close: %v", err)
	}
}

func TestServerShutdownStopsAccepting(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.OnMessage(func(msg *Message) *Message {
		return &Message{ID: msg.ID, Method: "ack"}
	})

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		_ = srv.Listen(context.Background())
	}()
	time.Sleep(20 * time.Millisecond)

	addr := srv.Addr()

	// Close server
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-listenDone // wait for Listen to return

	// Give OS a moment to release the port
	time.Sleep(50 * time.Millisecond)

	// New connections should fail
	_, err = NewClient(addr)
	if err == nil {
		t.Fatal("expected error connecting to closed server")
	}
}

func TestServerCloseUnblocksIdleConnections(t *testing.T) {
	srv := startTestServer(t, nil)
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked on an idle connection")
	}
}

func TestConnTransportReceiveHonorsContextCancellation(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newConnTransport(clientConn).Receive(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive error = %v, want context.Canceled", err)
	}
}

func TestResponseClaimDoesNotDeleteReplacement(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	// An unbuffered old receiver pauses delivery after the response is claimed,
	// making replacement of its registry entry deterministic.
	old := make(chan *Message)
	c := &Client{
		conn:    clientConn,
		t:       newConnTransport(clientConn),
		pending: map[string]chan *Message{"retry": old},
		streams: make(map[string]*clientStream),
		closed:  make(chan struct{}),
	}
	c.readWg.Add(1)
	go c.readLoop()
	t.Cleanup(func() {
		// Release a blocked old delivery even when an assertion fails.
		go func() {
			for range old {
			}
		}()
		_ = c.Close()
	})
	encoder := json.NewEncoder(serverConn)
	if err := encoder.Encode(&Message{ID: "retry", Method: "old"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		claimed := c.pending["retry"] == nil
		c.mu.Unlock()
		if claimed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("response must be removed from the registry before delivery")
		}
		time.Sleep(time.Millisecond)
	}
	replacement := make(chan *Message, 1)
	c.mu.Lock()
	c.pending["retry"] = replacement
	c.mu.Unlock()
	<-old
	if err := encoder.Encode(&Message{ID: "retry", Method: "new"}); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-replacement:
		if resp == nil || resp.Method != "new" {
			t.Fatalf("wrong retry response: %v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("old response removed the replacement request")
	}
}

func TestClientRejectsCrossModeDuplicateIDs(t *testing.T) {
	c := &Client{
		pending: make(map[string]chan *Message),
		streams: map[string]*clientStream{"shared": {messages: make(chan *Message, 1)}},
	}
	if _, err := c.Send(context.Background(), &Message{ID: "shared"}); err == nil {
		t.Fatal("Send accepted an active stream ID")
	}
	delete(c.streams, "shared")
	c.pending["shared"] = make(chan *Message, 1)
	if _, err := c.Stream(context.Background(), &Message{ID: "shared"}); err == nil {
		t.Fatal("Stream accepted an active request ID")
	}
}

func TestStreaming(t *testing.T) {
	// Create a server that manually streams responses
	ln, err := listenAndServeStreaming(t)
	if err != nil {
		t.Fatalf("streaming server: %v", err)
	}
	defer func() { _ = ln.Close() }()

	client, err := NewClient(ln.Addr().String())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := client.Stream(ctx, &Message{
		ID:     "stream-1",
		From:   "agent-a",
		Method: "agent.Stream",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var messages []*Message
	for msg := range ch {
		messages = append(messages, msg)
	}

	if len(messages) != 3 {
		t.Fatalf("expected 3 stream messages, got %d", len(messages))
	}
	if !messages[2].StreamEnd {
		t.Error("expected last message to have StreamEnd=true")
	}
}

// listenAndServeStreaming creates a TCP server that handles streaming requests.
func listenAndServeStreaming(t *testing.T) (net.Listener, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				transport := newConnTransport(c)
				for {
					msg, err := transport.Receive(context.Background())
					if err != nil {
						return
					}
					if msg.IsStream {
						// Send 3 streaming messages
						for i := 0; i < 3; i++ {
							resp := &Message{
								ID:        msg.ID,
								From:      "server",
								Method:    "agent.StreamChunk",
								Payload:   json.RawMessage(fmt.Sprintf(`{"chunk":%d}`, i)),
								StreamEnd: i == 2,
							}
							if err := transport.Send(context.Background(), resp); err != nil {
								return
							}
						}
					} else {
						resp := &Message{ID: msg.ID, From: "server", Method: "ack"}
						if err := transport.Send(context.Background(), resp); err != nil {
							return
						}
					}
				}
			}(conn)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	return ln, nil
}
