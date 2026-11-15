package dingtalk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/channel"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func newTestChannel(t *testing.T) *Channel {
	t.Helper()
	c, err := NewChannel(Config{
		ClientID:     "cid",
		ClientSecret: model.NewSecretStr("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewChannel_RequiresCredentials(t *testing.T) {
	if _, err := NewChannel(Config{}); err == nil {
		t.Fatal("missing credentials must error")
	}
	c, err := NewChannel(Config{ClientID: "x", ClientSecret: model.NewSecretStr("y")})
	if err != nil {
		t.Fatal(err)
	}
	if c.ChannelID() != ChannelType || c.Type() != ChannelType {
		t.Errorf("ids = %s/%s", c.Type(), c.ChannelID())
	}
	if !c.Capabilities().Markdown || c.Capabilities().Streaming {
		t.Errorf("v1 capabilities wrong: %+v", c.Capabilities())
	}
}

func TestHandleBotMessage_NormalisesAndFilters(t *testing.T) {
	c := newTestChannel(t)
	var mu sync.Mutex
	var got []*channel.Event
	emit := func(_ context.Context, in channel.Inbound) error {
		mu.Lock()
		got = append(got, in.Message)
		mu.Unlock()
		return nil
	}

	// Group message without @ → dropped (OnlyAtReply default true).
	c.handleBotMessage(context.Background(), &chatbot.BotCallbackDataModel{
		ConversationType: "2",
		Text:             chatbot.BotCallbackDataTextModel{Content: " hi "},
	}, emit)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("group message without @ must be dropped, got %d", n)
	}

	// Group message with @ → normalised.
	c.handleBotMessage(context.Background(), &chatbot.BotCallbackDataModel{
		ConversationType:          "2",
		ConversationId:            "conv-1",
		ConversationTitle:         "Team Chat",
		SenderStaffId:             "staff-1",
		SenderNick:                "Alice",
		MsgId:                     "msg-1",
		IsInAtList:                true,
		SessionWebhook:            "https://example/webhook",
		SessionWebhookExpiredTime: time.Now().Add(time.Hour).UnixMilli(),
		Text:                      chatbot.BotCallbackDataTextModel{Content: " hello bot "},
	}, emit)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	ev := got[0]
	if ev.Text != "hello bot" || ev.ChatID != "conv-1" || ev.Kind != channel.ChatKindGroup ||
		ev.ChannelUserID != "staff-1" || ev.ChannelUserName != "Alice" || ev.ChannelMessageID != "msg-1" {
		t.Errorf("bad normalisation: %+v", ev)
	}
	if url, ok := c.webhookFor("conv-1"); !ok || url != "https://example/webhook" {
		t.Errorf("webhook not stored: %q %v", url, ok)
	}
}

func TestHandleBotMessage_PrivateChatNoAtNeeded(t *testing.T) {
	c := newTestChannel(t)
	var got int
	emit := func(_ context.Context, _ channel.Inbound) error { got++; return nil }
	c.handleBotMessage(context.Background(), &chatbot.BotCallbackDataModel{
		ConversationType: "1",
		ConversationId:   "priv-1",
		Text:             chatbot.BotCallbackDataTextModel{Content: "hi"},
	}, emit)
	if got != 1 {
		t.Fatalf("private chat must pass without @, got %d", got)
	}
}

func TestWebhookExpiry(t *testing.T) {
	c := newTestChannel(t)
	c.rememberWebhook("conv-x", "https://example/x", time.Now().Add(-time.Minute).UnixMilli())
	if _, ok := c.webhookFor("conv-x"); ok {
		t.Fatal("expired webhook must not be used")
	}
}

func TestSendResponse_RendersTextAndConfirmPrompt(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestChannel(t)
	c.rememberWebhook("conv-1", srv.URL, time.Now().Add(time.Hour).UnixMilli())

	events := make(chan event.Event, 8)
	go func() {
		defer close(events)
		events <- event.NewReplyStartEvent("sess", "r1", "bot", message.RoleAssistant)
		events <- event.NewRequireUserConfirmEvent("r1", []message.ToolCallBlock{
			{Type: "tool_call", ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, State: message.ToolCallAsking},
		})
		events <- event.NewTextBlockDeltaEvent("r1", "blk", "the answer")
		events <- event.NewReplyEndEvent("sess", "r1")
	}()

	if err := c.SendResponse(context.Background(), channel.Response{
		ChatID: "conv-1", Kind: channel.ChatKindGroup, Events: events,
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 sends (confirm prompt + final), got %d", len(bodies))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatal(err)
	}
	md, _ := first["markdown"].(map[string]any)
	if md == nil || !strings.Contains(md["text"].(string), "Tool approval") {
		t.Errorf("first send must be the confirm prompt, got %v", first)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatal(err)
	}
	md2, _ := second["markdown"].(map[string]any)
	if md2 == nil || !strings.Contains(md2["text"].(string), "the answer") {
		t.Errorf("second send must carry the reply text, got %v", second)
	}
}

func TestSendResponse_NoWebhookErrors(t *testing.T) {
	c := newTestChannel(t)
	events := make(chan event.Event)
	close(events)
	err := c.SendResponse(context.Background(), channel.Response{ChatID: "nowhere", Events: events})
	if err == nil || !strings.Contains(err.Error(), "no live session webhook") {
		t.Fatalf("expected webhook error, got %v", err)
	}
}

func TestNotify(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestChannel(t)
	c.rememberWebhook("conv-1", srv.URL, time.Now().Add(time.Hour).UnixMilli())
	if err := c.Notify(context.Background(), "conv-1", channel.ChatKindGroup, "heads up"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("notify must hit the webhook once, got %d", hits)
	}
}

func TestLongMessagesSplit(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, err := NewChannel(Config{ClientID: "x", ClientSecret: model.NewSecretStr("y"), MaxMessageLength: 50})
	if err != nil {
		t.Fatal(err)
	}
	c.rememberWebhook("conv-1", srv.URL, time.Now().Add(time.Hour).UnixMilli())
	events := make(chan event.Event, 4)
	events <- event.NewTextBlockDeltaEvent("r1", "blk", strings.Repeat("x ", 80))
	close(events)
	if err := c.SendResponse(context.Background(), channel.Response{ChatID: "conv-1", Events: events}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits < 3 {
		t.Fatalf("160-char reply at max 50 must split into >=3 sends, got %d", hits)
	}
}
