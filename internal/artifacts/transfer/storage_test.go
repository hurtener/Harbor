package transfer_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/identity"

	// The transfer triad fixture provisions isolated PostgreSQL schemas only.
	_ "github.com/jackc/pgx/v5/stdlib"
)

func transferSchema(t *testing.T, dsn string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "transfer_test_" + hex.EncodeToString(suffix[:])
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); _ = db.Close() })
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

func TestTransfer_StorageTriadDurableReplayAndErasure(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newFixtureDriver(t, driver)
			g, data := f.grant(t, 1)
			if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
				t.Fatal(err)
			}
			receipt, err := f.source.Transfer(owner(t, g.Source), g)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.State != "completed" {
				t.Fatalf("receipt=%+v", receipt)
			}
			// New independent service instances share only committed storage.
			source, err := transfer.New(f.sourceCfg)
			if err != nil {
				t.Fatal(err)
			}
			target, err := transfer.New(f.targetCfg)
			if err != nil {
				t.Fatal(err)
			}
			f.now.Add(120)
			replay, err := source.Transfer(owner(t, g.Source), g)
			if err != nil || replay != receipt {
				t.Fatalf("restart replay=%+v err=%v", replay, err)
			}
			got, err := target.Status(owner(t, g.Destination), "import", g.TransferID)
			if err != nil || got != receipt {
				t.Fatalf("recipient receipt=%+v err=%v", got, err)
			}
			scope := artifacts.ArtifactScope{TenantID: g.Destination.Tenant, UserID: g.Destination.User, SessionID: g.Destination.Session}
			content, found, err := f.targetCfg.Artifacts.Get(t.Context(), scope, receipt.DestinationArtifactID)
			if err != nil || !found || string(content) != string(data) {
				t.Fatalf("recipient bytes found=%v err=%v", found, err)
			}
			if err = target.FenceSession(t.Context(), identity.Identity{TenantID: scope.TenantID, UserID: scope.UserID, SessionID: scope.SessionID}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.targetCfg.Artifacts.PutBytes(t.Context(), scope, []byte("late"), artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrScopeFenced) {
				t.Fatalf("late bytes=%v", err)
			}
			if _, err = target.Status(owner(t, g.Destination), "import", g.TransferID); !errors.Is(err, transfer.ErrRevoked) {
				t.Fatalf("erased receipt=%v", err)
			}
		})
	}
}
