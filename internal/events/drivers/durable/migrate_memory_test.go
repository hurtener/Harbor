package durable

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// TestMigration_MemoryPeakLargeHead measures the conversion's peak live heap
// for a 105k-row (~20 MB) legacy head, on SQLite and Postgres, plus
// other-session publish latency during it. It is a measurement, opt-in via
// HARBOR_MEASURE_MIGRATION_MEMORY=1; run it with GOMAXPROCS=1 and
// GOMEMLIMIT=400MiB to mirror a 512 MB / 0.5 CPU host. The asserted bound is
// a regression guard on the reduced peak.
func TestMigration_MemoryPeakLargeHead(t *testing.T) {
	if os.Getenv("HARBOR_MEASURE_MIGRATION_MEMORY") == "" {
		t.Skip("set HARBOR_MEASURE_MIGRATION_MEMORY=1 to measure conversion memory")
	}
	for _, name := range []string{"sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var store state.StateStore
			if name == "sqlite" {
				store = newSQLiteStore(t)
			} else {
				store = newPostgresStore(t)
			}
			// Production-shaped identifiers (UUID-length) give a ~20 MB head.
			id := identity.Quadruple{Identity: identity.Identity{
				TenantID:  "tenant-0b9f3c2e6d41a",
				UserID:    "user-5e2a91c47b3d4f0",
				SessionID: "session-c7d18e2f3a95",
			}}
			seedLegacySession(t, store, seedSpec{id: id, n: 105_000, at: indexEpoch})
			raw, err := store.Load(context.Background(), id, kindHead)
			mustf(t, err, "load head")
			headBytes := len(raw.Bytes)
			raw = state.StateRecord{}
			b := openIndexBus(t, store, 0, withMaintenance(func(m *maintenance) { m.prewritePause = migrationPrewritePause }))
			ctx := context.Background()

			publishLoad := func(stop <-chan struct{}) (time.Duration, int) {
				var mu sync.Mutex
				var worst time.Duration
				n := 0
				var wg sync.WaitGroup
				for p := 0; p < 4; p++ {
					wg.Add(1)
					go func(p int) {
						defer wg.Done()
						other := sid(fmt.Sprintf("load-%d", p))
						for {
							select {
							case <-stop:
								return
							default:
							}
							start := time.Now()
							mustf(t, b.Publish(ctx, testEvent(other, events.EventTypeRuntimeWarning, indexEpoch)), "publish")
							d := time.Since(start)
							mu.Lock()
							n++
							if d > worst {
								worst = d
							}
							mu.Unlock()
							time.Sleep(10 * time.Millisecond)
						}
					}(p)
				}
				<-stop
				wg.Wait()
				return worst, n
			}
			stopBase := make(chan struct{})
			var baseWorst time.Duration
			var baseN int
			baseDone := make(chan struct{})
			go func() { baseWorst, baseN = publishLoad(stopBase); close(baseDone) }()
			time.Sleep(2 * time.Second)
			close(stopBase)
			<-baseDone

			loadPeak := measurePeak(func() {
				rec, err := store.Load(ctx, id, kindHead)
				mustf(t, err, "load")
				runtime.KeepAlive(rec)
			})
			t.Logf("%s: a single Load of the legacy head peaks at +%.1f MiB live heap (driver read path)", name, mib(loadPeak))
			runtime.GC()
			runtime.GC()
			baseline := liveHeap()
			var peak, peakLive uint64
			liveBase := markedLive()
			stopSample := make(chan struct{})
			sampled := make(chan struct{})
			go func() {
				defer close(sampled)
				for {
					if h := liveHeap(); h > peak {
						peak = h
					}
					if l := markedLive(); l > peakLive {
						peakLive = l
					}
					select {
					case <-stopSample:
						return
					case <-time.After(time.Millisecond):
					}
				}
			}()
			stopLoad := make(chan struct{})
			var worst time.Duration
			var n int
			loadDone := make(chan struct{})
			go func() { worst, n = publishLoad(stopLoad); close(loadDone) }()
			start := time.Now()
			flipped, err := b.migrateSession(ctx, id)
			took := time.Since(start)
			close(stopLoad)
			<-loadDone
			close(stopSample)
			<-sampled
			mustf(t, err, "migrate")
			if !flipped {
				t.Fatal("not migrated")
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			t.Logf("%s: head %.1f MB; migration %s; heap objects baseline %.1f MiB, peak %.1f MiB (+%.1f MiB); GC-marked live peak +%.1f MiB; process heap sys %.1f MiB; GOMAXPROCS=%d; other-session publish worst %s during (%d publishes) vs %s baseline (%d)",
				name, float64(headBytes)/1e6, took.Round(time.Millisecond), mib(baseline), mib(peak), mib(peak-baseline), mib(peakLive-min(peakLive, liveBase)), mib(ms.HeapSys),
				runtime.GOMAXPROCS(0), worst.Round(time.Millisecond), n, baseWorst.Round(time.Millisecond), baseN)
			// Guard the live set (what the conversion holds), not transient
			// garbage the GC collects at its own pace.
			if extra := peakLive - min(peakLive, liveBase); extra > migrationLiveBudget {
				t.Fatalf("conversion GC-marked live peak +%.1f MiB exceeds the %.0f MiB budget", mib(extra), mib(migrationLiveBudget))
			}
			if rows := sessionIndexSeqs(t, store, id); len(rows) != 105_000 {
				t.Fatalf("migrated rows %d", len(rows))
			}
		})
	}
}

