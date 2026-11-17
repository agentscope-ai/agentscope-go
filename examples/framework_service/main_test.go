package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agenttest"
)

// testAgent builds an agent on a scripted model, so the endpoints can be
// exercised without any provider credentials or network access.
func testAgent(t *testing.T) *agent.UnifiedAgent {
	t.Helper()
	cm := agenttest.NewMockModel(
		agenttest.Default(agenttest.RespondWithText("It is sunny in Tokyo.")),
	)
	return newAgent(cm, newWeatherTool())
}

func TestGinEndpoints(t *testing.T) {
	handler := newGinEngine(testAgent(t), newMCPHandler(newWeatherTool()))
	assertAgentEndpoints(t, handler)
}

func TestEchoEndpoints(t *testing.T) {
	handler := newEchoRouter(testAgent(t), newMCPHandler(newWeatherTool()))
	assertAgentEndpoints(t, handler)
}

// assertAgentEndpoints exercises the routes every framework mount must serve:
// the SSE chat stream and the MCP tool endpoints under /mcp.
func assertAgentEndpoints(t *testing.T, handler http.Handler) {
	t.Helper()

	t.Run("chat stream replies over SSE", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/chat/stream?message=weather", nil)
		handler.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("chat stream status = %d, want %d", res.StatusCode, http.StatusOK)
		}
		if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read stream body: %v", err)
		}
		if !strings.Contains(string(body), "data:") {
			t.Errorf("stream body has no SSE data frames: %q", body)
		}
		if !strings.Contains(string(body), "sunny") {
			t.Errorf("stream body missing reply text: %q", body)
		}
	})

	t.Run("chat stream requires message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/chat/stream", nil)
		handler.ServeHTTP(rec, req)
		if rec.Result().StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Result().StatusCode, http.StatusBadRequest)
		}
	})

	t.Run("mcp health", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/health", nil))
		if rec.Result().StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Result().StatusCode, http.StatusOK)
		}
	})

	t.Run("mcp lists tools", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/tools", nil))
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "get_weather") {
			t.Errorf("tools list missing get_weather: %q", body)
		}
	})

	t.Run("mcp calls tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp/tools/get_weather",
			strings.NewReader(`{"location":"Tokyo"}`))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)

		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "sunny") {
			t.Errorf("tool response missing fixture payload: %q", body)
		}
	})
}
