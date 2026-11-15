package message_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Reconstruct a complete mixed reply through the public event protocol, then
// persist it without losing block order, binary content or answer metadata.
func TestAppendEventReconstructsMixedReply(t *testing.T) {
	const reply = "mixed-reply"
	const questionInput = `{"questions":[{"question":"Which client?","header":"Client","options":[{"label":"Go","description":"Go SDK"},{"label":"Python","description":"Python SDK"}]}]}`
	m := &message.Msg{ID: reply, Name: "assistant", Role: message.RoleAssistant}
	metadata := map[string]any{"answers": []any{map[string]any{"question": "Which client?", "selected": []any{"Go"}}}}
	end := event.NewToolResultEndEvent(reply, "call", message.ToolResultSuccess)
	end.Metadata = metadata
	stream := []event.Event{
		event.NewThinkingBlockStartEvent(reply, "thinking"),
		event.NewThinkingBlockDeltaEvent(reply, "thinking", "Compare "),
		event.NewThinkingBlockDeltaEvent(reply, "thinking", "the clients."),
		event.NewThinkingBlockEndEvent(reply, "thinking"),
		event.NewTextBlockStartEvent(reply, "text"),
		event.NewTextBlockDeltaEvent(reply, "text", "Choose "),
		event.NewTextBlockDeltaEvent("another-reply", "text", "unrelated text"),
		event.NewTextBlockDeltaEvent(reply, "text", "a client."),
		event.NewTextBlockEndEvent(reply, "text"),
		event.NewDataBlockStartEvent(reply, "audio", "audio/wav"),
		event.NewDataBlockDeltaEvent(reply, "audio", "YQ==", "audio/wav"),
		event.NewDataBlockDeltaEvent(reply, "audio", "YmM=", "audio/wav"),
		event.NewDataBlockEndEvent(reply, "audio"),
		event.NewToolCallStartEvent(reply, "call", "AskUser"),
		event.NewToolCallDeltaEvent(reply, "call", questionInput[:35]),
		event.NewToolCallDeltaEvent(reply, "call", questionInput[35:]),
		event.NewToolCallEndEvent(reply, "call"),
		event.NewToolResultStartEvent(reply, "call", "AskUser"),
		event.NewToolResultTextDeltaEvent(reply, "call", "Selected Go. "),
		event.NewToolResultDataDeltaEvent(reply, "call", "preview", "image/png", "eA==", ""),
		event.NewToolResultDataDeltaEvent(reply, "call", "preview", "image/png", "eXo=", ""),
		event.NewToolResultDataDeltaEvent(reply, "call", "link", "image/png", "", "https://example.com/preview.png"),
		event.NewToolResultTextDeltaEvent(reply, "call", "Client "),
		event.NewToolResultTextDeltaEvent(reply, "call", "confirmed."),
		end,
		event.NewHintBlockEvent(reply, "hint", "host", "Continue with Go."),
		event.NewModelCallEndEvent(reply, 12, 3),
		event.NewModelCallEndEvent(reply, 4, 1),
		event.NewReplyEndEvent("session", reply),
	}
	for _, e := range stream {
		m.AppendEvent(e)
	}
	want := []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", ID: "thinking", Thinking: "Compare the clients."},
		message.TextBlock{Type: "text", ID: "text", Text: "Choose a client."},
		message.DataBlock{Type: "data", ID: "audio", Source: message.Base64Source{Type: "base64", Data: "YWJj", MediaType: "audio/wav"}},
		message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "AskUser", Input: questionInput, State: message.ToolCallFinished},
		message.ToolResultBlock{Type: "tool_result", ID: "call", Name: "AskUser", State: message.ToolResultSuccess, Metadata: metadata,
			Output: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "Selected Go. "},
				message.DataBlock{Type: "data", ID: "preview", Source: message.Base64Source{Type: "base64", Data: "eHl6", MediaType: "image/png"}},
				message.DataBlock{Type: "data", ID: "link", Source: message.URLSource{Type: "url", URL: "https://example.com/preview.png", MediaType: "image/png"}},
				message.TextBlock{Type: "text", Text: "Client confirmed."},
			}},
		message.HintBlock{Type: "hint", ID: "hint", Source: "host", Hint: "Continue with Go."},
	}
	if !reflect.DeepEqual(m.Content, want) {
		t.Fatalf("reconstructed content = %#v, want %#v", m.Content, want)
	}
	if m.Usage == nil || m.Usage.InputTokens != 16 || m.Usage.OutputTokens != 4 {
		t.Fatalf("usage from both model calls was not accumulated: %+v", m.Usage)
	}
	if _, err := time.Parse(message.TimestampFormat, m.FinishedAt); err != nil {
		t.Fatalf("missing or invalid completion timestamp: %q", m.FinishedAt)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var restored message.Msg
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, *m) {
		t.Fatalf("serialized reconstructed message lost content: %s", data)
	}
}

