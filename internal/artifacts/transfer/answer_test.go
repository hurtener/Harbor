package transfer_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

func answerRequest() (types.ArtifactsExportAnswerRequest, []byte) {
	b := []byte("Only the final answer.\nUnicode: café 🦆")
	sum := sha256.Sum256(b)
	return types.ArtifactsExportAnswerRequest{RequestID: "answer-export", TaskID: "task", TurnID: "turn", TurnVersion: 1, AnswerSequence: 7, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(b))}, b
}
func TestMaterializeAnswer_ExactBytesReplayAndDeletedOutput(t *testing.T) {
	f := newFixture(t)
	g, _ := f.grant(t, 70)
	ctx := owner(t, g.Source)
	r, b := answerRequest()
	out, err := f.source.MaterializeAnswer(ctx, r, b, 4)
	if err != nil {
		t.Fatal(err)
	}
	if out.IncorporatedInputRevision != 4 || out.SHA256 != r.SHA256 {
		t.Fatalf("provenance %#v", out)
	}
	again, err := f.source.MaterializeAnswer(ctx, r, b, 4)
	if err != nil || again != out {
		t.Fatalf("replay %v", err)
	}
	changed := r
	changed.TurnVersion++
	if _, err = f.source.MaterializeAnswer(ctx, changed, b, 4); !errors.Is(err, transfer.ErrConflict) {
		t.Fatalf("changed selector %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: g.Source.Tenant, UserID: g.Source.User, SessionID: g.Source.Session}
	actual, found, err := f.sourceCfg.Artifacts.Get(ctx, scope, out.ArtifactID)
	if err != nil || !found || string(actual) != string(b) {
		t.Fatalf("final answer bytes %v %v", found, err)
	}
	if _, err = f.sourceCfg.Artifacts.Delete(ctx, scope, out.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.source.MaterializeAnswer(ctx, r, b, 4); !errors.Is(err, transfer.ErrNotFound) {
		t.Fatalf("deleted output recreated %v", err)
	}
}

// Pause only receipt persistence; the driver still performs the actual atomic
// predicates. This catches metadata resurrection after a completed scope wipe.
type pauseReceiptSave struct {
	state.StateStore
	prefix         string
	once           sync.Once
	ready, release chan struct{}
}

func (p *pauseReceiptSave) SaveIf(ctx context.Context, e []state.SlotExpectation, r state.StateRecord) error {
	if strings.Contains(r.Kind, p.prefix) {
		p.once.Do(func() { close(p.ready); <-p.release })
	}
	return p.StateStore.SaveIf(ctx, e, r)
}
func TestTransfer_ErasureBlocksLateReceiptPersistence(t *testing.T) {
	for _, kind := range []string{"import", "answer"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			g, _ := f.grant(t, 71)
			cfg := f.targetCfg
			p := &pauseReceiptSave{StateStore: cfg.State, prefix: "artifact.transfer." + kind + ".", ready: make(chan struct{}), release: make(chan struct{})}
			cfg.State = p
			svc, err := transfer.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := owner(t, g.Destination)
			done := make(chan error, 1)
			go func() {
				if kind == "import" {
					_, e := svc.Prepare(ctx, g)
					done <- e
				} else {
					r, b := answerRequest()
					_, e := svc.MaterializeAnswer(ctx, r, b, 0)
					done <- e
				}
			}()
			<-p.ready
			id := identity.Identity{TenantID: g.Destination.Tenant, UserID: g.Destination.User, SessionID: g.Destination.Session}
			if err = svc.FenceSession(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if _, err = f.targetCfg.State.DeleteScope(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			close(p.release)
			if err = <-done; err == nil {
				t.Fatal("late receipt mutation succeeded")
			}
			rows, err := f.targetCfg.State.ListKindForIdentity(t.Context(), identity.Quadruple{Identity: id}, state.InternalKindPrefix+"artifact.transfer.")
			if err != nil || len(rows) != 0 {
				t.Fatalf("metadata resurrected %d %v", len(rows), err)
			}
			reopened, err := transfer.New(f.targetCfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = reopened.Prepare(ctx, g); !errors.Is(err, transfer.ErrRevoked) {
				t.Fatalf("restart released fence %v", err)
			}
		})
	}
}
