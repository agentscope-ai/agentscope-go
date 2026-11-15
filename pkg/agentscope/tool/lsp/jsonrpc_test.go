package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func newBufReader(r io.Reader) *bufio.Reader {
	return bufio.NewReader(r)
}

func TestConnCallResponse(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	conn := NewConn(clientToServerW, serverToClientR)
	defer conn.Close()

	go func() {
		reader := newBufReader(clientToServerR)
		for {
			resp, err := readMessage(reader)
			if err != nil {
				serverToClientW.Close()
				return
			}
			result := map[string]string{"answer": "42"}
			resultData, _ := json.Marshal(result)
			response := Response{
				JSONRPC: "2.0",
				ID:      resp.ID,
				Result:  resultData,
			}
			data, _ := json.Marshal(response)
			writeMessage(serverToClientW, data)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result map[string]string
	err := conn.Call(ctx, "test/method", map[string]string{"key": "val"}, &result)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if result["answer"] != "42" {
		t.Fatalf("expected answer=42, got %v", result)
	}
}

func TestConnCallError(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	conn := NewConn(clientToServerW, serverToClientR)
	defer conn.Close()

	go func() {
		reader := newBufReader(clientToServerR)
		for {
			resp, err := readMessage(reader)
			if err != nil {
				serverToClientW.Close()
				return
			}
			response := Response{
				JSONRPC: "2.0",
				ID:      resp.ID,
				Error:   &ResponseError{Code: -32600, Message: "invalid request"},
			}
			data, _ := json.Marshal(response)
			writeMessage(serverToClientW, data)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := conn.Call(ctx, "test/error", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	respErr, ok := err.(*ResponseError)
	if !ok {
		t.Fatalf("expected *ResponseError, got %T", err)
	}
	if respErr.Code != -32600 {
		t.Fatalf("expected code -32600, got %d", respErr.Code)
	}
}

func TestConnCallContextCanceled(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	go io.Copy(io.Discard, clientToServerR)
	serverToClientR, serverToClientW := io.Pipe()

	conn := NewConn(clientToServerW, serverToClientR)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := conn.Call(ctx, "test/method", nil, nil)
	if err == nil {
		t.Fatal("expected context canceled error")
	}

	serverToClientW.Close()
	conn.Close()
}

func TestWriteAndReadMessage(t *testing.T) {
	r, w := io.Pipe()

	msg := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	go func() {
		writeMessage(w, []byte(msg))
		w.Close()
	}()

	resp, err := readMessage(newBufReader(r))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if resp.ID != 1 {
		t.Fatalf("expected ID=1, got %d", resp.ID)
	}
}

func TestNotify(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()

	conn := &Conn{
		writer:  clientToServerW,
		pending: make(map[int]chan *Response),
		done:    make(chan struct{}),
		closer:  clientToServerW,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := readMessage(newBufReader(clientToServerR))
		if err != nil {
			t.Errorf("readMessage: %v", err)
			return
		}
		if resp.ID != 0 {
			t.Errorf("expected notification (id=0), got id=%d", resp.ID)
		}
	}()

	if err := conn.Notify("test/notification", map[string]string{"key": "val"}); err != nil {
		t.Fatalf("Notify failed: %v", err)
	}
	<-done
}
