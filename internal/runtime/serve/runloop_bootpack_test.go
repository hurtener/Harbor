package serve

// runloop_bootpack_test.go — the HA-66 run-start boot baseline at the
// run-loop driver seam: the eager immutable boot-pack reader is looked up
// by the EXACT (tenant, effective-agent) key and bound into the run skill
// snapshot's membership; the concrete resolver's exact run-start operator
// tier binds deterministic boot/combined hashes plus one source marker per
// item; virtual allowlisting filters the reader without erasing that
// provenance; and 128 concurrent mixed lookups + snapshot captures never
// bleed across keys.

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/virtualagent"
)

// bootPackFake is a narrow immutable BootPackReader fake: one frozen
// (tenant, agent) bucket per key, Lookup returns deep copies. Used only for
// the concurrency test — the exact-key tests drive the REAL eager loader
// index (buildTestBootIndex).
type bootPackFake struct {
	entries map[bootpacks.Key][]bootpacks.Entry
}

func (f bootPackFake) Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool) {
	entries, ok := f.entries[bootpacks.Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return nil, false
	}
	out := make([]bootpacks.Entry, len(entries))
	for i, e := range entries {
		out[i] = e
		out[i].Skill = cloneSnapshotSkill(e.Skill)
	}
	return out, true
}

var _ agentcfgprotocol.BootPackReader = bootPackFake{}

// fakeBootEntry freezes one boot entry whose SemanticHash matches the
// canonical hash of its body — the same invariant the eager loader pins.
func fakeBootEntry(skill skills.Skill) bootpacks.Entry {
	return bootpacks.Entry{
		Skill:        skill,
		SemanticHash: skill.ContentHash,
		Source:       "/fake/boot",
	}
}

// cloneSnapshotSkill deep-copies the slice/map fields of a Skill so the
// fake's Lookup never shares mutable backing storage with a caller.
func cloneSnapshotSkill(s skills.Skill) skills.Skill {
	s.Tags = append([]string(nil), s.Tags...)
	s.Steps = append([]string(nil), s.Steps...)
	s.Preconditions = append([]string(nil), s.Preconditions...)
	s.FailureModes = append([]string(nil), s.FailureModes...)
	s.RequiredTools = append([]string(nil), s.RequiredTools...)
	s.RequiredNS = append([]string(nil), s.RequiredNS...)
	s.RequiredTags = append([]string(nil), s.RequiredTags...)
	if s.Extra != nil {
		m := make(map[string]any, len(s.Extra))
		for k, v := range s.Extra {
			m[k] = v
		}
		s.Extra = m
	}
	return s
}

