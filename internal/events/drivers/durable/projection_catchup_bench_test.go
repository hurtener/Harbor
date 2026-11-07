package durable_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

type countingStore struct {
	state.StateStore
	loads, lists, listBytes, loadBytes atomic.Int64
}

func (c *countingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	c.loads.Add(1)
	r, err := c.StateStore.Load(ctx, id, kind)
	c.loadBytes.Add(int64(len(r.Bytes)))
	return r, err
}

func (c *countingStore) ListKind(ctx context.Context, s state.ListScope, p string) ([]state.StateRecord, error) {
	c.lists.Add(1)
	recs, err := c.StateStore.ListKind(ctx, s, p)
	for _, r := range recs {
		c.listBytes.Add(int64(len(r.Bytes)))
	}
	return recs, err
}

type benchPayload struct {
	events.Sealed
	Note string
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return d
}

// TestProjectionCatchUpBench is an opt-in measurement (HARBOR_PROJECTION_BENCH=1):
// a synthetic durable log of N events over S sessions in bursts, then a cold
// catch-up from cursor 0 and a warm catch-up of the last W events, both paged
// at 256 like the turns materializer.
func TestProjectionCatchUpBench(t *testing.T) {
	if os.Getenv("HARBOR_PROJECTION_BENCH") == "" {
		t.Skip("reason: opt-in measurement; set HARBOR_PROJECTION_BENCH=1")
	}
	n := envInt("BENCH_EVENTS", 285_000)
	sessions := envInt("BENCH_SESSIONS", 2_000)
	burst := envInt("BENCH_BURST", 20)
	warm := envInt("BENCH_WARM", 2_560)
	var inner state.StateStore
	var err error
	if dsn := os.Getenv("BENCH_PG_DSN"); dsn != "" {
		inner, err = statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	} else {
		inner, err = stateinmem.New(config.StateConfig{Driver: "inmem"})
	}
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{StateStore: inner}
	cfg := config.EventsConfig{Driver: "durable", MaxSubscribersPerSession: 16, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second, ReplayBufferSize: 64, LegacyWritersDrained: true}
	bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	// The session-size mix: BENCH_LARGE sessions of BENCH_LARGE_SIZE events,
	// the rest share the remainder evenly; bursts interleave round-robin.
	large := envInt("BENCH_LARGE", 100)
	largeSize := envInt("BENCH_LARGE_SIZE", 1_000)
	sizes := make([]int, sessions)
	rest := n - large*largeSize
	for i := range sizes {
		if i < large {
			sizes[i] = largeSize
		} else {
			sizes[i] = rest / (sessions - large)
		}
	}
	start := time.Now()
	published := 0
	for published < n {
		progress := false
		for s := range sizes {
			k := min(burst, sizes[s])
			if k == 0 {
				continue
			}
			progress = true
			sizes[s] -= k
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("s-%05d", s)}}
			for range k {
				if err := bus.Publish(context.Background(), events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, Payload: benchPayload{Note: "x"}}); err != nil {
					t.Fatal(err)
				}
				published++
			}
		}
		if !progress {
			break
		}
	}
	n = published
	t.Logf("seeded %d events / %d sessions in %s", n, sessions, time.Since(start))
	src, err := events.OpenProjectionSource(bus)
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string, from uint64, maxPages int) {
		store.loads.Store(0)
		store.lists.Store(0)
		store.listBytes.Store(0)
		store.loadBytes.Store(0)
		cursor, pages, evs := from, 0, 0
		begin := time.Now()
		var firstPage time.Duration
		for {
			p0 := time.Now()
			page, err := src.Page(context.Background(), cursor, 256)
			if err != nil {
				t.Fatal(err)
			}
			if pages == 0 {
				firstPage = time.Since(p0)
			}
			pages++
			evs += len(page.Events)
			cursor = page.Next
			if page.Quality == events.ProjectionCurrent || len(page.Events) == 0 || (maxPages > 0 && pages >= maxPages) {
				break
			}
		}
		el := time.Since(begin)
		t.Logf("%s: from=%d pages=%d events=%d total=%s first_page=%s per_page=%s loads=%d (%.1f/page) load_MB=%.1f head_scans=%d head_scan_MB=%.1f (%.2f/page)",
			name, from, pages, evs, el, firstPage, el/time.Duration(pages), store.loads.Load(),
			float64(store.loads.Load())/float64(pages), float64(store.loadBytes.Load())/1e6, store.lists.Load(),
			float64(store.listBytes.Load())/1e6, float64(store.listBytes.Load())/1e6/float64(pages))
	}
	wm, _ := src.Watermark(context.Background())
	run("warm", wm-uint64(warm), 0)
	run("cold", 0, envInt("BENCH_COLD_PAGES", 0))
}
