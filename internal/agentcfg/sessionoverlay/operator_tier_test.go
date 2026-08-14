package sessionoverlay_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

// testOperatorSkill builds ONE shared operator skill body. The boot entry and
// the revision pack convert it with identical content but different
// scope/origin, so skills.CanonicalContentHash — which excludes
// origin/scope/provenance — is identical across the two sources.
func testOperatorSkill(name string) skills.Skill {
	return skills.Skill{
		Name:          name,
		Title:         "operator " + name,
		Description:   "operator body for " + name,
		Trigger:       "when asked about " + name,
		TaskType:      "domain",
		Tags:          []string{"ops", name},
		Steps:         []string{"do it", "verify it"},
		Preconditions: []string{"booted"},
		FailureModes:  []string{"fails"},
		RequiredTools: []string{"run_shell"},
		RequiredNS:    []string{"shell"},
		RequiredTags:  []string{"ops"},
	}
}

// testBootEntry builds one frozen boot baseline entry (the loader fixes
// Origin=pack, Scope=project) with a semantic hash matching its body.
func testBootEntry(name string) bootpacks.Entry {
	skill := testOperatorSkill(name)
	skill.Origin = skills.OriginPack
	skill.Scope = skills.ScopeProject
	entry := bootpacks.Entry{Skill: skill}
	entry.SemanticHash = skills.CanonicalContentHash(skill)
	return entry
}

// testBootEntries builds N distinct boot entries named prefix-0000..N-1.
func testBootEntries(prefix string, n int) []bootpacks.Entry {
	out := make([]bootpacks.Entry, 0, n)
	for i := range n {
		out = append(out, testBootEntry(fmt.Sprintf("%s-%04d", prefix, i)))
	}
	return out
}

// testRevisionPack builds one active-revision pack skill (AgentPackItem.Skill
// fixes Origin=pack, Scope=tenant). Content-identical to testBootEntry(name),
// so the two sources hash identically.
func testRevisionPack(name string) skills.Skill {
	skill := testOperatorSkill(name)
	skill.Origin = skills.OriginPack
	skill.Scope = skills.ScopeTenant
	return skill
}

// testRevisionPacks builds N distinct revision pack skills named
// prefix-0000..N-1.
func testRevisionPacks(prefix string, n int) []skills.Skill {
	out := make([]skills.Skill, 0, n)
	for i := range n {
		out = append(out, testRevisionPack(fmt.Sprintf("%s-%04d", prefix, i)))
	}
	return out
}

// bootPackSetHashEnvelope MUST byte-match the bootpacks index's framing
// envelope ("boot-pack-set-v1\x00") so the reference hash below pins the
// composer's boot_pack_set_hash to the eager index's contract.
const bootPackSetHashEnvelope = "boot-pack-set-v1\x00"

