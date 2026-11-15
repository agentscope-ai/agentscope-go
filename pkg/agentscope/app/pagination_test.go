package app

import "testing"

func TestParseCursorRequest_ZeroLimit(t *testing.T) {
	req := ParseCursorRequest("", 0)
	if req.Limit != 50 {
		t.Errorf("expected default limit 50, got %d", req.Limit)
	}
	if req.Cursor != "" {
		t.Errorf("expected empty cursor, got %q", req.Cursor)
	}
}

func TestParseCursorRequest_NegativeLimit(t *testing.T) {
	req := ParseCursorRequest("", -10)
	if req.Limit != 50 {
		t.Errorf("expected default limit 50 for negative input, got %d", req.Limit)
	}
}

func TestParseCursorRequest_ExceedsMax(t *testing.T) {
	req := ParseCursorRequest("", 500)
	if req.Limit != 200 {
		t.Errorf("expected capped limit 200, got %d", req.Limit)
	}
}

func TestParseCursorRequest_ValidLimit(t *testing.T) {
	req := ParseCursorRequest("", 75)
	if req.Limit != 75 {
		t.Errorf("expected limit 75, got %d", req.Limit)
	}
}

func TestParseCursorRequest_WithCursor(t *testing.T) {
	req := ParseCursorRequest("abc123cursor", 25)
	if req.Cursor != "abc123cursor" {
		t.Errorf("expected cursor abc123cursor, got %q", req.Cursor)
	}
	if req.Limit != 25 {
		t.Errorf("expected limit 25, got %d", req.Limit)
	}
}
