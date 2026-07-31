package agentcfg_test

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
)

// extrasystemblocks_test.go — the ORDER-IS-SEMANTIC guards for the additive
// prompt-blocks section (phase 222 / D-367).
//
// The whole point of these tests is an ASYMMETRY. `Skills.Names` is
// sortDedup'd and `OAuthProviders` is sorted by name, because for those a
// re-ordering of a SET must not perturb the content hash. Blocks are the
// opposite: the declared order IS the render order, so a re-ordering MUST
// change the hash and MUST show up in the diff. The two behaviours are
// asserted side by side below so the asymmetry is documented by test rather
// than only by prose — and so the mutation "someone made blocks consistent
// with skills by adding a sort" turns them red.

func blocks(pairs ...string) *agentcfg.ExtraSystemBlocks {
	if len(pairs)%2 != 0 {
		panic("blocks() takes name/body pairs")
	}
	out := make([]agentcfg.NamedBlock, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, agentcfg.NamedBlock{Name: pairs[i], Body: pairs[i+1]})
	}
	return &agentcfg.ExtraSystemBlocks{Blocks: out}
}

// TestNormalizePayload_ExtraSystemBlocks_PreservesDeclaredOrder is the
// must-not-sort guard. Mutation: adding a sort to normalizeNamedBlocks turns
// this red.
func TestNormalizePayload_ExtraSystemBlocks_PreservesDeclaredOrder(t *testing.T) {
	// Deliberately reverse-alphabetical: a sort would reorder it.
	in := agentcfg.ConfigPayload{ExtraSystemBlocks: blocks(
		"zulu", "z body",
		"alpha", "a body",
		"mike", "m body",
	)}
	got := agentcfg.NormalizePayload(in).ExtraSystemBlockList()
	want := []string{"zulu", "alpha", "mike"}
	if len(got) != len(want) {
		t.Fatalf("normalized %d blocks, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("normalized order = %v, want %v — the normalizer SORTED the blocks; order is the RENDER order here (unlike Skills.Names / OAuthProviders)",
				names(got), want)
		}
	}
}

// TestContentHash_ExtraSystemBlocks_OrderIsSemantic pins the asymmetry
// itself: re-ordering BLOCKS changes the hash, re-ordering SKILLS does not.
// Both halves are asserted in one test so a future contributor cannot
// "fix the inconsistency" without reading why it exists.
func TestContentHash_ExtraSystemBlocks_OrderIsSemantic(t *testing.T) {
	forward, err := agentcfg.ContentHash(agentcfg.ConfigPayload{
		ExtraSystemBlocks: blocks("alpha", "a", "beta", "b"),
	})
	if err != nil {
		t.Fatalf("hash forward: %v", err)
	}
	reversed, err := agentcfg.ContentHash(agentcfg.ConfigPayload{
		ExtraSystemBlocks: blocks("beta", "b", "alpha", "a"),
	})
	if err != nil {
		t.Fatalf("hash reversed: %v", err)
	}
	if forward == reversed {
		t.Fatalf("re-ordering the blocks produced the SAME content hash %q — the canonical form is sorting them, so a re-order would be an invisible no-op revision that silently re-orders the operator's prompt", forward)
	}

	// The sibling behaviour, asserted alongside: skills order is NOT
	// semantic, and that must stay true.
	sf, err := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b"}}})
	if err != nil {
		t.Fatalf("hash skills forward: %v", err)
	}
	sr, err := agentcfg.ContentHash(agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"b", "a"}}})
	if err != nil {
		t.Fatalf("hash skills reversed: %v", err)
	}
	if sf != sr {
		t.Fatalf("re-ordering the SKILLS changed the content hash — the sibling section's order is not semantic and must stay canonicalised")
	}
}

// TestNormalizePayload_ExtraSystemBlocks_DropsPhantomsAndDuplicates — an
// empty name, an empty/whitespace body and a duplicate name never reach the
// canonical form (the write door refuses a duplicate outright; this is the
// defensive canonicalisation behind the direct set_revision door). The FIRST
// occurrence of a name wins AND HOLDS ITS POSITION.
func TestNormalizePayload_ExtraSystemBlocks_DropsPhantomsAndDuplicates(t *testing.T) {
	in := agentcfg.ConfigPayload{ExtraSystemBlocks: blocks(
		"keep", "body",
		"", "orphan body",
		"empty-body", "   ",
		"keep", "a later duplicate",
	)}
	got := agentcfg.NormalizePayload(in).ExtraSystemBlockList()
	if len(got) != 1 || got[0].Name != "keep" || got[0].Body != "body" {
		t.Fatalf("canonical form = %+v, want exactly one block {keep, body}", got)
	}
}

