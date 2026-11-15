package message

import (
	"encoding/base64"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// AppendEvent incrementally reconstructs this Msg from a streaming event.
// It accepts any event value and uses interface checks to extract data.
// Events whose ReplyID does not match this message's ID are ignored.
//
// The event package's concrete types implement the necessary accessor
// interfaces (replyIDer, eventTyper, blockIDer, etc.) so they work
// directly with this method without circular imports.
func (m *Msg) AppendEvent(ev any) {
	ri, ok := ev.(interface{ GetReplyID() string })
	if !ok || ri.GetReplyID() != m.ID {
		return
	}

	var evType string
	if et, ok := ev.(interface{ EventTypeString() string }); ok {
		evType = et.EventTypeString()
	}
	if evType == "" {
		return
	}

	switch evType {
	case "reply_end":
		m.FinishedAt = time.Now().Format(TimestampFormat)

	case "model_call_end":
		type tokenInfo interface {
			GetInputTokens() int
			GetOutputTokens() int
		}
		if te, ok := ev.(tokenInfo); ok {
			if m.Usage == nil {
				m.Usage = &Usage{}
			}
			m.Usage.InputTokens += te.GetInputTokens()
			m.Usage.OutputTokens += te.GetOutputTokens()
		}

	case "text_block_start":
		if be, ok := ev.(interface{ GetBlockID() string }); ok {
			m.Content = append(m.Content, TextBlock{Type: "text", ID: be.GetBlockID(), Text: ""})
		}

	case "text_block_delta":
		m.applyBlockDelta(ContentBlockText, ev)

	case "thinking_block_start":
		if be, ok := ev.(interface{ GetBlockID() string }); ok {
			m.Content = append(m.Content, ThinkingBlock{Type: "thinking", ID: be.GetBlockID(), Thinking: ""})
		}

	case "thinking_block_delta":
		m.applyThinkingDelta(ev)

	case "data_block_start":
		if be, ok := ev.(interface{ GetBlockID() string }); ok {
			mt := ""
			if me, ok := ev.(interface{ GetMediaType_() string }); ok {
				mt = me.GetMediaType_()
			}
			m.Content = append(m.Content, DataBlock{
				Type:   "data",
				ID:     be.GetBlockID(),
				Source: Base64Source{Type: "base64", Data: "", MediaType: mt},
			})
		}

	case "data_block_delta":
		if be, ok := ev.(interface{ GetBlockID() string }); ok {
			if de, ok := ev.(interface{ GetData() string }); ok {
				idx := m.findBlockByTypeAndID(ContentBlockData, be.GetBlockID())
				if idx >= 0 {
					if db, ok := m.Content[idx].(DataBlock); ok {
						if src, ok := db.Source.(Base64Source); ok {
							src.Data = mergeBase64Chunks(src.Data, de.GetData())
							db.Source = src
							m.Content[idx] = db
						}
					}
				}
			}
		}

	case "tool_call_start":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			name := ""
			if ne, ok := ev.(interface{ GetToolCallName() string }); ok {
				name = ne.GetToolCallName()
			}
			m.Content = append(m.Content, ToolCallBlock{
				Type: "tool_call", ID: te.GetToolCallID(), Name: name, Input: "", State: ToolCallPending,
			})
		}

	case "tool_call_delta":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			if de, ok := ev.(interface{ GetDelta() string }); ok {
				idx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
				if idx >= 0 {
					if tc, ok := m.Content[idx].(ToolCallBlock); ok {
						tc.Input += de.GetDelta()
						m.Content[idx] = tc
					}
				}
			}
		}

	case "tool_result_start":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			name := ""
			if ne, ok := ev.(interface{ GetToolCallName() string }); ok {
				name = ne.GetToolCallName()
			}
			m.Content = append(m.Content, ToolResultBlock{
				Type: "tool_result", ID: te.GetToolCallID(), Name: name, Output: "", State: ToolResultRunning,
			})
		}

	case "tool_result_text_delta":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			if de, ok := ev.(interface{ GetDelta() string }); ok {
				idx := m.findBlockByTypeAndID(ContentBlockToolResult, te.GetToolCallID())
				if idx >= 0 {
					if tr, ok := m.Content[idx].(ToolResultBlock); ok {
						switch out := tr.Output.(type) {
						case string:
							tr.Output = out + de.GetDelta()
						case []ContentBlock:
							// Output was promoted by data deltas; keep the
							// blocks and merge the text into the trailing
							// TextBlock (HARNESS review R6-M1).
							tr.Output = appendTextToBlocks(out, de.GetDelta())
						default:
							tr.Output = de.GetDelta()
						}
						m.Content[idx] = tr
					}
				}
			}
		}

	case "tool_result_data_delta":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			idx := m.findBlockByTypeAndID(ContentBlockToolResult, te.GetToolCallID())
			if idx >= 0 {
				if tr, ok := m.Content[idx].(ToolResultBlock); ok {
					blockID := ""
					if be, ok := ev.(interface{ GetBlockID() string }); ok {
						blockID = be.GetBlockID()
					}
					mt := ""
					if me, ok := ev.(interface{ GetMediaType_() string }); ok {
						mt = me.GetMediaType_()
					}
					data := ""
					if de, ok := ev.(interface{ GetData() string }); ok {
						data = de.GetData()
					}
					url := ""
					if ue, ok := ev.(interface{ GetURL() string }); ok {
						url = ue.GetURL()
					}
					var db DataBlock
					switch {
					case data != "":
						db = DataBlock{Type: "data", ID: blockID,
							Source: Base64Source{Type: "base64", Data: data, MediaType: mt}}
					case url != "":
						db = DataBlock{Type: "data", ID: blockID,
							Source: URLSource{Type: "url", URL: url, MediaType: mt}}
					default:
						return
					}
					tr.Output = appendResultData(tr.Output, db)
					m.Content[idx] = tr
				}
			}
		}

	case "tool_result_end":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			if se, ok := ev.(interface{ GetState() ToolResultState }); ok {
				idx := m.findBlockByTypeAndID(ContentBlockToolResult, te.GetToolCallID())
				if idx >= 0 {
					if tr, ok := m.Content[idx].(ToolResultBlock); ok {
						tr.State = se.GetState()
						if metadata, ok := ev.(interface{ GetMetadata() map[string]any }); ok {
							tr.Metadata = metadata.GetMetadata()
						}
						m.Content[idx] = tr
					}
				}
				tcIdx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
				if tcIdx >= 0 {
					if tc, ok := m.Content[tcIdx].(ToolCallBlock); ok {
						tc.State = ToolCallFinished
						m.Content[tcIdx] = tc
					}
				}
			}
		}

	case "hint_block":
		if he, ok := ev.(interface{ GetHint() string }); ok {
			blockID := ""
			if be, ok := ev.(interface{ GetBlockID() string }); ok {
				blockID = be.GetBlockID()
			}
			source := ""
			if se, ok := ev.(interface{ GetSource() string }); ok {
				source = se.GetSource()
			}
			m.Content = append(m.Content, HintBlock{
				Type: "hint", ID: blockID, Source: source, Hint: he.GetHint(),
			})
		}

	case "require_user_confirm":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			idx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
			if idx >= 0 {
				if tc, ok := m.Content[idx].(ToolCallBlock); ok {
					tc.State = ToolCallAsking
					m.Content[idx] = tc
				}
			}
		}

	case "user_confirm_result":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			idx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
			if idx >= 0 {
				if tc, ok := m.Content[idx].(ToolCallBlock); ok {
					tc.State = ToolCallAllowed
					m.Content[idx] = tc
				}
			}
		}

	case "require_external_execution":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			idx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
			if idx >= 0 {
				if tc, ok := m.Content[idx].(ToolCallBlock); ok {
					tc.State = ToolCallSubmitted
					m.Content[idx] = tc
				}
			}
		}

	case "external_execution_result":
		if te, ok := ev.(interface{ GetToolCallID() string }); ok {
			idx := m.findBlockByTypeAndID(ContentBlockToolCall, te.GetToolCallID())
			if idx >= 0 {
				if tc, ok := m.Content[idx].(ToolCallBlock); ok {
					tc.State = ToolCallFinished
					m.Content[idx] = tc
				}
			}
		}

	case "exceed_max_iters":
		// No content mutation needed; stored in metadata for observability.
		if m.Metadata == nil {
			m.Metadata = make(types.JSONObject)
		}
		m.Metadata["exceed_max_iters"] = true
	}
}

