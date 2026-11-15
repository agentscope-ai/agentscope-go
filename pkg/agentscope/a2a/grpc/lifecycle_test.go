package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func newPipeClient(t *testing.T) (*Client, *connTransport) {
	t.Helper()
	peer, conn := net.Pipe()
	c := &Client{
		conn:    conn,
		t:       newConnTransport(conn),
		pending: make(map[string]chan *Message),
		streams: make(map[string]*clientStream),
		closed:  make(chan struct{}),
	}
	c.readWg.Add(1)
	go c.readLoop()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = c.Close()
	})
	return c, newConnTransport(peer)
}

func TestStreamCancellationReleasesIdleStream(t *testing.T) {
	c, peer := newPipeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		if _, err := peer.Receive(ctx); err != nil {
			serverDone <- err
			return
		}
		req, err := peer.Receive(ctx)
		if err == nil {
			err = peer.Send(ctx, &Message{ID: req.ID, Method: "ack"})
		}
		serverDone <- err
	}()
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	ch, err := c.Stream(streamCtx, &Message{ID: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("idle stream unexpectedly produced a message")
		}
	case <-ctx.Done():
		t.Fatal("cancel did not close the idle stream")
	}
	c.mu.Lock()
	registered := c.streams["idle"] != nil
	c.mu.Unlock()
	if registered {
		t.Fatal("canceled stream remains registered")
	}
	if _, err := c.Send(ctx, &Message{ID: "ordinary"}); err != nil {
		t.Fatalf("stream cancellation broke the connection: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSlowStreamDoesNotBlockOtherRequests(t *testing.T) {
	c, peer := newPipeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	produced := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		if _, err := peer.Receive(ctx); err != nil {
			produced <- err
			return
		}
		for i := 0; i < 80; i++ {
			if err := peer.Send(ctx, &Message{ID: "slow", Method: "chunk"}); err != nil {
				produced <- err
				return
			}
		}
		produced <- nil
		req, err := peer.Receive(ctx)
		if err == nil {
			err = peer.Send(ctx, &Message{ID: req.ID, Method: "ack"})
		}
		serverDone <- err
	}()
	ch, err := c.Stream(ctx, &Message{ID: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-produced; err != nil {
		t.Fatalf("slow stream blocked the shared reader: %v", err)
	}
	if _, err := c.Send(ctx, &Message{ID: "ordinary"}); err != nil {
		t.Fatalf("ordinary request blocked behind a slow stream: %v", err)
	}
	count := 0
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				if count != 64 {
					t.Fatalf("received %d buffered messages, want 64", count)
				}
				if err := <-serverDone; err != nil {
					t.Fatal(err)
				}
				return
			}
			count++
			if msg.StreamEnd {
				t.Fatal("overflow must not masquerade as successful stream completion")
			}
		case <-ctx.Done():
			t.Fatal("overflowed stream did not close")
		}
	}
}

func TestPeerClosePreservesQueuedStreamEnd(t *testing.T) {
	c, peer := newPipeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		if _, err := peer.Receive(ctx); err != nil {
			return
		}
		_ = peer.Send(ctx, &Message{ID: "complete", StreamEnd: true})
		_ = peer.Close()
	}()
	ch, err := c.Stream(ctx, &Message{ID: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ch:
		if msg == nil || !msg.StreamEnd {
			t.Fatal("peer close lost the already-received final message")
		}
	case <-ctx.Done():
		t.Fatal("stream did not finish")
	}
}

func TestStreamCancellationRacesWithDeliveryAndClientClose(t *testing.T) {
	for i := 0; i < 25; i++ {
		c, peer := newPipeClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		streamCtx, stop := context.WithCancel(ctx)
		ready := make(chan struct{})
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			if _, err := peer.Receive(ctx); err != nil {
				return
			}
			<-ready
			_ = peer.Send(ctx, &Message{ID: "race", StreamEnd: true})
		}()
		ch, err := c.Stream(streamCtx, &Message{ID: "race"})
		if err != nil {
			stop()
			cancel()
			close(ready)
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-ready
			stop()
		}()
		go func() {
			defer wg.Done()
			<-ready
			_ = c.Close()
		}()
		close(ready)
		wg.Wait()
		for range ch {
		}
		c.mu.Lock()
		remaining := len(c.streams)
		c.mu.Unlock()
		if remaining != 0 {
			t.Errorf("iteration %d retained %d stream registrations", i, remaining)
		}
		<-serverDone
		cancel()
	}
}

type writeSignalConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

func TestSendCancellationWhileWaitingForWriter(t *testing.T) {
	peer, conn := net.Pipe()
	defer func() { _ = peer.Close() }()
	defer func() { _ = conn.Close() }()
	signaled := &writeSignalConn{Conn: conn, entered: make(chan struct{})}
	transport := newConnTransport(signaled)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- transport.Send(ctx, &Message{ID: "first"}) }()
	select {
	case <-signaled.entered:
	case <-ctx.Done():
		t.Fatal("first write did not start")
	}
	waitCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err := transport.Send(waitCtx, &Message{ID: "canceled"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting sender error = %v", err)
	}
	if _, err := newConnTransport(peer).Receive(ctx); err != nil {
		t.Fatalf("waiting sender cancellation broke the active write: %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestSerializationErrorKeepsConnectionUsable(t *testing.T) {
	c, peer := newPipeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		req, err := peer.Receive(ctx)
		if err == nil {
			err = peer.Send(ctx, &Message{ID: req.ID, Method: "ack"})
		}
		serverDone <- err
	}()
	if _, err := c.Send(ctx, &Message{ID: "invalid", Payload: json.RawMessage("{")}); err == nil {
		t.Fatal("invalid JSON message was accepted")
	}
	if _, err := c.Send(ctx, &Message{ID: "valid"}); err != nil {
		t.Fatalf("local serialization error damaged the connection: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type partialFailureConn struct{ net.Conn }

func (c *partialFailureConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p[:1])
	if err != nil {
		return n, err
	}
	return n, io.ErrUnexpectedEOF
}

func TestFailedPartialWriteClosesConnection(t *testing.T) {
	peer, conn := net.Pipe()
	defer func() { _ = peer.Close() }()
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	readDone := make(chan int, 1)
	go func() {
		data, _ := io.ReadAll(peer)
		readDone <- len(data)
	}()
	err := newConnTransport(&partialFailureConn{Conn: conn}).Send(ctx, &Message{ID: "partial"})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Send error = %v", err)
	}
	select {
	case n := <-readDone:
		if n != 1 {
			t.Fatalf("peer received %d bytes, want one partial byte", n)
		}
	case <-ctx.Done():
		t.Fatal("partial write left the damaged connection open")
	}
}
