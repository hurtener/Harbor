package transfer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	artpg "github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	artsqlite "github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	statemem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepg "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type fixture struct {
	source, target       *transfer.Service
	sourceCfg, targetCfg transfer.Config
	key                  ed25519.PrivateKey
	now                  atomic.Int64
	server               *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	return newFixtureDriver(t, "inmem")
}
func newFixtureDriver(t *testing.T, driver string) *fixture {
	t.Helper()
	f := &fixture{}
	f.now.Store(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix())
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	newCfg := func(audience string) transfer.Config {
		var arts artifacts.ArtifactStore
		var st state.StateStore
		var e error
		switch driver {
		case "inmem":
			arts, e = artmem.New(config.ArtifactsConfig{})
			if e == nil {
				st, e = statemem.New(config.StateConfig{})
			}
		case "sqlite":
			dir := t.TempDir()
			arts, e = artsqlite.New(config.ArtifactsConfig{DSN: filepath.Join(dir, "artifacts.db")})
			if e == nil {
				st, e = statesqlite.New(config.StateConfig{DSN: filepath.Join(dir, "state.db")})
			}
		case "postgres":
			dsn := os.Getenv("HARBOR_PG_DSN")
			if dsn == "" {
				t.Skip("HARBOR_PG_DSN not set; transfer restart requires a test database")
			}
			dsn = transferSchema(t, dsn)
			arts, e = artpg.New(config.ArtifactsConfig{DSN: dsn})
			if e == nil {
				st, e = statepg.New(config.StateConfig{DSN: dsn})
			}
		default:
			t.Fatalf("unknown fixture driver %q", driver)
		}
		if e != nil {
			t.Fatal(e)
		}

		bus, e := eventmem.New(config.EventsConfig{MaxSubscribersPerSession: 8, SubscriberBufferSize: 64, IdleTimeout: 30 * time.Second, DropWindow: time.Second, ReplayBufferSize: 128}, patterns.New())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			_ = bus.Close(context.Background())
			_ = st.Close(context.Background())
			_ = arts.Close(context.Background())
		})
		return transfer.Config{LegacyWritersDrained: true, Audience: audience, Epoch: 1, MaxBytes: 1 << 20, Keys: map[string]ed25519.PublicKey{"key": pub}, Artifacts: arts, State: st, Bus: bus, Clock: func() time.Time { return time.Unix(f.now.Load(), 0).UTC() }}
	}
	f.targetCfg = newCfg("target")
	f.target, err = transfer.New(f.targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(f.target.Handler())
	t.Cleanup(f.server.Close)
	sender, err := transfer.NewHTTPSender(transfer.HTTPConfig{Peers: map[string]string{"target": f.server.URL}, Timeout: 10 * time.Second, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sender.Close)
	f.sourceCfg = newCfg("source")
	f.sourceCfg.Sender = sender
	f.source, err = transfer.New(f.sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func owner(t *testing.T, e types.ArtifactTransferEndpoint) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(t.Context(), identity.Identity{TenantID: e.Tenant, UserID: e.User, SessionID: e.Session})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func (f *fixture) grant(t *testing.T, n int) (types.ArtifactTransferGrant, []byte) {
	t.Helper()
	data := []byte(fmt.Sprintf("binary\x00\xff evidence %d", n))
	src := types.ArtifactTransferEndpoint{Audience: "source", Tenant: fmt.Sprint("tenant-", n), User: "owner", Session: "source", Epoch: 1}
	dst := types.ArtifactTransferEndpoint{Audience: "target", Tenant: src.Tenant, User: "recipient", Session: "destination", Epoch: 1}
	ref, err := f.sourceCfg.Artifacts.PutBytes(t.Context(), artifacts.ArtifactScope{TenantID: src.Tenant, UserID: src.User, SessionID: src.Session}, data, artifacts.PutOpts{Namespace: "fixture", MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := transfer.Sign(types.ArtifactTransferGrant{Version: 1, KeyID: "key", TransferID: fmt.Sprint("copy-", n), Purpose: "bounded-assignment", Source: src, Destination: dst, ArtifactID: ref.ID, SHA256: ref.SHA256, MimeType: ref.MimeType, SizeBytes: ref.SizeBytes, IssuedAt: f.targetCfg.Clock(), ExpiresAt: f.targetCfg.Clock().Add(time.Minute)}, f.key)
	if err != nil {
		t.Fatal(err)
	}
	return g, data
}
func TestTransfer_DirectHTTP_ByteExactReplayAndIsolation(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 1)
	if _, err := f.source.Transfer(owner(t, g.Source), g); err == nil {
		t.Fatal("unadmitted recipient accepted")
	}
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	r, err := f.source.Transfer(owner(t, g.Source), g)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "completed" || r.DestinationArtifactID == "" {
		t.Fatalf("receipt %#v", r)
	}
	got, found, err := f.targetCfg.Artifacts.Get(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant, UserID: g.Destination.User, SessionID: g.Destination.Session}, r.DestinationArtifactID)
	if err != nil || !found || string(got) != string(data) {
		t.Fatalf("binary mismatch %v %v", found, err)
	}
	foreign := g.Destination
	foreign.User = "unrelated"
	if _, err = f.target.Status(owner(t, foreign), "import", g.TransferID); !errors.Is(err, transfer.ErrNotFound) {
		t.Fatalf("foreign status %v", err)
	}
	f.now.Add(120)
	again, err := f.source.Transfer(owner(t, g.Source), g)
	if err != nil || again != r {
		t.Fatalf("completed replay changed %v", err)
	}
	changed := g
	changed.Purpose = "different"
	changed, err = transfer.Sign(changed, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.source.Transfer(owner(t, g.Source), changed); !errors.Is(err, transfer.ErrConflict) {
		t.Fatalf("conflicting grant %v", err)
	}
}
func TestTransfer_RefusesWrongAuthorityExpiryAndCorruption(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 2)
	if _, err := f.target.Prepare(t.Context(), g); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("unverified owner %v", err)
	}
	wrong := g.Destination
	wrong.Session = "other"
	if _, err := f.target.Prepare(owner(t, wrong), g); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("wrong owner %v", err)
	}
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.Import(t.Context(), g, append(data, 'x')); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatalf("corrupt bytes %v", err)
	}
	wrongSource := g.Source
	wrongSource.User = "other"
	if _, err := f.source.Transfer(owner(t, wrongSource), g); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("wrong source %v", err)
	}
	f.now.Add(120)
	if _, err := f.source.Transfer(owner(t, g.Source), g); !errors.Is(err, transfer.ErrExpired) {
		t.Fatalf("expired export %v", err)
	}
	if _, err := f.target.Import(t.Context(), g, data); !errors.Is(err, transfer.ErrExpired) {
		t.Fatalf("expired import %v", err)
	}
}
func TestTransfer_RevocationPreventsByteMovement(t *testing.T) {
	f := newFixture(t)
	g, _ := f.grant(t, 3)
	ctx := owner(t, g.Destination)
	if _, err := f.target.Prepare(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.Revoke(ctx, "import", g.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.Transfer(owner(t, g.Source), g); err == nil {
		t.Fatal("revoked transfer succeeded")
	}
	refs, err := f.targetCfg.Artifacts.List(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant})
	if err != nil || len(refs) != 0 {
		t.Fatalf("bytes moved %d %v", len(refs), err)
	}
}
func TestTransfer_ConcurrentOwners(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newFixtureDriver(t, driver)

			var wg sync.WaitGroup
			for i := range 100 {
				g, data := f.grant(t, i+100)
				src, dst := owner(t, g.Source), owner(t, g.Destination)
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := f.target.Prepare(dst, g); err != nil {
						t.Error(err)
						return
					}
					r, err := f.source.Transfer(src, g)
					if err != nil {
						t.Error(err)
						return
					}
					got, found, err := f.targetCfg.Artifacts.Get(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant, UserID: g.Destination.User, SessionID: g.Destination.Session}, r.DestinationArtifactID)
					if err != nil || !found || string(got) != string(data) {
						t.Errorf("cross-talk %v", err)
					}
				}()
			}
			wg.Wait()
		})
	}
}

