package inference

import (
	"encoding/json"
	"math"
	"strings"
)

type usageSample struct {
	usage         Usage
	input, output bool
	invalid       bool
}

func (s usageSample) known() bool { return s.input && s.output && !s.invalid }
func readCount(m map[string]json.RawMessage, key string) (int64, bool, bool) {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return 0, false, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0, false, true
	}
	return n, true, false
}
func mergeUsage(old usageSample, next usageSample) usageSample {
	if next.input {
		old.usage.InputTokens = next.usage.InputTokens
		old.usage.CacheReadTokens = next.usage.CacheReadTokens
		old.usage.CacheWriteTokens = next.usage.CacheWriteTokens
		old.input = true
		old.usage.InputKnown = next.usage.InputKnown
	}
	if next.output {
		old.usage.OutputTokens = next.usage.OutputTokens
		old.output = true
		old.usage.OutputKnown = next.usage.OutputKnown
	}
	old.invalid = old.invalid || next.invalid
	return old
}
func decodeUsage(api string, data []byte) usageSample {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return usageSample{invalid: true}
	}
	var fields map[string]json.RawMessage
	key := "usage"
	if api == "gemini-chat" {
		key = "usageMetadata"
	}
	if api == "anthropic" {
		if raw, ok := root["message"]; ok {
			var message map[string]json.RawMessage
			if json.Unmarshal(raw, &message) == nil {
				root = message
			}
		}
	}
	if json.Unmarshal(root[key], &fields) != nil {
		return usageSample{}
	}
	inKey, outKey := "prompt_tokens", "completion_tokens"
	switch api {
	case "anthropic":
		inKey, outKey = "input_tokens", "output_tokens"
	case "gemini-chat":
		inKey, outKey = "promptTokenCount", "candidatesTokenCount"
	case "openai-embedding", "dashscope-multimodal":
		inKey = "total_tokens"
	}
	in, hasIn, badIn := readCount(fields, inKey)
	out, hasOut, badOut := readCount(fields, outKey)
	s := usageSample{input: hasIn, output: hasOut, invalid: badIn || badOut, usage: Usage{InputTokens: in, OutputTokens: out}}
	switch api {
	case "openai-embedding", "dashscope-multimodal":
		s.output = true
	case "anthropic":
		read, _, badRead := readCount(fields, "cache_read_input_tokens")
		write, _, badWrite := readCount(fields, "cache_creation_input_tokens")
		s.usage.CacheReadTokens = read
		s.usage.CacheWriteTokens = write
		s.invalid = s.invalid || badRead || badWrite
	case "gemini-chat":
		cached, _, badCached := readCount(fields, "cachedContentTokenCount")
		thoughts, _, badThoughts := readCount(fields, "thoughtsTokenCount")
		if cached > in || thoughts > math.MaxInt64-out {
			s.invalid = true
		} else {
			s.usage.InputTokens -= cached
			s.usage.CacheReadTokens = cached
			s.usage.OutputTokens += thoughts
		}
		s.invalid = s.invalid || badCached || badThoughts
	default:
		var details map[string]json.RawMessage
		if raw, ok := fields["prompt_tokens_details"]; ok && string(raw) != "null" {
			if json.Unmarshal(raw, &details) != nil {
				s.invalid = true
			} else {
				cached, _, bad := readCount(details, "cached_tokens")
				if cached > in {
					s.invalid = true
				} else {
					s.usage.InputTokens -= cached
					s.usage.CacheReadTokens = cached
				}
				s.invalid = s.invalid || bad
			}
		}
	}
	if api == "xai-chat" {
		var details map[string]json.RawMessage
		if raw, ok := fields["completion_tokens_details"]; ok && string(raw) != "null" {
			if json.Unmarshal(raw, &details) != nil {
				s.invalid = true
			} else {
				reasoning, _, bad := readCount(details, "reasoning_tokens")
				if reasoning > math.MaxInt64-s.usage.OutputTokens {
					s.invalid = true
				} else {
					s.usage.OutputTokens += reasoning
				}
				s.invalid = s.invalid || bad
			}
		}
	}
	s.usage.InputKnown = s.input && !s.invalid
	s.usage.OutputKnown = s.output && !s.invalid
	return s
}
func (op *Operation) observeStream(data string) {
	s := op.stream
	if strings.TrimSpace(data) == "[DONE]" {
		switch op.deployment.cfg.Descriptor.API {
		case "openai-chat", "xai-chat":
			s.terminal = true
		default:
			s.invalid = true
		}
		return
	}
	var root map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &root) != nil {
		s.invalid = true
		return
	}
	if raw, ok := root["error"]; ok && string(raw) != "null" {
		s.invalid = true
	}
	s.usage = mergeUsage(s.usage, decodeUsage(op.deployment.cfg.Descriptor.API, []byte(data)))
	switch op.deployment.cfg.Descriptor.API {
	case "anthropic":
		var kind string
		_ = json.Unmarshal(root["type"], &kind)
		if kind == "message_stop" {
			s.terminal = true
		}
		if kind == "error" {
			s.invalid = true
		}
	case "gemini-chat":
		var candidates []struct {
			FinishReason string `json:"finishReason"`
		}
		if json.Unmarshal(root["candidates"], &candidates) == nil {
			for _, c := range candidates {
				if c.FinishReason != "" {
					s.terminal = true
				}
			}
		}
	}
}
