// Package dingtalk connects agents to DingTalk as an enterprise robot.
//
// The official DingTalk Stream SDK owns the long-lived inbound connection
// (client id/secret, no public endpoint needed); replies go back through
// the per-message session webhook delivered with each inbound message.
//
// v1 scope (vs Python agentscope's DingTalk channel): text/Markdown
// replies and text-mode tool confirmation (reply y / n / a). AI-card
// streaming, media upload/download and user search are not ported yet.
package dingtalk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dingclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/channel"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/logging"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const (
	// ChannelType is the platform type id.
	ChannelType = "dingtalk"

	conversationGroup   = "2"
	defaultMaxLen       = 4000
	noTextReply         = "(Agent returned no text content)"
	agentErrorReply     = "❌ Agent encountered an error. Please check the agent configuration."
	maxItersNotice      = "⚠ Exceeded the maximum reasoning-acting iterations."
	confirmPromptHeader = "🔐 **Tool approval needed** — reply **y** (allow), **n** (deny) or **a** (always allow)"
)

// Config builds a Channel.
type Config struct {
	// ChannelID identifies this channel instance (default "dingtalk").
	ChannelID string
	// ClientID is the DingTalk application AppKey.
	ClientID string
	// ClientSecret is the DingTalk application AppSecret.
	ClientSecret model.SecretStr
	// ReplyWithoutAt: respond to group messages even when the bot is
	// not @-mentioned. Default false — in groups the bot only answers
	// messages that @ it (Python parity: only_at_reply defaults true).
	ReplyWithoutAt bool
	// MaxMessageLength bounds one outbound message (default 4000).
	MaxMessageLength int
}

type webhookEntry struct {
	url       string
	expiresAt time.Time
}

// Channel implements channel.Channel for DingTalk.
type Channel struct {
	cfg        Config
	capability channel.Capability

	mu       sync.Mutex
	status   channel.Status
	webhooks map[string]webhookEntry
	cancel   context.CancelFunc
	client   *dingclient.StreamClient
}

// NewChannel creates the DingTalk channel.
func NewChannel(cfg Config) (*Channel, error) {
	if cfg.ClientID == "" || cfg.ClientSecret.IsEmpty() {
		return nil, fmt.Errorf("dingtalk: ClientID and ClientSecret are required")
	}
	if cfg.ChannelID == "" {
		cfg.ChannelID = ChannelType
	}
	if cfg.MaxMessageLength <= 0 {
		cfg.MaxMessageLength = defaultMaxLen
	}
	return &Channel{
		cfg: cfg,
		capability: channel.Capability{
			Text:             true,
			Markdown:         true,
			Interactive:      false, // v1: text-mode confirmations
			Streaming:        false, // v1: no AI-card streaming
			MaxMessageLength: cfg.MaxMessageLength,
		},
		status:   channel.Status{State: channel.StatusStopped},
		webhooks: map[string]webhookEntry{},
	}, nil
}

// Type implements channel.Channel.
func (c *Channel) Type() string { return ChannelType }

// ChannelID implements channel.Channel.
func (c *Channel) ChannelID() string { return c.cfg.ChannelID }

// Capabilities implements channel.Channel.
func (c *Channel) Capabilities() channel.Capability { return c.capability }

// Status implements channel.Channel.
func (c *Channel) Status() channel.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Channel) setStatus(state channel.StatusState, lastErr string) {
	c.mu.Lock()
	c.status = channel.Status{State: state, LastError: lastErr}
	c.mu.Unlock()
}

