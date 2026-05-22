package react

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// TestRepairTierFor_CountToTierMapping pins the load-bearing tier
// policy: count <= 0 → none, 1 → reminder, 2 → warning, >= 3 →
// critical (Phase 83c — D-145).
func TestRepairTierFor_CountToTierMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		count int
		want  RepairTier
	}{
		{-3, RepairTierNone},
		{0, RepairTierNone},
		{1, RepairTierReminder},
		{2, RepairTierWarning},
		{3, RepairTierCritical},
		{4, RepairTierCritical},
		{99, RepairTierCritical},
	}
	for _, tc := range cases {
		if got := repairTierFor(tc.count); got != tc.want {
			t.Errorf("repairTierFor(%d) = %q, want %q", tc.count, got, tc.want)
		}
	}
}

// TestRepairGuidance_GoldenFixtures asserts each tier's exported copy
// constant byte-matches its checked-in golden fixture. A copy change
// to a constant fails this test until the fixture is regenerated —
// so copy edits show up as a reviewable diff (Phase 83c plan §test
// plan).
func TestRepairGuidance_GoldenFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture string
		got     string
	}{
		{"finish_reminder.txt", ReminderFinishGuidance},
		{"finish_warning.txt", WarningFinishGuidance},
		{"finish_critical.txt", CriticalFinishGuidance},
		{"args_reminder.txt", ReminderArgsGuidance},
		{"args_warning.txt", WarningArgsGuidance},
		{"args_critical.txt", CriticalArgsGuidance},
		{"multi_action_reminder.txt", ReminderMultiActionGuidance},
		{"multi_action_warning.txt", WarningMultiActionGuidance},
		{"multi_action_critical.txt", CriticalMultiActionGuidance},
	}
	for _, tc := range cases {
		path := filepath.Join("testdata", "repair_guidance", tc.fixture)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(want) != tc.got {
			t.Errorf("copy for %s drifted from fixture\n golden: %q\n  const: %q",
				tc.fixture, string(want), tc.got)
		}
	}
}

// TestRepairGuidance_EachTierBodyNamesItsTier is a defensive check
// mirroring the smoke script: a copy-paste typo that reuses the wrong
// tier's text would leave a body without its own tier name. Every
// constant must open with its tier word.
func TestRepairGuidance_EachTierBodyNamesItsTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tier RepairTier
		copy string
	}{
		{RepairTierReminder, ReminderFinishGuidance},
		{RepairTierWarning, WarningFinishGuidance},
		{RepairTierCritical, CriticalFinishGuidance},
		{RepairTierReminder, ReminderArgsGuidance},
		{RepairTierWarning, WarningArgsGuidance},
		{RepairTierCritical, CriticalArgsGuidance},
		{RepairTierReminder, ReminderMultiActionGuidance},
		{RepairTierWarning, WarningMultiActionGuidance},
		{RepairTierCritical, CriticalMultiActionGuidance},
	}
	for _, tc := range cases {
		if !strings.HasPrefix(tc.copy, string(tc.tier)+":") {
			t.Errorf("copy %q does not open with tier %q", tc.copy, tc.tier)
		}
	}
}

// TestRenderRepairGuidance_NilAndZeroCounters asserts the no-
// augmentation cases render the empty string.
func TestRenderRepairGuidance_NilAndZeroCounters(t *testing.T) {
	t.Parallel()
	if got := renderRepairGuidance(nil); got != "" {
		t.Errorf("renderRepairGuidance(nil) = %q, want empty", got)
	}
	if got := renderRepairGuidance(&planner.RepairCounters{}); got != "" {
		t.Errorf("renderRepairGuidance(zero) = %q, want empty", got)
	}
}