// referenceBootPackSetHash reimplements the bootpacks index's set hash over
// name+semantic-hash pairs (canonical-name sorted, length-framed) so the
// composer's boot hash is pinned to the index contract without needing a
// filesystem load.
func referenceBootPackSetHash(t *testing.T, entries []bootpacks.Entry) string {
	t.Helper()
	sorted := append([]bootpacks.Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return canonicalTestName(sorted[i].Skill.Name) < canonicalTestName(sorted[j].Skill.Name)
	})
	h := sha256.New()
	_, _ = io.WriteString(h, bootPackSetHashEnvelope)
	for _, e := range sorted {
		writeTestFramed(h, canonicalTestName(e.Skill.Name))
		writeTestFramed(h, e.SemanticHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeTestFramed(w io.Writer, s string) {
	_, _ = io.WriteString(w, strconv.Itoa(len(s)))
	_, _ = io.WriteString(w, ":")
	_, _ = io.WriteString(w, s)
	_, _ = io.WriteString(w, ";")
}

func canonicalTestName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// operatorTierItemNames returns the deterministic canonical-name order of a
// tier's items.
func operatorTierItemNames(tier sessionoverlay.OperatorTier) []string {
	items := tier.Items()
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Skill.Name
	}
	return out
}

func TestComposeOperatorTier_StrictMergeMatrix(t *testing.T) {
	t.Parallel()
	alpha := testBootEntry("alpha")
	alphaRev := testRevisionPack("alpha")
	if skills.CanonicalContentHash(alpha.Skill) != skills.CanonicalContentHash(alphaRev) {
		t.Fatalf("fixture: boot and revision alpha must hash identically")
	}
	// A revision body with the SAME canonical name but DIFFERENT content.
	conflictRev := alphaRev
	conflictRev.Steps = []string{"different"}

	t.Run("boot only", func(t *testing.T) {
		tier, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{alpha}, nil)
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", tier.Len())
		}
		if source, ok := tier.Source("ALPHA"); !ok || source != skills.OperatorTierSourceBoot {
			t.Fatalf("Source(alpha) = (%q, %v), want boot", source, ok)
		}
		if got := operatorTierItemNames(tier); fmt.Sprint(got) != "[alpha]" {
			t.Fatalf("items = %v", got)
		}
		if tier.BootPackSetHash() == "" {
			t.Fatal("boot set hash must be present for a boot-only tier")
		}
		if tier.RevisionHash() != "" {
			t.Fatalf("revision hash = %q, want absent", tier.RevisionHash())
		}
		if tier.CombinedHash() == "" {
			t.Fatal("combined hash must be present for a non-empty tier")
		}
	})

	t.Run("revision only", func(t *testing.T) {
		tier, err := sessionoverlay.ComposeOperatorTier(nil, []skills.Skill{alphaRev})
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", tier.Len())
		}
		if source, ok := tier.Source("alpha"); !ok || source != skills.OperatorTierSourceRevision {
			t.Fatalf("Source(alpha) = (%q, %v), want revision", source, ok)
		}
		if tier.BootPackSetHash() != "" {
			t.Fatalf("boot set hash = %q, want absent", tier.BootPackSetHash())
		}
		if tier.RevisionHash() == "" {
			t.Fatal("revision hash must be present for a revision-only tier")
		}
	})

	t.Run("same name same hash dedupes to both", func(t *testing.T) {
		tier, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{alpha}, []skills.Skill{alphaRev})
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 1 {
			t.Fatalf("Len() = %d, want exactly one deduped item", tier.Len())
		}
		item, ok := tier.Get("alpha")
		if !ok || item.Source != skills.OperatorTierSourceBoth {
			t.Fatalf("Get(alpha) = (%+v, %v), want one item source=both", item, ok)
		}
		if item.Skill.Scope != skills.ScopeProject {
			t.Fatalf("source=both item scope = %q, want the retained boot body (project)", item.Skill.Scope)
		}
		if tier.BootPackSetHash() == "" || tier.RevisionHash() == "" || tier.CombinedHash() == "" {
			t.Fatalf("hashes = (%q, %q, %q), want all present", tier.BootPackSetHash(), tier.RevisionHash(), tier.CombinedHash())
		}
	})

	t.Run("same name differing hash fails loud", func(t *testing.T) {
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{alpha}, []skills.Skill{conflictRev})
		if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierConflict", err)
		}
	})

	t.Run("same name differing hash revision wins never silently", func(t *testing.T) {
		// Reverse source order must behave identically: boot first, revision
		// second is the canonical order, but the composer must fail the same
		// way regardless of input order.
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{alpha}, []skills.Skill{conflictRev})
		if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("reversed order = %v, want ErrOperatorTierConflict", err)
		}
	})

	t.Run("canonical alias with differing raw name fails loud", func(t *testing.T) {
		// " Alpha " and "alpha" share the canonical identity but their raw
		// names differ, so their canonical content hashes differ: the merge
		// must report a typed conflict, never silently split into two items
		// (the canonical key makes the alias collision visible) and never
		// silently overwrite.
		aliased := testBootEntry(" Alpha ")
		aliasedRev := testRevisionPack("alpha")
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{aliased}, []skills.Skill{aliasedRev})
		if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierConflict", err)
		}
	})

	t.Run("within-boot duplicate same hash dedupes", func(t *testing.T) {
		same := testBootEntry("beta")
		tier, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{same, same}, nil)
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", tier.Len())
		}
		if source, _ := tier.Source("beta"); source != skills.OperatorTierSourceBoot {
			t.Fatalf("Source(beta) = %q, want boot", source)
		}
	})

	t.Run("within-boot duplicate differing hash fails loud", func(t *testing.T) {
		first := testBootEntry("gamma")
		second := testBootEntry("gamma")
		second.Skill.Steps = []string{"different"}
		second.SemanticHash = skills.CanonicalContentHash(second.Skill)
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{first, second}, nil)
		if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierConflict", err)
		}
	})

	t.Run("within-revision duplicate differing hash fails loud", func(t *testing.T) {
		first := testRevisionPack("delta")
		second := testRevisionPack("delta")
		second.Steps = []string{"different"}
		_, err := sessionoverlay.ComposeOperatorTier(nil, []skills.Skill{first, second})
		if !errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierConflict", err)
		}
	})

	t.Run("boot entry with tampered semantic hash fails loud", func(t *testing.T) {
		tampered := testBootEntry("epsilon")
		tampered.SemanticHash = strings.Repeat("f", 64)
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{tampered}, nil)
		if !errors.Is(err, sessionoverlay.ErrOperatorTierInvalid) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierInvalid", err)
		}
	})

	t.Run("invalid skill body fails loud", func(t *testing.T) {
		bad := testBootEntry("zeta")
		bad.Skill.Steps = nil
		bad.SemanticHash = skills.CanonicalContentHash(bad.Skill)
		_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{bad}, nil)
		if !errors.Is(err, sessionoverlay.ErrOperatorTierInvalid) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierInvalid", err)
		}
	})

	t.Run("cyclic or unsupported Extra fails loud", func(t *testing.T) {
		for name, extra := range map[string]map[string]any{
			"cycle": func() map[string]any {
				value := map[string]any{}
				value["self"] = value
				return value
			}(),
			"unsupported": {"func": func() {}},
		} {
			t.Run(name, func(t *testing.T) {
				bad := testBootEntry("extra-" + name)
				bad.Skill.Extra = extra
				bad.SemanticHash = skills.CanonicalContentHash(bad.Skill)
				_, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{bad}, nil)
				if !errors.Is(err, sessionoverlay.ErrOperatorTierInvalid) {
					t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierInvalid", err)
				}
			})
		}
	})

	t.Run("empty inputs yield an empty tier with absent hashes", func(t *testing.T) {
		tier, err := sessionoverlay.ComposeOperatorTier(nil, nil)
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 0 || len(tier.Items()) != 0 {
			t.Fatalf("empty tier Len=%d Items=%d, want 0", tier.Len(), len(tier.Items()))
		}
		if tier.BootPackSetHash() != "" || tier.RevisionHash() != "" || tier.CombinedHash() != "" {
			t.Fatalf("empty tier hashes = (%q, %q, %q), want all absent", tier.BootPackSetHash(), tier.RevisionHash(), tier.CombinedHash())
		}
		if _, ok := tier.Source("anything"); ok {
			t.Fatal("empty tier Source = present")
		}
	})
}

