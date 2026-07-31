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
// this transform over each declaration candidate. Nothing is keyed on
// catalog composition or ordering, so a name's provider-safe form does not
// shift when the catalog grows mid-run (discovered tools are appended per
// turn) and a replayed historical tool_call still matches the current
// declaration. Already-valid names within budget (the reserved planner
// controls, short source-joined names) are returned unchanged, so the same
// recomputation matches them by identity — no separate exact-match branch
// exists, and none may be re-added: an exact match that outranks the
// recomputation dispatches a DROPPED collider (see
// resolveDeclaredToolName).
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
//
// A dropped collider is dropped TOTALLY: it is unreachable for the turn
// even when its own catalog name IS the provider-safe string (an MCP
// server named `clock` exposing `now` produces the catalog key `clock_now`,
// which collides with the built-in `clock.now`). The one declaration under
// that name belongs to whichever tool claimed it, and that is the tool the
// name executes. Partial reachability would be worse than the drop: the
// model would read one tool's schema and invoke another's code.
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

// minDigestBudget is the smallest budget at which the full
// `<tail>_<digest>` shape fits — one separator plus the whole digest, with
// a zero-width tail at the boundary. Below it only a digest PREFIX fits.
//
// Production never reaches the sub-budget arm ([maxToolNameBytes] is 44),
// but the budget is a PARAMETER of [sanitizeToolNameTo], and a parameter
// nobody validated is not the "impossible by construction" carve-out a
// panic needs (CLAUDE.md §5). The arm makes the function TOTAL instead:
// the retained-tail width used to go negative below this boundary and
// panic on a slice bound.
const minDigestBudget = toolNameDigestBytes + 1

// shortenToolName reduces an over-budget provider-safe name to exactly
// `budget` bytes by retaining its tail and appending a digest of the whole
// string. See sanitizeToolName's godoc for why the tail is the half worth
// keeping.
//
// At or above [minDigestBudget] the result is `<retained tail>_<8-hex
// digest>`. Below it no tail survives and the result is a PREFIX of the
// digest — still deterministic, still a pure function of the input, and
// still discriminating for as long as the width allows. A non-positive
// budget cannot represent any name at all and yields the empty string.
func shortenToolName(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	digest := hex.EncodeToString(sum[:])[:toolNameDigestBytes]
	if budget < minDigestBudget {
		return digest[:budget]
	}
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
// (e.g. "inventory_check" → "inventory.check").
//
// # It re-derives the FORWARD transform, in the declaration's own precedence
//
// Resolution walks the same candidates buildToolDeclarations walks, in the
// same order, and returns the first whose sanitized name equals the
// returned name: reserved planner controls, then the always-loaded catalog
// view, then the per-run discovered tools. Whichever candidate KEEPS a
// declaration is therefore the one that declaration's name dispatches to,
// by construction.
//
// An exact catalog match is deliberately NOT a branch of its own. It used
// to be the first one, and it was a silent mis-dispatch: a residual
// collision (the disallowed-character mapping is many-to-one, so
// `clock.now` and `clock_now` both declare as `clock_now`) drops one of the
// two, and when the DROPPED tool's catalog name happened to be the
// provider-safe string itself, the exact match won and executed the dropped
// tool — under the declared tool's description and args schema, with no
// error and no diagnostic. The model read tool A and tool B ran. Reaching
// that shape needs nothing exotic: built-in names are dotted (`clock.now`)
// and injected tool-source keys are `<sourceID>_<tool>`, so an MCP server
// named `clock` exposing `now` collides with the built-in. The exact branch
// is also redundant — a catalog name that is already provider-safe and
// in-budget sanitizes to ITSELF, so the scan matches it, and anything else
// falls through to the verbatim passthrough that returns the same value the
// exact branch did.
//
// Scanning rather than consulting a recorded declared→catalog map is the
// deliberate choice: a map is per-projection state that would have to be
// carried on the RunContext (the planner artifact may not hold it — the
// concurrent-reuse contract), and any path that reached the projector
// WITHOUT it — a resumed run, a replayed trajectory, a caller that projects
// a response it did not declare for — would fall back to exactly the branch
// this defect lived in, silently. The scan is O(catalog) per tool call
// against an LLM round trip; the forward path already pays the same cost
// twice per turn.
//
// The precedence is stable under the one way the catalog moves mid-run:
// discovered tools are APPENDED, so a later arrival can never displace an
// earlier claimant of a provider-safe name.
//
// An unmatched name is returned verbatim — the executor then fails loud on
// an unknown tool, exactly as before this sanitization existed.
func resolveDeclaredToolName(rc *planner.RunContext, name string) string {
	if rc == nil || rc.Catalog == nil {
		return name
	}
	// Reserved planner controls are declared first and always win their
	// name, so an operator tool that sanitizes onto one was DROPPED and
	// must not be reachable through it. The projector's reserved-name
	// switch already intercepts these before resolution; the guard is here
	// because "unreachable by construction" is the reasoning this function
	// was wrong about once.
	if isReservedControlName(name) {
		return name
	}
	for _, t := range rc.Catalog.List() {
		if t.Name != "" && sanitizeToolName(t.Name) == name {
			return t.Name
		}
	}
	// Discovered tools reach req.Tools through their own arm and are absent
	// from the always-loaded List() view, so they need their own scan —
	// without it every discovered tool whose name is dotted or over-budget
	// is declared to the model and then undispatchable.
	for _, d := range rc.DiscoveredTools {
		if d == "" || sanitizeToolName(d) != name {
			continue
		}
		if _, ok := rc.Catalog.Resolve(d); ok {
			return d
		}
	}
	return name
}

// isReservedControlName reports whether name is one of the planner's
// reserved control meta-tools. These are never catalog tools: they are
// declared by the planner itself and intercepted by name in the projector.
func isReservedControlName(name string) bool {
	switch name {
	case FinishToolName, SpawnTaskToolName, AwaitTaskToolName, TaskStatusToolName,
		CancelTaskToolName, SteerTaskToolName, PauseTaskToolName, ResumeTaskToolName:
		return true
	default:
		return false
	}
}
