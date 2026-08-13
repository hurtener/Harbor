package virtualagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestProfile_OutputContract_NormalizesAndPinsSchemaHash(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	sum := sha256.Sum256(schema)
	p := NormalizeProfile(Profile{
		Key: "review", Parent: "agent-a", InputPatterns: []string{"image/*", "*.png", "*.png"},
		InputCount: 2, InputDisposition: "ref", OutputSchema: schema,
	})
	if got := p.OutputSchemaHash; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("schema hash = %q, want %x", got, sum)
	}
	if err := ValidateProfile(p); err != nil {
		t.Fatalf("ValidateProfile: %v", err)
	}
	if len(p.InputPatterns) != 2 {
		t.Fatalf("normalized input patterns = %v, want de-duplicated patterns", p.InputPatterns)
	}
}

func TestProfile_OutputContract_RejectsForgedSchemaHash(t *testing.T) {
	p := NormalizeProfile(Profile{
		Key: "review", Parent: "agent-a", OutputSchema: json.RawMessage(`{"type":"object"}`),
		OutputSchemaHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err := ValidateProfile(p); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("ValidateProfile error = %v, want ErrInvalidProfile", err)
	}
}

// jsonMarshalOverlay renders an Overlay to JSON (the canonical form the
// structural-shape test inspects).
func jsonMarshalOverlay(o Overlay) (string, error) {
	raw, err := json.Marshal(o)
	return string(raw), err
}

func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }
func intPtr(i int) *int         { return &i }

func testProfile(key string) Profile {
	return Profile{
		Key:    Key(key),
		Label:  "test label " + key,
		Parent: "top-agent",
		Overlay: Overlay{
			Model:     strPtr("profile-model"),
			MaxTokens: intPtr(4000),
			Skills:    &[]string{"review", "summarize"},
		},
	}
}

