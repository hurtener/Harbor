package protocol

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	harborprotocol "github.com/hurtener/Harbor/internal/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
)

func TestAgentPackWireAdapter_InspectPreservesDistinctLayers(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identityForWireTest(t)
	ctx := effectivePackContext(t, id)
	sharedBoot := effectivePackItem("shared", "boot-body", "boot-origin")
	sharedRevision := effectivePackItem("shared", "boot-body", "revision-origin")
	revisionOnly := effectivePackItem("revision-only", "revision-body", "revision-only-origin")
	seedEffectivePack(t, registry, id, "agent-a", []skills.AgentPackItem{sharedRevision, revisionOnly})
	service := newEffectivePackService(t, registry, store, &effectivePackBootReader{entries: map[string][]bootpacks.Entry{
		"tenant-a\x00agent-a": {effectiveBootEntry(t, sharedBoot)},
	}})

	resp, err := service.Inspect(ctx, prototypes.AgentConfigAgentPacksInspectRequest{AgentID: "agent-a"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if resp.AgentID != "agent-a" || len(resp.BootPacks) != 1 || len(resp.RevisionPacks) != 2 || len(resp.EffectivePacks) != 2 {
		t.Fatalf("inspect projection = %+v", resp)
	}
	if resp.BootPacks[0].Source != "boot" || resp.BootPacks[0].Editable {
		t.Fatalf("boot projection = %+v", resp.BootPacks[0])
	}
	if resp.RevisionPacks[0].Source != "revision" || !resp.RevisionPacks[0].Editable {
		t.Fatalf("revision projection = %+v", resp.RevisionPacks[0])
	}
	var shared prototypes.AgentConfigAgentPackInspection
	for _, item := range resp.EffectivePacks {
		if item.PackID == "shared" {
			shared = item
		}
	}
	if shared.Source != "both" || shared.Editable || shared.Pack.OriginRef != "boot-origin" {
		t.Fatalf("effective shared projection = %+v", shared)
	}
	if len(resp.CompositionHash) != 64 || len(resp.BootPackSetHash) != 64 || resp.ProtocolVersion != prototypes.ProtocolVersion {
		t.Fatalf("inspect hashes/version = (%q, %q, %q)", resp.CompositionHash, resp.BootPackSetHash, resp.ProtocolVersion)
	}
}

func TestAgentPackWireAdapter_CopyReturnsPluralOutcomes(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identityForWireTest(t)
	ctx := effectivePackContext(t, id)
	alpha := effectivePackItem("alpha", "same", "")
	beta := effectivePackItem("beta", "new", "")
	seedEffectivePack(t, registry, id, "source", []skills.AgentPackItem{alpha, beta})
	seedEffectivePack(t, registry, id, "target", []skills.AgentPackItem{alpha})
	service := newEffectivePackService(t, registry, store, nil)
	source, err := service.Inspect(ctx, prototypes.AgentConfigAgentPacksInspectRequest{AgentID: "source"})
	if err != nil {
		t.Fatalf("inspect source: %v", err)
	}
	target, err := service.Inspect(ctx, prototypes.AgentConfigAgentPacksInspectRequest{AgentID: "target"})
	if err != nil {
		t.Fatalf("inspect target: %v", err)
	}
	copyResp, err := service.Copy(ctx, prototypes.AgentConfigAgentPacksCopyRequest{
		SourceAgentID:                 "source",
		TargetAgentID:                 "target",
		PackIDs:                       []string{"beta", "alpha"},
		ExpectedSourceCompositionHash: source.CompositionHash,
		ExpectedTargetCompositionHash: target.CompositionHash,
		IdempotencyKey:                "wire-copy-1",
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if len(copyResp.Outcomes) != 2 || copyResp.Outcomes[0] != (prototypes.AgentConfigAgentPackCopyOutcome{PackID: "alpha", Outcome: "noop"}) || copyResp.Outcomes[1] != (prototypes.AgentConfigAgentPackCopyOutcome{PackID: "beta", Outcome: "copied"}) {
		t.Fatalf("copy outcomes = %+v", copyResp.Outcomes)
	}
	if copyResp.SourceAgentID != "source" || copyResp.TargetAgentID != "target" || len(copyResp.CompositionHash) != 64 || len(copyResp.BootPackSetHash) != 64 {
		t.Fatalf("copy response = %+v", copyResp)
	}
}

func TestAgentPackWireAdapter_EmptyHashesAndSentinelClassification(t *testing.T) {
	registry, store := newEffectivePackRegistry(t)
	id := identityForWireTest(t)
	service := newEffectivePackService(t, registry, store, nil)
	resp, err := service.Inspect(effectivePackContext(t, id), prototypes.AgentConfigAgentPacksInspectRequest{AgentID: "empty"})
	if err != nil {
		t.Fatalf("empty inspect: %v", err)
	}
	if len(resp.CompositionHash) != 64 || len(resp.BootPackSetHash) != 64 {
		t.Fatalf("empty hashes = (%q, %q)", resp.CompositionHash, resp.BootPackSetHash)
	}
	cases := []struct {
		name string
		err  error
		want harborprotocol.AgentPacksErrorCode
	}{
		{name: "invalid", err: ErrAgentPackCopyInvalid, want: harborprotocol.AgentPacksErrorInvalid},
		{name: "not found", err: ErrAgentPackNotFound, want: harborprotocol.AgentPacksErrorNotFound},
		{name: "stale", err: ErrAgentPackCopyExpectation, want: harborprotocol.AgentPacksErrorStale},
		{name: "collision", err: ErrAgentPackCopyCollision, want: harborprotocol.AgentPacksErrorConflict},
		{name: "idempotency", err: ErrAgentPackCopyIdempotencyConflict, want: harborprotocol.AgentPacksErrorIdempotencyConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAgentPackError(tc.err); got != tc.want {
				t.Fatalf("ClassifyAgentPackError = %q, want %q", got, tc.want)
			}
		})
	}
	if got := ClassifyAgentPackError(errors.New("storage unavailable")); got != harborprotocol.AgentPacksErrorUnavailable {
		t.Fatalf("unknown error class = %q", got)
	}
}

func identityForWireTest(t *testing.T) identity.Identity {
	t.Helper()
	return identity.Identity{TenantID: "tenant-a", UserID: "operator", SessionID: "session-a"}
}
