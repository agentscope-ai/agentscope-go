package metrics

import (
	"strings"
	"sync"
)

// seriesKey joins label values into a stable per-series key.
func seriesKey(labels []string) string { return strings.Join(labels, "\x1f") }

// InMemoryCounter is a thread-safe Counter that keeps its total in memory and,
// additionally, a per-label-set breakdown so labeled metrics do not collapse to
// a single series.
type InMemoryCounter struct {
	mu     sync.Mutex
	value  float64
	series map[string]float64
}

// Inc increments the counter by 1.
func (c *InMemoryCounter) Inc(labels ...string) { c.Add(1, labels...) }

// Add increments the counter by value for the given label set.
func (c *InMemoryCounter) Add(value float64, labels ...string) {
	c.mu.Lock()
	c.value += value
	if c.series == nil {
		c.series = make(map[string]float64)
	}
	c.series[seriesKey(labels)] += value
	c.mu.Unlock()
}

// Value returns the counter total across all label sets.
func (c *InMemoryCounter) Value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

// ValueFor returns the counter total for a specific label set.
func (c *InMemoryCounter) ValueFor(labels ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.series == nil {
		return 0
	}
	return c.series[seriesKey(labels)]
}

// InMemoryHistogram is a thread-safe Histogram that retains every observation.
type InMemoryHistogram struct {
	mu      sync.Mutex
	sum     float64
	samples []float64
}

// Observe records a single observation. Label values are ignored.
func (h *InMemoryHistogram) Observe(value float64, _ ...string) {
	h.mu.Lock()
	h.sum += value
	h.samples = append(h.samples, value)
	h.mu.Unlock()
}

// Count returns the number of recorded observations.
func (h *InMemoryHistogram) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.samples)
}

// Sum returns the sum of all recorded observations.
func (h *InMemoryHistogram) Sum() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sum
}

// Samples returns a copy of all recorded observations.
func (h *InMemoryHistogram) Samples() []float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]float64, len(h.samples))
	copy(out, h.samples)
	return out
}

// InMemoryProvider is a MetricsProvider that stores metrics in memory. It is
// safe for concurrent use and returns the same instrument for repeated calls
// with the same name.
type InMemoryProvider struct {
	mu         sync.Mutex
	counters   map[string]*InMemoryCounter
	histograms map[string]*InMemoryHistogram
}

// NewInMemoryProvider returns a ready-to-use InMemoryProvider.
func NewInMemoryProvider() *InMemoryProvider {
	return &InMemoryProvider{
		counters:   make(map[string]*InMemoryCounter),
		histograms: make(map[string]*InMemoryHistogram),
	}
}

// Counter returns the counter identified by name, creating it if needed.
func (p *InMemoryProvider) Counter(name, _ string, _ ...string) Counter {
	return p.GetCounter(name)
}

// Histogram returns the histogram identified by name, creating it if needed.
func (p *InMemoryProvider) Histogram(name, _ string, _ []float64, _ ...string) Histogram {
	return p.GetHistogram(name)
}

// GetCounter returns the concrete counter identified by name, creating it if
// needed. Repeated calls with the same name return the same instance.
func (p *InMemoryProvider) GetCounter(name string) *InMemoryCounter {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.counters[name]
	if !ok {
		c = &InMemoryCounter{}
		p.counters[name] = c
	}
	return c
}

// GetHistogram returns the concrete histogram identified by name, creating it
// if needed. Repeated calls with the same name return the same instance.
func (p *InMemoryProvider) GetHistogram(name string) *InMemoryHistogram {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.histograms[name]
	if !ok {
		h = &InMemoryHistogram{}
		p.histograms[name] = h
	}
	return h
}

// Snapshot returns a map of counter and histogram values. Counters map to
// their current total; histograms contribute a "<name>_count" and
// "<name>_sum" entry.
func (p *InMemoryProvider) Snapshot() map[string]float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]float64, len(p.counters)+2*len(p.histograms))
	for name, c := range p.counters {
		out[name] = c.Value()
	}
	for name, h := range p.histograms {
		out[name+"_count"] = float64(h.Count())
		out[name+"_sum"] = h.Sum()
	}
	return out
}
