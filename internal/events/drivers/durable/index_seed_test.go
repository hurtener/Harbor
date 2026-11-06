package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// seedSpec describes one synthetic session history written directly to a
// store (bypassing publish so 100k-row histories are fast to build).
type seedSpec struct {
	id       identity.Quadruple
	startSeq uint64 // first sequence is startSeq+1
	n        int
	at       time.Time
	step     time.Duration
	typeOf   func(i int) events.EventType
	bodies   bool // write entry bodies too
}

func (s seedSpec) event(i int) events.Event {
	typ := events.EventTypeRuntimeWarning
	if s.typeOf != nil {
		typ = s.typeOf(i)
	}
	step := s.step
	if step == 0 {
		step = time.Millisecond
	}
	run := ""
	if i%3 == 0 {
		run = "run-a"
	}
	id := s.id
	id.RunID = run
	return events.Event{
		Type:       typ,
		Identity:   id,
		OccurredAt: s.at.Add(time.Duration(i) * step),
		Sequence:   s.startSeq + uint64(i) + 1,
		Payload:    events.RedactedMap{Data: map[string]any{"i": float64(i)}},
	}
}

// writeRecords persists records expected-absent in bounded batches.
func writeRecords(t testing.TB, store state.StateStore, recs []state.StateRecord) {
	t.Helper()
	const batch = 200
	for start := 0; start < len(recs); start += batch {
		end := min(start+batch, len(recs))
		exps := make([]state.SlotExpectation, 0, end-start)
		for _, r := range recs[start:end] {
			exps = append(exps, state.SlotExpectation{Identity: r.Identity, Kind: r.Kind})
		}
		if err := store.SaveBatchIf(context.Background(), exps, recs[start:end]); err != nil {
			t.Fatalf("seed batch [%d,%d): %v", start, end, err)
		}
	}
}

func seedBodies(t testing.TB, store state.StateStore, spec seedSpec) []events.Event {
	t.Helper()
	evs := make([]events.Event, spec.n)
	var recs []state.StateRecord
	for i := range spec.n {
		evs[i] = spec.event(i)
		// Boot recovery reads the oldest legacy body to seed the retention
		// horizon, so the first body is always written.
		if !spec.bodies && i != 0 {
			continue
		}
		raw, err := encodeEvent(evs[i])
		if err != nil {
			t.Fatalf("encodeEvent: %v", err)
		}
		recs = append(recs, state.StateRecord{ID: state.NewEventID(), Identity: sessionKey(spec.id), Kind: kindEntryPrefix + seqToken(evs[i].Sequence), Bytes: raw})
	}
	writeRecords(t, store, recs)
	return evs
}

