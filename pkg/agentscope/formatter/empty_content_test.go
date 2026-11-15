package formatter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestAnthropicGeminiEmptyContent(t *testing.T) {
	for _, f := range []MultiAgentFormatter{NewAnthropicFormatter(), NewGeminiFormatter()} {
		for _, blocks := range [][]message.ContentBlock{
			nil,
			{message.TextBlock{Type: "text", Text: ""}},
			{message.HintBlock{Type: "hint", Hint: ""}},
			{message.HintBlock{Type: "hint", Hint: []message.ContentBlock{message.TextBlock{Type: "text"}}}},
		} {
			msgs := []*message.Msg{{Name: "empty", Role: message.RoleUser, Content: blocks}, message.UserMsg("alice", "hello")}
			got, err := f.Format(msgs)
			want, wantErr := f.Format(msgs[1:])
			if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%T empty content %v: got %#v (%v), want %#v", f, blocks, got, err, want)
			}
		}
	}
}

func TestAnthropicGeminiHintMediaAndWhitespace(t *testing.T) {
	imageBlock := message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}}
	for _, f := range []MultiAgentFormatter{NewAnthropicFormatter(), NewGeminiFormatter()} {
		for _, blocks := range [][]message.ContentBlock{
			{imageBlock},
			{message.TextBlock{Type: "text"}, imageBlock, message.TextBlock{Type: "text", Text: " "}},
		} {
			got, err := f.Format([]*message.Msg{{Name: "alice", Role: message.RoleUser, Content: []message.ContentBlock{message.HintBlock{Type: "hint", Hint: blocks}}}})
			want, wantErr := f.Format([]*message.Msg{{Name: "alice", Role: message.RoleUser, Content: blocks}})
			if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%T hint lost supported content: got %#v (%v), want %#v", f, got, err, want)
			}
		}
	}
}

func TestAnthropicGeminiSenderAfterFiltering(t *testing.T) {
	for _, f := range []MultiAgentFormatter{NewAnthropicFormatter(), NewGeminiFormatter()} {
		alice := message.UserMsg("alice", "hello")
		msgs := []*message.Msg{nil, {Name: "system", Role: message.RoleSystem}, {Name: "empty", Role: message.RoleUser}, alice}
		got, err := f.FormatMultiAgent(msgs, "bot")
		want, wantErr := f.FormatMultiAgent([]*message.Msg{alice}, "bot")
		if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%T sender changed after filtering: got %#v (%v), want %#v", f, got, err, want)
		}
	}
}

func TestGeminiSystemInstructionFiltersEmptyBlocksBeforeJoining(t *testing.T) {
	for _, texts := range [][]string{{"", ""}, {"", " ", ""}, {"", "one", "", "two"}} {
		var blocks []message.ContentBlock
		var nonempty []string
		for _, text := range texts {
			blocks = append(blocks, message.TextBlock{Type: "text", Text: text})
			if text != "" {
				nonempty = append(nonempty, text)
			}
		}
		got := ExtractGeminiSystemInstruction([]*message.Msg{{Role: message.RoleSystem, Content: blocks}})
		var want map[string]any
		if len(nonempty) > 0 {
			want = map[string]any{"parts": []map[string]any{{"text": strings.Join(nonempty, "\n")}}}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("texts=%q: got %#v, want %#v", texts, got, want)
		}
	}
}
