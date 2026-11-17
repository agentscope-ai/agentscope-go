package tts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashScopeStreamFailureIsObservable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	m := NewDashScopeRealtimeTTS(&DashScopeRealtimeConfig{APIKey: "test", BaseURL: server.URL})
	ch, err := m.SynthesizeStream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for resp := range ch {
		count++
		if resp.Err == nil || !strings.Contains(resp.Err.Error(), "HTTP 500") || !resp.IsLast || len(resp.Content) != 0 {
			t.Fatalf("bad error response: %+v", resp)
		}
	}
	if count == 0 {
		t.Fatal("HTTP 500 silently closed the stream without any result or error")
	}
	if count != 1 {
		t.Fatalf("got %d responses", count)
	}
}

type failingTTSTransport func(*http.Request) (*http.Response, error)

func (f failingTTSTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingTTSBody struct{ err error }

func (b failingTTSBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingTTSBody) Close() error             { return nil }

func TestDashScopeStreamErrorCauses(t *testing.T) {
	sentinel := errors.New("fixture failure")
	for _, kind := range []string{"transport", "body", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			m := NewDashScopeRealtimeTTS(&DashScopeRealtimeConfig{APIKey: "test"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := sentinel
			if kind == "cancel" {
				cancel()
				want = context.Canceled
			}
			m.client.Transport = failingTTSTransport(func(r *http.Request) (*http.Response, error) {
				if kind == "cancel" {
					return nil, r.Context().Err()
				}
				if kind == "transport" {
					return nil, sentinel
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: failingTTSBody{sentinel}}, nil
			})
			ch, err := m.SynthesizeStream(ctx, "hello")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for resp := range ch {
				count++
				if !errors.Is(resp.Err, want) || !resp.IsLast || len(resp.Content) != 0 {
					t.Fatalf("bad failure: %+v", resp)
				}
			}
			if count != 1 {
				t.Fatalf("got %d results", count)
			}
		})
	}
}

func TestDashScopeStreamSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/test")
		_, _ = io.WriteString(w, "audio")
	}))
	defer server.Close()
	m := NewDashScopeRealtimeTTS(&DashScopeRealtimeConfig{APIKey: "test", BaseURL: server.URL})
	ch, err := m.SynthesizeStream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for resp := range ch {
		count++
		if resp.Err != nil || !resp.IsLast || string(resp.Content) != "audio" || resp.MediaType != "audio/test" {
			t.Fatalf("bad success: %+v", resp)
		}
	}
	if count != 1 {
		t.Fatalf("got %d results", count)
	}
}
