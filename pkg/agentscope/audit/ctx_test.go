package audit

import (
	"context"
	"testing"
)

func TestReplyIDCtxRoundTrip(t *testing.T) {
	ctx := WithReplyID(context.Background(), "reply-42")
	if got := ReplyIDFromCtx(ctx); got != "reply-42" {
		t.Errorf("ReplyIDFromCtx = %q, want reply-42", got)
	}
	if got := ReplyIDFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx should yield empty reply ID, got %q", got)
	}
	if ctx2 := WithReplyID(context.Background(), ""); ctx2.Value(replyIDKey{}) != nil {
		t.Error("empty reply ID must not be attached")
	}
}
