package agent

import (
	"fmt"
	"unicode/utf8"
)

// TruncateToolResult keeps the head and tail of tool text within an estimated
// token budget, including the elision marker. It cuts at UTF-8 rune boundaries.
// The estimate charges one token per four ASCII bytes (rounded up) and one per
// non-ASCII byte. It does not measure the provider's actual token usage.
func TruncateToolResult(text string, tokenLimit int) (string, bool) {
	if text == "" {
		return text, false
	}
	if tokenLimit <= 0 {
		return "", true
	}
	units := toolTextUnits(text)
	// Compare the rounded estimate before multiplying: tokenLimit can be MaxInt.
	if (units+3)/4 <= uint64(tokenLimit) {
		return text, false
	}
	return fitToolText(text, uint64(tokenLimit)*4, ""), true
}

// Count in quarter tokens so the whole output rounds once.
func toolTextUnits(text string) uint64 {
	units := uint64(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			units += 3
		}
	}
	return units
}

// fitToolText is called for text already known to exceed the budget.
func fitToolText(text string, budget uint64, path string) string {
	if path != "" {
		full := fmt.Sprintf("\n<system-reminder>The remaining content has been offloaded to '%s'.</system-reminder>", path)
		short := "\nOffloaded: " + path
		_, first := utf8.DecodeRuneInString(text)
		_, last := utf8.DecodeLastRuneInString(text)
		edges := toolTextUnits(text[:first]) + toolTextUnits(text[len(text)-last:])
		for _, notice := range []string{full, short} {
			if toolTextUnits(notice)+uint64(len(shortToolElision))+edges <= budget {
				return fitToolTextWithNotice(text, budget, notice)
			}
		}
		if toolTextUnits(short) <= budget {
			return fitToolTextWithNotice(text, budget, short)
		}
	}
	return fitToolTextWithNotice(text, budget, "")
}

const (
	toolElisionFormat = "\n<<<TRUNCATED: %d bytes elided>>>\n"
	shortToolElision  = "..."
)

func fitToolTextWithNotice(text string, budget uint64, notice string) string {
	// The number of elided bytes can only shrink, so reserving space for
	// the original byte count covers every candidate without rescanning it.
	marker := fmt.Sprintf(toolElisionFormat, len(text))
	noticeUnits := toolTextUnits(notice)
	markerUnits := toolTextUnits(marker)
	_, first := utf8.DecodeRuneInString(text)
	_, last := utf8.DecodeLastRuneInString(text)
	edgeUnits := toolTextUnits(text[:first]) + toolTextUnits(text[len(text)-last:])
	fullMarker := markerUnits+noticeUnits+edgeUnits <= budget
	if !fullMarker {
		marker = shortToolElision
		markerUnits = uint64(len(marker))
	}
	if markerUnits+noticeUnits > budget {
		return notice
	}

	remaining := budget - markerUnits - noticeUnits
	head, tail := 0, len(text)
	var headUnits, tailUnits uint64
	for head < tail {
		_, left := utf8.DecodeRuneInString(text[head:tail])
		_, right := utf8.DecodeLastRuneInString(text[head:tail])
		leftUnits := toolTextUnits(text[head : head+left])
		rightUnits := toolTextUnits(text[tail-right : tail])
		// Prefer the side that has used less of the budget. If its next
		// rune cannot fit, the other side may still have room.
		switch {
		case leftUnits <= remaining && (headUnits <= tailUnits || rightUnits > remaining):
			head += left
			headUnits += leftUnits
			remaining -= leftUnits
		case rightUnits <= remaining:
			tail -= right
			tailUnits += rightUnits
			remaining -= rightUnits
		default:
			remaining = 0
		}
		if remaining == 0 {
			break
		}
	}
	if fullMarker {
		marker = fmt.Sprintf(toolElisionFormat, tail-head)
	}
	return text[:head] + marker + text[tail:] + notice
}