// mergeBase64Chunks correctly concatenates two base64-encoded binary chunks.
// It decodes both, concatenates the raw bytes, and re-encodes.
// Falls back to string concatenation if decoding fails.
func mergeBase64Chunks(existing, delta string) string {
	if existing == "" {
		return delta
	}
	existingBytes, err1 := base64.StdEncoding.DecodeString(existing)
	deltaBytes, err2 := base64.StdEncoding.DecodeString(delta)
	if err1 != nil || err2 != nil {
		return existing + delta
	}
	merged := make([]byte, len(existingBytes)+len(deltaBytes))
	copy(merged, existingBytes)
	copy(merged[len(existingBytes):], deltaBytes)
	return base64.StdEncoding.EncodeToString(merged)
}

func (m *Msg) applyBlockDelta(blockType ContentBlockType, ev any) {
	be, ok := ev.(interface{ GetBlockID() string })
	if !ok {
		return
	}
	de, ok := ev.(interface{ GetDelta() string })
	if !ok {
		return
	}
	idx := m.findBlockByTypeAndID(blockType, be.GetBlockID())
	if idx < 0 {
		return
	}
	if tb, ok := m.Content[idx].(TextBlock); ok {
		tb.Text += de.GetDelta()
		m.Content[idx] = tb
	}
}