func TestComposeOperatorTier_Cap256(t *testing.T) {
	t.Parallel()
	t.Run("256 unique combined items is accepted", func(t *testing.T) {
		boot := testBootEntries("shared", 128)
		revision := testRevisionPacks("rev", 128)
		tier, err := sessionoverlay.ComposeOperatorTier(boot, revision)
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 256 {
			t.Fatalf("Len() = %d, want 256", tier.Len())
		}
	})

	t.Run("257 unique combined items fails loud", func(t *testing.T) {
		boot := testBootEntries("shared", 256)
		revision := testRevisionPacks("extra", 1)
		_, err := sessionoverlay.ComposeOperatorTier(boot, revision)
		if !errors.Is(err, sessionoverlay.ErrOperatorTierBound) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierBound", err)
		}
	})

	t.Run("256 boot + 256 revision identical content dedupes to 256 both", func(t *testing.T) {
		boot := testBootEntries("shared", 256)
		revision := testRevisionPacks("shared", 256)
		tier, err := sessionoverlay.ComposeOperatorTier(boot, revision)
		if err != nil {
			t.Fatalf("ComposeOperatorTier: %v", err)
		}
		if tier.Len() != 256 {
			t.Fatalf("Len() = %d, want 256 after full dedup", tier.Len())
		}
		for _, item := range tier.Items() {
			if item.Source != skills.OperatorTierSourceBoth {
				t.Fatalf("item %q source = %q, want both", item.Skill.Name, item.Source)
			}
		}
	})

	t.Run("257 boot-only items fails loud", func(t *testing.T) {
		_, err := sessionoverlay.ComposeOperatorTier(testBootEntries("boot", 257), nil)
		if !errors.Is(err, sessionoverlay.ErrOperatorTierBound) {
			t.Fatalf("ComposeOperatorTier = %v, want ErrOperatorTierBound", err)
		}
	})
}

