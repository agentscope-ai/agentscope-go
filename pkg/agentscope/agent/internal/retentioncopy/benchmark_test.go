package retentioncopy

import (
	"context"
	"strings"
	"testing"
)

// These measure copying only, not capture lock time or live inference. The
// snapshot seam and its agent lock instrumentation belong to later units.
func BenchmarkCopy(b *testing.B) {
	for _, kind := range []string{"payload_1MiB", "empty_containers_2048"} {
		b.Run(kind, func(b *testing.B) {
			l := Limits{MaxMessages: 1, MaxBlocks: 1, MaxNodes: 10000, MaxPayloadBytes: 2 << 20, MaxAllocBytes: 4 << 20, MaxDepth: 16}
			var input any
			if kind == "payload_1MiB" {
				input = []byte(strings.Repeat("x", 1<<20))
			} else {
				v := make([]any, 2048)
				for i := range v {
					v[i] = []any{}
				}
				input = v
			}
			c, err := New(l)
			if err != nil {
				b.Fatal(err)
			}
			_, stats, err := c.Copy(context.Background(), input)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := c.Copy(context.Background(), input); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(stats.Nodes), "nodes/op")
			b.ReportMetric(float64(stats.AllocBytes), "charged-B/op")
		})
	}
}
