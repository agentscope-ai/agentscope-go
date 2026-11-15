package app

// CursorPage is the response envelope for cursor-based pagination.
type CursorPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// CursorRequest captures pagination parameters from the client.
type CursorRequest struct {
	Cursor string // opaque cursor from a previous response (empty = start)
	Limit  int    // max items to return (default 50, capped at 200)
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// ParseCursorRequest builds a CursorRequest with validated limit.
func ParseCursorRequest(cursor string, limit int) CursorRequest {
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	return CursorRequest{
		Cursor: cursor,
		Limit:  limit,
	}
}
