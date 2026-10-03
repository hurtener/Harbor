package serve

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	turnmem "github.com/hurtener/Harbor/internal/sessions/turns/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Exercise the real BuildMux join, exact task result, and retained turn read.
// A selector that exists but is omitted from assembly must fail this gate.
func TestFinalAnswer_ProductionWiringExactSealedProvenance(t *testing.T) {
	deps := buildProjWiringMux(t)
	arts, e := artmem.New(config.ArtifactsConfig{})
	if e != nil {
		t.Fatal(e)
	}
	deps.in.Artifacts = arts
	t.Cleanup(func() { _ = arts.Close(context.Background()) })
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "sealed-export"}
	ctx, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := deps.tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = deps.tasks.MarkRunning(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = deps.tasks.AcceptInput(ctx, task.ID, "correction", "corrected text"); err != nil {
		t.Fatal(err)
	}
	if _, err = deps.tasks.MarkInputApplied(ctx, task.ID, "correction", 1); err != nil {
		t.Fatal(err)
	}
	answer := "The exact sealed answer."
	raw, err := json.Marshal(planner.AnswerEnvelope{Answer: answer, FinishReason: string(planner.FinishGoal), IncorporatedInputRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = deps.tasks.MarkComplete(ctx, task.ID, tasks.TaskResult{Value: raw, IncorporatedInputRevision: 1}); err != nil {
		t.Fatal(err)
	}
	store, err := turnmem.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	projector, err := turns.New(store)
	if err != nil {
		t.Fatal(err)
	}
	row, err := projector.Append(ctx, id, turns.Append{TurnID: turns.TurnID(task.ID), TaskID: string(task.ID), Query: "original"})
	if err != nil {
		t.Fatal(err)
	}
	row, err = projector.Update(ctx, id, row.TurnID, row.Version, turns.Update{Answer: &turns.Answer{State: turns.AnswerStateInline, Inline: answer, Seq: 1, Complete: turns.CompletenessComplete}})
	if err != nil {
		t.Fatal(err)
	}
	row, err = projector.Seal(ctx, id, row.TurnID, row.Version, turns.Seal{Status: turns.StatusComplete, FinishReason: turns.FinishGoal})
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deps.in.Cfg.Artifacts.Transfer = &config.ArtifactTransferConfig{LegacyWritersDrained: true, Audience: "source", Epoch: 1, MaxBytes: 4096, Timeout: time.Second, PublicKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)}}
	deps.in.TurnsProjector = projector
	built, err := BuildMux(deps.in)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(answer))
	request := types.ArtifactsExportAnswerRequest{Scope: types.ArtifactScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, RequestID: "exact-answer", TaskID: string(task.ID), TurnID: string(row.TurnID), TurnVersion: int64(row.Version), AnswerSequence: int64(row.Answer.Seq), SHA256: hex.EncodeToString(hash[:]), SizeBytes: int64(len(answer))}
	call := func(r types.ArtifactsExportAnswerRequest, who identity.Identity) (int, []byte) {
		t.Helper()
		b, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		return postMux(t, built.Mux, "/v1/control/artifacts.export_answer", who, string(b))
	}
	status, body := call(request, id)
	if status != http.StatusOK {
		t.Fatalf("export status=%d body=%s", status, body)
	}
	var exported types.ArtifactsExportAnswerResponse
	if err = json.Unmarshal(body, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.IncorporatedInputRevision != 1 || exported.ArtifactID == "" {
		t.Fatalf("export provenance=%+v", exported)
	}
	bytes, found, err := deps.in.Artifacts.Get(ctx, artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, exported.ArtifactID)
	if err != nil || !found || string(bytes) != answer {
		t.Fatalf("stored answer found=%v err=%v", found, err)
	}
	status, body = call(request, id)
	if status != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", status, body)
	}
	var replay types.ArtifactsExportAnswerResponse
	if err = json.Unmarshal(body, &replay); err != nil || replay != exported {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	wrong := request
	wrong.TurnVersion++
	if status, _ = call(wrong, id); status != http.StatusConflict {
		t.Fatalf("changed selector status=%d", status)
	}
	foreign := id
	foreign.UserID = "other"
	if status, _ = call(request, foreign); status == http.StatusOK {
		t.Fatal("foreign owner exported answer")
	}
	if _, err = projector.Erase(ctx, id); err != nil {
		t.Fatal(err)
	}
	if status, _ = call(request, id); status != http.StatusNotFound {
		t.Fatalf("erased turn status=%d", status)
	}
}
