package agent

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

// TestReply_RecordedCallsCarryReplyID locks HARNESS_DESIGN A2: model calls
// recorded by the replay middleware carry the enclosing reply's ID.
func TestReply_RecordedCallsCarryReplyID(t *testing.T) {
	rec := replay.NewRecorder()
	mock := &mockChatModel{
		responses: []model.ChatResponse{{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
			IsLast:  true,
		}},
	}
	a := NewUnifiedAgent("corr-agent", "You are helpful.", mock, WithMiddlewares(rec))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}

	replyID := ""
	for evt := range ch {
		if rs, ok := evt.(event.ReplyStartEvent); ok {
			replyID = rs.ReplyID
		}
	}
	if replyID == "" {
		t.Fatal("no ReplyStartEvent observed")
	}

	entries := rec.Tape().Entries
	if len(entries) == 0 {
		t.Fatal("recorder captured no model calls")
	}
	for i, e := range entries {
		if e.ReplyID != replyID {
			t.Errorf("entry %d ReplyID = %q, want %q", i, e.ReplyID, replyID)
		}
	}
}
