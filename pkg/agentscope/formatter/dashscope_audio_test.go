package formatter

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func audioTestMsg(mediaType string) []*message.Msg {
	return []*message.Msg{
		message.UserMsg("user", []message.ContentBlock{
			message.DataBlock{
				Type: "data",
				ID:   "aud_1",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      "QUJD",
					MediaType: mediaType,
				},
			},
		}),
	}
}

func findInputAudio(t *testing.T, result []map[string]any) map[string]any {
	t.Helper()
	if len(result) == 0 {
		t.Fatal("no formatted messages")
	}
	content, ok := result[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is %T, want []map[string]any", result[0]["content"])
	}
	for _, m := range content {
		if m["type"] == "input_audio" {
			ia, ok := m["input_audio"].(map[string]any)
			if !ok {
				t.Fatalf("input_audio is %T", m["input_audio"])
			}
			return ia
		}
	}
	t.Fatal("no input_audio block found in formatted content")
	return nil
}

// TestDashScopeFormatter_Base64AudioDataURL locks in upstream fix #2315:
// DashScope requires base64 audio wrapped in a data URL, and mpeg maps to mp3.
func TestDashScopeFormatter_Base64AudioDataURL(t *testing.T) {
	result, err := NewDashScopeFormatter().Format(audioTestMsg("audio/mpeg"))
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	ia := findInputAudio(t, result)
	if got, want := ia["data"], "data:;base64,QUJD"; got != want {
		t.Errorf("data = %v, want %q", got, want)
	}
	if got, want := ia["format"], "mp3"; got != want {
		t.Errorf("format = %v, want %q (mpeg must map to mp3)", got, want)
	}
}

func TestDashScopeFormatter_Base64AudioWAV(t *testing.T) {
	result, err := NewDashScopeFormatter().Format(audioTestMsg("audio/wav"))
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	ia := findInputAudio(t, result)
	if got, want := ia["data"], "data:;base64,QUJD"; got != want {
		t.Errorf("data = %v, want %q", got, want)
	}
	if got, want := ia["format"], "wav"; got != want {
		t.Errorf("format = %v, want %q", got, want)
	}
}

// TestOpenAIFormatter_Base64AudioRaw guards the standard OpenAI behavior:
// raw base64 in data plus the plain format suffix (no data URL).
func TestOpenAIFormatter_Base64AudioRaw(t *testing.T) {
	result, err := NewOpenAIFormatter().Format(audioTestMsg("audio/wav"))
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	ia := findInputAudio(t, result)
	if got, want := ia["data"], "QUJD"; got != want {
		t.Errorf("data = %v, want raw base64 %q", got, want)
	}
	if got, want := ia["format"], "wav"; got != want {
		t.Errorf("format = %v, want %q", got, want)
	}
}