// snapshotRows resolves the snapshot's bound reader over the run ctx and
// returns its deterministic List rows.
func snapshotRows(t *testing.T, snapshot skills.RunSkillReaderSnapshot, q identity.Quadruple, base skills.SkillStore) []string {
	t.Helper()
	reader, err := skills.ResolveSkillReader(withRunSnapshot(t, q, snapshot), q, base)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.List(t.Context(), q, skills.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return skillNames(rows)
}

// TestRunLoopDriver_BootPackReader_ExactKeyComposesBootBaseline proves the
// driver looks up the eager index by EXACTLY (tenant, effective-agent) and
// binds the frozen entries into the run snapshot: the snapshot carries the
// deterministic index boot set hash, a combined operator-tier hash, the
// boot provenance marker, and the boot skill is the reader's only row.
func TestRunLoopDriver_BootPackReader_ExactKeyComposesBootBaseline(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-1"}
	const agentID = "agent-x"
	activateRunSnapshotAgent(t, st, q, agentID)
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
	if err != nil || !ok {
		t.Fatalf("capture exact-key snapshot: ok=%t err=%v", ok, err)
	}
	if !snapshot.HasOperatorTier() {
		t.Fatal("exact-key snapshot has no operator tier — the boot baseline did not compose")
	}
	wantBootHash, _ := idx.BootPackSetHash("t1", "agent-x")
	if snapshot.BootPackSetHash() != wantBootHash {
		t.Fatalf("snapshot boot set hash = %q, want the eager index's %q", snapshot.BootPackSetHash(), wantBootHash)
	}
	if snapshot.OperatorTierHash() == "" {
		t.Fatal("snapshot combined operator-tier hash is absent for a composed boot baseline")
	}
	src, found := snapshot.OperatorTierSource("playbook")
	if !found || src != skills.OperatorTierSourceBoot {
		t.Fatalf("playbook source = (%q, %v), want (boot, true)", src, found)
	}
	if got := snapshotRows(t, snapshot, q, base); !equalStrings(got, []string{"playbook"}) {
		t.Fatalf("exact-key snapshot rows = %v, want [playbook]", got)
	}
}

// TestRunLoopDriver_BootPackReader_ForeignTenantOrAgentMisses proves a
// lookup miss (a different tenant OR a different agent than the frozen
// index key) binds NO boot baseline: no identity is invented, there is no
// fallback to the boot agent id, and the composed tier stays absent.
func TestRunLoopDriver_BootPackReader_ForeignTenantOrAgentMisses(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	cases := []struct {
		name    string
		q       identity.Quadruple
		agentID string
	}{
		{
			name:    "foreign-tenant",
			q:       identity.Quadruple{Identity: identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s1"}, RunID: "run-f1"},
			agentID: "agent-x",
		},
		{
			name:    "foreign-agent",
			q:       identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-f2"},
			agentID: "other-agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activateRunSnapshotAgent(t, st, tc.q, tc.agentID)
			snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), tc.agentID, tc.q, nil)
			if err != nil || !ok {
				t.Fatalf("capture %s snapshot: ok=%t err=%v", tc.name, ok, err)
			}
			if snapshot.HasOperatorTier() {
				t.Fatalf("%s snapshot bound an operator tier; the frozen index has no such key", tc.name)
			}
			if snapshot.BootPackSetHash() != "" {
				t.Fatalf("%s snapshot boot set hash = %q, want absent", tc.name, snapshot.BootPackSetHash())
			}
			if rows := snapshotRows(t, snapshot, tc.q, base); len(rows) != 0 {
				t.Fatalf("%s snapshot rows = %v, want none (no boot baseline)", tc.name, rows)
			}
		})
	}
}

