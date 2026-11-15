package audit

import "context"

type replyIDKey struct{}

// WithReplyID attaches the correlating reply ID to ctx so audit loggers can
// stamp it on entries (HARNESS_DESIGN A2). Set by the agent at reply start.
func WithReplyID(ctx context.Context, replyID string) context.Context {
	if replyID == "" {
		return ctx
	}
	return context.WithValue(ctx, replyIDKey{}, replyID)
}

// ReplyIDFromCtx extracts the reply ID attached by WithReplyID, or "".
func ReplyIDFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(replyIDKey{}).(string)
	return id
}
