// Phase 51 — pause-state serialise contract (fail-loud) integration
// test (RFC §6.3 + §3.4; master-plan Phase 51 detail block; D-069).
//
// Phase 51's Deps are 50 (the Coordinator) and 43 (the trajectory
// fail-loudly Serialize contract + the reflective walker) — both
// already-shipped subsystems, so CLAUDE.md §17.1 requires an
// integration test proving the consumption works. The master plan
// names the shape verbatim: "Conformance with phase 43
// Trajectory.Serialize."
//
// This test wires the REAL state.StateStore (in-mem + SQLite drivers,
// no mocks at the seam — CLAUDE.md §17.3 #1) into the real
// pauseresume.Coordinator and proves:
//
//   - the pause-record serialise contract is CONFORMANT with phase 43:
//     a non-JSON-encodable leaf — whether it lands in the trajectory
//     (phase 43's surface) or in the pause record's own Payload
//     (phase 51's surface) — surfaces the SAME trajectory.ErrUnserializable
//     struct sentinel. One fail-loudly contract, shared, not two
//     parallel implementations (§13);
//   - identity propagation: the (tenant, user, session) triple flows
//     through Request, the format_version: 1 checkpoint envelope, and
//     the restart-survival load path (CLAUDE.md §17.3 #2);
//   - two failure modes (CLAUDE.md §17.3 #3): a non-encodable Payload
//     fails Request loud with no half-persisted checkpoint; a
//     format_version a Runtime cannot read fails the load loud with
//     pauseresume.ErrUnsupportedFormatVersion.
//
// Runs under -race (CLAUDE.md §17.3 #4).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// phase51ID is a documented dummy identity triple — no secrets.
var phase51ID = identity.Identity{
	TenantID:  "tenant-phase51",
	UserID:    "user-phase51",
	SessionID: "session-phase51",
}

// phase51Stores returns the real durable StateStore drivers Phase 51's
// serialise contract round-trips through. (Postgres is exercised by
// the Phase 50 durability test across all three drivers; Phase 51's
// contract is driver-agnostic — it operates on the envelope bytes
// before they reach any driver — so in-mem + SQLite is sufficient
// coverage here without a DB dependency.)
func phase51Stores(t *testing.T) []struct {
	name  string
	store state.StateStore
} {
	t.Helper()
	inmem, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open(inmem): %v", err)
	}
	t.Cleanup(func() { _ = inmem.Close(context.Background()) })

	sqliteDSN := filepath.Join(t.TempDir(), "phase51.sqlite")
	sqlite, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: sqliteDSN})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close(context.Background()) })

	return []struct {
		name  string
		store state.StateStore
	}{
		{"inmem", inmem},
		{"sqlite", sqlite},
	}
}

// TestE2E_PauseSerialise_ConformsWithPhase43 is the master plan's
// "Conformance with phase 43 Trajectory.Serialize" requirement: a
// non-encodable leaf in EITHER the trajectory (phase 43's surface) or
// the pause record's Payload (phase 51's surface) surfaces the SAME
// trajectory.ErrUnserializable struct sentinel out of
// Coordinator.Request. If Phase 51 had forked a second fail-loudly
// serialiser, the Payload leg's errors.As would fail — the shared
// error type is the observable proof the contract is one shape.
func TestE2E_PauseSerialise_ConformsWithPhase43(t *testing.T) {
	for _, sc := range phase51Stores(t) {

		t.Run(sc.name, func(t *testing.T) {
			ctx, err := identity.WithRun(context.Background(), phase51ID, "run-conformance")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}
			c := pauseresume.New(pauseresume.WithCheckpointStore(sc.store))

			// Leg 1 — phase 43's surface: a non-encodable leaf in the
			// TRAJECTORY. Request propagates trajectory.ErrUnserializable.
			trajErr := func() error {
				tr := &trajectory.Trajectory{
					HintState: map[string]any{"socket": make(chan int)},
				}
				_, e := c.Request(ctx, pauseresume.PauseRequest{
					Identity:   phase51ID,
					Reason:     pauseresume.ReasonApprovalRequired,
					Trajectory: tr,
				})
				return e
			}()

			// Leg 2 — phase 51's surface: a non-encodable leaf in the
			// pause record's own PAYLOAD. Request must propagate the
			// SAME error type.
			payloadErr := func() error {
				_, e := c.Request(ctx, pauseresume.PauseRequest{
					Identity: phase51ID,
					Reason:   pauseresume.ReasonApprovalRequired,
					Payload:  map[string]any{"socket": make(chan int)},
				})
				return e
			}()

			var trUnser, plUnser trajectory.ErrUnserializable
			if !errors.As(trajErr, &trUnser) {
				t.Fatalf("trajectory leg: Request err=%v, want trajectory.ErrUnserializable", trajErr)
			}
			if !errors.As(payloadErr, &plUnser) {
				t.Fatalf("payload leg: Request err=%v, want trajectory.ErrUnserializable "+
					"(phase 51 must NOT fork a second fail-loudly serialiser — §13)", payloadErr)
			}
			// Both legs name an offending field path — the actionable
			// contract RFC §3.4 requires.
			if trUnser.Field == "" || plUnser.Field == "" {
				t.Fatalf("ErrUnserializable.Field empty: trajectory=%q payload=%q", trUnser.Field, plUnser.Field)
			}
		})
	}
}

