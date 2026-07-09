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

// TestNormalizePayload_Naming_InertDropsAndTrims proves an entirely-inert
// section drops away, a section with any field set survives, and the model is
// trimmed.
func TestNormalizePayload_Naming_InertDropsAndTrims(t *testing.T) {
	// Inert (auto false + all zero + empty model) → dropped.
	got := agentcfg.NormalizePayload(agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "  ")})
	if got.Naming != nil {
		t.Fatalf("inert naming section should drop to nil, got %+v", got.Naming)
	}
	hNone, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{})
	hInert, _ := agentcfg.ContentHash(agentcfg.ConfigPayload{Naming: namingSection(false, 0, 0, 0, 0, "")})
	if hNone != hInert {
		t.Fatal("an inert naming section must hash equal to no section")
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

// TestDiffNaming_PerFieldDelta proves each dimension reports its own delta.
func TestDiffNaming_PerFieldDelta(t *testing.T) {
	from := agentcfg.ConfigPayload{Naming: namingSection(true, 1, 2, 3, 80, "m1")}
	to := agentcfg.ConfigPayload{Naming: namingSection(false, 1, 2, 3, 80, "m1")} // only auto flipped
	d := agentcfg.DiffNaming(from, to)
	if !d.Changed() || !d.AutoChanged || d.AutoFrom != true || d.AutoTo != false {
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