// TestProfileHash_CanonicalRepresentationIsShared pins the
// YAML/revision canonical-representation invariant: two profiles with
// identical content — one built field-by-field, one built via
// NormalizeProfile with a re-ordered list — hash equal, and any content
// change bumps the hash.
func TestProfileHash_CanonicalRepresentationIsShared(t *testing.T) {
	a := testProfile("reviewer")
	b := testProfile("reviewer")
	// Re-order the skill list — order is not semantic for a set.
	b.Overlay.Skills = &[]string{"summarize", "review"}

	ha, err := a.Hash()
	if err != nil {
		t.Fatalf("Hash(a): %v", err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatalf("Hash(b): %v", err)
	}
	if ha != hb {
		t.Fatalf("re-ordered profile hashes differ: %s vs %s (the canonical form is not shared)", ha, hb)
	}

	c := testProfile("reviewer")
	c.Overlay.MaxTokens = intPtr(8000)
	hc, err := c.Hash()
	if err != nil {
		t.Fatalf("Hash(c): %v", err)
	}
	if hc == ha {
		t.Fatalf("content change did not bump the profile hash")
	}
	if len(hc) != 64 {
		t.Fatalf("profile hash is not a 64-hex SHA-256: %q", hc)
	}
}

// TestOverlay_NarrowOnlyShapeIsStructural pins that the overlay has no
// widening dimension: no providers / hooks / memory / capabilities /
// parent-profile reference / A2A target fields can exist. The set of
// JSON keys an Overlay marshals is exactly the bounded narrow-only set.
func TestOverlay_NarrowOnlyShapeIsStructural(t *testing.T) {
	var o Overlay
	raw, err := jsonMarshalOverlay(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{
		"provider", "hook", "memory", "capabil", "a2a", "parent_profile", "enable", "session",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("overlay JSON carries a forbidden widening dimension %q: %s", forbidden, raw)
		}
	}
}

// TestValidateOverlay_RejectsWideningOrOversizedValues pins the
// fail-loud validation boundary: values that would widen or overflow
// are rejected, never silently clamped.
func TestValidateOverlay_RejectsWideningOrOversizedValues(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Overlay)
		wantErr error
	}{
		{"empty model", func(o *Overlay) { o.Model = strPtr("  ") }, ErrInvalidOverlay},
		{"over-long model", func(o *Overlay) { o.Model = strPtr(strings.Repeat("m", MaxModelLength+1)) }, ErrInvalidOverlay},
		{"negative temperature", func(o *Overlay) { o.Temperature = f64Ptr(-0.1) }, ErrInvalidOverlay},
		{"temperature above 2", func(o *Overlay) { o.Temperature = f64Ptr(2.1) }, ErrInvalidOverlay},
		{"zero max_tokens", func(o *Overlay) { o.MaxTokens = intPtr(0) }, ErrInvalidOverlay},
		{"oversized max_tokens", func(o *Overlay) { o.MaxTokens = intPtr(MaxMaxTokens + 1) }, ErrInvalidOverlay},
		{"unknown reasoning effort", func(o *Overlay) { o.ReasoningEffort = strPtr("turbo") }, ErrInvalidOverlay},
		{"oversized skills set", func(o *Overlay) { o.Skills = &[]string{strings.Repeat("s", MaxSkillEntries+1)} }, ErrInvalidOverlay},
		{"blank skill name", func(o *Overlay) { o.Skills = &[]string{" "} }, ErrInvalidOverlay},
		{"oversized tool list", func(o *Overlay) { o.DisabledTools = []string{strings.Repeat("t", MaxToolListEntries+1)} }, ErrInvalidOverlay},
		{"zero max_steps", func(o *Overlay) { o.MaxSteps = intPtr(0) }, ErrInvalidOverlay},
		{"oversized instructions", func(o *Overlay) { o.Instructions = strings.Repeat("x", MaxInstructionsBytes+1) }, ErrInvalidOverlay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testProfile("k").Overlay
			tc.mutate(&o)
			err := ValidateOverlay(o)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateOverlay = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateOverlay_AcceptsEveryNarrowDimension pins the valid narrow
// surface end-to-end.
func TestValidateOverlay_AcceptsEveryNarrowDimension(t *testing.T) {
	o := Overlay{
		Model:           strPtr("narrow-model"),
		Temperature:     f64Ptr(0.2),
		MaxTokens:       intPtr(2000),
		ReasoningEffort: strPtr("low"),
		Skills:          &[]string{"review"},
		DisabledTools:   []string{"danger.tool"},
		PausedServers:   []string{"slow.server"},
		MaxSteps:        intPtr(10),
		TokenBudget:     intPtr(5000),
		Instructions:    "You are a specialist code reviewer.",
	}
	if err := ValidateOverlay(o); err != nil {
		t.Fatalf("ValidateOverlay(valid) = %v", err)
	}
}

// TestSkillsPresence_Semantics pins that a nil Skills pointer means "no
// narrowing" while a non-nil (even empty) pointer means "narrow to
// exactly this set" — the omission byte-compatibility contract.
func TestSkillsPresence_Semantics(t *testing.T) {
	if NormalizeOverlay(Overlay{}).Skills != nil {
		t.Fatal("zero overlay must leave Skills nil (omission byte-compatible)")
	}
	o := NormalizeOverlay(Overlay{Skills: &[]string{}})
	if o.Skills == nil {
		t.Fatal("explicit empty skills set must stay non-nil (narrow to nothing)")
	}
	if len(*o.Skills) != 0 {
		t.Fatalf("explicit empty skills set has %d members", len(*o.Skills))
	}
}

// TestMap_OwnerBindingAndUniqueness pins the one-owner invariant: every
// profile's parent must equal the map owner, keys must be unique, and a
// violating map is rejected loud.
func TestMap_OwnerBindingAndUniqueness(t *testing.T) {
	good := Map{
		Owner:    "top-agent",
		Profiles: []Profile{testProfile("a"), testProfile("b")},
	}
	if err := ValidateMap(good); err != nil {
		t.Fatalf("ValidateMap(good) = %v", err)
	}

	orphan := good
	orphan.Profiles[0].Parent = "other-agent"
	if err := ValidateMap(orphan); !errors.Is(err, ErrInvalidMap) {
		t.Fatalf("ValidateMap(parent mismatch) = %v, want ErrInvalidMap", err)
	}

	dup := Map{
		Owner:    "top-agent",
		Profiles: []Profile{testProfile("a"), testProfile("a")},
	}
	if err := ValidateMap(dup); !errors.Is(err, ErrInvalidMap) {
		t.Fatalf("ValidateMap(duplicate) = %v, want ErrInvalidMap", err)
	}

	noOwner := good
	noOwner.Owner = ""
	if err := ValidateMap(noOwner); !errors.Is(err, ErrInvalidMap) {
		t.Fatalf("ValidateMap(no owner) = %v, want ErrInvalidMap", err)
	}
}

// TestFrozenMap_UnknownInvalidStaleFailsBeforePersistence pins the
// spawn-time boundary: unknown key, invalid overlay, and a stale live
// revision all fail loud, and only a fully valid frozen profile binds.
func TestFrozenMap_UnknownInvalidStaleFailsBeforePersistence(t *testing.T) {
	m := Map{
		Owner:    "top-agent",
		Profiles: []Profile{testProfile("reviewer")},
	}
	frozen, err := NewFrozenMap(m, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap: %v", err)
	}

	if _, err := frozen.Bind(testProfile("nope")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Bind(unknown) = %v, want ErrUnknown", err)
	}

	// Stale live revision: the pinned rev is "rev-1"; the live reader
	// reports "rev-2" — the spawn must fail before persistence.
	live := func(context.Context) (string, string, error) { return "rev-2", strings.Repeat("b", 64), nil }
	staleFrozen, err := NewFrozenMap(m, "rev-1", strings.Repeat("a", 64), live)
	if err != nil {
		t.Fatalf("NewFrozenMap(stale): %v", err)
	}
	if err := staleFrozen.VerifyCurrent(context.Background()); !errors.Is(err, ErrStale) {
		t.Fatalf("VerifyCurrent(stale) = %v, want ErrStale", err)
	}

	// Live still matching: valid bind.
	liveOK := func(context.Context) (string, string, error) { return "rev-1", strings.Repeat("a", 64), nil }
	okFrozen, err := NewFrozenMap(m, "rev-1", strings.Repeat("a", 64), liveOK)
	if err != nil {
		t.Fatalf("NewFrozenMap(ok): %v", err)
	}
	if err := okFrozen.VerifyCurrent(context.Background()); err != nil {
		t.Fatalf("VerifyCurrent(ok) = %v", err)
	}
	b, err := okFrozen.Bind(testProfile("reviewer"))
	if err != nil {
		t.Fatalf("Bind(valid) = %v", err)
	}
	if err := ValidateBinding(b); err != nil {
		t.Fatalf("ValidateBinding(bind) = %v", err)
	}
	if b.AgentID != "top-agent" || b.Parent != "top-agent" {
		t.Fatalf("binding agent/parent = %q/%q, want top-agent/top-agent", b.AgentID, b.Parent)
	}
	if b.ConfigRevisionID != "rev-1" {
		t.Fatalf("binding revision = %q, want rev-1", b.ConfigRevisionID)
	}
}

// TestFrozenMap_VerifyPin_RestartReproducesExactProfile pins the
// restart contract: an identical current map reproduces the exact
// profile; a moved revision (stale), an edited profile (tampered), or a
// missing key (missing) all fail the child run loud.
func TestFrozenMap_VerifyPin_RestartReproducesExactProfile(t *testing.T) {
	m := Map{Owner: "top-agent", Profiles: []Profile{testProfile("reviewer")}}
	frozen, err := NewFrozenMap(m, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap: %v", err)
	}
	b, err := frozen.Bind(testProfile("reviewer"))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Restart with the SAME config revision + profile: exact reproduction.
	p, err := frozen.VerifyPin(b)
	if err != nil {
		t.Fatalf("VerifyPin(same) = %v, want exact profile", err)
	}
	if p.Key != "reviewer" {
		t.Fatalf("pinned profile key = %q, want reviewer", p.Key)
	}

	// Restart with a moved config revision: stale.
	movedRev, err := NewFrozenMap(m, "rev-2", strings.Repeat("b", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap(moved): %v", err)
	}
	if _, err := movedRev.VerifyPin(b); !errors.Is(err, ErrStale) {
		t.Fatalf("VerifyPin(moved revision) = %v, want ErrStale", err)
	}

	// Restart with an edited profile definition: tampered.
	edited := testProfile("reviewer")
	edited.Overlay.MaxTokens = intPtr(9000)
	editedMap := Map{Owner: "top-agent", Profiles: []Profile{edited}}
	editedFrozen, err := NewFrozenMap(editedMap, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap(edited): %v", err)
	}
	if _, err := editedFrozen.VerifyPin(b); !errors.Is(err, ErrTampered) {
		t.Fatalf("VerifyPin(edited profile) = %v, want ErrTampered", err)
	}

	// Restart with the profile removed: missing.
	emptyMap := Map{Owner: "top-agent"}
	emptyFrozen, err := NewFrozenMap(emptyMap, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap(empty): %v", err)
	}
	if _, err := emptyFrozen.VerifyPin(b); !errors.Is(err, ErrMissing) {
		t.Fatalf("VerifyPin(missing key) = %v, want ErrMissing", err)
	}

	// A tampered binding field (label rewrite) fails loud.
	tampered := b
	tampered.Label = "forged label"
	if _, err := frozen.VerifyPin(tampered); !errors.Is(err, ErrTampered) {
		t.Fatalf("VerifyPin(tampered binding) = %v, want ErrTampered", err)
	}
}

func TestBinding_ProfileSnapshotIsSealedAndTamperEvident(t *testing.T) {
	p := testProfile("reviewer")
	frozen, err := NewFrozenMap(Map{Owner: "top-agent", Profiles: []Profile{p}}, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap: %v", err)
	}
	b, err := frozen.Bind(p)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	b.Profile.Overlay.DisabledTools = append(b.Profile.Overlay.DisabledTools, "forged")
	if _, err := frozen.VerifyPin(b); !errors.Is(err, ErrTampered) {
		t.Fatalf("VerifyPin(tampered snapshot) = %v, want ErrTampered", err)
	}

	got, ok := frozen.Profile("reviewer")
	if !ok {
		t.Fatal("Profile(reviewer) missing")
	}
	got.Overlay.DisabledTools = append(got.Overlay.DisabledTools, "caller-mutation")
	again, _ := frozen.Profile("reviewer")
	if len(again.Overlay.DisabledTools) != len(p.Overlay.DisabledTools) {
		t.Fatalf("frozen profile was mutated through returned slices: %v", again.Overlay.DisabledTools)
	}
}

// TestOverlayNarrowingOperations pins the intersection / union / clamp
// helpers: skills narrow by intersection, tool exclusions only ever
// grow, and limits never widen past the parent's ceiling.
func TestOverlayNarrowingOperations(t *testing.T) {
	got := IntersectStrings([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	want := []string{"b", "c"}
	if !equalStrings(got, want) {
		t.Fatalf("IntersectStrings = %v, want %v", got, want)
	}
	if IntersectStrings([]string{"a"}, nil) != nil {
		t.Fatal("IntersectStrings with nil keep must be nil (no narrowing)")
	}

	u := UnionStrings([]string{"x", "y"}, []string{"y", "z"})
	if !equalStrings(u, []string{"x", "y", "z"}) {
		t.Fatalf("UnionStrings = %v, want [x y z]", u)
	}

	if got := ClampMax(100, intPtr(50)); got != 50 {
		t.Fatalf("ClampMax(100, 50) = %d, want 50", got)
	}
	if got := ClampMax(30, intPtr(50)); got != 30 {
		t.Fatalf("ClampMax(30, 50) = %d, want 30", got)
	}
	if got := ClampMax(30, nil); got != 30 {
		t.Fatalf("ClampMax(30, nil) = %d, want 30", got)
	}

	if got := OverlayClampMaxSteps(100, intPtr(200)); got != 100 {
		t.Fatalf("OverlayClampMaxSteps(100, 200) = %d, want 100 (never widen)", got)
	}
	if got := OverlayClampMaxSteps(100, intPtr(50)); got != 50 {
		t.Fatalf("OverlayClampMaxSteps(100, 50) = %d, want 50", got)
	}
	if got := OverlayClampMaxSteps(100, nil); got != 100 {
		t.Fatalf("OverlayClampMaxSteps(100, nil) = %d, want 100", got)
	}

	if got := OverlayClampMaxTokens(intPtr(4000), intPtr(8000)); got == nil || *got != 4000 {
		t.Fatalf("OverlayClampMaxTokens(4000, 8000) = %v, want 4000", got)
	}
	if got := OverlayClampMaxTokens(intPtr(4000), nil); got == nil || *got != 4000 {
		t.Fatalf("OverlayClampMaxTokens(4000, nil) = %v, want 4000", got)
	}
	if got := OverlayClampMaxTokens(nil, intPtr(3000)); got == nil || *got != 3000 {
		t.Fatalf("OverlayClampMaxTokens(nil, 3000) = %v, want 3000", got)
	}

	if got := OverlayClampTokenBudget(0, intPtr(5000)); got != 5000 {
		t.Fatalf("OverlayClampTokenBudget(0, 5000) = %d, want 5000 (introducing a limit is narrowing)", got)
	}
	if got := OverlayClampTokenBudget(2000, intPtr(5000)); got != 2000 {
		t.Fatalf("OverlayClampTokenBudget(2000, 5000) = %d, want 2000", got)
	}
	if got := OverlayClampTokenBudget(2000, intPtr(1000)); got != 1000 {
		t.Fatalf("OverlayClampTokenBudget(2000, 1000) = %d, want 1000", got)
	}
	if got := OverlayClampTokenBudget(2000, nil); got != 2000 {
		t.Fatalf("OverlayClampTokenBudget(2000, nil) = %d, want 2000", got)
	}
}

// TestBinding_ValidateRejectsMalformed pins the persisted-binding shape
// gate.
func TestBinding_ValidateRejectsMalformed(t *testing.T) {
	good := Binding{
		AgentID: "top-agent", Key: "reviewer", Label: "reviewer",
		Parent: "top-agent", ConfigRevisionID: "rev-1",
		ConfigDigest: strings.Repeat("a", 64), ProfileHash: strings.Repeat("b", 64),
	}
	if err := ValidateBinding(good); err != nil {
		t.Fatalf("ValidateBinding(good) = %v", err)
	}
	bad := []struct {
		name   string
		mutate func(*Binding)
	}{
		{"empty agent", func(b *Binding) { b.AgentID = "" }},
		{"bad key", func(b *Binding) { b.Key = "UPPER" }},
		{"parent != agent", func(b *Binding) { b.Parent = "other" }},
		{"short hash", func(b *Binding) { b.ProfileHash = "abc" }},
		{"non-hex hash", func(b *Binding) { b.ProfileHash = strings.Repeat("z", 64) }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			b := good
			tc.mutate(&b)
			if err := ValidateBinding(b); err == nil {
				t.Fatalf("ValidateBinding(%s) = nil, want error", tc.name)
			}
		})
	}
}

// TestFrozenMap_CtxCarriers pins the per-run ctx transport: the frozen
// map and the run's own binding ride ctx and are recovered by the
// dispatch executor, and a fresh ctx has neither.
func TestFrozenMap_CtxCarriers(t *testing.T) {
	ctx := context.Background()
	if FrozenMapFrom(ctx) != nil {
		t.Fatal("bare ctx must carry no frozen map")
	}
	if RunBindingFrom(ctx) != nil {
		t.Fatal("bare ctx must carry no run binding")
	}
	m := Map{Owner: "top-agent", Profiles: []Profile{testProfile("reviewer")}}
	frozen, err := NewFrozenMap(m, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatalf("NewFrozenMap: %v", err)
	}
	b, _ := frozen.Bind(testProfile("reviewer"))
	ctx = WithFrozenMap(ctx, frozen)
	ctx = WithRunBinding(ctx, &b)
	if got := FrozenMapFrom(ctx); got == nil || got.Owner != "top-agent" {
		t.Fatalf("FrozenMapFrom = %+v, want owner top-agent", got)
	}
	if got := RunBindingFrom(ctx); got == nil || got.Key != "reviewer" {
		t.Fatalf("RunBindingFrom = %+v, want key reviewer", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
