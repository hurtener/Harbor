package agentcfg_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

// hooksSection builds a run-completion-hook config section.
func hooksSection(tool string, timeoutMS int) *agentcfg.HooksSection {
	return &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: tool, TimeoutMS: timeoutMS}}
}

// TestContentHash_IncludesHooks proves a revision that changes ONLY the
// run-lifecycle-hook section produces a distinct content hash (a real,
// diffable revision).
func TestContentHash_IncludesHooks(t *testing.T) {
	base := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}}
	withHook := agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
		Hooks:  hooksSection("sink", 5000),
	}
	hb, err := agentcfg.ContentHash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	hh, err := agentcfg.ContentHash(withHook)
	if err != nil {
		t.Fatalf("hash withHook: %v", err)
	}
	if hb == hh {
		t.Fatal("adding a hooks section did not change the content hash")
	}
	// A different tool is a distinct hash.
	t1, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Hooks: hooksSection("sink-a", 0)})
	t2, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Hooks: hooksSection("sink-b", 0)})
	if t1 == t2 {
		t.Fatal("different hook tools hashed equal")
	}
	// A different timeout is a distinct hash.
	t3, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Hooks: hooksSection("sink", 1000)})
	t4, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Hooks: hooksSection("sink", 2000)})
	if t3 == t4 {
		t.Fatal("different hook timeouts hashed equal")
	}
}

// TestNormalizePayload_Hooks_EmptyToolDropsSection proves an empty /
// whitespace-only tool normalises the whole hooks section away (a hook with
// no target is not a hook), and a negative timeout normalises to 0.
func TestNormalizePayload_Hooks_EmptyToolDropsSection(t *testing.T) {
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Hooks: hooksSection("   ", 5000)})
	if got.Hooks != nil {
		t.Fatalf("empty-tool hooks section should drop to nil, got %+v", got.Hooks)
	}
	// Hash equality: an empty-tool section equals no section.
	hNone, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{})
	hEmpty, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Hooks: hooksSection("", 0)})
	if hNone != hEmpty {
		t.Fatal("an empty-tool hooks section must hash equal to no section")
	}
	// Negative timeout normalises to 0.
	gotNeg := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Hooks: hooksSection("sink", -1)})
	if gotNeg.Hooks == nil || gotNeg.Hooks.RunCompletion == nil || gotNeg.Hooks.RunCompletion.TimeoutMS != 0 {
		t.Fatalf("negative timeout should normalise to 0, got %+v", gotNeg.Hooks)
	}
	// Whitespace around a real tool is trimmed.
	gotTrim := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Hooks: hooksSection("  sink  ", 3000)})
	if gotTrim.Hooks == nil || gotTrim.Hooks.RunCompletion.Tool != "sink" {
		t.Fatalf("tool should be trimmed to 'sink', got %+v", gotTrim.Hooks)
	}
}

// TestDiffHooks_PerFieldDelta proves each hook dimension reports its own
// change + from/to (an unset hook is "").
func TestDiffHooks_PerFieldDelta(t *testing.T) {
	from := agentcfg.ConfigPayload{Hooks: hooksSection("sink-a", 1000)}
	to := agentcfg.ConfigPayload{Hooks: hooksSection("sink-b", 1000)} // tool changed, timeout same
	d := agentcfg.DiffHooks(from, to)
	if !d.Changed() {
		t.Fatal("Changed() = false, want true")
	}
	if !d.RunCompletionToolChanged || d.RunCompletionToolFrom != "sink-a" || d.RunCompletionToolTo != "sink-b" {
		t.Errorf("tool delta = %+v", d)
	}
	if d.RunCompletionTimeoutChanged {
		t.Errorf("timeout should be unchanged (1000 → 1000): %+v", d)
	}
	// set → unset.
	d2 := agentcfg.DiffHooks(agentcfg.ConfigPayload{Hooks: hooksSection("sink", 2000)}, agentcfg.ConfigPayload{})
	if !d2.RunCompletionToolChanged || d2.RunCompletionToolFrom != "sink" || d2.RunCompletionToolTo != "" {
		t.Errorf("tool set→unset delta = %+v", d2)
	}
	if !d2.RunCompletionTimeoutChanged || d2.RunCompletionTimeoutFrom != "2000" || d2.RunCompletionTimeoutTo != "" {
		t.Errorf("timeout set→unset delta = %+v", d2)
	}
}

func TestDiffHooks_NoChange(t *testing.T) {
	same := agentcfg.ConfigPayload{Hooks: hooksSection("sink", 5000)}
	if agentcfg.DiffHooks(same, same).Changed() {
		t.Error("identical hooks sections reported a change")
	}
	if agentcfg.DiffHooks(agentcfg.ConfigPayload{}, agentcfg.ConfigPayload{}).Changed() {
		t.Error("two empty payloads reported a change")
	}
}

// TestRunCompletionHookView proves the convenience accessor.
func TestRunCompletionHookView(t *testing.T) {
	if _, ok := (agentcfg.ConfigPayload{}).RunCompletionHookView(); ok {
		t.Error("empty payload reported a run-completion hook")
	}
	rc, ok := (agentcfg.ConfigPayload{Hooks: hooksSection("sink", 7000)}).RunCompletionHookView()
	if !ok || rc.Tool != "sink" || rc.TimeoutMS != 7000 {
		t.Errorf("RunCompletionHookView = %+v, ok=%v", rc, ok)
	}
}
