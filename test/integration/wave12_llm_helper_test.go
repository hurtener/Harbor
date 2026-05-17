// Package integration_test — Wave 12 §17.5 audit F5 closure helper.
//
// This file is split out from wave12_test.go to keep the LLM /
// planner-driver self-registration imports out of the main test
// file's import block (which would couple the inspect-topology
// scenarios to the planner stack unnecessarily).
//
// The blank imports below fire two registrations:
//   - `internal/llm/mock` — registers the "mock" LLM driver. Without
//     this the planner-resolve test cannot build an LLMClient for
//     FactoryDeps.LLM.
//   - `internal/planner/react` — registers the "react" planner
//     driver. Without this `planner.Resolve("react", ...)` returns
//     ErrDriverUnknown.

package integration_test

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	_ "github.com/hurtener/Harbor/internal/planner/react"
)

// llmCloseableClient is the narrow surface the F5 planner test needs:
// an LLMClient with a Close method. The full LLMClient interface
// satisfies this; the alias documents the test's narrow requirement.
type llmCloseableClient = llm.LLMClient

// llmConfigSnapshotForMock returns a minimal ConfigSnapshot the mock
// driver accepts. The mock ignores Provider/Model/APIKey/BaseURL/
// Timeout/ModelProfiles — only the Driver field is load-bearing.
func llmConfigSnapshotForMock() llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:   "mock",
		Provider: "mock",
		Model:    "mock-model",
	}
}

// llmOpenMock opens the mock-driver LLM client via the same path
// `cmd/harbor/cmd_dev.go::bootDevStack` uses when HARBOR_DEV_ALLOW_MOCK
// is set. Deps.Artifacts + Deps.Bus are required by the llm.Open
// validator (the materialise pass needs Artifacts; the event-emission
// path needs Bus); an in-mem ArtifactStore + in-mem EventBus satisfy
// the contract for the planner-resolve test path.
func llmOpenMock(snap llm.ConfigSnapshot) (llm.LLMClient, error) {
	ctx := context.Background()
	artStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		return nil, err
	}
	redactor, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		return nil, err
	}
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     64,
		IdleTimeout:              5 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, redactor)
	if err != nil {
		return nil, err
	}
	return llm.Open(ctx, snap, llm.Deps{Artifacts: artStore, Bus: bus})
}
