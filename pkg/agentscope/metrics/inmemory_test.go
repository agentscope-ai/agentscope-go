package metrics

import (
	"sync"
	"testing"
)

func TestInMemoryProviderCounter(t *testing.T) {
	p := NewInMemoryProvider()
	c := p.Counter("requests", "total requests")
	c.Inc()
	c.Inc()
	c.Add(3)

	got := p.GetCounter("requests").Value()
	if got != 5 {
		t.Fatalf("Value() = %v, want 5", got)
	}
}

func TestInMemoryProviderHistogram(t *testing.T) {
	p := NewInMemoryProvider()
	h := p.Histogram("latency", "request latency", nil)
	h.Observe(1)
	h.Observe(2)
	h.Observe(4)

	hist := p.GetHistogram("latency")
	if hist.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", hist.Count())
	}
	if hist.Sum() != 7 {
		t.Fatalf("Sum() = %v, want 7", hist.Sum())
	}
	samples := hist.Samples()
	if len(samples) != 3 {
		t.Fatalf("len(Samples()) = %d, want 3", len(samples))
	}
	// Verify Samples() returns a copy: mutating it must not affect the histogram.
	samples[0] = 100
	if hist.Samples()[0] == 100 {
		t.Fatal("Samples() did not return a copy")
	}
}

func TestInMemoryProviderSnapshot(t *testing.T) {
	p := NewInMemoryProvider()
	p.Counter("a", "").Add(2)
	p.Counter("b", "").Inc()
	p.Histogram("h", "", nil).Observe(5)

	snap := p.Snapshot()
	if snap["a"] != 2 {
		t.Errorf("snapshot[a] = %v, want 2", snap["a"])
	}
	if snap["b"] != 1 {
		t.Errorf("snapshot[b] = %v, want 1", snap["b"])
	}
	if snap["h_count"] != 1 {
		t.Errorf("snapshot[h_count] = %v, want 1", snap["h_count"])
	}
	if snap["h_sum"] != 5 {
		t.Errorf("snapshot[h_sum] = %v, want 5", snap["h_sum"])
	}
}

func TestInMemoryProviderIdempotent(t *testing.T) {
	p := NewInMemoryProvider()
	c1 := p.Counter("dup", "")
	c2 := p.Counter("dup", "")
	if c1 != c2 {
		t.Fatal("Counter returned different instruments for same name")
	}
	if c1.(*InMemoryCounter) != p.GetCounter("dup") {
		t.Fatal("GetCounter returned a different instrument")
	}

	h1 := p.Histogram("duph", "", nil)
	h2 := p.Histogram("duph", "", nil)
	if h1 != h2 {
		t.Fatal("Histogram returned different instruments for same name")
	}
}

func TestInMemoryProviderConcurrencySafe(t *testing.T) {
	p := NewInMemoryProvider()
	c := p.Counter("hits", "")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if got := p.GetCounter("hits").Value(); got != 100 {
		t.Fatalf("Value() = %v, want 100", got)
	}
}
