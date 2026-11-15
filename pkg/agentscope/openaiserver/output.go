package openaiserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
)

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionChoice struct {
	Index   int               `json:"index"`
	Message completionMessage `json:"message"`
	Finish  string            `json:"finish_reason"`
}

type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *usage             `json:"usage,omitempty"`
}
type output struct {
	w       http.ResponseWriter
	req     *chatRequest
	id      string
	created int64
	started bool
	content strings.Builder
}

func newOutput(w http.ResponseWriter, r *chatRequest) *output {
	return &output{w: w, req: r, id: "chatcmpl-" + agentscope.GenerateID(), created: time.Now().Unix()}
}
func (o *output) chunk(choices any, u *usage) error {
	v := struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices any    `json:"choices"`
		Usage   *usage `json:"usage"`
	}{o.id, "chat.completion.chunk", o.created, o.req.Model, choices, u}
	return o.sse(v)
}

type delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}
type chunkChoice struct {
	Index  int     `json:"index"`
	Delta  delta   `json:"delta"`
	Finish *string `json:"finish_reason"`
}

func (o *output) start() error {
	if o.started {
		return nil
	}
	o.w.Header().Set("Content-Type", "text/event-stream")
	o.w.Header().Set("Cache-Control", "no-cache")
	o.w.Header().Set("X-Accel-Buffering", "no")
	o.started = true
	return o.chunk([]chunkChoice{{Delta: delta{Role: "assistant"}}}, nil)
}
func (o *output) text(text string) error {
	if !o.req.Stream {
		o.content.WriteString(text)
		return nil
	}
	if text == "" {
		return nil
	}
	if err := o.start(); err != nil {
		return err
	}
	return o.chunk([]chunkChoice{{Delta: delta{Content: text}}}, nil)
}
func (o *output) finish(u *usage) error {
	if !o.req.Stream {
		writeJSON(o.w, 200, completionResponse{
			ID: o.id, Object: "chat.completion", Created: o.created, Model: o.req.Model,
			Choices: []completionChoice{{Message: completionMessage{Role: "assistant", Content: o.content.String()}, Finish: "stop"}}, Usage: u,
		})
		return nil
	}
	if err := o.start(); err != nil {
		return err
	}
	stop := "stop"
	if err := o.chunk([]chunkChoice{{Finish: &stop}}, nil); err != nil {
		return err
	}
	if o.req.StreamOptions != nil && o.req.StreamOptions.IncludeUsage && u != nil {
		if err := o.chunk([]chunkChoice{}, u); err != nil {
			return err
		}
	}
	return o.done()
}
func (o *output) fail(err *apiError) error {
	if !o.started {
		writeError(o.w, err)
		return nil
	}
	if e := o.sse(struct {
		Error *apiError `json:"error"`
	}{err}); e != nil {
		return e
	}
	return o.done()
}
func (o *output) sse(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(o.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return http.NewResponseController(o.w).Flush()
}
func (o *output) done() error {
	if _, err := fmt.Fprint(o.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return http.NewResponseController(o.w).Flush()
}
