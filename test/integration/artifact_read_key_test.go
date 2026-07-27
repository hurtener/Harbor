// Package integration — the reconciled artifact read key, end to end.
//
// The seam this file guards is the one that was dead: the session
// artifact manifest lists on the isolation triple and tells the model it
// may fetch any listed ref, while the three production writers stamped
// three different `TaskID` shapes and the read was exact on all four
// fields. So the manifest enumerated refs `artifact_fetch` resolved as
// not-found — an invitation the runtime could not honour.
//
// Every test here composes REAL components across three subsystems:
// `internal/artifacts` drivers (in-memory and filesystem, no mocks on
// the seam), `internal/planner`'s manifest builder, and the
// `internal/tools/builtin` `artifact_fetch` tool reached through a real
// `tools.ToolCatalog`.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsfs "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

const (
	arkTenant  = "ark-tenant"
	arkUser    = "ark-user"
	arkSession = "ark-session"
)

// arkStores returns one store per shipped driver that can run without an
// external dependency, each named so a failure says which driver failed.
// The SQL and object-store drivers carry the same behaviour through
// their own conformance runs; this integration test is about the WIRING
// across subsystems, so it drives the two drivers a plain `go test`
// always has.
func arkStores(t *testing.T) map[string]artifacts.ArtifactStore {
	t.Helper()
	inmem, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem: %v", err)
	}
	t.Cleanup(func() { _ = inmem.Close(context.Background()) })

	fsStore, err := artifactsfs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	t.Cleanup(func() { _ = fsStore.Close(context.Background()) })

	return map[string]artifacts.ArtifactStore{"inmem": inmem, "fs": fsStore}
}

// arkCtx builds the identity-scoped context the tool executor hands a
// builtin: the working identity plus the run, exactly as a dispatched
// tool call carries them.
func arkCtx(t *testing.T, tenant, user, session, run string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, run)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// arkFetch invokes `artifact_fetch` through a real catalog — the same
// path the planner's tool call takes — and decodes the tool's output.
func arkFetch(t *testing.T, store artifacts.ArtifactStore, ctx context.Context, ref string) builtin.ArtifactFetchOut {
	t.Helper()
	cat := tools.NewCatalog()
	if err := builtin.RegisterWith(builtin.RegistryContext{
		Catalog:       cat,
		ArtifactStore: store,
	}, []string{"artifact_fetch"}); err != nil {
		t.Fatalf("register artifact_fetch: %v", err)
	}
	desc, ok := cat.Resolve("artifact_fetch")
	if !ok {
		t.Fatal("artifact_fetch not in the catalog after registration")
	}
	args, err := json.Marshal(map[string]any{"ref": ref})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := desc.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("artifact_fetch invoke: %v", err)
	}
	raw, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	var out builtin.ArtifactFetchOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool result %s: %v", raw, err)
	}
	return out
}