func TestComposeOperatorTier_DeterministicOrderingAndHashes(t *testing.T) {
	t.Parallel()
	boot := testBootEntries("shared", 3)
	revision := append([]skills.Skill{testRevisionPack("zulu")}, testRevisionPacks("shared", 3)...)
	// Same inputs, different order.
	shuffledBoot := []bootpacks.Entry{boot[2], boot[0], boot[1]}
	shuffledRevision := []skills.Skill{revision[2], revision[3], revision[0], revision[1]}

	first, err := sessionoverlay.ComposeOperatorTier(boot, revision)
	if err != nil {
		t.Fatalf("ComposeOperatorTier: %v", err)
	}
	second, err := sessionoverlay.ComposeOperatorTier(shuffledBoot, shuffledRevision)
	if err != nil {
		t.Fatalf("ComposeOperatorTier shuffled: %v", err)
	}

	if !reflect.DeepEqual(operatorTierItemNames(first), operatorTierItemNames(second)) {
		t.Fatalf("order differs: %v vs %v", operatorTierItemNames(first), operatorTierItemNames(second))
	}
	// Deterministic canonical-name order.
	wantNames := []string{"shared-0000", "shared-0001", "shared-0002", "zulu"}
	if got := operatorTierItemNames(first); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("items = %v, want %v", got, wantNames)
	}
	// 3 of the 4 names exist in both sources.
	for _, name := range []string{"shared-0000", "shared-0001", "shared-0002"} {
		if source, _ := first.Source(name); source != skills.OperatorTierSourceBoth {
			t.Fatalf("Source(%s) = %q, want both", name, source)
		}
	}
	if source, _ := first.Source("zulu"); source != skills.OperatorTierSourceRevision {
		t.Fatalf("Source(zulu) = %q, want revision", source)
	}

	if first.BootPackSetHash() != second.BootPackSetHash() ||
		first.RevisionHash() != second.RevisionHash() ||
		first.CombinedHash() != second.CombinedHash() {
		t.Fatalf("hashes not deterministic: (%q,%q,%q) vs (%q,%q,%q)",
			first.BootPackSetHash(), first.RevisionHash(), first.CombinedHash(),
			second.BootPackSetHash(), second.RevisionHash(), second.CombinedHash())
	}
	if first.CombinedHash() == first.BootPackSetHash() || first.CombinedHash() == first.RevisionHash() {
		t.Fatal("combined hash collided with boot or revision envelope")
	}

	// The boot set hash must match the bootpacks index framing contract.
	wantBoot := referenceBootPackSetHash(t, boot)
	if first.BootPackSetHash() != wantBoot {
		t.Fatalf("boot set hash = %q, want index-framing reference %q", first.BootPackSetHash(), wantBoot)
	}
	if first.BootPackSetHash() != second.BootPackSetHash() {
		t.Fatal("boot set hash not stable across calls")
	}
}

func TestComposeOperatorTier_DeepCopyImmutability(t *testing.T) {
	t.Parallel()
	withExtra := testBootEntry("extra")
	withExtra.Skill.Extra = map[string]any{"nested": map[string]any{"list": []any{map[string]any{"value": "original"}}}}
	withExtra.SemanticHash = skills.CanonicalContentHash(withExtra.Skill)
	revisionExtra := testRevisionPack("extra")
	revisionExtra.Extra = map[string]any{"nested": map[string]any{"list": []any{map[string]any{"value": "original"}}}}
	revision := []skills.Skill{revisionExtra}

	tier, err := sessionoverlay.ComposeOperatorTier([]bootpacks.Entry{withExtra}, revision)
	if err != nil {
		t.Fatalf("ComposeOperatorTier: %v", err)
	}

	item, ok := tier.Get("extra")
	if !ok {
		t.Fatal("Get(extra) missing")
	}
	// Mutate the returned deep copy, then re-read: the tier must be unchanged.
	item.Skill.Extra["nested"].(map[string]any)["list"].([]any)[0].(map[string]any)["value"] = "mutated"
	item.Skill.Steps = append(item.Skill.Steps, "smuggled")
	again, _ := tier.Get("extra")
	if got := again.Skill.Extra["nested"].(map[string]any)["list"].([]any)[0].(map[string]any)["value"]; got != "original" {
		t.Fatalf("nested Extra alias leaked mutation: %v", got)
	}
	if fmt.Sprint(again.Skill.Steps) != "[do it verify it]" {
		t.Fatalf("Steps leaked mutation: %v", again.Skill.Steps)
	}

	// Mutating the INPUT slices after compose must not affect the tier.
	withExtra.Skill.Steps[0] = "mutated after compose"
	revision[0].Steps[0] = "mutated after compose"
	final, _ := tier.Get("extra")
	if final.Skill.Steps[0] != "do it" {
		t.Fatalf("input mutation reached the composed tier: %v", final.Skill.Steps)
	}

	// Items() must never share mutable backing arrays with a caller.
	listed := tier.Items()
	listed[0].Skill.Steps[0] = "mutated via Items"
	if got := tier.Items()[0].Skill.Steps[0]; got != "do it" {
		t.Fatalf("Items() alias leaked mutation: %v", got)
	}
}

