package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestService_ConcurrentSameAgentWrites_NoLostSibling is the wave-end
// regression for the lost-update race (audit FAIL-1): every convenience verb
// does a read-active → rebuild-siblings → set-revision read-modify-write, and
// the registry is last-write-wins. Without per-agent write serialisation, two
// concurrent edits to the SAME agent each rebuild the sibling sections from
// the other's pre-write snapshot, silently reverting the concurrent sibling
// change. N≥100 concurrent writes via THREE different verbs (prompt /
// tool-exposure / llm-params) against ONE shared Service+registry must leave
// ALL THREE sections present in the final active revision, and the parent
// chain must stay linear (no fork). Run under -race.
func TestService_ConcurrentSameAgentWrites_NoLostSibling(t *testing.T) {
	s := svc(t, false)
	ctx := context.Background()
	const n = 120

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 3 {
			case 0:
				_, err = s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
					Identity: scope(), AgentID: testAgentID,
					PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr(fmt.Sprintf("base-%d", i))},
				})
			case 1:
				_, err = s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
					Identity: scope(), AgentID: testAgentID,
					ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{fmt.Sprintf("srv-%d", i)}},
				})
			case 2:
				_, err = s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
					Identity: scope(), AgentID: testAgentID,
					LLMParams: prototypes.AgentConfigLLMParams{Temperature: f64(0.5)},
				})
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}

	// All three sections must COEXIST: each was set by ≥1 goroutine, and the
	// per-agent write lock guarantees no verb rebuilt from a snapshot missing
	// a concurrently-committed sibling. The lost-update bug would clobber at
	// least one section to absent.
	get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !get.Set || get.Revision == nil {
		t.Fatalf("get: %+v err=%v", get, err)
	}
	p := get.Revision.Payload
	if p.PromptLayers == nil {
		t.Error("prompt-layer section lost under concurrent writes (lost-update race)")
	}
	if p.ToolExposure == nil {
		t.Error("tool-exposure section lost under concurrent writes (lost-update race)")
	}
	if p.LLMParams == nil {
		t.Error("llm-params section lost under concurrent writes (lost-update race)")
	}

	// The parent chain must be LINEAR — serialised writes parent each new
	// revision off the immediately-prior active, so no two revisions share a
	// parent (a shared parent = a fork from a stale snapshot).
	list, err := s.ListRevisions(ctx, prototypes.AgentConfigListRevisionsRequest{
		Identity: scope(), AgentID: testAgentID,
	})
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	seenParent := map[string]string{}
	for _, r := range list.Revisions {
		if r.ParentRevisionID == "" {
			continue
		}
		if prev, dup := seenParent[r.ParentRevisionID]; dup {
			t.Fatalf("parent chain forked: revisions %s and %s both parent off %s",
				prev, r.RevisionID, r.ParentRevisionID)
		}
		seenParent[r.ParentRevisionID] = r.RevisionID
	}

	// Goroutine baseline restored — no per-write goroutine leaked (§11). The
	// write path is synchronous; allow a brief settle for the just-joined
	// workers to be reaped before asserting.
	var after int
	for i := 0; i < 50; i++ {
		after = runtime.NumGoroutine()
		if after <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
