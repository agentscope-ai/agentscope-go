package participant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Leased wraps t so each call holds a lease on the resource resourceOf
// returns (for example the file path argument) while it runs. A call whose
// resource is leased to another holder returns an error result naming the
// holder; resourceOf returning "" runs without a lease.
//
// The lease is renewed every ttl/2 while the call runs and released when it
// returns (for a streaming tool, when its stream closes). A failed renewal is
// logged. When another holder took the lease meanwhile (it expired, for
// example because renewal stalled), renewal stops and the call's result ends
// with a note that the resource was not exclusively held for the whole call.
// Calls of the same holder on the same resource share the lease, which is
// released by the last one. The wrapper keeps t's name, schema, permission and
// read-only answers and every optional interface t implements:
// tool.InputValidator, tool.ExternalResultValidator, its tool middlewares,
// tool.StreamingTool and permission.CompoundRuleMatcher.
func Leased(t tool.Tool, leases *thread.Leases, holder string, ttl time.Duration, resourceOf func(args map[string]any) string) tool.Tool {
	base := &leasedTool{Tool: t, leases: leases, holder: holder, ttl: ttl, resourceOf: resourceOf}
	st, streaming := t.(tool.StreamingTool)
	cm, compound := t.(permission.CompoundRuleMatcher)
	switch {
	case streaming && compound:
		return &leasedStreamingCompound{leasedStreaming{base, st}, cm}
	case streaming:
		return &leasedStreaming{base, st}
	case compound:
		return &leasedCompound{base, cm}
	default:
		return base
	}
}

// leasedTool delegates every tool.Tool method to the wrapped tool and holds
// the lease around Execute.
type leasedTool struct {
	tool.Tool
	leases     *thread.Leases
	holder     string
	ttl        time.Duration
	resourceOf func(args map[string]any) string
}

// Execute runs the wrapped tool while holding the lease.
func (l *leasedTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	release, refusal := l.acquire(input)
	if refusal != nil {
		return refusal, nil
	}
	defer release()
	resp, err := l.Tool.Execute(ctx, input)
	if note := release(); note != "" && resp != nil {
		cp := *resp
		cp.Content = append(append([]message.ContentBlock(nil), resp.Content...), noteBlock(note))
		resp = &cp
	}
	return resp, err
}

func noteBlock(text string) message.TextBlock {
	return message.TextBlock{Type: "text", Text: text}
}

// ValidateInput delegates to the wrapped tool's tool.InputValidator.
func (l *leasedTool) ValidateInput(input map[string]any) error {
	if v, ok := l.Tool.(tool.InputValidator); ok {
		return v.ValidateInput(input)
	}
	return nil
}

// ValidateExternalResult delegates to the wrapped tool's
// tool.ExternalResultValidator.
func (l *leasedTool) ValidateExternalResult(input map[string]any, result *message.ToolResultBlock) error {
	if v, ok := l.Tool.(tool.ExternalResultValidator); ok {
		return v.ValidateExternalResult(input, result)
	}
	return nil
}

// GetMiddlewares returns the wrapped tool's middlewares, so the toolkit runs
// them around the leased call.
func (l *leasedTool) GetMiddlewares() []tool.ToolMiddleware {
	if m, ok := l.Tool.(interface{ GetMiddlewares() []tool.ToolMiddleware }); ok {
		return m.GetMiddlewares()
	}
	return nil
}

// acquire takes the lease for the call's resource. It returns the release
// function, or an error response when the resource is held by another holder.
// Release may be called more than once; every call returns the note for the
// call's result, empty unless another holder took the lease during the call.
func (l *leasedTool) acquire(input map[string]any) (func() string, *tool.ToolResponse) {
	resource := ""
	if l.resourceOf != nil {
		resource = l.resourceOf(input)
	}
	if resource == "" {
		return func() string { return "" }, nil
	}
	key := leaseKey{leases: l.leases, resource: resource, holder: l.holder}
	if err := inflight.acquire(key, l.ttl); err != nil {
		if errors.Is(err, thread.ErrLeaseHeld) {
			other, _ := l.leases.Holder(resource)
			return nil, tool.NewErrorResponse(fmt.Errorf("%s is in use by %s; wait for it to finish or work on something else", resource, other))
		}
		return nil, tool.NewErrorResponse(fmt.Errorf("lease %s: %w", resource, err))
	}
	stop := make(chan struct{})
	var renew sync.WaitGroup
	var lostTo string // written by the renewal goroutine, read after renew.Wait
	renew.Add(1)
	go func() {
		defer renew.Done()
		lostTo = l.renew(resource, stop)
	}()
	var once sync.Once
	var note string
	return func() string {
		once.Do(func() {
			close(stop)
			renew.Wait()
			inflight.release(key)
			if lostTo != "" {
				note = fmt.Sprintf("[The lease on %s was lost during this call: %s took it, so the resource was not "+
					"exclusively held for the whole call.]", resource, lostTo)
			}
		})
		return note
	}, nil
}