func TestComposeOperatorTier_RealLoaderIndexHashEquality(t *testing.T) {
	// The boot_pack_set_hash computed by the composer over boot entries that
	// came out of the REAL eager loader must byte-match the index's own
	// BootPackSetHash — proving the shared strict composer and the boot
	// loader agree on the deterministic contract over real parsed files.
	root := t.TempDir()
	dir := filepath.Join(root, "foundation")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `---
name: workbench-foundation
title: Workbench Foundation
trigger: when asked about workbench
task_type: domain
tags: [ops, boot]
required_tools: [run_shell]
---
Boot skill.

## Steps
- do the thing
- verify the thing

## Preconditions
- booted

## Failure modes
- fails
`
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	imp, err := importer.New(importer.Deps{Store: store})
	if err != nil {
		t.Fatalf("importer.New: %v", err)
	}
	t.Cleanup(func() {
		_ = imp.Close(context.Background())
		_ = store.Close(context.Background())
	})
	ix, err := bootpacks.New(context.Background(), []config.BootAgentPackConfig{
		{TenantID: "acme", AgentID: "harbor-dev-agent", Directory: root, Include: []string{"foundation"}},
	}, bootpacks.Deps{Parser: imp, Catalog: staticTestCatalog{"run_shell": true}})
	if err != nil {
		t.Fatalf("bootpacks.New: %v", err)
	}
	entries, ok := ix.Lookup("acme", "harbor-dev-agent")
	if !ok || len(entries) != 1 {
		t.Fatalf("Lookup = %d entries, ok=%v", len(entries), ok)
	}
	indexHash, _ := ix.BootPackSetHash("acme", "harbor-dev-agent")
	tier, err := sessionoverlay.ComposeOperatorTier(entries, nil)
	if err != nil {
		t.Fatalf("ComposeOperatorTier over real loader entries: %v", err)
	}
	if tier.BootPackSetHash() != indexHash {
		t.Fatalf("composer boot set hash %q != index boot set hash %q", tier.BootPackSetHash(), indexHash)
	}
	// The real loader's frozen entry composes as source=boot with the
	// parser-fixed envelope.
	item, ok := tier.Get("workbench-foundation")
	if !ok || item.Source != skills.OperatorTierSourceBoot || item.Skill.Origin != skills.OriginPack {
		t.Fatalf("real loader item = (%+v, %v), want source=boot Origin=pack", item, ok)
	}
}

func TestComposeOperatorTier_ConcurrentReuse(t *testing.T) {
	baseline := runtime.NumGoroutine()
	boot := testBootEntries("shared", 32)
	revision := testRevisionPacks("shared", 32)
	shared, err := sessionoverlay.ComposeOperatorTier(boot, revision)
	if err != nil {
		t.Fatalf("ComposeOperatorTier: %v", err)
	}
	wantItems := shared.Items()
	wantHash := shared.CombinedHash()

	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// Concurrent compose with identical inputs must produce a
				// byte-identical tier.
				composed, err := sessionoverlay.ComposeOperatorTier(boot, revision)
				if err != nil {
					errs <- err
					return
				}
				if composed.CombinedHash() != wantHash || !reflect.DeepEqual(composed.Items(), wantItems) {
					errs <- fmt.Errorf("%d composed tier drifted", i)
				}
				return
			}
			// Concurrent reads against ONE shared tier must be byte-identical
			// and mutation-free.
			if !reflect.DeepEqual(shared.Items(), wantItems) {
				errs <- fmt.Errorf("%d shared tier Items drifted", i)
				return
			}
			if _, ok := shared.Source("shared-0000"); !ok {
				errs <- fmt.Errorf("%d shared tier Source missing", i)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertGoroutinesRestored(t, baseline)
}

// staticTestCatalog is the minimal tool-catalog fixture the real-loader test
// needs to satisfy the loader's required-tools gate.
type staticTestCatalog map[string]bool

func (c staticTestCatalog) Compatible(tool string) bool { return c[tool] }
