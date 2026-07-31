package react

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/hurtener/Harbor/internal/planner"
)

// maxToolNameBytes is the model-visible function-name budget.
//
// The PROVIDER ceiling is 64: OpenAI-compatible native tool-calling
// rejects a name that is not ^[a-zA-Z0-9_-]{1,64}$ with a 400. Harbor
// spends less than the ceiling on purpose, because a tool name is not
// paid once — it is paid on EVERY turn, TWICE per tool (the `req.Tools[]`
// declaration and the `<available_tools>` prompt section), plus once more
// each time the model writes the name to invoke it.
//
// Catalog keys are `<sourceID>_<tool>` and the source id is repeated on
// every one of its tools, so a long id multiplies straight into the
// per-turn prompt. The catalog key has to stay long and globally unique —
// the tool catalog is flat and process-global — but the MODEL-VISIBLE name
// does not: resolveDeclaredToolName maps whatever the model returns back
// to the real catalog key, and that indirection is what lets the declared
// form be cheaper than the key.
//
// 44 is chosen against the retained-tail width it implies:
// maxToolNameBytes - toolNameDigestBytes - 1 = 35 bytes of retained tail.
// It is the SMALLEST budget at which every verb in a representative
// GitHub-MCP tool set still renders byte-exact — the longest,
// `get_pull_request_review_comments`, is 32 — so shortening never costs
// the model the ability to read what a tool DOES. Tightening to 40 already
// clips that verb; loosening to 48 buys no comprehension and gives back
// roughly half the saving.
//
// Names already within budget are returned unchanged, so a well-named
// catalog pays nothing for this.
const maxToolNameBytes = 44

// toolNameDigestBytes is the width of the hex digest appended when a name
// has to be shortened. 32 bits of digest over a catalog of a few hundred
// over-long tools leaves the birthday collision probability in the
// 1e-6 range, and the retained tail has to match as well for the shortened
// forms to actually collide.
const toolNameDigestBytes = 8

// sanitizeToolName maps a catalog tool name to the model-visible form
// native tool-calling requires. Harbor's dotted naming convention
// ("clock.now", "inventory.check") cannot be sent verbatim, so the
// transform replaces every disallowed character with '_' and keeps the
// result inside [maxToolNameBytes] (tool names are ASCII in practice).
//
// # Shortening keeps the tail, not the head
//
// Tool names injected by a source are joined as `<sourceID>_<tool>`, so
// the DISCRIMINATING and semantically useful part of an over-long name is
// its TAIL — every tool of one source shares the head. A plain head
// truncation therefore maps every tool of a source whose id is long enough
// onto ONE model-visible name, and the caller's dedup then collapses the
// whole source down to a single declaration.
//
// So an over-budget name keeps its last bytes and gives up the rest to an
// 8-hex-character digest of the FULL sanitized string:
//
//	<...retained tail...>_<8-hex digest>   (exactly maxToolNameBytes)
//
// The digest restores the discrimination the discarded head carried, and
// the retained tail keeps the verb model-visible.
//
// # It stays a pure function
//
// The transform is deterministic and has no inverse stored anywhere:
// resolveDeclaredToolName recovers the real catalog name by recomputing
// this transform over each catalog tool. Nothing is keyed on catalog
// composition or ordering, so a name's provider-safe form does not shift
// when the catalog grows mid-run (discovered tools are appended per turn)
// and a replayed historical tool_call still matches the current
// declaration. Already-valid names within budget (the reserved planner
// controls, short source-joined names) are returned unchanged, so they
// round-trip by exact match.
//
// The purity is load-bearing, not incidental: it is what rules out the
// obvious alternative of assigning each source a short ALIAS. An alias
// derived from catalog composition or ordering would shift the moment the
// catalog grew — discovered tools are appended per turn — and a replayed
// historical tool_call would then no longer match any current declaration.
//
// # Residual collisions are still possible
//
// The disallowed-character mapping is many-to-one — `clock.now` and
// `clock/now` both sanitize to `clock_now` — so distinct catalog names can
// still land on one provider-safe form regardless of length. That is a
// catalog-naming problem the transform cannot fix; buildToolDeclarations
// announces it rather than dropping a declaration silently.
func sanitizeToolName(name string) string {
	return sanitizeToolNameTo(name, maxToolNameBytes)
}

// sanitizeToolNameTo is sanitizeToolName against an explicit byte budget.
// The budget is a constant in production; taking it as a parameter is what
// lets the cost measurement sweep it without mutating package state.
func sanitizeToolNameTo(name string, budget int) string {
	clean := true
	for _, r := range name {
		if !isToolNameRune(r) {
			clean = false
			break
		}
	}
	if clean && len(name) <= budget {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > budget {
		s = shortenToolName(s, budget)
	}
	return s
}

// shortenToolName reduces an over-budget provider-safe name to exactly
// `budget` bytes by retaining its tail and appending a digest of the whole
// string. See sanitizeToolName's godoc for why the tail is the half worth
// keeping.
func shortenToolName(s string, budget int) string {
	sum := sha256.Sum256([]byte(s))
	digest := hex.EncodeToString(sum[:])[:toolNameDigestBytes]
	// One separator byte between the retained tail and the digest.
	keep := budget - toolNameDigestBytes - 1
	return s[len(s)-keep:] + "_" + digest
}

func isToolNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	default:
		return false
	}
}

// resolveDeclaredToolName maps a provider-returned tool-call name back to
// the real catalog tool name. Declarations are sent to the LLM under their
// sanitized names, so a returned name may differ from the catalog name
// (e.g. "inventory_check" → "inventory.check"). An exact catalog match
// wins (already-valid names + discovered MCP tools); otherwise the first
// catalog tool whose sanitized name matches is returned. An unmatched name
// is returned verbatim — the executor then fails loud on an unknown tool,
// exactly as before this sanitization existed.
func resolveDeclaredToolName(rc *planner.RunContext, name string) string {
	if rc == nil || rc.Catalog == nil {
		return name
	}
	if _, ok := rc.Catalog.Resolve(name); ok {
		return name
	}
	for _, t := range rc.Catalog.List() {
		if t.Name != "" && sanitizeToolName(t.Name) == name {
			return t.Name
		}
	}
	return name
}