// renew extends the lease every ttl/2 until stop closes. The first renewal
// acquired the same resource for the same holder and ttl, so a renewal can
// only fail because another holder took the lease after it expired; the
// failure is logged, renewal stops and renew returns that holder.
func (l *leasedTool) renew(resource string, stop <-chan struct{}) string {
	ticker := time.NewTicker(max(l.ttl/2, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return ""
		case <-ticker.C:
		}
		_, err := l.leases.Acquire(resource, l.holder, l.ttl)
		if err == nil {
			continue
		}
		// The holder may already have released it again.
		other, _ := l.leases.Holder(resource)
		other = cmp.Or(other, "another holder")
		agentscope.Log().WithError(err).WithField("resource", resource).WithField("holder", l.holder).WithField("taken_by", other).
			Warn("participant: renewing the lease failed; another holder took it during a call")
		return other
	}
}

// leaseKey identifies one holder's lease on one resource in one table.
type leaseKey struct {
	leases   *thread.Leases
	resource string
	holder   string
}

// inflight counts the running calls per lease, so concurrent calls of one
// holder do not release a lease another of its calls still needs.
var inflight = &leaseCounter{n: map[leaseKey]int{}}

// leaseCounter takes and releases leases under one mutex, so a call that
// takes a lease its holder already holds cannot fall between another call's
// last count and its release.
type leaseCounter struct {
	mu sync.Mutex
	n  map[leaseKey]int
}

// acquire takes or extends the lease for one more call of its holder.
func (c *leaseCounter) acquire(k leaseKey, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := k.leases.Acquire(k.resource, k.holder, ttl); err != nil {
		return err
	}
	c.n[k]++
	return nil
}

// release ends one call and releases the lease when it was the last.
func (c *leaseCounter) release(k leaseKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[k]--
	if c.n[k] > 0 {
		return
	}
	delete(c.n, k)
	_ = k.leases.Release(k.resource, k.holder)
}

// leasedStreaming adds tool.StreamingTool for a streaming wrapped tool.
type leasedStreaming struct {
	*leasedTool
	st tool.StreamingTool
}

// ExecuteStream holds the lease until the wrapped tool's stream closes.
func (l *leasedStreaming) ExecuteStream(ctx context.Context, input map[string]any) (<-chan tool.ToolChunk, error) {
	release, refusal := l.acquire(input)
	if refusal != nil {
		ch := make(chan tool.ToolChunk, 1)
		ch <- tool.ToolChunk{Content: refusal.Content, IsFinal: true, State: refusal.State}
		close(ch)
		return ch, nil
	}
	in, err := l.st.ExecuteStream(ctx, input)
	if err != nil {
		release()
		return nil, err
	}
	out := make(chan tool.ToolChunk)
	go func() {
		defer close(out)
		defer release()
		// Every chunk is delivered while the call's context lives. Once it
		// ended, a chunk waits at most leaseSendWait for the reader; a
		// reader that does not take it has left, and the rest of the
		// wrapped stream is drained without forwarding. The lease is held
		// until the wrapped stream closes either way.
		gone := false
		send := func(c tool.ToolChunk) {
			if gone {
				return
			}
			if ctx.Err() == nil {
				select {
				case out <- c:
					return
				case <-ctx.Done():
				}
			}
			timer := time.NewTimer(leaseSendWait)
			defer timer.Stop()
			select {
			case out <- c:
			case <-timer.C:
				gone = true
			}
		}
		// The final chunk is held until the stream closes, so the note of a
		// lease lost during the call can still be added to it.
		var final *tool.ToolChunk
		for chunk := range in {
			if final != nil {
				send(*final)
				final = nil
			}
			if chunk.IsFinal {
				c := chunk
				final = &c
				continue
			}
			send(chunk)
		}
		note := release()
		switch {
		case final != nil:
			if note != "" {
				final.Content = append(append([]message.ContentBlock(nil), final.Content...), noteBlock(note))
			}
			send(*final)
		case note != "":
			send(tool.ToolChunk{Content: []message.ContentBlock{noteBlock(note)}, IsFinal: true})
		}
	}()
	return out, nil
}

// leaseSendWait bounds how long a leased stream waits for its reader once
// the call's context ended. Tests shorten it.
var leaseSendWait = 5 * time.Second

// leasedCompound adds permission.CompoundRuleMatcher.
type leasedCompound struct {
	*leasedTool
	cm permission.CompoundRuleMatcher
}

func (l *leasedCompound) MatchAllSubcommands(rules []string, input map[string]any) bool {
	return l.cm.MatchAllSubcommands(rules, input)
}

func (l *leasedCompound) MatchAnySubcommand(rules []string, input map[string]any) bool {
	return l.cm.MatchAnySubcommand(rules, input)
}

// leasedStreamingCompound is a streaming tool with compound rule matching.
type leasedStreamingCompound struct {
	leasedStreaming
	cm permission.CompoundRuleMatcher
}

func (l *leasedStreamingCompound) MatchAllSubcommands(rules []string, input map[string]any) bool {
	return l.cm.MatchAllSubcommands(rules, input)
}

func (l *leasedStreamingCompound) MatchAnySubcommand(rules []string, input map[string]any) bool {
	return l.cm.MatchAnySubcommand(rules, input)
}
