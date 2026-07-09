package agentcfg_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

func namingSection(auto bool, after, repeat, maxReps, maxLen int, model string) *agentcfg.NamingSection {
	return &agentcfg.NamingSection{
		Auto: auto, AfterTurns: after, RepeatEvery: repeat,
		MaxRepetitions: maxReps, MaxTitleLen: maxLen, Model: model,
	}
}

// TestContentHash_IncludesNaming proves a revision that changes ONLY the
// naming section produces a distinct content hash.
func TestContentHash_IncludesNaming(t *testing.T) {
	base := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}}
	withNaming := agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
		Naming: namingSection(true, 1, 0, 0, 80, ""),
	}
	hb, _ := agentcfg.ContentHash(base)
	hn, _ := agentcfg.ContentHash(withNaming)
	if hb == hn {
		t.Fatal("adding a naming section did not change the content hash")
	}
	// Distinct policies hash distinctly.
	a, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Naming: namingSection(true, 1, 2, 3, 80, "")})
	b, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Naming: namingSection(true, 1, 2, 4, 80, "")})
	if a == b {
		t.Fatal("different max_repetitions hashed equal")
	}
}

// TestNormalizePayload_Naming_PresenceIsPreserved proves any non-nil naming
// section survives normalization VERBATIM (model trimmed) — section presence
// is the operator's signal. The load-bearing case is the bare `{auto: false}`
// opt-out: dropping it as "inert" would silently discard an explicit
// per-agent opt-out over a yaml-on fleet default (the M1 footgun — the agent
// would keep auto-naming and spending after a 200 OK opt-out).
func TestNormalizePayload_Naming_PresenceIsPreserved(t *testing.T) {
	// A bare auto:false section (the explicit opt-out) is PRESERVED.
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "  ")})
	if got.Naming == nil {
		t.Fatal("a bare auto:false naming section was dropped — the explicit opt-out signal is lost")
	}
	if got.Naming.Auto {
		t.Fatalf("auto flipped on: %+v", got.Naming)
	}
	// Presence is hash-distinguishable from absence (a bare opt-out section
	// is a REAL revision, never normalized into the no-section state).
	hNone, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{})
	hOptOut, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "")})
	if hNone == hOptOut {
		t.Fatal("a present auto:false naming section must hash differently from no section")
	}
	// Idempotency: a re-set of the same opt-out hashes equal (the idempotent
	// no-op re-set is between two PRESENT sections, not present-vs-absent).
	hOptOut2, _ := agentcfg.ContentHash(agentcfg.NormalizePayload(agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "  ")}))
	if hOptOut != hOptOut2 {
		t.Fatal("normalization is not a fixpoint for the naming opt-out section")
	}
	// Auto=true survives.
	gotOn := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Naming: namingSection(true, 0, 0, 0, 0, "")})
	if gotOn.Naming == nil || !gotOn.Naming.Auto {
		t.Fatalf("auto=true naming section should survive, got %+v", gotOn.Naming)
	}
	// Model trimmed.
	gotTrim := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Naming: namingSection(true, 1, 0, 0, 80, "  profile-a  ")})
	if gotTrim.Naming == nil || gotTrim.Naming.Model != "profile-a" {
		t.Fatalf("model should be trimmed, got %+v", gotTrim.Naming)
	}
}

// TestDiffNaming_PerFieldDelta proves each dimension reports its own delta,
// with Auto as the tri-state "" (absent) / "false" / "true" display string.
func TestDiffNaming_PerFieldDelta(t *testing.T) {
	from := agentcfg.ConfigPayload{Naming: namingSection(true, 1, 2, 3, 80, "m1")}
	to := agentcfg.ConfigPayload{Naming: namingSection(false, 1, 2, 3, 80, "m1")} // only auto flipped
	d := agentcfg.DiffNaming(from, to)
	if !d.Changed() || !d.AutoChanged || d.AutoFrom != "true" || d.AutoTo != "false" {
		t.Errorf("auto delta = %+v", d)
	}
	if d.ModelChanged || d.AfterTurnsChanged {
		t.Errorf("unchanged dimensions reported a change: %+v", d)
	}
	// set → unset.
	d2 := agentcfg.DiffNaming(agentcfg.ConfigPayload{Naming: namingSection(true, 5, 0, 0, 90, "m")}, agentcfg.ConfigPayload{})
	if !d2.AfterTurnsChanged || d2.AfterTurnsFrom != "5" || d2.AfterTurnsTo != "" {
		t.Errorf("after_turns set→unset delta = %+v", d2)
	}
	if !d2.MaxTitleLenChanged || d2.MaxTitleLenFrom != "90" || d2.MaxTitleLenTo != "" {
		t.Errorf("max_title_len set→unset delta = %+v", d2)
	}
	if agentcfg.DiffNaming(from, from).Changed() {
		t.Error("identical naming sections reported a change")
	}
}

// TestDiffNaming_AbsentVsBareOptOut_Registers proves the diff is NOT blind to
// the bare `{auto: false}` opt-out revision the presence-preserve fix made
// meaningful: an absent section and a present bare opt-out differ in BOTH
// directions (Auto tri-state "" vs "false"), so the revision never renders as
// "no change in any section" (the phantom-revision follow-up).
func TestDiffNaming_AbsentVsBareOptOut_Registers(t *testing.T) {
	absent := agentcfg.ConfigPayload{}
	bareOptOut := agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "")}

	// absent -> bare opt-out.
	d := agentcfg.DiffNaming(absent, bareOptOut)
	if !d.Changed() || !d.AutoChanged {
		t.Fatalf("absent->bareOptOut did not register: %+v", d)
	}
	if d.AutoFrom != "" || d.AutoTo != "false" {
		t.Errorf("absent->bareOptOut auto tri-state = %q->%q, want \"\"->\"false\"", d.AutoFrom, d.AutoTo)
	}

	// bare opt-out -> absent (a rollback past the opt-out).
	d2 := agentcfg.DiffNaming(bareOptOut, absent)
	if !d2.Changed() || !d2.AutoChanged {
		t.Fatalf("bareOptOut->absent did not register: %+v", d2)
	}
	if d2.AutoFrom != "false" || d2.AutoTo != "" {
		t.Errorf("bareOptOut->absent auto tri-state = %q->%q, want \"false\"->\"\"", d2.AutoFrom, d2.AutoTo)
	}

	// Two absent sections and two identical bare opt-outs are still no-change.
	if agentcfg.DiffNaming(absent, absent).Changed() {
		t.Error("absent vs absent reported a change")
	}
	if agentcfg.DiffNaming(bareOptOut, bareOptOut).Changed() {
		t.Error("bareOptOut vs bareOptOut reported a change")
	}
}

// TestNamingView proves the convenience accessor.
func TestNamingView(t *testing.T) {
	if _, ok := (agentcfg.ConfigPayload{}).NamingView(); ok {
		t.Error("empty payload reported a naming section")
	}
	n, ok := (agentcfg.ConfigPayload{Naming: namingSection(true, 2, 0, 0, 100, "m")}).NamingView()
	if !ok || !n.Auto || n.AfterTurns != 2 || n.MaxTitleLen != 100 {
		t.Errorf("NamingView = %+v, ok=%v", n, ok)
	}
}
