package turns

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOutputManifest_UnknownEmptyAndImmutable(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	ctx := context.Background()
	row, err := appendTurn(p, tripleA(), "output-turn")
	if err != nil {
		t.Fatal(err)
	}
	if row.OutputManifest.Version != 0 {
		t.Fatal("fabricated legacy provenance")
	}
	seal := OutputManifestSeal{Version: 1, SHA256: strings.Repeat("a", 64), InputRevision: 7}
	row, err = p.Update(ctx, tripleA(), row.TurnID, row.Version, Update{Outputs: []Attachment{}, OutputManifest: &seal, Answer: &Answer{State: AnswerStateEmpty}})
	if err != nil {
		t.Fatal(err)
	}
	if row.OutputManifest != seal || len(row.Outputs) != 0 {
		t.Fatal("known empty missing")
	}
	if _, err = p.Update(ctx, tripleA(), row.TurnID, row.Version, Update{Outputs: []Attachment{{ID: "late"}}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("late output accepted: %v", err)
	}
	changed := seal
	changed.SHA256 = strings.Repeat("b", 64)
	if _, err = p.Update(ctx, tripleA(), row.TurnID, row.Version, Update{Outputs: []Attachment{}, OutputManifest: &changed}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("late seal change: %v", err)
	}
	row, err = p.Seal(ctx, tripleA(), row.TurnID, row.Version, Seal{Status: StatusComplete})
	if err != nil {
		t.Fatal(err)
	}
	// A delayed cost/usage observation cannot change the completion/export
	// selector. Exact turn version and the output seal stay stable for replay.
	if _, err = p.Update(ctx, tripleA(), row.TurnID, row.Version, Update{Usage: &Usage{Model: "late-model"}, EventSeq: 99}); !errors.Is(err, ErrTurnSealed) {
		t.Fatalf("late usage mutated completion: %v", err)
	}
	fresh, err := p.Get(ctx, tripleA(), row.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version != row.Version || fresh.Answer.Seq != row.Answer.Seq || fresh.OutputManifest != row.OutputManifest {
		t.Fatal("late usage changed immutable result selector")
	}
	if _, err = p.Update(ctx, tripleA(), row.TurnID, row.Version, Update{Outputs: []Attachment{}, OutputManifest: &seal}); !errors.Is(err, ErrTurnSealed) {
		t.Fatalf("sealed turn changed: %v", err)
	}
}
