package prometheus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProvider_ScrapeExposesLabeledMetrics proves counters/histograms are
// exported per label set and appear in the /metrics scrape output.
func TestProvider_ScrapeExposesLabeledMetrics(t *testing.T) {
	p := New()

	c := p.Counter("as_model_call_total", "model calls", "provider")
	c.Inc("openai")
	c.Add(2, "openai")
	c.Inc("anthropic")

	h := p.Histogram("as_model_call_seconds", "durations", []float64{0.1, 1, 10}, "provider")
	h.Observe(0.5, "openai")

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	for _, want := range []string{
		`as_model_call_total{provider="openai"} 3`,
		`as_model_call_total{provider="anthropic"} 1`,
		"as_model_call_seconds_bucket",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape output missing %q\n---\n%s", want, out)
		}
	}
}

// TestProvider_LabelMismatchNoPanic ensures a wrong label-value count does not
// panic the Prometheus vector (values are aligned to the declared names).
func TestProvider_LabelMismatchNoPanic(t *testing.T) {
	p := New()
	c := p.Counter("as_test_total", "t", "a", "b")
	c.Inc("only-one")    // fewer values than names
	c.Inc("x", "y", "z") // more values than names
	_ = p.Counter("as_test_total", "t", "a", "b")
}