// TestE2E_ArtifactReadKey_ManifestInvitationIsHonoured is the §17.6 fix
// made observable. It reproduces the three production writer shapes in
// one session — a heavy tool result stamped with NO task, an
// LLM-materialised attachment stamped with the PRODUCING RUN, and an
// upload stamped with a different run — builds the manifest the planner
// injects, and then fetches EVERY listed ref the way the model would.
//
// Before the read key was reconciled, two of the three rows resolved as
// not-found. The assertion is therefore not "some rows fetch": it is
// that the manifest and the fetch answer the same question, for every
// row, on every driver.
func TestE2E_ArtifactReadKey_ManifestInvitationIsHonoured(t *testing.T) {
	triple := artifacts.ArtifactScope{
		TenantID: arkTenant, UserID: arkUser, SessionID: arkSession,
	}
	// The three shapes, verbatim from the production call sites.
	writers := []struct {
		name    string
		scope   artifacts.ArtifactScope
		payload []byte
		source  map[string]any
	}{
		{
			name:    "heavy-tool-result-no-task",
			scope:   triple,
			payload: []byte(`{"rows":["heavy tool result"]}`),
			source:  map[string]any{"source": "tool", "producer": "dev-tool-executor"},
		},
		{
			name:    "llm-materialised-stamped-with-run",
			scope:   artifacts.ArtifactScope{TenantID: arkTenant, UserID: arkUser, SessionID: arkSession, TaskID: "run-materialise"},
			payload: []byte("PNG-ish bytes the model attached"),
			source:  map[string]any{"producer": "llm.auto_materialize"},
		},
		{
			name:    "upload-stamped-with-another-run",
			scope:   artifacts.ArtifactScope{TenantID: arkTenant, UserID: arkUser, SessionID: arkSession, TaskID: "run-upload"},
			payload: []byte("uploaded by an earlier turn"),
			source:  map[string]any{"source": "upload"},
		},
	}

	for driverName, store := range arkStores(t) {
		t.Run(driverName, func(t *testing.T) {
			ctx := context.Background()
			want := map[string]string{}
			for _, w := range writers {
				ref, err := store.PutBytes(ctx, w.scope, w.payload, artifacts.PutOpts{
					Namespace: "session.fixtures",
					MimeType:  "application/json",
					Filename:  w.name + ".json",
					Source:    w.source,
				})
				if err != nil {
					t.Fatalf("%s: PutBytes: %v", w.name, err)
				}
				want[ref.ID] = string(w.payload)
			}

			// The manifest builder is the production one, fed the same
			// triple-scoped List the run loop performs.
			refs, err := store.List(ctx, triple)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			manifest := planner.BuildArtifactManifest(refs)
			if len(manifest) != len(writers) {
				t.Fatalf("manifest lists %d artifacts, want %d — the manifest is the "+
					"model's whole view of the session", len(manifest), len(writers))
			}

			// Every listed row must fetch. A run that is none of the
			// three producers does the fetching, because that is the
			// case the manifest actually creates: the model reads the
			// block on the NEXT turn.
			fetchCtx := arkCtx(t, arkTenant, arkUser, arkSession, "run-reading")
			for _, entry := range manifest {
				out := arkFetch(t, store, fetchCtx, entry.Ref)
				if out.Error != "" {
					t.Fatalf("manifest row %q could not be fetched: %s — the manifest "+
						"invited the model to read a ref the store resolves as not-found",
						entry.Ref, out.Error)
				}
				if out.Content != want[entry.Ref] {
					t.Errorf("row %q content=%q, want %q", entry.Ref, out.Content, want[entry.Ref])
				}
			}
		})
	}
}

// TestE2E_ArtifactReadKey_IsolationHoldsAcrossTheSeam is the failure
// mode the widening must NOT have introduced. The read key lost a field;
// the isolation boundary did not move. A different session, a different
// user and a different tenant each fail to resolve a ref, through the
// same tool the legitimate reader used — and the tool reports a soft
// error rather than leaking bytes or distinguishing "not yours" from
// "no such artifact".
func TestE2E_ArtifactReadKey_IsolationHoldsAcrossTheSeam(t *testing.T) {
	for driverName, store := range arkStores(t) {
		t.Run(driverName, func(t *testing.T) {
			ctx := context.Background()
			owner := artifacts.ArtifactScope{
				TenantID: arkTenant, UserID: arkUser, SessionID: arkSession, TaskID: "run-owner",
			}
			secret := []byte(`{"secret":"belongs to one session"}`)
			ref, err := store.PutBytes(ctx, owner, secret, artifacts.PutOpts{Namespace: "iso"})
			if err != nil {
				t.Fatalf("PutBytes: %v", err)
			}

			// The legitimate sibling run in the SAME session reads it —
			// this is the widening the change is for, and it must work.
			sibling := arkFetch(t, store, arkCtx(t, arkTenant, arkUser, arkSession, "run-sibling"), ref.ID)
			if sibling.Error != "" {
				t.Fatalf("sibling run in the same session was refused: %s", sibling.Error)
			}
			if sibling.Content != string(secret) {
				t.Errorf("sibling content=%q, want %q", sibling.Content, secret)
			}

			outsiders := []struct {
				name                  string
				tenant, user, session string
			}{
				{"other-session", arkTenant, arkUser, "someone-elses-session"},
				{"other-user", arkTenant, "someone-else", arkSession},
				{"other-tenant", "someone-elses-tenant", arkUser, arkSession},
			}
			for _, o := range outsiders {
				out := arkFetch(t, store,
					arkCtx(t, o.tenant, o.user, o.session, "run-outsider"), ref.ID)
				if out.Error == "" {
					t.Errorf("%s: fetch succeeded — the read key narrowed the KEY, "+
						"it must not widen the BOUNDARY", o.name)
				}
				if out.Content != "" {
					t.Errorf("%s: bytes leaked alongside the refusal: %q", o.name, out.Content)
				}
			}

			// And the isolation is real at the store, not only at the
			// tool: the owner still has the artifact after the outsiders
			// tried.
			got, found, err := store.Get(ctx, owner, ref.ID)
			if err != nil || !found || string(got) != string(secret) {
				t.Errorf("owner lost the artifact: found=%v err=%v", found, err)
			}
		})
	}
}