// seedLegacySession writes a metadata-ready legacy (v1.36-format) head, as
// the v1 publish path would have produced it, plus optional bodies.
func seedLegacySession(t testing.TB, store state.StateStore, spec seedSpec) []events.Event {
	t.Helper()
	evs := seedBodies(t, store, spec)
	head := headRecord{}
	for _, ev := range evs {
		m, err := metadataRecordFromEvent(ev)
		if err != nil {
			t.Fatalf("metadataRecordFromEvent: %v", err)
		}
		head.Sequences = append(head.Sequences, ev.Sequence)
		head.Metadata = append(head.Metadata, m)
	}
	head.MetadataReady = len(evs) > 0
	head.MetadataValidatedCount = len(evs)
	head.MetadataIntegrityChecksum = metadataIntegrityChecksum(head.Sequences, head.Metadata)
	raw, err := encodeHead(head)
	if err != nil {
		t.Fatalf("encodeHead: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: sessionKey(spec.id), Kind: kindHead, Bytes: raw}); err != nil {
		t.Fatalf("seed legacy head: %v", err)
	}
	floorAuthority(t, store, spec.startSeq+uint64(spec.n))
	return evs
}

// seedV2Session writes a v2 header plus sealed chunks directly.
func seedV2Session(t testing.TB, store state.StateStore, spec seedSpec) []events.Event {
	t.Helper()
	evs := seedBodies(t, store, spec)
	hdr := newIndexHeader()
	rows := make([]indexRow, len(evs))
	for i, ev := range evs {
		r, err := rowFromEvent(ev)
		if err != nil {
			t.Fatalf("rowFromEvent: %v", err)
		}
		rows[i] = r
	}
	var recs []state.StateRecord
	sealed, err := hdr.appendRows(rows)
	if err != nil {
		t.Fatalf("appendRows: %v", err)
	}
	for _, c := range sealed {
		raw, err := encodeChunk(c)
		if err != nil {
			t.Fatalf("encodeChunk: %v", err)
		}
		recs = append(recs, state.StateRecord{ID: state.NewEventID(), Identity: sessionKey(spec.id), Kind: chunkKind(c.Index), Bytes: raw})
	}
	writeRecords(t, store, recs)
	raw, err := encodeHeader(hdr)
	if err != nil {
		t.Fatalf("encodeHeader: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{ID: state.NewEventID(), Identity: sessionKey(spec.id), Kind: kindHead, Bytes: raw}); err != nil {
		t.Fatalf("seed header: %v", err)
	}
	floorAuthority(t, store, spec.startSeq+uint64(spec.n))
	return evs
}

// floorAuthority raises the global sequence authority to at least seq.
func floorAuthority(t testing.TB, store state.StateStore, seq uint64) {
	t.Helper()
	b := &bus{store: store}
	for attempt := 0; attempt < 16; attempt++ {
		current, exp, err := b.loadSequenceAuthority(context.Background())
		if err != nil {
			t.Fatalf("load authority: %v", err)
		}
		if exp.ExpectedEventID != "" && current >= seq {
			return
		}
		raw, _ := json.Marshal(sequenceAuthorityRecord{Sequence: seq})
		err = store.SaveBatchIf(context.Background(), []state.SlotExpectation{exp},
			[]state.StateRecord{state.NewInternalRecord(state.NewEventID(), sequenceAuthorityIdentity, kindSequenceAuthority, raw)})
		if err == nil {
			return
		}
	}
	t.Fatalf("floor authority to %d did not converge", seq)
}

func sid(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: session}}
}

func newSQLiteStore(t testing.TB) state.StateStore {
	t.Helper()
	store, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: t.TempDir() + "/index.db"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// newPostgresStore opens a fresh-schema Postgres store (the default pool of
// 3 connections — the production default), or skips without a DSN.
func newPostgresStore(t testing.TB) state.StateStore {
	t.Helper()
	store, _ := newPostgresStoreDSN(t)
	return store
}

// newPostgresStoreDSN also returns the schema DSN so a test can open a second
// independent pool (another runtime process) on the same schema.
func newPostgresStoreDSN(t testing.TB) (state.StateStore, string) {
	t.Helper()
	base := postgresTestDSN()
	if base == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping real-Postgres index acceptance")
	}
	dsn := durablePostgresFreshSchema(t, base)
	return openPostgresDSN(t, dsn), dsn
}

func openPostgresDSN(t testing.TB, dsn string) state.StateStore {
	t.Helper()
	store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("statepostgres.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// storeMatrix runs fn against inmem, SQLite and (when configured) Postgres.
func storeMatrix(t *testing.T, fn func(t *testing.T, store state.StateStore)) {
	t.Helper()
	for _, name := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var store state.StateStore
			switch name {
			case "inmem":
				store = newInmemTestStore(t)
			case "sqlite":
				store = newSQLiteStore(t)
			default:
				store = newPostgresStore(t)
			}
			fn(t, store)
		})
	}
}

func mustf(t testing.TB, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}

func postgresTestDSN() string {
	if dsn := os.Getenv("HARBOR_PG_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("HARBOR_TEST_PG_DSN")
}

func newInmemTestStore(t testing.TB) state.StateStore {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	return store
}
