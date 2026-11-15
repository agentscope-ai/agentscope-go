package metrics

import (
	"fmt"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

func TestMetricsHookRecordsModelCall(t *testing.T) {
	p := newRecordingProvider()
	h := NewMetricsHook(p)

	h.BeforeModelCall(protocol.StateReason, 0)
	h.AfterModelCall(protocol.StateReason, 0, nil)

	counter := p.counters[ModelCallTotal]
	if counter == nil {
		t.Fatal("ModelCallTotal counter not created")
	}
	if counter.value != 1 {
		t.Errorf("ModelCallTotal = %v, want 1", counter.value)
	}
}

func TestMetricsHookRecordsModelCallError(t *testing.T) {
	p := newRecordingProvider()
	h := NewMetricsHook(p)

	h.BeforeModelCall(protocol.StateReason, 0)
	h.AfterModelCall(protocol.StateReason, 0, fmt.Errorf("api error"))

	errCounter := p.counters[ModelCallErrors]
	if errCounter == nil {
		t.Fatal("ModelCallErrors counter not created")
	}
	if errCounter.value != 1 {
		t.Errorf("ModelCallErrors = %v, want 1", errCounter.value)
	}
}

func TestMetricsHookRecordsToolExec(t *testing.T) {
	p := newRecordingProvider()
	h := NewMetricsHook(p)

	h.BeforeToolExec(protocol.StateAct, 0, "bash")
	h.AfterToolExec(protocol.StateAct, 0, "bash", nil)

	counter := p.counters[ToolExecTotal]
	if counter == nil {
		t.Fatal("ToolExecTotal counter not created")
	}
	if counter.value != 1 {
		t.Errorf("ToolExecTotal = %v, want 1", counter.value)
	}
}

func TestMetricsHookRecordsLoopIteration(t *testing.T) {
	p := newRecordingProvider()
	h := NewMetricsHook(p)

	h.OnStateTransition(protocol.StateReason, protocol.StateInspect, 0)
	h.OnStateTransition(protocol.StateInspect, protocol.StateAct, 0)
	h.OnStateTransition(protocol.StateAct, protocol.StateReason, 0)

	counter := p.counters[LoopIterations]
	if counter == nil {
		t.Fatal("LoopIterations counter not created")
	}
	if counter.value != 1 {
		t.Errorf("LoopIterations = %v, want 1", counter.value)
	}
}

func TestMetricsHookActiveLoops(t *testing.T) {
	p := newRecordingProvider()
	h := NewMetricsHook(p)

	h.OnLoopStart()
	counter := p.counters[ActiveLoops]
	if counter == nil {
		t.Fatal("ActiveLoops counter not created")
	}
	if counter.value != 1 {
		t.Errorf("ActiveLoops after start = %v, want 1", counter.value)
	}

	h.OnLoopEnd(nil)
	if counter.value != 0 {
		t.Errorf("ActiveLoops after end = %v, want 0", counter.value)
	}
}

func TestMetricsHookNoopProviderDoesNotPanic(t *testing.T) {
	h := NewMetricsHook(Noop())
	h.BeforeModelCall(protocol.StateReason, 0)
	h.AfterModelCall(protocol.StateReason, 0, nil)
	h.BeforeToolExec(protocol.StateAct, 0, "bash")
	h.AfterToolExec(protocol.StateAct, 0, "bash", nil)
	h.OnStateTransition(protocol.StateReason, protocol.StateInspect, 0)
	h.OnLoopStart()
	h.OnLoopEnd(nil)
}

// --- Recording test helpers ---

type recordingCounter struct {
	mu    sync.Mutex
	value float64
}

func (c *recordingCounter) Inc(_ ...string) {
	c.mu.Lock()
	c.value++
	c.mu.Unlock()
}

func (c *recordingCounter) Add(v float64, _ ...string) {
	c.mu.Lock()
	c.value += v
	c.mu.Unlock()
}

type recordingHistogram struct {
	mu      sync.Mutex
	samples []float64
}

func (h *recordingHistogram) Observe(v float64, _ ...string) {
	h.mu.Lock()
	h.samples = append(h.samples, v)
	h.mu.Unlock()
}

type recordingProvider struct {
	mu         sync.Mutex
	counters   map[string]*recordingCounter
	histograms map[string]*recordingHistogram
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{
		counters:   make(map[string]*recordingCounter),
		histograms: make(map[string]*recordingHistogram),
	}
}

func (p *recordingProvider) Counter(name, _ string, _ ...string) Counter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.counters[name]; ok {
		return c
	}
	c := &recordingCounter{}
	p.counters[name] = c
	return c
}

func (p *recordingProvider) Histogram(name, _ string, _ []float64, _ ...string) Histogram {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.histograms[name]; ok {
		return h
	}
	h := &recordingHistogram{}
	p.histograms[name] = h
	return h
}
