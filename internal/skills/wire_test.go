package skills_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/skills"
)

func hashSkill() skills.Skill {
	return skills.Skill{
		Name:          "deploy",
		Title:         "Deploy the service",
		Description:   "Roll out a new revision",
		Trigger:       "user asks to deploy",
		TaskType:      "ops",
		Tags:          []string{"b", "a"},
		Steps:         []string{"build", "push", "verify"},
		Preconditions: []string{"tests green"},
		FailureModes:  []string{"rollback"},
		RequiredTools: []string{"kubectl", "docker"},
		RequiredNS:    []string{"prod"},
		RequiredTags:  []string{"deploy"},
		Origin:        skills.OriginGenerated,
		Scope:         skills.ScopeProject,
		Extra:         map[string]any{"model": "claude", "tokens": 1200},
	}
}

func TestCanonicalContentHash_DeterministicAndStable(t *testing.T) {
	t.Parallel()
	s := hashSkill()
	h1 := skills.CanonicalContentHash(s)
	h2 := skills.CanonicalContentHash(s)
	if h1 != h2 {
		t.Fatalf("CanonicalContentHash not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(h1))
	}
}

func TestCanonicalContentHash_ExcludesProvenanceAndLifecycleFields(t *testing.T) {
	t.Parallel()
	base := hashSkill()
	want := skills.CanonicalContentHash(base)

	// Provenance fields are excluded (D-046): the same content imported
	// two ways must hash identically.
	prov := base
	prov.Origin = skills.OriginPack
	prov.OriginRef = "pack://somewhere"
	prov.Scope = skills.ScopeTenant
	prov.ScopeTenantID = "tenant-x"
	prov.ScopeProjectID = "proj-y"
	if got := skills.CanonicalContentHash(prov); got != want {
		t.Errorf("hash changed on provenance-field mutation: got %q, want %q", got, want)
	}

	// Lifecycle fields are excluded — they evolve without content change.
	life := base
	life.UseCount = 999
	if got := skills.CanonicalContentHash(life); got != want {
		t.Errorf("hash changed on UseCount mutation: got %q, want %q", got, want)
	}
}

func TestCanonicalContentHash_SliceOrderNormalisation(t *testing.T) {
	t.Parallel()
	base := hashSkill()
	want := skills.CanonicalContentHash(base)

	// Set-shaped slices are sorted before hashing — caller-side ordering
	// noise must not perturb the hash.
	reordered := base
	reordered.Tags = []string{"a", "b"}
	reordered.RequiredTools = []string{"docker", "kubectl"}
	reordered.RequiredNS = []string{"prod"}
	reordered.RequiredTags = []string{"deploy"}
	if got := skills.CanonicalContentHash(reordered); got != want {
		t.Errorf("hash changed on set-slice reorder: got %q, want %q", got, want)
	}

	// Ordered slices (Steps) DO participate in order — reordering them
	// is a genuine content change.
	stepsReordered := base
	stepsReordered.Steps = []string{"push", "build", "verify"}
	if got := skills.CanonicalContentHash(stepsReordered); got == want {
		t.Error("hash unchanged on Steps reorder — Steps order must be load-bearing")
	}
}

func TestCanonicalContentHash_ContentChangeFlipsHash(t *testing.T) {
	t.Parallel()
	base := hashSkill()
	want := skills.CanonicalContentHash(base)

	for _, tc := range []struct {
		name   string
		mutate func(*skills.Skill)
	}{
		{"name", func(s *skills.Skill) { s.Name = "deploy-v2" }},
		{"title", func(s *skills.Skill) { s.Title = "Deploy differently" }},
		{"description", func(s *skills.Skill) { s.Description = "changed" }},
		{"trigger", func(s *skills.Skill) { s.Trigger = "changed" }},
		{"task type", func(s *skills.Skill) { s.TaskType = "infra" }},
		{"tags", func(s *skills.Skill) { s.Tags = []string{"a", "b", "c"} }},
		{"steps", func(s *skills.Skill) { s.Steps = []string{"build"} }},
		{"preconditions", func(s *skills.Skill) { s.Preconditions = nil }},
		{"failure modes", func(s *skills.Skill) { s.FailureModes = []string{"alert"} }},
		{"required tools", func(s *skills.Skill) { s.RequiredTools = []string{"kubectl"} }},
		{"required ns", func(s *skills.Skill) { s.RequiredNS = []string{"staging"} }},
		{"required tags", func(s *skills.Skill) { s.RequiredTags = nil }},
		{"extra", func(s *skills.Skill) { s.Extra = map[string]any{"model": "other"} }},
	} {
		s := hashSkill()
		tc.mutate(&s)
		if got := skills.CanonicalContentHash(s); got == want {
			t.Errorf("hash unchanged after mutating %s — that field must participate in the hash", tc.name)
		}
	}
}

func TestCanonicalContentHash_ExtraCanonicalisation(t *testing.T) {
	t.Parallel()
	// Key order in the Extra map must not matter — canonicalExtra sorts.
	a := hashSkill()
	a.Extra = map[string]any{"alpha": "1", "beta": true, "gamma": int64(7), "delta": 3.5, "eps": nil}
	b := hashSkill()
	b.Extra = map[string]any{"eps": nil, "delta": 3.5, "gamma": int64(7), "beta": true, "alpha": "1"}
	if skills.CanonicalContentHash(a) != skills.CanonicalContentHash(b) {
		t.Error("Extra key order perturbed the hash — canonicalExtra must sort keys")
	}

	// An unhashable value type renders as a stable sentinel rather than
	// panicking or producing a non-deterministic hash.
	weird := hashSkill()
	weird.Extra = map[string]any{"fn": []string{"not", "a", "scalar"}}
	h1 := skills.CanonicalContentHash(weird)
	h2 := skills.CanonicalContentHash(weird)
	if h1 != h2 {
		t.Error("unhashable Extra value produced a non-deterministic hash")
	}

	// Empty / nil Extra is well-defined.
	noExtra := hashSkill()
	noExtra.Extra = nil
	if got := skills.CanonicalContentHash(noExtra); len(got) != 64 {
		t.Errorf("nil Extra hash length = %d, want 64", len(got))
	}
}
