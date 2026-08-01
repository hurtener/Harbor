package react

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// extrasystemblocks_test.go — the render of the durable, operator-authored
// named prompt blocks (phase 222 / D-367).
//
// Three properties are load-bearing and each has a mutation that turns a
// test here red:
//
//  1. DECLARED ORDER is render order (mutation: add a sort, or carry the
//     blocks in a map).
//  2. Bodies are VERBATIM (mutation: route them through
//     escapeUntrustedSection, the way <user_instructions> is).
//  3. ABSENT is byte-identical to the pre-change output (mutation: emit an
//     empty wrapper, or join a stray separator).

func esbRC(blocks ...planner.NamedBlock) planner.RunContext {
	return planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{ExtraSystemBlocks: blocks}}
}

// TestRenderExtraSystemBlocks_DeclaredOrderWithLabels — the blocks render in
// slice order, each behind a plain-text `[name]` label.
func TestRenderExtraSystemBlocks_DeclaredOrderWithLabels(t *testing.T) {
	rc := esbRC(
		planner.NamedBlock{Name: "zulu", Body: "Z GUIDANCE"},
		planner.NamedBlock{Name: "alpha", Body: "A GUIDANCE"},
	)
	body := systemBody(t, rc, DefaultSystemPrompt)

	for _, want := range []string{"[zulu]", "Z GUIDANCE", "[alpha]", "A GUIDANCE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered body is missing %q. Body: %s", want, body)
		}
	}
	zi := strings.Index(body, "Z GUIDANCE")
	ai := strings.Index(body, "A GUIDANCE")
	if zi > ai {
		t.Fatalf("blocks rendered in the WRONG order (zulu@%d after alpha@%d) — the declared slice order IS the render order; something sorted them", zi, ai)
	}
	if !strings.Contains(body, "<additional_guidance>") {
		t.Fatalf("blocks did not compose into the existing <additional_guidance> section. Body: %s", body)
	}
}

// TestRenderExtraSystemBlocks_NameIsNeverATag — the name renders as a
// PLAIN-TEXT label, never as an XML tag. Minting a tag from config data would
// make the prompt taxonomy a function of caller input.
func TestRenderExtraSystemBlocks_NameIsNeverATag(t *testing.T) {
	rc := esbRC(planner.NamedBlock{Name: "compliance", Body: "B"})
	body := systemBody(t, rc, DefaultSystemPrompt)
	for _, forbidden := range []string{"<compliance", "</compliance>", `<block name=`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("a block name was emitted as an XML tag (%q) — attribution is a DATA-MODEL property, not a prompt-syntax one. Body: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, "[compliance]") {
		t.Fatalf("the plain-text label is missing. Body: %s", body)
	}
}

// TestRenderExtraSystemBlocks_BodyIsVerbatim — a body containing `<`, `>` and
// `&` is NOT entity-escaped. This is the property that distinguishes the
// operator-trusted additive position from the escaped `<user_instructions>`.
// Mutation: routing block bodies through escapeUntrustedSection turns this
// red.
func TestRenderExtraSystemBlocks_BodyIsVerbatim(t *testing.T) {
	const raw = `Emit <result> tags and use A & B when x > y.`
	rc := esbRC(planner.NamedBlock{Name: "fmt", Body: raw})
	body := systemBody(t, rc, DefaultSystemPrompt)
	if !strings.Contains(body, raw) {
		t.Fatalf("the block body was not rendered verbatim — it was escaped or mangled. Body: %s", body)
	}
	if strings.Contains(body, "&lt;result&gt;") || strings.Contains(body, "A &amp; B") {
		t.Fatalf("the block body was entity-escaped. Blocks are admin-written and operator-trusted; escaping them defends against a writer who can already replace the whole spine via PromptLayers.Base, and mangles operator text. Body: %s", body)
	}
}

func TestRenderExtraSystemBlocks_PreservesSurroundingWhitespace(t *testing.T) {
	const raw = "  first line\nsecond line\n  "
	rc := esbRC(planner.NamedBlock{Name: "fmt", Body: raw})
	body := systemBody(t, rc, DefaultSystemPrompt)
	if !strings.Contains(body, "[fmt]\n"+raw) {
		t.Fatalf("valid block whitespace was trimmed instead of rendered verbatim. Body: %q", body)
	}
	if strings.Contains(body, "[fmt]\nfirst line") {
		t.Fatalf("leading block whitespace was removed. Body: %q", body)
	}
}

// TestRenderExtraSystemBlocks_UserLayerStaysEscaped is the contrast half,
// asserted alongside so the asymmetry is documented by test: the SAME
// characters in the lower-tier user layer ARE escaped, because a claim-free
// session path can write that layer.
func TestRenderExtraSystemBlocks_UserLayerStaysEscaped(t *testing.T) {
	rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		UserPromptLayer:   sp(`please emit <result> tags`),
		ExtraSystemBlocks: []planner.NamedBlock{{Name: "fmt", Body: `emit <result> tags`}},
	}}
	body := systemBody(t, rc, DefaultSystemPrompt)
	if !strings.Contains(body, "&lt;result&gt;") {
		t.Fatalf("the USER layer is no longer escaped — the lower tier's structural boundary is gone. Body: %s", body)
	}
	if !strings.Contains(body, "emit <result> tags") {
		t.Fatalf("the BLOCK body is no longer verbatim. Body: %s", body)
	}
}