// migrationLiveBudget bounds the GC-marked live-heap increase of converting
// a 105k-row / 20 MB legacy head (measured ~20–30 MiB after the streaming
// decode; ~50–70 MiB before it).
const migrationLiveBudget = 48 << 20

func liveHeap() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// markedLive is the heap marked live by the most recent GC cycle.
func markedLive() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

func mib(v uint64) float64 { return float64(v) / (1 << 20) }

// measurePeak runs fn and returns the live-heap high-water mark above the
// post-GC baseline.
func measurePeak(fn func()) uint64 {
	runtime.GC()
	runtime.GC()
	base := liveHeap()
	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if h := liveHeap(); h > peak {
				peak = h
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if peak < base {
		return 0
	}
	return peak - base
}

// TestStreamLegacyRows_MatchesFullDecodeAndFallsBack proves the streaming
// decoder returns exactly the rows the ordinary decode path does, verifies a
// prefix, and declines (so the caller falls back) on any unauthenticated or
// unexpected shape.
func TestStreamLegacyRows_MatchesFullDecodeAndFallsBack(t *testing.T) {
	store := newInmemTestStore(t)
	id := sid("stream")
	seedLegacySession(t, store, seedSpec{id: id, n: 3000, at: indexEpoch, typeOf: func(i int) events.EventType {
		if i%5 == 0 {
			return events.EventTypeAdminScopeUsed
		}
		return events.EventTypeRuntimeWarning
	}})
	rec, err := store.Load(context.Background(), id, kindHead)
	mustf(t, err, "load")
	head, err := decodeHead(rec.Bytes)
	mustf(t, err, "decode")
	want, err := legacyRows(head)
	mustf(t, err, "rows")
	got, err := streamLegacyRows(rec.Bytes, id, nil, 0)
	mustf(t, err, "stream")
	if len(got) != len(want) {
		t.Fatalf("stream returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
	// Prefix mode: the same head extends its own first 2,000 rows.
	extended, err := streamLegacyRows(rec.Bytes, id, append([]indexRow(nil), want[:2000]...), 0)
	if err != nil || len(extended) != 3000 || extended[2999] != want[2999] {
		t.Fatalf("prefix stream: %d rows, %v", len(extended), err)
	}
	bad := append([]indexRow(nil), want[:2000]...)
	bad[1500].RunID = "other"
	if _, err := streamLegacyRows(rec.Bytes, id, bad, 0); !errors.Is(err, errLegacyStreamUnsupported) {
		t.Fatalf("changed prefix on the stream = %v, want a decline", err)
	}
	b := &bus{store: store, clock: realClock{}, logger: discardTestLogger()}
	if _, err := b.loadLegacyRows(context.Background(), id, rec.Bytes, bad); !errors.Is(err, ErrIndexIntegrity) {
		t.Fatalf("changed prefix through the full path = %v, want ErrIndexIntegrity", err)
	}
	declines := func(name string, raw []byte) {
		t.Helper()
		if _, err := streamLegacyRows(raw, id, nil, 0); !errors.Is(err, errLegacyStreamUnsupported) {
			t.Fatalf("%s = %v, want the stream to decline (full-path decides)", name, err)
		}
	}
	// Trailing data / concatenated values, which json.Unmarshal rejects.
	declines("trailing garbage", append(append([]byte(nil), rec.Bytes...), []byte("garbage")...))
	declines("concatenated heads", append(append([]byte(nil), rec.Bytes...), rec.Bytes...))
	// Case-variant and duplicate keys, which json.Unmarshal honours.
	trimmed := rec.Bytes[:len(rec.Bytes)-1]
	declines("case-variant key", append(append([]byte(nil), trimmed...), []byte(`,"Sequences":[1,2,3,4]}`)...))
	declines("case-variant ready key", append(append([]byte(nil), trimmed...), []byte(`,"Metadata_Ready":false}`)...))
	declines("duplicate key", append(append([]byte(nil), trimmed...), []byte(`,"metadata_ready":true}`)...))
	declines("unknown key", append(append([]byte(nil), trimmed...), []byte(`,"extra":1}`)...))
	// A partial backfill checkpoint (2 validated rows for 5 sequences, not
	// ready) and a stale row in a not-ready head must fall back to the body
	// backfill, not fail hard.
	partial := headRecord{Sequences: head.Sequences[:5], Metadata: append([]eventMetadataRecord(nil), head.Metadata[:2]...), MetadataValidatedCount: 2}
	partial.MetadataIntegrityChecksum = metadataIntegrityChecksum(partial.Sequences[:2], partial.Metadata)
	raw, _ := encodeHead(partial)
	declines("partial checkpoint", raw)
	stale := headRecord{Sequences: head.Sequences[:5], Metadata: append([]eventMetadataRecord(nil), head.Metadata[:5]...)}
	stale.Metadata[3].TenantID = "someone-else"
	stale.Metadata[1].Sequence = stale.Metadata[2].Sequence
	raw, _ = encodeHead(stale)
	declines("stale rows in a not-ready head", raw)
	// Unauthenticated or reordered heads are declined, never trusted.
	tampered := head
	tampered.Metadata = append([]eventMetadataRecord(nil), head.Metadata...)
	tampered.Metadata[7].Type = events.EventTypeRuntimeError
	raw, _ = encodeHead(tampered)
	if _, err := streamLegacyRows(raw, id, nil, 0); !errors.Is(err, errLegacyStreamUnsupported) {
		t.Fatalf("checksum mismatch = %v, want fallback", err)
	}
	reordered := []byte(`{"metadata":[],"sequences":[1]}`)
	if _, err := streamLegacyRows(reordered, id, nil, 0); !errors.Is(err, errLegacyStreamUnsupported) {
		t.Fatalf("reordered head = %v, want fallback", err)
	}
	notReady := head
	notReady.MetadataReady = false
	raw, _ = encodeHead(notReady)
	if _, err := streamLegacyRows(raw, id, nil, 0); !errors.Is(err, errLegacyStreamUnsupported) {
		t.Fatalf("not-ready head = %v, want fallback", err)
	}
}
