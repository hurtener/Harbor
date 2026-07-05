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
	for i := range n {
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
	for range 50 {
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

// TestService_ConcurrentInterleavedSectionWrites_HooksNeverDropped is the
// D-283 concurrency case: with a run-completion hook pinned FIRST, N>=100
// concurrent section-scoped writes across FOUR verbs that each own a
// DIFFERENT section (prompt / tool-exposure / llm-params / skills — none of
// them Hooks) must never drop the pinned hook. This is the interleaved-
// sections guarantee the rebuild-completeness fix protects under CONCURRENT
// same-agent writes, not just the serial case the hooks_test.go regression
// covers. Run under -race.
func TestService_ConcurrentInterleavedSectionWrites_HooksNeverDropped(t *testing.T) {
	s := svc(t, true) // withSkills — the skills verb needs a SkillStore
	ctx := context.Background()
	const n = 120

	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "run-transcript-sink", TimeoutMS: 5000},
			},
		},
	}); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 4 {
			case 0:
				_, err = s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
					Identity: scope(), AgentID: testAgentID,
					PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr(fmt.Sprintf("ibase-%d", i))},
				})
			case 1:
				_, err = s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
					Identity: scope(), AgentID: testAgentID,
					ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{fmt.Sprintf("isrv-%d", i)}},
				})
			case 2:
				_, err = s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
					Identity: scope(), AgentID: testAgentID,
					LLMParams: prototypes.AgentConfigLLMParams{Temperature: f64(0.6)},
				})
			case 3:
				_, err = s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
					Identity: scope(), AgentID: testAgentID,
					Skill: prototypes.AgentConfigSkillInput{
						Name: fmt.Sprintf("iskill-%d", i), Trigger: "t", Steps: []string{"s"},
						Origin: "generated", Scope: "session",
					},
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
		t.Fatalf("concurrent interleaved write failed: %v", err)
	}

	get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !get.Set || get.Revision == nil {
		t.Fatalf("get: %+v err=%v", get, err)
	}
	p := get.Revision.Payload
	// The pinned hook must survive EVERY concurrent sibling-section write —
	// the D-283 guarantee under concurrency, not just serially.
	if p.Hooks == nil || p.Hooks.RunCompletion == nil || p.Hooks.RunCompletion.Tool != "run-transcript-sink" {
		t.Fatalf("hooks dropped under concurrent interleaved-section writes: %+v", p.Hooks)
	}
	if p.PromptLayers == nil || p.ToolExposure == nil || p.LLMParams == nil || p.Skills == nil {
		t.Errorf("a sibling section was lost under concurrent writes: prompt=%v expo=%v llm=%v skills=%v",
			p.PromptLayers, p.ToolExposure, p.LLMParams, p.Skills)
	}

	var after int
	for range 50 {
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

// TestService_ConcurrentMixedScopeWrites is the user-tier concurrent-reuse
// gate (D-025 + the scope-aware write lock): N≥100 concurrent invocations
// against ONE shared Service mixing admin-scope writes (to the agent chain),
// user-scope writes from DISTINCT users, and repeated user-scope writes from
// the SAME user. It asserts no data races (run under -race), no context bleed
// (each user's final active is its OWN content, never another user's or the
// agent chain's), and no goroutine leak. Because the per-owner write lock keys
// ConfigScopeUser by (scope, tenant, real-user, agent), distinct users use
// distinct mutexes and never serialise across each other.
func TestService_ConcurrentMixedScopeWrites(t *testing.T) {
	s := svc(t, false)
	ctx := context.Background()
	const users = 40
	const repeatsPerUser = 3

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, users*repeatsPerUser+users)

	// Admin-scope writers (distinct agents so each is its own owner).
	for a := range users {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			agent := fmt.Sprintf("agent-%d", a)
			if _, err := s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
				Identity: scope(), AgentID: agent,
				PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr("admin-base")},
			}); err != nil {
				errs <- fmt.Errorf("admin %d: %w", a, err)
			}
		}(a)
	}

	// User-scope writers: distinct users, each writing repeatsPerUser times to
	// the SAME shared agent. The final per-user active must be the user's own
	// latest content.
	for u := range users {
		for r := range repeatsPerUser {
			wg.Add(1)
			go func(u, r int) {
				defer wg.Done()
				user := fmt.Sprintf("user-%d", u)
				if _, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
					Identity: prototypes.IdentityScope{Tenant: "t", User: user, Session: "s"}, AgentID: testAgentID,
					Payload: prototypes.AgentConfigUserPayload{UserPrompt: fmt.Sprintf("prompt-%d-%d", u, r)},
				}); err != nil {
					errs <- fmt.Errorf("user %d/%d: %w", u, r, err)
				}
			}(u, r)
		}
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent mixed-scope write failed: %v", err)
	}

	// Each user's active reflects ONE of their own prompts (never another
	// user's, never the agent base) — no context bleed.
	for u := range users {
		user := fmt.Sprintf("user-%d", u)
		get, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{
			Identity: prototypes.IdentityScope{Tenant: "t", User: user, Session: "s"}, AgentID: testAgentID,
		})
		if err != nil || !get.Set || get.Revision == nil || get.Revision.Payload.PromptLayers == nil || get.Revision.Payload.PromptLayers.User == nil {
			t.Fatalf("user %d get: set=%v err=%v", u, get.Set, err)
		}
		want := fmt.Sprintf("prompt-%d-", u)
		if got := *get.Revision.Payload.PromptLayers.User; len(got) < len(want) || got[:len(want)] != want {
			t.Fatalf("context bleed: user %d active prompt=%q, want prefix %q", u, got, want)
		}
	}

	var after int
	for range 50 {
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