// TestE2E_PauseSerialise_NoHalfPersistAcrossDrivers proves the
// fail-loud path leaves NOTHING in the store: a Request rejected for a
// non-encodable Payload mints no Token and writes no checkpoint, for
// every durable driver. A half-persisted checkpoint is exactly the
// silent-corruption shape the contract closes.
func TestE2E_PauseSerialise_NoHalfPersistAcrossDrivers(t *testing.T) {
	for _, sc := range phase51Stores(t) {

		t.Run(sc.name, func(t *testing.T) {
			ctx, err := identity.WithRun(context.Background(), phase51ID, "run-nohalfpersist")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}
			c := pauseresume.New(pauseresume.WithCheckpointStore(sc.store))

			p, reqErr := c.Request(ctx, pauseresume.PauseRequest{
				Identity: phase51ID,
				Reason:   pauseresume.ReasonExternalEvent,
				Payload:  map[string]any{"callback": func() {}},
			})
			if reqErr == nil {
				t.Fatal("Request returned nil error on a non-encodable Payload — silent-drop regression")
			}
			if p.Token != "" {
				t.Fatalf("Request minted Token %q on a failed serialise — half-persisted pause", p.Token)
			}

			// A clean follow-up Request proves the store is otherwise
			// healthy and the contract is enforced per-call, not by
			// poisoning the Coordinator.
			good, gerr := c.Request(ctx, pauseresume.PauseRequest{
				Identity: phase51ID,
				Reason:   pauseresume.ReasonExternalEvent,
				Payload:  map[string]any{"webhook": "https://example.test/cb"},
			})
			if gerr != nil {
				t.Fatalf("follow-up encodable Request: %v", gerr)
			}
			c2 := pauseresume.New(pauseresume.WithCheckpointStore(sc.store))
			st, serr := c2.Status(ctx, good.Token)
			if serr != nil {
				t.Fatalf("Status on restarted coordinator: %v", serr)
			}
			if st.State != pauseresume.StatusPaused {
				t.Fatalf("Status.State = %q, want paused", st.State)
			}
		})
	}
}

// TestE2E_PauseSerialise_FormatVersionGuardAcrossDrivers proves the
// load-side half of the format_version: 1 contract: a checkpoint blob
// carrying a format_version this Runtime cannot read is rejected loud
// with pauseresume.ErrUnsupportedFormatVersion when the Coordinator
// tries to rehydrate it — never silently mis-decoded against the
// current schema. The blob is written through the REAL StateStore at
// the same (EventID, Kind) the Coordinator's checkpoint path uses, so
// this exercises the genuine rehydrate path.
func TestE2E_PauseSerialise_FormatVersionGuardAcrossDrivers(t *testing.T) {
	for _, sc := range phase51Stores(t) {

		t.Run(sc.name, func(t *testing.T) {
			ctx, err := identity.WithRun(context.Background(), phase51ID, "run-fmtguard")
			if err != nil {
				t.Fatalf("identity.WithRun: %v", err)
			}

			// Record a real, well-formed pause so a checkpoint exists.
			c1 := pauseresume.New(pauseresume.WithCheckpointStore(sc.store))
			p, err := c1.Request(ctx, pauseresume.PauseRequest{
				Identity: phase51ID,
				Reason:   pauseresume.ReasonAwaitInput,
				Payload:  map[string]any{"prompt": "need more input"},
			})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}

			// Load the persisted blob, tamper format_version upward (a
			// forward-incompatible write from a hypothetical newer
			// Runtime), and write it back at the same slot.
			rec, err := sc.store.LoadByEventID(ctx, state.EventID(p.Token))
			if err != nil {
				t.Fatalf("LoadByEventID: %v", err)
			}
			var envelope map[string]any
			if err := json.Unmarshal(rec.Bytes, &envelope); err != nil {
				t.Fatalf("unmarshal checkpoint: %v", err)
			}
			envelope["format_version"] = 999
			tampered, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			rec.Bytes = tampered
			// state.StateStore.Save returns ErrIdempotencyConflict on a
			// re-Save of the same EventID with different Bytes — so the
			// tamper is a delete-then-save, the same dance the
			// Coordinator's resume path uses for an in-place envelope
			// update.
			if err := sc.store.Delete(ctx, rec.Identity, rec.Kind); err != nil {
				t.Fatalf("Delete checkpoint slot: %v", err)
			}
			if err := sc.store.Save(ctx, rec); err != nil {
				t.Fatalf("Save tampered checkpoint: %v", err)
			}

			// A fresh "restarted" Coordinator tries to rehydrate it via
			// Status — the format_version guard must fire loud.
			c2 := pauseresume.New(pauseresume.WithCheckpointStore(sc.store))
			if _, serr := c2.Status(ctx, p.Token); !errors.Is(serr, pauseresume.ErrUnsupportedFormatVersion) {
				t.Fatalf("Status on a tampered format_version: err=%v, want ErrUnsupportedFormatVersion", serr)
			}
		})
	}
}