func TestAppendEventIgnoresUnrelatedAndOrphanDeltas(t *testing.T) {
	m := &message.Msg{ID: "reply", Content: []message.ContentBlock{message.TextBlock{Type: "text", ID: "present", Text: "unchanged"}}}
	before, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []any{
		nil,
		map[string]any{"type": "text_block_delta", "delta": "not an event"},
		event.NewTextBlockDeltaEvent("other-reply", "present", "wrong reply"),
		event.NewTextBlockDeltaEvent("reply", "missing", "orphan"),
		event.NewThinkingBlockDeltaEvent("reply", "missing", "orphan"),
		event.NewDataBlockDeltaEvent("reply", "missing", "YQ==", "image/png"),
		event.NewToolCallDeltaEvent("reply", "missing", `{}`),
		event.NewToolResultTextDeltaEvent("reply", "missing", "orphan"),
		event.NewToolResultDataDeltaEvent("reply", "missing", "image", "image/png", "YQ==", ""),
		event.NewToolResultEndEvent("reply", "missing", message.ToolResultSuccess),
		event.NewCustomEvent("reply", "host.status", map[string]any{"status": "waiting"}),
	} {
		m.AppendEvent(e)
	}
	after, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("unrelated events changed the message: before %s, after %s", before, after)
	}
}

func TestAppendEventAssemblesBinaryToolResultChunks(t *testing.T) {
	for _, chunks := range [][]string{{"YQ==", "Yg=="}, {"Y", "WI="}} {
		m := &message.Msg{ID: "reply"}
		m.AppendEvent(event.NewToolResultStartEvent("reply", "call", "preview"))
		for _, chunk := range chunks {
			m.AppendEvent(event.NewToolResultDataDeltaEvent("reply", "call", "image", "image/png", chunk, ""))
		}
		m.AppendEvent(event.NewToolResultDataDeltaEvent("reply", "call", "other-image", "image/png", "Y2Q=", ""))
		m.AppendEvent(event.NewToolResultEndEvent("reply", "call", message.ToolResultSuccess))
		results := m.GetContentBlocks(message.ContentBlockToolResult)
		if len(results) != 1 {
			t.Fatalf("results = %#v", results)
		}
		blocks, ok := results[0].(message.ToolResultBlock).Output.([]message.ContentBlock)
		if !ok || len(blocks) != 2 {
			t.Fatalf("chunks did not preserve the two image blocks: %#v", results)
		}
		for i, expected := range []struct{ id, data string }{{"image", "ab"}, {"other-image", "cd"}} {
			block := blocks[i].(message.DataBlock)
			if block.ID != expected.id {
				t.Fatalf("image ID = %q, want %q", block.ID, expected.id)
			}
			data := block.Source.(message.Base64Source).Data
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil || string(decoded) != expected.data {
				t.Fatalf("chunks %q, block %s became %q (%v), want %s", chunks, block.ID, decoded, err, expected.data)
			}
		}
	}
}
