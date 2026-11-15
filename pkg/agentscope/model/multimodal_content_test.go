package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// TestConvertMessagesToOpenAI_MultimodalNotStringified proves that a user message
// containing an image DataBlock is serialized as a structured OpenAI content array
// (with an image_url part), not collapsed into a JSON string blob (which the model
// would read as plain text, losing the image entirely).
func TestConvertMessagesToOpenAI_MultimodalNotStringified(t *testing.T) {
	msg := message.NewMsg("user", message.RoleUser, []message.ContentBlock{
		message.TextBlock{Text: "what is in this image?"},
		message.DataBlock{
			Type:   "image",
			Source: message.Base64Source{Type: "base64", Data: "aGVsbG8=", MediaType: "image/png"},
		},
	})

	out := convertMessagesToOpenAI([]*message.Msg{msg})
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}

	// The content must be a structured array, not a string.
	if s, isString := out[0].Content.(string); isString {
		t.Fatalf("multimodal content was stringified to %q; expected structured array", s)
	}

	// Marshaling the message must produce an image_url part.
	b, err := json.Marshal(out[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), "image_url") {
		t.Fatalf("serialized content missing image_url part: %s", b)
	}
}