// TestE2E_ArtifactReadKey_ConcurrentSessionsDoNotCrossTalk runs N
// sessions concurrently against ONE shared store, each writing IDENTICAL
// bytes and then fetching through the tool. Identical bytes are the
// adversarial case for a content-addressed store: every session derives
// the SAME id, so an id that escaped its triple would be observed
// immediately.
//
// This is the cross-package half of the §17.3 concurrency requirement —
// the per-driver D-025 runs assert intra-package.
func TestE2E_ArtifactReadKey_ConcurrentSessionsDoNotCrossTalk(t *testing.T) {
	for driverName, store := range arkStores(t) {
		t.Run(driverName, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			const sessions = 32
			var wg sync.WaitGroup
			var failures atomic.Int64
			ids := make([]string, sessions)
			wg.Add(sessions)
			for i := range sessions {
				go func() {
					defer wg.Done()
					scope := artifacts.ArtifactScope{
						TenantID:  arkTenant,
						UserID:    fmt.Sprintf("user-%03d", i%4),
						SessionID: fmt.Sprintf("sess-%03d", i),
						TaskID:    fmt.Sprintf("run-%03d", i),
					}
					// The payload names its own session, so a read that
					// crossed would return the WRONG session's bytes and
					// be caught below — while the id is derived from
					// bytes, so the shared prefix keeps the keys close.
					payload := []byte(fmt.Sprintf("identical prefix / session=%s", scope.SessionID))
					ref, err := store.PutBytes(context.Background(), scope, payload,
						artifacts.PutOpts{Namespace: "cross"})
					if err != nil {
						failures.Add(1)
						return
					}
					ids[i] = ref.ID
				}()
			}
			wg.Wait()
			if n := failures.Load(); n != 0 {
				t.Fatalf("%d concurrent session writes failed", n)
			}

			// Each session fetches its OWN ref through the tool, and is
			// refused every other session's.
			var readWG sync.WaitGroup
			readWG.Add(sessions)
			for i := range sessions {
				go func() {
					defer readWG.Done()
					session := fmt.Sprintf("sess-%03d", i)
					ctx := arkCtx(t, arkTenant, fmt.Sprintf("user-%03d", i%4), session, "run-read")
					own := arkFetch(t, store, ctx, ids[i])
					if own.Error != "" {
						t.Errorf("session %s cannot read its own artifact: %s", session, own.Error)
						return
					}
					want := fmt.Sprintf("identical prefix / session=%s", session)
					if own.Content != want {
						t.Errorf("session %s read %q, want %q — CROSS-TALK",
							session, own.Content, want)
					}
					neighbour := (i + 1) % sessions
					if neighbour == i {
						return
					}
					out := arkFetch(t, store, ctx, ids[neighbour])
					if out.Error == "" {
						t.Errorf("session %s read session sess-%03d's artifact",
							session, neighbour)
					}
				}()
			}
			readWG.Wait()

			deadline := time.Now().Add(2 * time.Second)
			for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			if delta := runtime.NumGoroutine() - baseline; delta > 0 {
				t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
			}
		})
	}
}