// TestRenderRepairGuidance_SingleCounterTiers asserts each counter at
// each tier renders exactly its tier copy.
func TestRenderRepairGuidance_SingleCounterTiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    planner.RepairCounters
		want string
	}{
		{"finish-reminder", planner.RepairCounters{FinishRepair: 1}, ReminderFinishGuidance},
		{"finish-warning", planner.RepairCounters{FinishRepair: 2}, WarningFinishGuidance},
		{"finish-critical", planner.RepairCounters{FinishRepair: 5}, CriticalFinishGuidance},
		{"args-reminder", planner.RepairCounters{ArgsRepair: 1}, ReminderArgsGuidance},
		{"args-warning", planner.RepairCounters{ArgsRepair: 2}, WarningArgsGuidance},
		{"args-critical", planner.RepairCounters{ArgsRepair: 3}, CriticalArgsGuidance},
		{"multi-reminder", planner.RepairCounters{MultiAction: 1}, ReminderMultiActionGuidance},
		{"multi-warning", planner.RepairCounters{MultiAction: 2}, WarningMultiActionGuidance},
		{"multi-critical", planner.RepairCounters{MultiAction: 9}, CriticalMultiActionGuidance},
	}
	for _, tc := range cases {
		c := tc.c
		if got := renderRepairGuidance(&c); got != tc.want {
			t.Errorf("%s: renderRepairGuidance = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRenderRepairGuidance_MultipleCountersFixedOrder asserts that
// when several counters trip, the lines render in the fixed finish →
// args → multi_action order regardless of which is highest.
func TestRenderRepairGuidance_MultipleCountersFixedOrder(t *testing.T) {
	t.Parallel()
	c := planner.RepairCounters{FinishRepair: 3, ArgsRepair: 1, MultiAction: 2}
	got := renderRepairGuidance(&c)
	want := CriticalFinishGuidance + "\n" + ReminderArgsGuidance + "\n" + WarningMultiActionGuidance
	if got != want {
		t.Errorf("multi-counter render mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildAdditionalGuidance_OperatorAndRepairMerge asserts the
// <additional_guidance> body composes operator content above repair
// guidance, omits the section when both are empty, and renders only
// the non-empty side otherwise.
func TestBuildAdditionalGuidance_OperatorAndRepairMerge(t *testing.T) {
	t.Parallel()

	// Both empty → empty.
	if got := buildAdditionalGuidance("", planner.RunContext{}); got != "" {
		t.Errorf("both-empty: got %q, want empty", got)
	}

	// Operator only.
	if got := buildAdditionalGuidance("op text", planner.RunContext{}); got != "op text" {
		t.Errorf("operator-only: got %q, want %q", got, "op text")
	}

	// Repair only.
	rcRepair := planner.RunContext{RepairCounters: &planner.RepairCounters{ArgsRepair: 1}}
	if got := buildAdditionalGuidance("", rcRepair); got != ReminderArgsGuidance {
		t.Errorf("repair-only: got %q, want %q", got, ReminderArgsGuidance)
	}

	// Both — operator above repair, blank-line separated.
	got := buildAdditionalGuidance("op text", rcRepair)
	want := "op text\n\n" + ReminderArgsGuidance
	if got != want {
		t.Errorf("both: got %q, want %q", got, want)
	}
}

// TestUpdateRepairCounters_IncrementAndReset exercises the runtime-
// side counter update: increment on the matching signal, reset on a
// clean step.
func TestUpdateRepairCounters_IncrementAndReset(t *testing.T) {
	t.Parallel()

	// Nil counters → no-op (no panic).
	updateRepairCounters(planner.RunContext{}, planner.CallTool{Tool: "x"}, repair.RepairOutcome{})

	c := &planner.RepairCounters{}
	rc := planner.RunContext{RepairCounters: c}

	// Args-repair signal increments ArgsRepair.
	updateRepairCounters(rc, planner.CallTool{Tool: "x"}, repair.RepairOutcome{ArgsRepaired: true})
	if c.ArgsRepair != 1 {
		t.Fatalf("after args repair: ArgsRepair = %d, want 1", c.ArgsRepair)
	}
	updateRepairCounters(rc, planner.CallTool{Tool: "x"}, repair.RepairOutcome{ArgsRepaired: true})
	if c.ArgsRepair != 2 {
		t.Fatalf("after second args repair: ArgsRepair = %d, want 2", c.ArgsRepair)
	}
	// Clean tool-call step resets ArgsRepair + MultiAction.
	updateRepairCounters(rc, planner.CallTool{Tool: "x"}, repair.RepairOutcome{})
	if c.ArgsRepair != 0 || c.MultiAction != 0 {
		t.Fatalf("clean step did not reset: %+v", *c)
	}

	// Multi-action signal increments MultiAction.
	updateRepairCounters(rc, planner.CallParallel{}, repair.RepairOutcome{MultiAction: true})
	if c.MultiAction != 1 {
		t.Fatalf("after multi-action: MultiAction = %d, want 1", c.MultiAction)
	}

	// Graceful-failure finish increments FinishRepair.
	updateRepairCounters(rc, planner.Finish{Reason: planner.FinishNoPath}, repair.RepairOutcome{})
	if c.FinishRepair != 1 {
		t.Fatalf("after no-path finish: FinishRepair = %d, want 1", c.FinishRepair)
	}
	updateRepairCounters(rc, planner.Finish{Reason: planner.FinishNoPath}, repair.RepairOutcome{})
	if c.FinishRepair != 2 {
		t.Fatalf("after second no-path finish: FinishRepair = %d, want 2", c.FinishRepair)
	}
	// Clean goal finish resets FinishRepair.
	updateRepairCounters(rc, planner.Finish{Reason: planner.FinishGoal}, repair.RepairOutcome{})
	if c.FinishRepair != 0 {
		t.Fatalf("clean finish did not reset FinishRepair: %d", c.FinishRepair)
	}
}

// TestUpdateRepairCounters_NonQualityFinishLeavesFinishCounter
// asserts a Cancelled / deadline finish does not touch FinishRepair.
func TestUpdateRepairCounters_NonQualityFinishLeavesFinishCounter(t *testing.T) {
	t.Parallel()
	c := &planner.RepairCounters{FinishRepair: 2}
	rc := planner.RunContext{RepairCounters: c}
	updateRepairCounters(rc, planner.Finish{Reason: planner.FinishCancelled}, repair.RepairOutcome{})
	if c.FinishRepair != 2 {
		t.Errorf("cancelled finish changed FinishRepair: %d, want 2", c.FinishRepair)
	}
}

// TestUpdateRepairCounters_ConcurrentDisjointRunContexts is the
// D-145 cross-run isolation check at the counter level: 100+
// goroutines, each with its OWN RepairCounters, run updateRepairCounters
// many times. Under -race, a shared counter or package state would
// trip; the per-RunContext scoping must keep each goroutine's counter
// independent.
func TestUpdateRepairCounters_ConcurrentDisjointRunContexts(t *testing.T) {
	t.Parallel()
	const goroutines = 128
	const iterations = 50

	var wg sync.WaitGroup
	results := make([]int, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c := &planner.RepairCounters{}
			rc := planner.RunContext{RepairCounters: c}
			for range iterations {
				// Every goroutine drives a steady args-repair signal —
				// its OWN counter must reach exactly `iterations`.
				updateRepairCounters(rc, planner.CallTool{Tool: "x"},
					repair.RepairOutcome{ArgsRepaired: true})
			}
			results[idx] = c.ArgsRepair
		}(g)
	}
	wg.Wait()

	for idx, got := range results {
		if got != iterations {
			t.Errorf("goroutine %d: ArgsRepair = %d, want %d (cross-run bleed)",
				idx, got, iterations)
		}
	}
}
