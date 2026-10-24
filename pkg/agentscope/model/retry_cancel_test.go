package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type retryTestTransport func(*http.Request) (*http.Response, error)

func (f retryTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func retryTestModel(t *testing.T, provider string, transport retryTestTransport) ChatModel {
	t.Helper()
	client := &http.Client{Transport: transport}
	var m ChatModel
	var err error
	switch provider {
	case "openai":
		m, err = NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture", HTTPClient: client})
	case "anthropic":
		m, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "fixture", Model: "fixture", HTTPClient: client})
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func retryTestResponse(provider string, status int) *http.Response {
	body := `{"error":{"message":"temporarily unavailable"}}`
	if status == http.StatusOK {
		body = `{"id":"reply","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		if provider == "anthropic" {
			body = `{"id":"reply","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
		}
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Retry-After": {"1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestProviderRetryWaitCancellation(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, delay := range []struct {
			name  string
			value time.Duration
		}{{"explicit", time.Hour}, {"default", 0}} {
			for _, mode := range []string{"cancel", "deadline"} {
				t.Run(provider+"/"+delay.name+"/"+mode, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						wantErr := context.Canceled
						if mode == "deadline" {
							cancel()
							ctx, cancel = context.WithTimeout(context.Background(), 2500*time.Millisecond)
							wantErr = context.DeadlineExceeded
						}
						defer cancel()
						var calls atomic.Int32
						m := retryTestModel(t, provider, func(*http.Request) (*http.Response, error) {
							calls.Add(1)
							return retryTestResponse(provider, http.StatusServiceUnavailable), nil
						})
						done := make(chan error, 1)
						go func() {
							_, err := m.Chat(ctx, []*message.Msg{message.UserMsg("user", "hello")}, WithRetries(1, delay.value))
							done <- err
						}()

						// Retry-After: 1 makes the inner HTTP retries take exactly two
						// seconds. Wait until those finish and Chat is in its own wait.
						time.Sleep(2 * time.Second)
						synctest.Wait()
						if got := calls.Load(); got != 3 {
							t.Fatalf("HTTP calls before provider wait = %d, want 3", got)
						}
						if mode == "deadline" {
							time.Sleep(500 * time.Millisecond)
						} else {
							cancel()
						}
						stopped := time.Now()
						err := <-done
						if !errors.Is(err, wantErr) {
							t.Errorf("Chat error = %v, want %v", err, wantErr)
						}
						if elapsed := time.Since(stopped); elapsed != 0 {
							t.Errorf("Chat waited %v after cancellation", elapsed)
						}
						if got := calls.Load(); got != 3 {
							t.Errorf("HTTP calls after cancellation = %d, want 3", got)
						}
					})
				})
			}
		}
	}
}

func TestProviderRetryCanceledBeforeWait(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, delay := range []time.Duration{time.Hour, 0, -time.Second} {
			t.Run(provider+"/"+delay.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var calls int
					m := retryTestModel(t, provider, func(*http.Request) (*http.Response, error) {
						calls++
						if calls == 3 {
							cancel()
							return retryTestResponse(provider, http.StatusServiceUnavailable), nil
						}
						if calls > 3 {
							return retryTestResponse(provider, http.StatusOK), nil
						}
						return retryTestResponse(provider, http.StatusServiceUnavailable), nil
					})
					started := time.Now()
					_, err := m.Chat(ctx, []*message.Msg{message.UserMsg("user", "hello")}, WithRetries(1, delay))
					if !errors.Is(err, context.Canceled) {
						t.Errorf("Chat error = %v, want context.Canceled", err)
					}
					if elapsed := time.Since(started); elapsed != 2*time.Second {
						t.Errorf("Chat took %v, want only the two inner retry delays", elapsed)
					}
					if calls != 3 {
						t.Errorf("HTTP calls = %d, want 3", calls)
					}
				})
			})
		}
	}
}

func TestProviderRetryWaitControls(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, tc := range []struct {
			name       string
			failStatus int
			failCalls  int
			retries    int
			delay      time.Duration
			wantCalls  int
			wantTime   time.Duration
			wantErr    bool
		}{
			{"immediate_success", 503, 0, 1, time.Second, 1, 0, false},
			{"retry_success", 503, 3, 1, 2 * time.Second, 4, 4 * time.Second, false},
			{"default_delay", 503, 3, 1, 0, 4, 3 * time.Second, false},
			{"negative_delay", 503, 3, 1, -time.Second, 4, 2 * time.Second, false},
			{"permanent_error", 400, 9, 2, time.Second, 1, 0, true},
			{"exhausted", 503, 9, 1, time.Second, 6, 5 * time.Second, true},
			{"no_provider_retry", 503, 9, 0, time.Hour, 3, 2 * time.Second, true},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var calls int
					m := retryTestModel(t, provider, func(*http.Request) (*http.Response, error) {
						calls++
						if calls <= tc.failCalls {
							return retryTestResponse(provider, tc.failStatus), nil
						}
						return retryTestResponse(provider, http.StatusOK), nil
					})
					started := time.Now()
					response, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("user", "hello")}, WithRetries(tc.retries, tc.delay))
					if (err != nil) != tc.wantErr {
						t.Fatalf("Chat error = %v, want error %v", err, tc.wantErr)
					}
					if !tc.wantErr && response == nil {
						t.Error("successful Chat returned no response")
					}
					if calls != tc.wantCalls || time.Since(started) != tc.wantTime {
						t.Errorf("calls/time = %d/%v, want %d/%v", calls, time.Since(started), tc.wantCalls, tc.wantTime)
					}
				})
			})
		}
	}
}

func TestProviderChatAlreadyCanceled(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var calls int
			m := retryTestModel(t, provider, func(*http.Request) (*http.Response, error) {
				calls++
				return retryTestResponse(provider, http.StatusOK), nil
			})
			_, err := m.Chat(ctx, []*message.Msg{message.UserMsg("user", "hello")}, WithRetries(1, time.Second))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Chat error = %v, want context.Canceled", err)
			}
			if calls != 0 {
				t.Errorf("HTTP calls with canceled context = %d, want 0", calls)
			}
		})
	}
}

func TestProviderChatNilContext(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var calls int
			m := retryTestModel(t, provider, func(*http.Request) (*http.Response, error) {
				calls++
				return retryTestResponse(provider, http.StatusOK), nil
			})
			_, err := m.Chat(nil, []*message.Msg{message.UserMsg("user", "hello")}) //nolint:staticcheck // Exercise explicit nil-context validation.
			if err == nil || calls != 0 {
				t.Errorf("nil context: error = %v, HTTP calls = %d; want error and no calls", err, calls)
			}
		})
	}
}
