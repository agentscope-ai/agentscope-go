package model

import (
	"errors"
	"fmt"
	"testing"

	aserr "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

func TestIsContextLimitExceeded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed sentinel", aserr.ErrModelContextLimit, true},
		{"wrapped sentinel", fmt.Errorf("anthropic: %w", aserr.ErrModelContextLimit), true},
		{
			"anthropic prompt too long",
			errors.New(`anthropic: httpx: unexpected status 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213504 tokens > 200000 maximum"}}`),
			true,
		},
		{
			"openai context_length_exceeded",
			errors.New(`openai: httpx: unexpected status 400: {"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`),
			true,
		},
		{
			"openai maximum context length wording",
			errors.New(`openai: httpx: unexpected status 400: This model's maximum context length is 65536 tokens`),
			true,
		},
		{
			"gemini token count",
			errors.New(`gemini: httpx: unexpected status 400: The input token count (1296495) exceeds the maximum number of tokens allowed (1048576).`),
			true,
		},
		{
			"ollama input length",
			errors.New(`ollama: the input length exceeds the context length`),
			true,
		},
		{
			"dashscope range",
			errors.New(`dashscope: httpx: unexpected status 400: Range of input length should be [1, 6000]`),
			true,
		},
		{
			"moonshot token limit",
			errors.New(`moonshot: httpx: unexpected status 400: Your request exceeded model token limit: 131072`),
			true,
		},
		{
			"http 413",
			errors.New(`openai: httpx: unexpected status 413: Request Entity Too Large`),
			true,
		},
		{
			"rate limit is not context overflow",
			errors.New(`openai: httpx: unexpected status 429: rate limit exceeded, too many tokens per minute`),
			false,
		},
		{
			"auth error is not context overflow",
			errors.New(`anthropic: httpx: unexpected status 401: invalid api key`),
			false,
		},
		{
			"generic bad request is not context overflow",
			errors.New(`openai: httpx: unexpected status 400: unknown parameter foo`),
			false,
		},
		{
			"stream error is not context overflow",
			errors.New(`anthropic: stream error overloaded_error: Overloaded`),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsContextLimitExceeded(tc.err); got != tc.want {
				t.Errorf("IsContextLimitExceeded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
