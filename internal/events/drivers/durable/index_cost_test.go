package durable

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestIndexV2_AC1_PerAppendCostIndependentOfHistory measures publish latency
// on real Postgres into sessions retaining 1k, 50k and 200k events. Publishes
// are interleaved round-robin so drift affects every session equally. Each
// p50 must be within ±20 % of the 1k session's and below 15 ms.
func TestIndexV2_AC1_PerAppendCostIndependentOfHistory(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("per-append latency acceptance (skipped under -short and -race)")
	}
	store := newPostgresStore(t)
	sizes := []int{1_000, 50_000, 200_000}
	ids := make([]identity.Quadruple, len(sizes))
	var start uint64
	for i, n := range sizes {
		ids[i] = sid(fmt.Sprintf("history-%d", n))
		seedV2Session(t, store, seedSpec{id: ids[i], startSeq: start, n: n, at: indexEpoch})
		start += uint64(n)
	}
	b := openIndexBus(t, store, 0)
	p50s := measureAppendP50(t, b, ids, 300)
	for i, n := range sizes {
		t.Logf("retained=%d publish p50=%s", n, p50s[i])
		if p50s[i] > 15*time.Millisecond {
			t.Fatalf("retained=%d p50 %s >= 15ms", n, p50s[i])
		}
		if dev := math.Abs(float64(p50s[i]-p50s[0])) / float64(p50s[0]); dev > 0.20 {
			t.Fatalf("retained=%d p50 %s deviates %.0f%% from the 1k p50 %s (want within ±20%%)", n, p50s[i], dev*100, p50s[0])
		}
	}
}

func measureAppendP50(t testing.TB, b *bus, ids []identity.Quadruple, rounds int) []time.Duration {
	lat := make([][]time.Duration, len(ids))
	ctx := context.Background()
	for r := range rounds {
		for i, id := range ids {
			ev := testEvent(id, events.EventTypeRuntimeWarning, indexEpoch.Add(time.Duration(r)*time.Second))
			began := time.Now()
			if err := b.Publish(ctx, ev); err != nil {
				t.Fatalf("publish: %v", err)
			}
			lat[i] = append(lat[i], time.Since(began))
		}
	}
	out := make([]time.Duration, len(ids))
	for i := range lat {
		sort.Slice(lat[i], func(a, c int) bool { return lat[i][a] < lat[i][c] })
		out[i] = lat[i][len(lat[i])/2]
	}
	return out
}

// BenchmarkIndexV2_PublishByRetainedHistory reports per-publish cost into a
// session retaining N events (inmem store, so it isolates index work).
func BenchmarkIndexV2_PublishByRetainedHistory(b *testing.B) {
	for _, n := range []int{1_000, 50_000, 200_000} {
		b.Run(fmt.Sprintf("retained=%d", n), func(b *testing.B) {
			store := newInmemTestStore(b)
			id := sid("bench")
			seedV2Session(b, store, seedSpec{id: id, n: n, at: indexEpoch})
			bb := openIndexBus(b, store, 0)
			b.ResetTimer()
			for range b.N {
				if err := bb.Publish(context.Background(), testEvent(id, events.EventTypeRuntimeWarning, indexEpoch)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