// TestRunLoopDriver_BootPackReader_SameHashRevisionDedupesBoth proves the
// strict composer dedupes an active-revision pack item whose canonical
// content equals the boot entry as source=both, retaining the boot body.
func TestRunLoopDriver_BootPackReader_SameHashRevisionDedupesBoth(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-1"}
	const agentID = "agent-x"
	activateRunSnapshotAgent(t, st, q, agentID)
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	// The active revision carries the SAME canonical content (title, trigger,
	// task_type, steps match the boot body) → one combined item marked both.
	if _, err := reg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "playbook", Title: "playbook title", Trigger: "boot trigger",
			TaskType: "domain", Steps: []string{"boot step"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
	if err != nil || !ok {
		t.Fatalf("capture same-hash snapshot: ok=%t err=%v", ok, err)
	}
	src, found := snapshot.OperatorTierSource("playbook")
	if !found || src != skills.OperatorTierSourceBoth {
		t.Fatalf("playbook source = (%q, %v), want (both, true) — same-hash revision must dedupe", src, found)
	}
	if snapshot.BootPackSetHash() == "" {
		t.Fatal("same-hash snapshot lost the boot baseline set hash")
	}
	// The boot body is the higher-authority source retained for a both item.
	reader, err := skills.ResolveSkillReader(withRunSnapshot(t, q, snapshot), q, base)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(t.Context(), q, "playbook")
	if err != nil {
		t.Fatal(err)
	}
	if got.Trigger != "boot trigger" || got.Steps[0] != "boot step" {
		t.Fatalf("both item body = (trigger %q, steps %v), want the boot body", got.Trigger, got.Steps)
	}
}

// TestRunLoopDriver_BootPackReader_DifferingRevisionFailsSnapshot proves a
// same-name / differing-hash active revision fails the snapshot capture
// LOUD (the strict composer's conflict) — never a silent overwrite.
func TestRunLoopDriver_BootPackReader_DifferingRevisionFailsSnapshot(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-1"}
	const agentID = "agent-x"
	activateRunSnapshotAgent(t, st, q, agentID)
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	// The active revision declares the SAME canonical name with DIFFERENT
	// content → the strict composer must reject the run-start snapshot.
	if _, err := reg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "playbook", Title: "playbook title", Trigger: "other trigger",
			TaskType: "domain", Steps: []string{"boot step"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	_, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
	if err == nil {
		t.Fatal("capture with a differing-hash revision must fail loud")
	}
	if ok {
		t.Fatal("capture with a differing-hash revision returned a snapshot")
	}
	if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
		t.Fatalf("differing-hash capture error = %v, want ErrOperatorTierConflict", err)
	}
}

// TestRunLoopDriver_BootPackReader_DeterministicHashesAndEveryItemSource
// proves the snapshot exposes the deterministic boot set hash (byte-identical
// to the eager index), a distinct combined hash, and one source marker per
// operator-tier item.
func TestRunLoopDriver_BootPackReader_DeterministicHashesAndEveryItemSource(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-1"}
	const agentID = "agent-x"
	activateRunSnapshotAgent(t, st, q, agentID)
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	if _, err := reg.SetRevision(t.Context(), q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "ops-runbook", Title: "ops runbook title", Trigger: "when ops",
			TaskType: "domain", Steps: []string{"do ops"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
	if err != nil || !ok {
		t.Fatalf("capture deterministic-hashes snapshot: ok=%t err=%v", ok, err)
	}
	wantBoot, _ := idx.BootPackSetHash("t1", "agent-x")
	if snapshot.BootPackSetHash() != wantBoot {
		t.Fatalf("snapshot boot set hash %q != eager index hash %q", snapshot.BootPackSetHash(), wantBoot)
	}
	if snapshot.OperatorTierHash() == "" || snapshot.OperatorTierHash() == snapshot.BootPackSetHash() {
		t.Fatalf("combined hash %q must be present and distinct from the boot set hash %q", snapshot.OperatorTierHash(), snapshot.BootPackSetHash())
	}
	wantSources := map[string]skills.OperatorTierSource{
		"playbook":    skills.OperatorTierSourceBoot,
		"ops-runbook": skills.OperatorTierSourceRevision,
	}
	for name, want := range wantSources {
		src, found := snapshot.OperatorTierSource(name)
		if !found || src != want {
			t.Fatalf("item %q source = (%q, %v), want (%q, true)", name, src, found, want)
		}
	}
	if rows := snapshotRows(t, snapshot, q, base); !equalStrings(rows, []string{"ops-runbook", "playbook"}) {
		t.Fatalf("deterministic-hashes snapshot rows = %v, want [ops-runbook playbook]", rows)
	}
}

// TestRunLoopDriver_BootPackReader_VirtualAllowlistKeepsProvenance proves
// virtual allowlisting wraps the reader WITHOUT erasing the underlying
// exact run-start tier's hashes or per-item provenance: a profile that
// narrows AWAY the boot skill still yields a snapshot whose boot set hash
// and boot provenance survive, while the reader itself is filtered.
func TestRunLoopDriver_BootPackReader_VirtualAllowlistKeepsProvenance(t *testing.T) {
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, RunID: "run-1"}
	const agentID = "agent-x"
	activateRunSnapshotAgent(t, st, q, agentID)
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = idx

	// The virtual profile narrows to a skill the boot baseline does NOT
	// declare — the allowlist reader must deny the boot skill at the reader
	// boundary while the snapshot keeps the exact run-start provenance.
	allow := []string{"unrelated-skill"}
	profile := &virtualagent.Profile{Overlay: virtualagent.Overlay{Skills: &allow}}
	snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, profile)
	if err != nil || !ok {
		t.Fatalf("capture allowlisted snapshot: ok=%t err=%v", ok, err)
	}
	if !snapshot.HasOperatorTier() {
		t.Fatal("allowlisted snapshot lost its operator tier — the virtual wrapper erased provenance")
	}
	wantBoot, _ := idx.BootPackSetHash("t1", "agent-x")
	if snapshot.BootPackSetHash() != wantBoot {
		t.Fatalf("allowlisted snapshot boot set hash = %q, want %q", snapshot.BootPackSetHash(), wantBoot)
	}
	src, found := snapshot.OperatorTierSource("playbook")
	if !found || src != skills.OperatorTierSourceBoot {
		t.Fatalf("allowlisted playbook source = (%q, %v), want (boot, true)", src, found)
	}
	if rows := snapshotRows(t, snapshot, q, base); len(rows) != 0 {
		t.Fatalf("allowlisted reader rows = %v, want none (the virtual allowlist filtered the boot skill)", rows)
	}
}

// TestRunLoopDriver_BootPackReader_ConcurrentLookupsAndSnapshotsNoBleed
// runs 128 concurrent mixed direct lookups + full snapshot captures against
// ONE shared driver + ONE shared fake reader under -race: each goroutine
// must observe ONLY its own (tenant, agent) boot entries — no cross-key
// bleed in the direct Lookup, the snapshot's hashes, its provenance, or its
// reader rows.
func TestRunLoopDriver_BootPackReader_ConcurrentLookupsAndSnapshotsNoBleed(t *testing.T) {
	const runs = 128
	reg := acTestRegistry(t)
	st := runSnapshotState(t)
	base := &runSnapshotReader{}
	fake := bootPackFake{entries: make(map[bootpacks.Key][]bootpacks.Entry, runs)}
	driver, _ := newRunSnapshotDriver(t, reg, base, st)
	driver.bootPackReader = fake
	for i := range runs {
		q := concurrencySnapshotQ(i)
		agentID := "agent-" + q.SessionID
		activateRunSnapshotAgent(t, st, q, agentID)
		skill := bootSkillFor("boot-"+q.SessionID, "trigger "+q.SessionID, "step "+q.SessionID)
		fake.entries[bootpacks.Key{TenantID: q.TenantID, AgentID: agentID}] = []bootpacks.Entry{fakeBootEntry(skill)}
	}

	errCh := make(chan error, runs*2)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := concurrencySnapshotQ(i)
			agentID := "agent-" + q.SessionID
			// Leg 1: the direct immutable lookup.
			entries, ok := fake.Lookup(q.TenantID, agentID)
			if !ok || len(entries) != 1 || entries[0].Skill.Name != "boot-"+q.SessionID {
				errCh <- fmt.Errorf("run %d: direct lookup bled: %d entries ok=%v", i, len(entries), ok)
				return
			}
			// Leg 2: the full run-start snapshot capture.
			snapshot, ok, err := driver.captureRunSkillSnapshot(t.Context(), agentID, q, nil)
			if err != nil || !ok {
				errCh <- fmt.Errorf("run %d: capture: ok=%t err=%w", i, ok, err)
				return
			}
			if snapshot.BootPackSetHash() == "" {
				errCh <- fmt.Errorf("run %d: snapshot lost its boot baseline", i)
				return
			}
			src, found := snapshot.OperatorTierSource("boot-" + q.SessionID)
			if !found || src != skills.OperatorTierSourceBoot {
				errCh <- fmt.Errorf("run %d: own source = (%q, %v), want (boot, true)", i, src, found)
				return
			}
			// No foreign boot item may appear in THIS run's tier.
			neighbor := concurrencySnapshotQ((i + 1) % runs)
			if _, foreign := snapshot.OperatorTierSource("boot-" + neighbor.SessionID); foreign {
				errCh <- fmt.Errorf("run %d: foreign boot item %q bled into the tier", i, "boot-"+neighbor.SessionID)
				return
			}
			if rows := snapshotRows(t, snapshot, q, base); !equalStrings(rows, []string{"boot-" + q.SessionID}) {
				errCh <- fmt.Errorf("run %d: snapshot rows %v, want [boot-%s]", i, rows, q.SessionID)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