// StartListening launches the DingTalk Stream connection and routes
// normalised inbound events to emit. It returns once the listener
// goroutine is launched; the SDK reconnects internally.
func (c *Channel) StartListening(ctx context.Context, emit channel.EmitFunc) error {
	if emit == nil {
		return fmt.Errorf("dingtalk: nil emit callback")
	}
	cred := dingclient.NewAppCredentialConfig(c.cfg.ClientID, c.cfg.ClientSecret.Value())
	cli := dingclient.NewStreamClient(
		dingclient.WithAppCredential(cred),
		dingclient.WithAutoReconnect(true),
	)
	if err := cli.CheckConfigValid(); err != nil {
		c.setStatus(channel.StatusFailed, err.Error())
		return fmt.Errorf("dingtalk: invalid credentials: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.client = cli
	c.mu.Unlock()

	// Route inbound under runCtx (the SDK calls handlers with a detached
	// Background context): replies spawned from these events must die on
	// Close instead of parking forever (HARNESS R7-M2).
	cli.RegisterChatBotCallbackRouter(func(_ context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
		c.handleBotMessage(runCtx, data, emit)
		return nil, nil
	})

	c.setStatus(channel.StatusConnecting, "")
	go func() {
		err := cli.Start(runCtx)
		switch {
		case runCtx.Err() != nil:
			c.setStatus(channel.StatusStopped, "")
		case err != nil:
			c.setStatus(channel.StatusFailed, err.Error())
			logging.Warn("dingtalk: stream connection ended", "err", err)
		default:
			c.setStatus(channel.StatusStopped, "")
		}
	}()
	// The SDK manages (re)connection internally; report connected once
	// the loop is running.
	c.setStatus(channel.StatusConnected, "")
	return nil
}

// handleBotMessage normalises one inbound bot message.
func (c *Channel) handleBotMessage(ctx context.Context, data *chatbot.BotCallbackDataModel, emit channel.EmitFunc) {
	if data == nil {
		return
	}
	isGroup := data.ConversationType == conversationGroup
	if isGroup && !c.cfg.ReplyWithoutAt && !data.IsInAtList {
		return
	}
	text := strings.TrimSpace(data.Text.Content)
	if text == "" {
		return
	}

	c.rememberWebhook(data.ConversationId, data.SessionWebhook, data.SessionWebhookExpiredTime)

	kind := channel.ChatKindPrivate
	if isGroup {
		kind = channel.ChatKindGroup
	}
	ev := &channel.Event{
		ChannelID:        c.cfg.ChannelID,
		ChannelUserID:    firstNonEmpty(data.SenderStaffId, data.SenderId),
		ChannelUserName:  data.SenderNick,
		ChatID:           data.ConversationId,
		ChatName:         data.ConversationTitle,
		ChannelMessageID: data.MsgId,
		Kind:             kind,
		Text:             text,
		Metadata: map[string]any{
			"sender_corp_id": data.SenderCorpId,
			"is_admin":       data.IsAdmin,
		},
		ReceivedAt: time.Now(),
	}
	if err := emit(ctx, channel.Inbound{Message: ev}); err != nil {
		logging.Warn("dingtalk: emit failed", "err", err)
	}
}

func (c *Channel) rememberWebhook(conversationID, url string, expiredAtMillis int64) {
	if url == "" {
		return
	}
	c.mu.Lock()
	c.webhooks[conversationID] = webhookEntry{
		url:       url,
		expiresAt: time.UnixMilli(expiredAtMillis),
	}
	c.mu.Unlock()
}

func (c *Channel) webhookFor(chatID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.webhooks[chatID]
	if !ok || time.Now().After(w.expiresAt) {
		return "", false
	}
	return w.url, true
}

// SendResponse consumes the reply stream, sending the confirmation prompt
// when the reply parks and the final Markdown text when the reply ends.
func (c *Channel) SendResponse(ctx context.Context, r channel.Response) error {
	var text strings.Builder
	var notices []string
	for evt := range r.Events {
		switch e := evt.(type) {
		case event.TextBlockDeltaEvent:
			text.WriteString(e.Delta)
		case event.RequireUserConfirmEvent:
			// Never stop consuming the stream on a send failure — the
			// gateway blocks forwarding until the stream is drained.
			if err := c.sendMarkdown(ctx, r.ChatID, "Tool approval", c.confirmPrompt(e.ToolCalls)); err != nil {
				logging.Warn("dingtalk: confirm prompt send failed", "chat", r.ChatID, "err", err)
			}
		case event.ExceedMaxItersEvent:
			notices = append(notices, maxItersNotice)
		case event.CustomEvent:
			if strings.Contains(e.Name, "error") {
				notices = append(notices, agentErrorReply)
			}
		case event.ReplyEndEvent:
			if e.Error != nil && e.Error.Message != "" {
				notices = append(notices, "❌ "+e.Error.Message)
			}
		}
	}

	final := strings.TrimSpace(text.String())
	if final == "" && len(notices) == 0 {
		final = noTextReply
	}
	if len(notices) > 0 {
		if final != "" {
			final += "\n\n"
		}
		final += strings.Join(notices, "\n")
	}
	return c.sendMarkdown(ctx, r.ChatID, "Agent reply", final)
}

// Notify implements channel.Notifier.
func (c *Channel) Notify(ctx context.Context, chatID string, _ channel.ChatKind, text string) error {
	return c.sendMarkdown(ctx, chatID, "Notice", text)
}

func (c *Channel) confirmPrompt(calls []message.ToolCallBlock) string {
	var sb strings.Builder
	sb.WriteString(confirmPromptHeader)
	for _, tc := range calls {
		fmt.Fprintf(&sb, "\n\n- `%s` %s", tc.Name, strings.TrimSpace(tc.Input))
	}
	return sb.String()
}

// sendMarkdown sends text (split to the message-length bound) through the
// chat's session webhook.
func (c *Channel) sendMarkdown(ctx context.Context, chatID, title, content string) error {
	url, ok := c.webhookFor(chatID)
	if !ok {
		return fmt.Errorf("dingtalk: no live session webhook for chat %s (message not sent)", chatID)
	}
	replier := chatbot.NewChatbotReplier()
	for _, chunk := range channel.SplitText(content, c.capability.MaxMessageLength) {
		if err := replier.SimpleReplyMarkdown(ctx, url, []byte(title), []byte(chunk)); err != nil {
			return fmt.Errorf("dingtalk: reply failed: %w", err)
		}
	}
	return nil
}

// Close implements channel.Channel.
func (c *Channel) Close() error {
	c.mu.Lock()
	cancel := c.cancel
	cli := c.client
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cli != nil {
		cli.Close()
	}
	c.setStatus(channel.StatusStopped, "")
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