// Fault injection wraps the real StateStore only at the commit boundary; all
// byte storage, conditional admission and peer HTTP remain production code.
type failCompletion struct {
	state.StateStore
	failed atomic.Bool
}

func (s *failCompletion) SaveIf(ctx context.Context, e []state.SlotExpectation, r state.StateRecord) error {
	if bytes.Contains(r.Bytes, []byte(`"state":"completed"`)) && !s.failed.Swap(true) {
		return errors.New("injected completion persistence failure")
	}
	return s.StateStore.SaveIf(ctx, e, r)
}

func TestTransfer_CrashAfterBlob_ReceiptRecoveryWithoutReimport(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 4)
	cfg := f.targetCfg
	cfg.State = &failCompletion{StateStore: cfg.State}
	target, err := transfer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(t, g.Destination)
	if _, err = target.Prepare(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Import(t.Context(), g, data); err == nil {
		t.Fatal("fault not reached")
	}
	f.now.Add(120)
	recovered, err := transfer.New(f.targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := recovered.Status(ctx, "import", g.TransferID)
	if err != nil || r.State != "completed" {
		t.Fatalf("recovery %s %v", r.State, err)
	}
	refs, err := f.targetCfg.Artifacts.List(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant})
	if err != nil || len(refs) != 1 {
		t.Fatalf("copies %d %v", len(refs), err)
	}
}
func TestTransfer_PeerOriginAndRedirectRefusal(t *testing.T) {
	for _, u := range []string{"http://example.com", "https://example.com/path", "https://user:password@example.com", "https://example.com?next=evil", "file:///etc/passwd"} {
		if _, err := transfer.NewHTTPSender(transfer.HTTPConfig{Peers: map[string]string{"target": u}, Timeout: time.Second, AllowLoopbackHTTP: true}); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	var reached atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer evil.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	s, err := transfer.NewHTTPSender(transfer.HTTPConfig{Peers: map[string]string{"target": redirect.URL}, Timeout: time.Second, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Probe(t.Context(), types.ArtifactTransferGrant{Destination: types.ArtifactTransferEndpoint{Audience: "target"}}); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached.Load() {
		t.Fatal("redirect target received request")
	}
}

func TestTransfer_ConcurrentExactReplayAndCancellationIsolation(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 900)
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(owner(t, g.Source))
			defer cancel()
			if i%2 == 0 {
				cancel()
			}
			receipt, err := f.source.Transfer(ctx, g)
			if i%2 == 0 {
				if err == nil {
					t.Error("cancelled source returned success")
				}
				return
			}
			if err != nil || receipt.State != "completed" {
				t.Errorf("sibling replay=%+v err=%v", receipt, err)
			}
		}(i)
	}
	wg.Wait()
	refs, err := f.targetCfg.Artifacts.List(t.Context(), artifacts.ArtifactScope{TenantID: g.Destination.Tenant, UserID: g.Destination.User, SessionID: g.Destination.Session})
	if err != nil || len(refs) != 1 {
		t.Fatalf("exact retries created %d artifacts: %v", len(refs), err)
	}
	got, found, err := f.targetCfg.Artifacts.Get(t.Context(), refs[0].Scope, refs[0].ID)
	if err != nil || !found || string(got) != string(data) {
		t.Fatalf("exact retry changed bytes: %v", err)
	}
}
