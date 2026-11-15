package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type imgTestModel struct{}

func (imgTestModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{}, nil
}
func (imgTestModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	ch := make(chan model.ChatResponse)
	close(ch)
	return ch, nil
}
func (imgTestModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 1 }

func imageBlock(id, media string) message.DataBlock {
	return message.DataBlock{
		Type: "data", ID: id,
		Source: message.Base64Source{Type: "base64", Data: "AAAA", MediaType: media},
	}
}

// Upstream #2362: the oldest images beyond MaxImageNum are replaced by text
// reminders; user/top-level and nested containers follow Python's rules.
func TestLimitContextImagesReplacesOldest(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{})
	a.state.Context = []*message.Msg{
		{
			Name: "u", Role: message.RoleUser,
			Content: []message.ContentBlock{imageBlock("i1", "image/png")},
		},
		{
			Name: "a", Role: message.RoleAssistant,
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "x"}},
		},
		{
			Name: "a", Role: message.RoleAssistant,
			Content: []message.ContentBlock{message.DataBlock{
				Type: "data", ID: "i2",
				Source: message.URLSource{Type: "url", URL: "https://cdn/x.png", MediaType: "image/png"},
			}},
		},
	}

	a.limitContextImages(1)

	// Oldest (i1, user message) → TextBlock reminder (user messages accept
	// only text/data blocks).
	first := a.state.Context[0].Content[0]
	tb, ok := first.(message.TextBlock)
	if !ok {
		t.Fatalf("user image replacement = %T, want TextBlock", first)
	}
	if !strings.Contains(tb.Text, "removed to free up context space") {
		t.Errorf("replacement text = %q", tb.Text)
	}
	// Newest (i2, assistant, URL source) survives.
	if _, ok := a.state.Context[2].Content[0].(message.DataBlock); !ok {
		t.Errorf("newest image should survive, got %T", a.state.Context[2].Content[0])
	}
}

func TestLimitContextImagesURLBecomesOffloadHint(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{})
	a.state.Context = []*message.Msg{
		{
			Name: "a", Role: message.RoleAssistant,
			Content: []message.ContentBlock{
				message.DataBlock{Type: "data", ID: "i1", Source: message.URLSource{Type: "url", URL: "https://cdn/old.png", MediaType: "image/png"}},
				message.DataBlock{Type: "data", ID: "i2", Source: message.Base64Source{Type: "base64", Data: "AA", MediaType: "image/jpeg"}},
			},
		},
	}
	a.limitContextImages(1)
	// Assistant top-level → HintBlock pointing at the URL.
	hb, ok := a.state.Context[0].Content[0].(message.HintBlock)
	if !ok {
		t.Fatalf("replacement = %T, want HintBlock", a.state.Context[0].Content[0])
	}
	hint := hb.GetHintText()
	if !strings.Contains(hint, "https://cdn/old.png") || !strings.Contains(hint, "offloaded") {
		t.Errorf("hint = %q, want offload pointer", hint)
	}
}

func TestLimitContextImagesNestedContainers(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{})
	a.state.Context = []*message.Msg{
		{
			Name: "u", Role: message.RoleUser,
			Content: []message.ContentBlock{imageBlock("top", "image/png")},
		},
		{
			Name: "t", Role: message.RoleUser,
			Content: []message.ContentBlock{message.ToolResultBlock{
				Type: "tool_result", ID: "c1", Name: "read",
				Output: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "see:"},
					imageBlock("nested", "image/png"),
				},
				State: message.ToolResultSuccess,
			}},
		},
	}
	a.limitContextImages(1)
	// Oldest = top-level image in msg 0; the nested one (newer) survives.
	if _, ok := a.state.Context[0].Content[0].(message.TextBlock); !ok {
		t.Fatalf("top-level image not replaced: %T", a.state.Context[0].Content[0])
	}
	trb := a.state.Context[1].Content[0].(message.ToolResultBlock)
	list := trb.Output.([]message.ContentBlock)
	if _, ok := list[1].(message.DataBlock); !ok {
		t.Errorf("nested image should survive, got %T", list[1])
	}

	// Second pass with a zero cap replaces the remaining (nested) image in
	// place with a TextBlock — nested containers only accept text/data blocks.
	a.limitContextImages(0)
	trb = a.state.Context[1].Content[0].(message.ToolResultBlock)
	list = trb.Output.([]message.ContentBlock)
	tb, ok := list[1].(message.TextBlock)
	if !ok {
		t.Fatalf("second pass should replace the nested image with a TextBlock, got %T", list[1])
	}
	if !strings.Contains(tb.Text, "removed to free up context space") {
		t.Errorf("nested replacement text = %q", tb.Text)
	}
}

func TestLimitContextImagesNoopUnderLimit(t *testing.T) {
	a := NewUnifiedAgent("t", "sys", imgTestModel{})
	a.state.Context = []*message.Msg{
		{Name: "u", Role: message.RoleUser, Content: []message.ContentBlock{imageBlock("i1", "image/png")}},
	}
	a.limitContextImages(5)
	if _, ok := a.state.Context[0].Content[0].(message.DataBlock); !ok {
		t.Error("image under the limit must not be touched")
	}
}