// TestRenderExtraSystemBlocks_SlotIsBelowBakedGuidanceAboveExtraInstructions
// pins the fixed slot in buildAdditionalGuidance: baked operator guidance →
// blocks → additive extra-instructions → per-turn repair guidance.
func TestRenderExtraSystemBlocks_SlotIsBelowBakedGuidanceAboveExtraInstructions(t *testing.T) {
	rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		ExtraInstructions: sp("TENANT_ADDITIVE"),
		ExtraSystemBlocks: []planner.NamedBlock{{Name: "blk", Body: "BLOCK_BODY"}},
	}}
	req := defaultBuilder{extraGuidance: "BAKED_OPERATOR"}.Build(rc, DefaultSystemPrompt)
	if len(req.Messages) == 0 || req.Messages[0].Content.Text == nil {
		t.Fatal("no system message")
	}
	body := *req.Messages[0].Content.Text

	baked := strings.Index(body, "BAKED_OPERATOR")
	block := strings.Index(body, "BLOCK_BODY")
	extra := strings.Index(body, "TENANT_ADDITIVE")
	if baked < 0 || block < 0 || extra < 0 {
		t.Fatalf("a contributor is missing (baked@%d block@%d extra@%d). Body: %s", baked, block, extra, body)
	}
	if baked >= block || block >= extra {
		t.Fatalf("slot order violated: want baked < blocks < extra-instructions, got baked@%d block@%d extra@%d. Body: %s", baked, block, extra, body)
	}
}

// TestRenderExtraSystemBlocks_SurvivesSystemPromptOverride — a session
// SystemPromptOverride replaces the base+user spine; the blocks are NOT
// suppressed with it, because buildAdditionalGuidance is reached on BOTH
// branches of the base request.
func TestRenderExtraSystemBlocks_SurvivesSystemPromptOverride(t *testing.T) {
	rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer:      sp("OPERATOR_BASE"),
		UserPromptLayer:      sp("USER_LAYER"),
		SystemPromptOverride: sp("SESSION_REPLACEMENT"),
		ExtraSystemBlocks:    []planner.NamedBlock{{Name: "blk", Body: "BLOCK_BODY"}},
	}}
	body := systemBody(t, rc, "ignored")
	if !strings.Contains(body, "SESSION_REPLACEMENT") {
		t.Fatalf("the session override did not take effect. Body: %s", body)
	}
	if strings.Contains(body, "OPERATOR_BASE") || strings.Contains(body, "USER_LAYER") {
		t.Fatalf("the base+user spine survived the session override. Body: %s", body)
	}
	if !strings.Contains(body, "BLOCK_BODY") || !strings.Contains(body, "[blk]") {
		t.Fatalf("the blocks were suppressed by the session override — they compose into the ADDITIVE position and must survive it. Body: %s", body)
	}
}

// TestRenderExtraSystemBlocks_AbsentIsByteIdentical is the byte-equality PIN:
// a nil block list, an empty list, and a list whose entries are all blank all
// produce EXACTLY the system content of a run that never had the field. No
// empty wrapper, no stray separator.
func TestRenderExtraSystemBlocks_AbsentIsByteIdentical(t *testing.T) {
	// The reference: an override bundle carrying everything EXCEPT blocks.
	ref := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
		BasePromptLayer:   sp("OPERATOR_BASE"),
		UserPromptLayer:   sp("USER_LAYER"),
		ExtraInstructions: sp("TENANT_ADDITIVE"),
	}}
	want := systemBody(t, ref, "ignored")

	cases := map[string][]planner.NamedBlock{
		"nil list":         nil,
		"empty list":       {},
		"all-blank bodies": {{Name: "a", Body: "   "}, {Name: "b", Body: ""}},
	}
	for name, blocks := range cases {
		t.Run(name, func(t *testing.T) {
			rc := planner.RunContext{Goal: "g", LLMOverrides: &planner.LLMOverrides{
				BasePromptLayer:   sp("OPERATOR_BASE"),
				UserPromptLayer:   sp("USER_LAYER"),
				ExtraInstructions: sp("TENANT_ADDITIVE"),
				ExtraSystemBlocks: blocks,
			}}
			got := systemBody(t, rc, "ignored")
			if got != want {
				t.Fatalf("an absent/empty block list perturbed the system content.\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// TestRenderExtraSystemBlocks_NoWrapperWhenEverythingElseIsEmpty — with no
// other contributor, an empty block list leaves the whole
// <additional_guidance> section omitted (optional sections are omitted, never
// emitted as empty tag pairs).
func TestRenderExtraSystemBlocks_NoWrapperWhenEverythingElseIsEmpty(t *testing.T) {
	body := systemBody(t, esbRC(), DefaultSystemPrompt)
	if strings.Contains(body, "<additional_guidance>") {
		t.Fatalf("an empty block list emitted an empty <additional_guidance> wrapper. Body: %s", body)
	}
}

// TestRenderExtraSystemBlocks_Deterministic — the SAME payload rendered 1000
// times is byte-identical. Cheap, and it is the guard that catches a map
// sneaking onto the render path in a later refactor.
func TestRenderExtraSystemBlocks_Deterministic(t *testing.T) {
	blocks := make([]planner.NamedBlock, 0, 16)
	for _, n := range []string{"m", "d", "z", "a", "q", "b", "y", "c", "x", "e", "w", "f", "v", "g", "u", "h"} {
		blocks = append(blocks, planner.NamedBlock{Name: n, Body: "body-" + n})
	}
	rc := esbRC(blocks...)
	first := systemBody(t, rc, DefaultSystemPrompt)
	for i := range 1000 {
		if got := systemBody(t, rc, DefaultSystemPrompt); got != first {
			t.Fatalf("render %d differed from the first — the block path is not deterministic (a map on the path?)", i)
		}
	}
}