func (m *Msg) applyThinkingDelta(ev any) {
	be, ok := ev.(interface{ GetBlockID() string })
	if !ok {
		return
	}
	de, ok := ev.(interface{ GetDelta() string })
	if !ok {
		return
	}
	idx := m.findBlockByTypeAndID(ContentBlockThinking, be.GetBlockID())
	if idx < 0 {
		return
	}
	if tb, ok := m.Content[idx].(ThinkingBlock); ok {
		tb.Thinking += de.GetDelta()
		m.Content[idx] = tb
	}
}

// appendTextToBlocks merges a text delta into the trailing TextBlock of a
// promoted tool-result Output, or appends a new TextBlock.
func appendTextToBlocks(blocks []ContentBlock, delta string) []ContentBlock {
	if len(blocks) > 0 {
		if tb, ok := blocks[len(blocks)-1].(TextBlock); ok {
			tb.Text += delta
			blocks[len(blocks)-1] = tb
			return blocks
		}
	}
	return append(blocks, TextBlock{Type: "text", Text: delta})
}

// appendResultData merges a streamed data block into a tool result Output
// (string or []ContentBlock), coalescing consecutive base64 chunks of the
// same block ID (tool_result_data_delta accumulation).
func appendResultData(output any, db DataBlock) any {
	appendOrMerge := func(blocks []ContentBlock) []ContentBlock {
		if len(blocks) > 0 {
			last := blocks[len(blocks)-1]
			if prev, ok := last.(DataBlock); ok && prev.ID == db.ID && db.ID != "" {
				if ps, ok := prev.Source.(Base64Source); ok {
					if ns, ok := db.Source.(Base64Source); ok {
						prev.Source = Base64Source{Type: "base64",
							Data: mergeBase64Chunks(ps.Data, ns.Data), MediaType: ns.MediaType}
						blocks[len(blocks)-1] = prev
						return blocks
					}
				}
			}
		}
		return append(blocks, db)
	}
	switch out := output.(type) {
	case nil:
		return []ContentBlock{db}
	case string:
		if out == "" {
			return []ContentBlock{db}
		}
		return []ContentBlock{TextBlock{Type: "text", Text: out}, db}
	case []ContentBlock:
		return appendOrMerge(out)
	default:
		return output
	}
}
