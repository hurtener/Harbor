package durable

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestDurable_Postgres_LegacyBackfillPreservesPayloadRunIDs is the hosted
// real-Postgres acceptance for the v1.29.0 -> v1.29.2 compatibility path. It
// seeds the exact old head shape (session-scoped storage key, no metadata)
// through the real StateStore driver, then proves restart adoption retains the
// event-body RunID in both metadata and full event reads.
func TestDurable_Postgres_LegacyBackfillPreservesPayloadRunIDs(t *testing.T) {
	baseDSN := os.Getenv("HARBOR_PG_DSN")
	if baseDSN == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping real-Postgres durable backfill acceptance")
	}
	dsn := durablePostgresFreshSchema(t, baseDSN)
	store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("statepostgres.New: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	id := metadataIdentity()
	runIDs := []string{"pg-run-alpha", "pg-run-beta", "pg-run-gamma"}
	seedLegacyHistoryWithRunIDs(t, store, runIDs, time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))

	first, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("first durable.New: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first durable.Close: %v", err)
	}

	// Leave a stale metadata RunID with the old checksum. Restart must reject
	// the stale projection and rebuild it from the canonical persisted bodies.
	headRec, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load adopted head: %v", err)
	}
	head, err := decodeHead(headRec.Bytes)
	if err != nil {
		t.Fatalf("decode adopted head: %v", err)
	}
	head.Metadata[1].RunID = "stale-pg-run"
	staleHead, err := encodeHead(head)
	if err != nil {
		t.Fatalf("encode stale head: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: id, Kind: kindHead, Bytes: staleHead,
	}); err != nil {
		t.Fatalf("save stale head: %v", err)
	}

	second, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("restart durable.New: %v", err)
	}
	defer func() { _ = second.Close(context.Background()) }()

	storedHead, err := store.Load(context.Background(), id, kindHead)
	if err != nil {
		t.Fatalf("load restarted head: %v", err)
	}
	adoptedHead, err := decodeHead(storedHead.Bytes)
	if err != nil {
		t.Fatalf("decode restarted head: %v", err)
	}
	if !headMetadataReady(adoptedHead) {
		t.Fatalf("restarted head is not checksum-authenticated: %+v", adoptedHead)
	}
	if got := adoptedHead.MetadataIntegrityChecksum; got != metadataIntegrityChecksum(adoptedHead.Sequences, adoptedHead.Metadata) {
		t.Fatalf("metadata checksum = %q, want canonical checksum", got)
	}
	for i, want := range runIDs {
		if got := adoptedHead.Metadata[i].RunID; got != want {
			t.Fatalf("metadata[%d].RunID = %q, want canonical payload RunID %q", i, got, want)
		}
	}

	filter := eventsWireFilter(id, nil)
	filter.RunIDs = []string{runIDs[1]}
	metadataPage, err := second.(events.EventMetadataReplayer).ListWindowMetadata(context.Background(), events.EventListQuery{
		Filter: filter,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("metadata list by RunID: %v", err)
	}
	if len(metadataPage.Events) != 1 || metadataPage.Events[0].Identity.RunID != runIDs[1] || metadataPage.Events[0].Sequence != 2 {
		t.Fatalf("metadata list by RunID = %+v, want seq=2 run=%q", metadataPage.Events, runIDs[1])
	}

	eventPage, err := second.(events.HistoryReplayer).ListWindow(context.Background(), events.EventListQuery{
		Filter: filter,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("event list by RunID: %v", err)
	}
	if len(eventPage.Events) != 1 || eventPage.Events[0].Identity.RunID != runIDs[1] || eventPage.Events[0].Sequence != 2 {
		t.Fatalf("event list by RunID = %+v, want seq=2 run=%q", eventPage.Events, runIDs[1])
	}
}

func durablePostgresFreshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	schema := "harbor_v1292_" + hex.EncodeToString(suffix[:])

	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+durablePostgresQuoteIdent(schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		dropDB, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup sql.Open: %v", err)
			return
		}
		defer func() { _ = dropDB.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		if _, err := dropDB.ExecContext(dropCtx, "DROP SCHEMA "+durablePostgresQuoteIdent(schema)+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return durablePostgresAppendSearchPath(baseDSN, schema)
}

func durablePostgresQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func durablePostgresAppendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			option := "-c search_path=" + schema
			if existing := q.Get("options"); existing != "" {
				option = existing + " " + option
			}
			q.Set("options", option)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return fmt.Sprintf("%s options='-c search_path=%s'", dsn, schema)
}
