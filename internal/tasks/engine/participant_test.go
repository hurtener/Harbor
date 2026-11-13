package engine_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Work spawned from inside a participant's run (a child task with no
// participant named) keeps acting as that writer; a malformed explicit
// participant is refused.
func TestSpawn_InheritsSeatedParticipant(t *testing.T) {
	eng := newCMEngine(t)
	ctx, err := identity.WithParticipant(cmCtx(t), identity.Participant{UserID: "writer", Speaker: "W"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "child"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, err := eng.Get(cmCtx(t), h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Participant == nil || got.Participant.UserID != "writer" || got.Participant.Speaker != "W" {
		t.Fatalf("participant = %+v, want inherited writer", got.Participant)
	}
	got.Participant.UserID = "mutated"
	again, err := eng.Get(cmCtx(t), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Participant.UserID != "writer" {
		t.Fatal("Get must return a copy of the participant, not the stored pointer")
	}

	plain, err := eng.Spawn(cmCtx(t), tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := eng.Get(cmCtx(t), plain.ID); p.Participant != nil {
		t.Fatalf("ordinary task participant = %+v, want nil", p.Participant)
	}

	if _, err := eng.Spawn(cmCtx(t), tasks.SpawnRequest{Identity: cmQuad(), Kind: tasks.KindForeground, Query: "bad",
		Participant: &identity.Participant{UserID: "w", Speaker: "line\nbreak"}}); err == nil {
		t.Fatal("a malformed participant must be refused")
	}
}