// TestNormalizePayload_ExtraSystemBlocks_AllEmptySectionDropsOut — a present
// but all-empty section normalises out entirely, matching the prompt-layer
// section's rule, so it never perturbs an existing revision's content hash.
func TestNormalizePayload_ExtraSystemBlocks_AllEmptySectionDropsOut(t *testing.T) {
	for name, in := range map[string]agentcfg.ConfigPayload{
		"empty list": {ExtraSystemBlocks: &agentcfg.ExtraSystemBlocks{}},
		"all-blank":  {ExtraSystemBlocks: blocks("", "", "  ", "  ")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := agentcfg.NormalizePayload(in); got.ExtraSystemBlocks != nil {
				t.Fatalf("all-empty section survived normalization: %+v", got.ExtraSystemBlocks)
			}
		})
	}
}

// TestContentHash_ExtraSystemBlocks_AbsentIsByteIdentical — a revision with
// no blocks section hashes EXACTLY as it did before the section existed
// (asserted against a payload carrying an all-empty section, which must
// normalise to the same canonical form as nil).
func TestContentHash_ExtraSystemBlocks_AbsentIsByteIdentical(t *testing.T) {
	base := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"s"}}}
	withEmpty := base
	withEmpty.ExtraSystemBlocks = &agentcfg.ExtraSystemBlocks{}

	a, err := agentcfg.ContentHash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	b, err := agentcfg.ContentHash(withEmpty)
	if err != nil {
		t.Fatalf("hash with-empty: %v", err)
	}
	if a != b {
		t.Fatalf("an all-empty blocks section perturbed the content hash (%q != %q) — an existing revision would spuriously re-hash", a, b)
	}
}

// TestDiffExtraSystemBlocks covers every arm, including the same-set,
// different-order case that has no analogue on the sorted sibling sections.
func TestDiffExtraSystemBlocks(t *testing.T) {
	cases := []struct {
		name string
		from *agentcfg.ExtraSystemBlocks
		to   *agentcfg.ExtraSystemBlocks
		want agentcfg.ExtraSystemBlocksDiff
	}{
		{
			name: "added",
			from: blocks("a", "1"),
			to:   blocks("a", "1", "b", "2"),
			want: agentcfg.ExtraSystemBlocksDiff{Added: []string{"b"}},
		},
		{
			name: "removed",
			from: blocks("a", "1", "b", "2"),
			to:   blocks("a", "1"),
			want: agentcfg.ExtraSystemBlocksDiff{Removed: []string{"b"}},
		},
		{
			name: "body changed",
			from: blocks("a", "1"),
			to:   blocks("a", "2"),
			want: agentcfg.ExtraSystemBlocksDiff{Changed: []string{"a"}},
		},
		{
			name: "same set, different order",
			from: blocks("a", "1", "b", "2"),
			to:   blocks("b", "2", "a", "1"),
			want: agentcfg.ExtraSystemBlocksDiff{Reordered: true},
		},
		{
			name: "identical",
			from: blocks("a", "1", "b", "2"),
			to:   blocks("a", "1", "b", "2"),
			want: agentcfg.ExtraSystemBlocksDiff{},
		},
		{
			name: "absent to present",
			from: nil,
			to:   blocks("a", "1"),
			want: agentcfg.ExtraSystemBlocksDiff{Added: []string{"a"}},
		},
		{
			// An add necessarily shifts positions; the reorder flag must NOT
			// also fire, or every membership change would look like a
			// re-ordering too.
			name: "prepend does not report a reorder",
			from: blocks("b", "2"),
			to:   blocks("a", "1", "b", "2"),
			want: agentcfg.ExtraSystemBlocksDiff{Added: []string{"a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentcfg.DiffExtraSystemBlocks(
				agentcfg.ConfigPayload{ExtraSystemBlocks: tc.from},
				agentcfg.ConfigPayload{ExtraSystemBlocks: tc.to},
			)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diff = %+v, want %+v", got, tc.want)
			}
			wantChanged := len(tc.want.Added) > 0 || len(tc.want.Removed) > 0 ||
				len(tc.want.Changed) > 0 || tc.want.Reordered
			if got.HasChanges() != wantChanged {
				t.Fatalf("HasChanges() = %v, want %v", got.HasChanges(), wantChanged)
			}
		})
	}
}

func names(in []agentcfg.NamedBlock) []string {
	out := make([]string, 0, len(in))
	for _, b := range in {
		out = append(out, b.Name)
	}
	return out
}
