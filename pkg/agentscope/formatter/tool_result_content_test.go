package formatter

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"strings"
	"testing"
)

func TestToolResultMediaPlaceholdersAreBounded(t *testing.T) {
	for _, source := range []any{
		message.URLSource{MediaType: "image/png", URL: "data:image/png;base64," + strings.Repeat("A", 10000)},
		message.URLSource{MediaType: strings.Repeat("x", 10000), URL: "https://example.com/" + strings.Repeat("x", 10000)},
		message.Base64Source{MediaType: "audio/wav", Data: strings.Repeat("A", 10000)},
	} {
		got := ConvertToolResultToString([]message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, message.DataBlock{Type: "data", Source: source}, message.TextBlock{Type: "text", Text: "last"}})
		if len(got) > 350 || !strings.HasPrefix(got, "first\n[") || !strings.HasSuffix(got, "]\nlast") || strings.Contains(got, "base64") {
			t.Fatalf("invalid media placeholder length=%d", len(got))
		}
	}
}
