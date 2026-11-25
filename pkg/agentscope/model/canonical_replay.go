package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// writerRankKey is the ToolCallBlock.Extra key under which a routing wrapper
// with canonical replay records the rank of the member that wrote a tool
// call: 1 for the local model of a ConnectivityAwareModel, the chain index
// for a FallbackChatModel. Rank 0, the most preferred member, is never
// recorded. The mark is stored with the agent's messages and checkpoints;
// formatters do not send it.
const writerRankKey = "agentscope_writer_rank"

// markWriter returns resp with every tool call in its content marked as
// written by the member of the given rank. The response, its content slice
// and each marked call's Extra map are copies; resp itself is not modified.
func markWriter(resp *ChatResponse, rank int) *ChatResponse {
	if resp == nil || rank <= 0 {
		return resp
	}
	var content []message.ContentBlock
	for i, b := range resp.Content {
		call, ok := b.(message.ToolCallBlock)
		if !ok {
			continue
		}
		if content == nil {
			content = append([]message.ContentBlock(nil), resp.Content...)
		}
		extra := make(map[string]any, len(call.Extra)+1)
		maps.Copy(extra, call.Extra)
		extra[writerRankKey] = rank
		call.Extra = extra
		content[i] = call
	}
	if content == nil {
		return resp
	}
	out := *resp
	out.Content = content
	return &out
}

// markWriterStream forwards in and marks the tool calls of its final
// response as written by the member of the given rank. It closes the
// returned channel when in closes or ctx ends; after ctx ends it keeps
// draining in, so a producer that does not watch ctx is not left blocked.
func markWriterStream(ctx context.Context, in <-chan ChatResponse, rank int) <-chan ChatResponse {
	if rank <= 0 {
		return in
	}
	out := make(chan ChatResponse)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				go discard(in)
				return
			case r, ok := <-in:
				if !ok {
					return
				}
				if r.IsLast {
					r = *markWriter(&r, rank)
				}
				select {
				case out <- r:
				case <-ctx.Done():
					go discard(in)
					return
				}
			}
		}
	}()
	return out
}

// discard reads in until it closes. It relies on the producer closing in,
// as the ChatStream contract requires.
func discard(in <-chan ChatResponse) {
	for range in {
	}
}

// canonicalFor returns msgs as the member of the given rank should receive
// them: the arguments of every tool call a less preferred member wrote (one
// marked with a higher rank) are rewritten as canonical JSON. Messages
// without such calls are shared with msgs; the others are shallow copies
// with new content slices, so the caller's messages are never modified.
func canonicalFor(msgs []*message.Msg, rank int) []*message.Msg {
	var out []*message.Msg
	for i, m := range msgs {
		if m == nil {
			continue
		}
		var content []message.ContentBlock
		for j, b := range m.Content {
			call, ok := b.(message.ToolCallBlock)
			if !ok {
				continue
			}
			if r, marked := writerRank(call.Extra); !marked || r <= rank {
				continue
			}
			canon, ok := canonicalJSON(call.Input)
			if !ok || canon == call.Input {
				continue
			}
			if content == nil {
				content = append([]message.ContentBlock(nil), m.Content...)
			}
			call.Input = canon
			content[j] = call
		}
		if content == nil {
			continue
		}
		if out == nil {
			out = append([]*message.Msg(nil), msgs...)
		}
		cp := *m
		cp.Content = content
		out[i] = &cp
	}
	if out == nil {
		return msgs
	}
	return out
}

// writerRank reads the writer mark. A checkpoint round trip turns the int
// into a float64 or a json.Number.
func writerRank(extra map[string]any) (int, bool) {
	switch v := extra[writerRankKey].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case json.Number:
		n, err := strconv.Atoi(v.String())
		return n, err == nil
	}
	return 0, false
}

// surrogateEscape matches a \u escape of a UTF-16 surrogate (U+D800 to
// U+DFFF). Decoding an unpaired one yields U+FFFD.
var surrogateEscape = regexp.MustCompile(`\\u[dD][89a-fA-F][0-9a-fA-F]{2}`)

// canonicalJSON re-encodes one JSON value with sorted object keys and no
// spaces (the measured cloud model wrote its own write_file calls this way).
// Numbers stay as written and HTML is not escaped, so only the form changes;
// a duplicated key keeps its last value, as encoding/json reads it. Input
// that is not exactly one JSON value has no canonical form, nor has input
// that decoding could alter: invalid UTF-8, or any surrogate escape, paired
// or not.
func canonicalJSON(s string) (string, bool) {
	if !utf8.ValidString(s) || surrogateEscape.MatchString(s) {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	var rest json.RawMessage
	if err := dec.Decode(&rest); !errors.Is(err, io.EOF) {
		return "", false
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}
