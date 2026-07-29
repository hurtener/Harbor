# Phase 213 — heavy-content threshold split by purpose

## Summary

`config.DefaultHeavyOutputThresholdBytes` (`internal/config/config.go:2041`) is 32 KiB and is referenced from eight sites across seven files, and reaches a further nine production wiring sites through the operator field `artifacts.heavy_output_threshold_bytes` it seeds. Those consumers answer materially different questions with one number: how many bytes may enter a model's context window, how many bytes a browser page may receive inline, how many bytes a terminal scrollback may absorb, and at what size a search index stops pretending a 256-rune snippet represents a record. This phase raises the LLM-context arm to 128 KiB — so an ordinary 40 KiB JSON tool result stops costing a second tool call to read back what the agent just produced — and PINS every non-LLM arm at its current 32 KiB behind its own named constant with its own godoc rationale. The per-consumer decision matrix, not the new number, is this phase's deliverable.

## RFC anchor

- RFC §6.5 — the LLM client layer and the context-window safety net whose comparison value this phase moves.
- RFC §6.10 — Artifacts: mandatory routing above a threshold, which is the property being resized rather than relaxed.
- RFC §6.13 — the typed event bus, whose `llm.context_leak` and heavy-offload events carry the threshold as reported data.

## Briefs informing this phase

- brief 05
- brief 07
- brief 15

## Brief findings incorporated

- **brief 05, open question 1 (line 359):** *"Heavy-output threshold. What size triggers mandatory ArtifactStore routing? Default proposed: 32 KB. Reasonable range 16 KB – 128 KB. Lower = more router overhead and smaller LLM context; higher = more context bloat risk. Should the threshold be a runtime config or per-tool overridable?"* 128 KiB is the TOP of the brief's own stated range, not a value outside it. This phase is therefore an ANSWER to a question the brief left open, and the answer is recorded at the range's ceiling with the round-trip cost as the reason — not a departure requiring justification.
- **brief 05 §1 (line 29 / line 301):** *"A heavy output above the threshold (default: 32KB, configurable) routes through the ArtifactStore — never inline. This is a runtime-level invariant, not a per-tool opt-in flag."* Binding on the shape of the split: the pinned constants are per-CONSUMER (one number per question), never per-TOOL (an opt-in an author could set). No tool, agent config or Protocol caller gains a way to move any of these values.
- **brief 07 §4 (line 189):** the `ObservationRenderer` *"performs LLM-facing redaction (heavy outputs replaced with artifact refs)."* This is the offload counterpart for the `RoleTool`-text leak class, and it is why that class's guard may move with the offload boundary — the pairing is dispatch↔LLM-edge, which the risk section states precisely rather than attributing it to `materializeRequest` (which does not walk text at all).
- **brief 06 (line 13):** *"it guarantees Console, third-party consoles, and `harbor dev` see exactly the same data shape that production observability sees. There is no privileged 'internal' view."* Binding on the Console-facing arms: `memory.get` / `memory.list` / `pause.list` / the flow catalog / the three `mcp.apps.*` reads all select between an inline payload and an artifact reference at this threshold. Moving it silently reshapes those replies for third-party Protocol clients as well as Harbor's own Console, which is the §8 reason those arms pin.
- **brief 15 §3 (lines 79, 93):** the deferred-loading strategies are ranked partly on round trips — B2 costs *"two LLM calls per discovery cycle"* while B3 is chosen as the *"planner-side tag prefilter (no meta-tool, no LLM round-trip)."* The transferable finding is the cost model, not the mechanism: a runtime decision that converts one provider call into three is expensive in exactly the currency the brief prices catalog strategies in. A 40 KiB tool result promoted to a stub costs call → stub → fetch.

## Findings I'm departing from (if any)

**One departure, from `internal/config/config.go:2035-2040`'s own single-sourcing rule** — recorded because the first draft of this plan copied that comment instead of grepping and got the consumer count wrong as a result.

The comment states: *"the dispatch executor's safety floor (`internal/runtime/dispatch`), the LLM-edge snapshot default (`llm.DefaultHeavyOutputThreshold`), and the search preview bound (`search.HeavyPreviewThreshold`) all reference this constant … No other literal copy of the value is allowed."*

That enumeration is STALE. `grep -rn "DefaultHeavyOutputThresholdBytes" --include="*.go" .` returns five further referencing sites the comment does not name: `internal/config/loader.go:391`, `internal/tui/renderers/registry.go:138` and `:152`, `internal/mcpconsole/apps.go:173`, and `internal/mcpconsole/toolcontext.go:94`. A comment that enumerates its own consumers rots the moment a sixth lands and there is no gate that notices.

The departure is from the rule's literal form, not its intent. "No other literal copy of the value is allowed" is correct for copies answering the SAME question; it is wrong as a prohibition on a different question that currently happens to have the same numeric answer. This phase restates the rule as: **no second constant may answer the same question; a different question takes its own named constant whose godoc states the question.** The amended comment stops enumerating consumers by name (the thing that rotted) and instead names the QUESTION the constant answers and points at D-358 for the matrix.

## Goals

- The LLM-context arm — the dispatch promote-to-stub boundary, the LLM-edge `findContextLeak` comparison, the auto-materialization boundary, and the trajectory compaction payload budget that derives from it — moves to 128 KiB as one value from one source.
- Every non-LLM consumer keeps its CURRENT 32 KiB behaviour, held by its own named constant whose godoc states which question it answers and why that question's answer is not the LLM-context answer.
- Every Protocol-visible inline-versus-reference selection point is enumerated with an explicit follows-the-raise / pins decision and a reason, so no wire reply silently changes shape (§8).
- The move stays operator-visible and operator-reversible: an explicit `artifacts.heavy_output_threshold_bytes` still wins for the LLM-context arm (§10), and the three shipped example configs stop pinning the old default.
- The stale single-sourcing comment stops being an enumeration a future reader can trust and stops being the source it was in this plan's first draft.

## Non-goals

- **Raising the `artifact_fetch` read ceilings.** `fetch_default_max_bytes` (64 KiB) and `fetch_hard_max_bytes` (1 MiB) are untouched. They bound a DELIBERATE read-back and were made operator policy in phase 209; the findings confirm they do not derive from this constant (`internal/config/config.go:2044+`). A read ceiling below the offload boundary is not incoherent: a model that gets a 128 KiB result inline never needs to fetch it, and one that gets a stub for a larger result pages it.
- **A separate ceiling for pass-by-reference egress.** Substituted bytes never enter a model's context, so the budget is network and memory rather than tokens. Phase 214 owns that knob and it is deliberately not derived from this one.
- **Making the Console-facing inline bound operator-configurable.** No `artifacts.console_inline_payload_bytes` field is added. The reopening condition is stated in Risks; adding a knob for a hypothetical operator is the ceremony §13 names.
- **Any change to which fields `findContextLeak` walks**, or to D-241's conversation-text exemption. Phase 210 widened the walk onto `Messages[].ToolCalls[].Args`; this phase changes the number it compares against and corrects one godoc claim about that field, nothing else.
- **Building an offloader for `ToolCalls[].Args`.** Named as the successor question in Risks, with the exact reason it is not this phase's work.
- **Per-tool threshold overrides.** brief 05 §1 rules the routing a runtime-level invariant rather than a per-tool opt-in; `docs/glossary.md:542`'s claim that "per-tool overrides land at Phase 26 via the tool catalog" is stale and is corrected in this phase's glossary edit rather than implemented.

## The consumer decision matrix

This is the phase's core deliverable. Every row was established by grep against the tree at `plan/v124-wave`, not from any comment.

### A. Direct references to `config.DefaultHeavyOutputThresholdBytes`

Eight referencing sites in seven files (`grep -rn "DefaultHeavyOutputThresholdBytes" --include="*.go" .`, excluding the declaration at `internal/config/config.go:2041` and its godoc).

| # | Site | What it decides | Decision |
|---|------|-----------------|----------|
| A1 | `internal/config/loader.go:391` | Seeds `ArtifactsConfig.HeavyOutputThresholdBytes` in `Defaults()`; the value an unset key resolves to | **RAISE** — it IS the LLM-context offload default |
| A2 | `internal/runtime/dispatch/dispatch.go:152` (`defaultHeavyThreshold`), fallback at `:240-241`, compared at `:1333` (`size < e.heavyThreshold` → raw, else `PutText` + summary) | Whether a tool result reaches the planner inline or as an artifact-backed summary | **RAISE** — the round-trip this phase exists to remove |
| A3 | `internal/llm/registry.go:228` (`DefaultHeavyOutputThreshold`) → `safety.go:98` `findContextLeak` and `materialize.go:44` `threshold` | The LLM-edge leak guard and the auto-materialization boundary | **RAISE** — qualified per leak class in Risks |
| A4 | `internal/llm/summarizer/trajectory.go:77` — `defaultTrajectoryPayloadBudget = llm.DefaultHeavyOutputThreshold - trajectoryPayloadHeadroom` (transitive through A3) | The compaction payload's aggregate byte budget | **RAISE, derived** — no edit; it must stay below the guard or compaction fails the run it exists to save |
| A5 | `internal/search/search.go:70` (`HeavyPreviewThreshold`), compared at `:423` | Whether a `SearchResultRow` ships a 256-rune preview or an empty preview plus a `Ref` | **PIN 32 KiB**, own literal + godoc |
| A6 | `internal/tui/renderers/registry.go:138` (string) and `:152` (byte slice) | Whether a normalised payload field renders or folds to `"[HEAVY CONTENT OMITTED: artifact reference required]"` | **PIN 32 KiB**, own literal + godoc |
| A7 | `internal/mcpconsole/apps.go:173` (`defaultHeavyThreshold`), used at `:229` (`ReadResource`) and `:301` (`CallTool`) | Whether an MCP resource / App tool result reaches the browser inline or as an artifact row | **PIN 32 KiB**, own constant + godoc |
| A8 | `internal/mcpconsole/toolcontext.go:94`, used at `:182` | Whether a captured App tool context's input/result half rides inline in the StateStore envelope or offloads | **PIN 32 KiB**, shares A7's constant (same package, same question) |

A5, A6 and A7/A8 are three distinct questions, so they take three constants — not one shared "32 KiB" that would re-create the coupling this phase is dissolving. A7 and A8 are one question in one package and share one constant.

### B. Production consumers of the operator field `cfg.Artifacts.HeavyOutputThresholdBytes`

Nine production wiring sites (`grep -rn "HeavyOutputThresholdBytes" --include="*.go" . | grep -v _test`). Every one of B4–B9 selects between an inline payload and an artifact reference on a Protocol reply, which makes the choice a §8 wire-behaviour question rather than a tuning question.

| # | Wiring site | Reached surface | Decision + reason |
|---|-------------|-----------------|-------------------|
| B1 | `internal/llm/from_config.go:58` → `LLMSnapshot.HeavyOutputThreshold` | `materializeRequest` + `findContextLeak` | **FOLLOWS** — this IS the LLM edge (A3's operator path) |
| B2 | `internal/runtime/assemble/assemble.go:1050` → `dispatch.WithHeavyThreshold` | `projectForLLM` (`dispatch.go:1333`) | **FOLLOWS** — A2's operator path |
| B3 | `internal/runtime/assemble/assemble.go:1100` → `llmsummarizer.WithTrajectoryHeavyOutputThreshold` (`trajectory.go:154-165`, budget = `threshold − 4096`) | Compaction payload budget | **FOLLOWS, derived** — A4's operator path |
| B4 | `internal/runtime/serve/mux.go:355` → `annotate.Deps.HeavyThresholdBytes` (`annotate.go:107` → `metrics.go:148` → `types/tools.go:388`) | `tools.content_stats.heavy_threshold_bytes` + `heavy_count` | **FOLLOWS** — pure REPORTING. It echoes the offload threshold and counts offload events against it; reporting the pinned number here would make the field lie. The wire SHAPE is unchanged; only the reported integer moves |
| B5 | `internal/runtime/serve/mux.go:319` → `transports.WithPauseList(coord, store, threshold)` (`transports.go:435-439`) → `stream.NewPauseListHandler` (`pause_list_handler.go:125`) | `pause.list` — inline pause payload vs artifact ref | **PINS** — a Console/TUI read. The consumer is a browser, not a model; there is no round trip to save (the Console fetches the ref as cheaply as the inline bytes), and raising it silently reshapes the reply for every Protocol client in the 32–128 KiB band |
| B6 | Same `muxConfig.heavyThreshold` as B5, consumed at `transports.go:929-931` → `stream.NewMemoryHandler(store, artStore, threshold)` → `memory_handler.go:366` | `memory.get` — `GetDeps.HeavyThreshold`; `get.go:38-40` guarantees EXACTLY ONE of `Value` / `ValueArtifact` is populated | **PINS** — same reason as B5, and the response is a documented sum type whose selected arm would flip |
| B7 | Same value, `memory_handler.go:402` | `memory.list` — `ListDeps.HeavyThreshold`, the per-row `HeavyContent` flag | **PINS** — and it MUST pin because `list.go:41-44` states normatively that *"`memory.list` and `memory.get` MUST agree, so both read the same threshold."* B6 and B7 are one decision, not two |
| B8 | `internal/runtime/serve/mux.go:411` → `flowprotocol.NewRegistryCatalog(registry, artifacts, threshold)` (`catalog.go:66-68`) | Flow catalog output, inline vs by-reference | **PINS** — a Console read, same reason as B5 |
| B9 | `internal/runtime/serve/mux.go:301` → `mcpconsole.AppsDeps.Threshold`; `internal/runtime/assemble/assemble.go:1000` → `mcpconsole.ToolContextDeps.Threshold` | `mcp.servers.read_resource`, `mcp.apps.call_tool`, `mcp.apps.tool_context` | **PINS** — A7/A8's operator path. These payloads render in a sandboxed iframe and by construction never enter the LLM context, which `apps.go:180-187` already argues at length for the `ui://` carve-out; the same argument applies to the rest of the surface |

**Mechanism for the pins on the operator path.** B5–B9 read the operator field today, so pinning is not a constant swap — the wiring must stop threading it. `mux.go:319`, `mux.go:411`, `mux.go:301` and `assemble.go:1000` pass `config.DefaultConsoleInlinePayloadBytes` instead. B4 (`mux.go:355`) and B2/B3 (`assemble.go:1050`, `:1100`) keep `cfg.Artifacts.HeavyOutputThresholdBytes` unchanged.

**Consequence, stated rather than buried.** On a DEFAULT configuration the pins are a byte-for-byte no-op — 32 KiB before, 32 KiB after. They are a behaviour change only for an operator who has set an explicit non-default `heavy_output_threshold_bytes`: their Console-facing bounds decouple from that setting and revert to 32 KiB. The one documented instance of an operator doing this is `docs/decisions.md:5219`, where the go-study-mcp studio App *"only rendered earlier behind a threshold-raising config workaround"* — a workaround phase 109g made obsolete with `appDocumentInlineCap` (2 MiB, `apps.go:198`). The reopening condition is in Risks.

### C. Not consumers

- `internal/config/validate.go:825` refuses only a NEGATIVE value; zero remains "unset". Unchanged.
- `scripts/smoke/phase-64a.sh`, `phase-111d.sh`, `phase-149.sh`, `phase-200.sh` write `heavy_output_threshold_bytes: 32768` explicitly into their fixture configs. Explicit values win, so these are unaffected and are deliberately NOT edited — editing them would weaken four smoke fixtures' independence from the default.

## Acceptance criteria

- [ ] `config.DefaultHeavyOutputThresholdBytes` is `128 * 1024`, and its godoc names the QUESTION it answers (how many bytes may enter a model's context) rather than enumerating consumers.
- [ ] `config.DefaultConsoleInlinePayloadBytes` is `32 * 1024` with a godoc stating it bounds a Protocol reply projected for a browser, not a model prompt, and pointing at D-358.
- [ ] `search.HeavyPreviewThreshold` and the new `internal/tui/renderers` fold constant no longer reference `config.DefaultHeavyOutputThresholdBytes`; each carries its own literal and its own godoc naming its question. Mechanically witnessed: `internal/search/search.go` and `internal/tui/renderers/registry.go` no longer import `internal/config` for this purpose.
- [ ] `internal/mcpconsole`'s fallback constant is sourced on `config.DefaultConsoleInlinePayloadBytes`, and `apps.go:177`'s prose stops carrying a hardcoded "32 KiB".
- [ ] A tool result of 64 KiB — inside the newly-inlined band — reaches the planner observation as the raw value with NO artifact written, asserted at `dispatch.projectForLLM`, not at the config layer.
- [ ] A tool result of 256 KiB is still promoted, and `ErrContextLeak` still fires at and above 128 KiB at the LLM edge, on all three walked classes (`RoleTool` text, a binary `DataURL` part, `ToolCalls[].Args`).
- [ ] The trajectory payload budget is 128 KiB − 4096 with NO edit to `internal/llm/summarizer/trajectory.go` — proving the derivation held.
- [ ] A `SearchResultRow` whose redacted preview is ≥ 32 KiB ships an EMPTY `Preview` and a populated `Ref` (`search.go:423` returns `("", true, nil)`); one at 64 KiB — inside the raised offload band — STILL ships a `Ref`, proving the search bound did not follow the raise. There is no truncation assertion, because the threshold performs selection and `PreviewMaxRunes = 256` (`search.go:77`) already caps every emitted preview after it.
- [ ] `memory.get` on a 64 KiB value still populates `ValueArtifact` and leaves `Value` empty; `memory.list` still flags the same row `HeavyContent` — asserted together, because `list.go:41-44` requires the two to agree.
- [ ] `pause.list`, the flow catalog and the three `mcp.apps.*` reads still select by-reference at 64 KiB on a default configuration, and — separately — on a configuration that sets `heavy_output_threshold_bytes: 262144`, proving the decoupling at B5–B9 landed.
- [ ] `tools.content_stats.heavy_threshold_bytes` reports `131072` on a default configuration and the operator's value when one is set.
- [ ] An existing `harbor.yaml` setting `heavy_output_threshold_bytes: 32768` resolves the LLM-context arm to 32 KiB — the operator's explicit value wins (§10) — and a configuration that omits the key resolves to 128 KiB.
- [ ] `findContextLeak`'s godoc (`safety.go:317-329`) no longer claims `ToolCalls[].Args` is *"offloadable through the same ArtifactStub path a tool RESULT takes"* as though a producer did so. `internal/planner/react/prompt.go:925` copies args through `safeArgs` (`:1875-1880`), which returns them unmodified, and `materialize.go` never walks `ToolCalls`. The godoc states that the arm is an unbacked detector and names the successor work.
- [ ] The three example configs no longer pin the old default, and `docs/CONFIG.md` states 131072.
- [ ] The two skills that hardcode the value are updated in this PR (§18), and the three hand-written `docs/site/concepts/` pages that state "32 KB" are updated (the skill and CONFIG site pages are `@include` stubs and correctly need no edit).
- [ ] Mutation-verified, each against the real tree: (a) restoring `search.HeavyPreviewThreshold`'s alias to `config.DefaultHeavyOutputThresholdBytes` fails the search bound test AND the smoke's static assertion; (b) restoring the TUI fold's alias fails `internal/tui/renderers/registry_test.go`; (c) re-threading `cfg.Artifacts.HeavyOutputThresholdBytes` at `mux.go:319` fails the decoupling arm of the integration test; (d) leaving `config.DefaultHeavyOutputThresholdBytes` at 32 KiB fails the inlined-band dispatch assertion.

## Files added or changed

### Constants and their consumers

- `internal/config/config.go` — `:2041` raised to `128 * 1024`; `:2031-2040` godoc rewritten (the stale enumeration removed); `DefaultConsoleInlinePayloadBytes` added; `:852-861`'s `ArtifactsConfig` field godoc updated for the new default and the decoupled Console arm.
- `internal/search/search.go` — `:63-70` `HeavyPreviewThreshold` de-aliased to its own literal with a rewritten godoc; the `internal/config` import dropped (it has no other use in the file).
- `internal/tui/renderers/registry.go` — a package-local fold constant added; `:138` and `:152` retargeted; the `internal/config` import dropped if unused.
- `internal/mcpconsole/apps.go` — `:173` re-sourced on `config.DefaultConsoleInlinePayloadBytes`; `:177`'s hardcoded "32 KiB" prose replaced with a reference to the named constant.
- `internal/mcpconsole/toolcontext.go` — `:94` re-sourced.
- `internal/runtime/serve/mux.go` — `:301`, `:319`, `:411` retargeted onto the pinned constant; `:355` deliberately unchanged.
- `internal/runtime/assemble/assemble.go` — `:1000` retargeted; `:1050` and `:1100` deliberately unchanged.

### Godoc corrected because it goes stale on merge

- `internal/llm/safety.go` — `:317-329`, the `ToolCalls[].Args` offloadability claim (see the acceptance criterion).
- `internal/llm/registry.go` — `:62` and `:224`, both hardcoding "32 KiB" in prose.
- `internal/runtime/dispatch/dispatch.go` — `:166`, hardcoding "32 KiB".
- `internal/protocol/transports/transports.go` — `:418-423` (`WithPauseList`) and `:922-928` (the memory-handler block), both naming `cfg.Artifacts.HeavyOutputThresholdBytes` as the source.
- `internal/protocol/transports/stream/pause_list_handler.go` — `:118-121`.
- `internal/protocol/transports/stream/memory_handler.go` — `:145-148`.
- `internal/memory/protocol/get.go` — `:28-31`.
- `internal/memory/protocol/list.go` — `:40-45`, including the "both read the same threshold" lockstep sentence.
- `internal/runtime/flow/protocol/catalog.go` — `:66-68`.

### Tests

- `internal/config/validate_core_test.go` — `:58` asserts the seeded default is `32*1024`; retargeted to `128*1024` and to the named constant so it cannot silently drift again.
- `internal/tui/renderers/registry_test.go` — `:46-59` builds a `strings.Repeat("x", 32768)` body and asserts the fold fires. It PASSES unchanged under the pin, which is exactly why it is retargeted onto the named fold constant: as written it is a coincidence, retargeted it is the pin's mutation witness.
- `internal/llm/summarizer/trajectory_test.go` — `:283`'s `if len(payload) > 32*1024` is a stale literal sentinel that no longer tracks the budget it is guarding; retargeted onto the derived budget. (`:486` already reads `llm.DefaultHeavyOutputThreshold` and needs no edit.)
- `internal/search/preview_bound_test.go` — **NEW.** `internal/search` ships no test that reaches `RedactAndCapPreview` at all (`aggregate_test.go`, `concurrent_reuse_test.go`, `testfixtures_test.go` are the package's only test files, and none names the function), so the heavy branch at `:423` is currently uncovered. Covers both sides of 32 KiB, the 64 KiB in-band case, the exact boundary, and the redactor-returned-non-string path at `:416-420`.
- `internal/runtime/dispatch/dispatch_test.go` — the inlined-band and still-promoted assertions at `projectForLLM`. **Overlaps phase 212's file list with no dependency edge between the two phases; the wave coordinator owns the merge order.**
- `internal/llm/safety_test.go`, `internal/llm/safety_toolargs_test.go`, `internal/llm/safety_rolescope_test.go` — the three leak classes at the new boundary. All three pass an explicit threshold today (`safety_rolescope_test.go:13`), so each gains an arm pinned to the resolved default rather than a literal.
- `internal/mcpconsole/apps_test.go`, `internal/mcpconsole/toolcontext_test.go` — the pin witnesses at `apps.go:229`, `:301`, `toolcontext.go:182`.
- `test/integration/heavy_threshold_test.go` — **NEW.** See Test plan.

### Operator-facing surfaces (§18 same-PR)

- `examples/harbor.yaml:355`, `examples/dev.yaml:184`, `examples/serve.yaml:110` — the pinned `32768` updated with a comment naming the Console-arm decoupling.
- `docs/CONFIG.md:742-756` — the `artifacts.heavy_output_threshold_bytes` entry: new default, and the statement that the Console-facing inline bound no longer tracks it. Reaches the docs site through the `@include` stub at `docs/site/reference/config.md`.
- `docs/skills/add-an-in-process-tool/SKILL.md:168` — surface `tools`; hardcodes ">32KB by default".
- `docs/skills/define-the-agent-yaml/SKILL.md:172` — surface `agent-yaml`; hardcodes "default 32768".
- `docs/site/concepts/artifacts-and-context-safety.md:32`, `docs/site/concepts/tools.md:63`, `docs/site/concepts/index.md:96` — hand-written pages stating "32 KB". These are NOT include stubs and do not update themselves.
- `docs/glossary.md` — `:542` (**HeavyOutputThreshold**: new default, and the stale "per-tool overrides land at Phase 26" claim removed, since brief 05 §1 rules the routing a runtime-level invariant), `:572` (**ImagePart / AudioPart / FilePart**: "32 KB default"), `:723` (**App-document cap**: the "32 KiB" it contrasts against is now the pinned Console bound, not the heavy-output threshold — the contrast is still true but the referent changed), plus the new terms below.
- `docs/plans/README.md` — the phase-213 row: `Status` → `Shipped` (§4.2 item 11), AND the detail block rewritten. **The current block at `:356` states "single-sourced into FOUR consumers" and carries the disproven ten-previews search rationale; leaving it is master-plan drift the audit cannot see.**
- `docs/decisions.md` — D-358.
- `scripts/smoke/phase-213.sh`.

## Public API surface

```go
// internal/config

// DefaultHeavyOutputThresholdBytes is the ONE source of the
// LLM-CONTEXT heavy-output default (128 KiB; RFC §6.5, §6.10): the
// byte size at or above which content the runtime is about to place
// into a model's context window is promoted to an artifact-backed
// stub instead. It answers exactly one question — how many bytes may
// enter a prompt — and every consumer that answers a DIFFERENT
// question carries its own named constant instead (D-358). No second
// constant may answer THIS question.
//
// 128 KiB sits at the top of the 16 KiB–128 KiB range research brief
// 05 named as reasonable. The cost is ~32k tokens of a 200k window
// for a result in the 32–128 KiB band; the saving is the extra
// planner turn a stub-then-fetch cycle costs for exactly that band.
// `Defaults()` seeds `ArtifactsConfig.HeavyOutputThresholdBytes` from
// it and an operator's explicit value overrides it.
const DefaultHeavyOutputThresholdBytes = 128 * 1024

// DefaultConsoleInlinePayloadBytes is the ONE source of the
// CONSOLE-FACING inline-payload bound (32 KiB): the byte size at or
// above which a Protocol reply projected for a browser — `pause.list`,
// `memory.get` / `memory.list`, the flow catalog, and the
// `mcp.servers.read_resource` / `mcp.apps.call_tool` /
// `mcp.apps.tool_context` reads — ships an artifact reference instead
// of inline bytes.
//
// It is deliberately NOT DefaultHeavyOutputThresholdBytes. That value
// prices tokens against a context window; this one prices bytes into
// an HTTP reply a browser renders, where a reference costs the reader
// one more round trip it was already going to make and an inline
// payload costs every reader on the page. The two happen to have had
// the same answer; they were never the same question (D-358).
const DefaultConsoleInlinePayloadBytes = 32 * 1024

// internal/search

// HeavyPreviewThreshold is the source-record classification bound: a
// SearchResultRow whose REDACTED preview reaches this byte length is
// too large to be honestly represented by a snippet, so the row ships
// an empty Preview plus a *SearchArtifactRef and the caller reads the
// record itself.
//
// It is NOT a context bound and NOT an offload bound, and it does not
// govern how many bytes ship: PreviewMaxRunes caps every emitted
// preview at 256 runes AFTER this check, so a row is either ~256 runes
// or a reference, at any threshold. It is Protocol-visible — it
// selects which arm of SearchResultRow is populated — so it is pinned
// at the value it has always had rather than tracking the LLM-context
// threshold it used to alias (D-358).
const HeavyPreviewThreshold = 32 * 1024
```

The `internal/tui/renderers` fold constant and the `internal/mcpconsole` fallback are unexported and are not public API surface; their shape is stated in Files added or changed.

## Test plan

- **Unit — config.** Threshold resolution: unset → 128 KiB; `32768` → 32 KiB; `0` → the seeded default; `-1` → refused by name at `validate.go:825`. `DefaultConsoleInlinePayloadBytes` is asserted NOT equal to `DefaultHeavyOutputThresholdBytes`, so a future author who "re-unifies" them fails a test rather than silently re-coupling the matrix.
- **Unit — dispatch.** `projectForLLM` at 31 KiB / 64 KiB / 127 KiB (all raw, no `PutText` call against the store) and at 128 KiB / 256 KiB (promoted, stub resolves). The store is asserted to have received ZERO writes in the inline band — an assertion that fails today and is the phase's central behavioural change.
- **Unit — LLM edge, per leak class.** `RoleTool` `Content.Text`, `RoleTool` `PartText`, an Image/Audio/File `DataURL`, and `ToolCalls[].Args`, each just below and at 128 KiB, asserting the `LeakSite` string and `SizeBytes` on the emitted `llm.context_leak` payload. Paired with a materialization arm proving a 64 KiB `DataURL` still auto-materializes to an `ArtifactStub` carrying the `artifact_fetch` hint (`materialize.go:181-196`) at the new threshold — the pair the raise rests on for that class.
- **Unit — trajectory budget.** The default budget is `128*1024 - 4096`; `WithTrajectoryHeavyOutputThreshold(cfg)` derives from the operator value; the floor at `trajectoryFragmentCap` still holds for a small configured threshold. Asserted with NO edit to `trajectory.go`, so the test proves the derivation rather than a re-typed number.
- **Unit — search preview bound.** New file. A redacted preview at 32767 / 32768 / 65536 bytes: the first inline (≤ 256 runes + ellipsis, `Ref` nil), the second and third `Preview == ""` with `Ref` populated. Plus the `RedactAndCapPreview` non-string-redactor path (`:416-420`) and the nil-redactor refusal (`:409-411`). Driven through a real `audit.Redactor` from `audit/drivers/patterns`, not a fake.
- **Unit — TUI fold + mcpconsole pins.** Each asserts the fold/offload still fires at 32 KiB with the LLM-context constant at 128 KiB — the assertion that fails if either pin regresses to the alias.
- **Integration — `test/integration/heavy_threshold_test.go`.** Required by §17.1 (this phase consumes shipped surfaces in dispatch, llm, memory, transports and mcpconsole). Real drivers throughout: `state/drivers/inmem`, `events/drivers/inmem`, an in-memory `ArtifactStore`, `audit/drivers/patterns`. Three arms:
  1. **Default configuration.** A 64 KiB tool result reaches the planner observation inline with nothing written to the artifact store; a 256 KiB result is promoted and the stub resolves through `artifact_fetch`. In the same stack, `memory.get` on a 64 KiB value populates `ValueArtifact` with `Value` empty, `memory.list` flags the row `HeavyContent`, and `pause.list` on a 64 KiB payload ships a reference — proving the pins under the raise.
  2. **Decoupling arm.** The same stack with `heavy_output_threshold_bytes: 262144`. Dispatch inlines a 200 KiB result (the operator's value won) while `memory.get` / `pause.list` / the flow catalog STILL offload at 32 KiB (the pin held). This arm is what fails if `mux.go:319` or `:411` keeps threading the operator field.
  3. **Failure mode.** An `ArtifactStore` whose `PutText` fails. `dispatch.projectForLLM:1336-1343` degrades to `HeavyTruncationSummary` with a `Warn` — asserted as a LOGGED, non-silent degradation, and asserted NOT to inline the heavy value. On the Console arms the same failure must surface as an error rather than an inlined payload.

  Identity propagation is asserted on every arm: each of the three runs under its own `(tenant, user, session)`, artifacts land under the writing scope, and a cross-tenant read of a promoted stub answers not-found. Concurrency: N=16 concurrent runs across four tenants, half in the inlined band and half above it, asserting no cross-talk in the observations and a restored goroutine baseline after teardown. Runs under `-race`.
- **Conformance:** N/A — no driver interface changes.
- **Concurrency / leak:** N/A for new artifacts; this phase builds none. The existing `internal/runtime/dispatch` D-025 concurrent-reuse run and `internal/search/concurrent_reuse_test.go` are re-run at the new values to confirm the projection and the searchers stay race-free when more results take the inline branch, per the §14 checkbox.

## Smoke script additions

`scripts/smoke/phase-213.sh`, classified `# PREFLIGHT_REQUIRES: live-server`.

- **Static (runs inline, no server needed).** `grep` asserts that `internal/search/search.go` and `internal/tui/renderers/registry.go` contain NO reference to `config.DefaultHeavyOutputThresholdBytes`, and that `internal/mcpconsole/apps.go` references `DefaultConsoleInlinePayloadBytes` rather than the heavy-output constant. This is the pin's mechanical gate: restoring any alias turns `OK` into `FAIL`.
- **Static.** `grep` asserts `examples/harbor.yaml`, `examples/dev.yaml` and `examples/serve.yaml` no longer carry `heavy_output_threshold_bytes: 32768`, and that `docs/CONFIG.md` states `131072`.
- **Live.** `tools.content_stats` against the first tool in the dev catalog (following `scripts/smoke/phase-73f.sh:121-137`'s shape, including its empty-catalog `skip`) asserts `.heavy_threshold_bytes == 131072` on the default `harbor dev` boot — the only Protocol surface that reports the resolved threshold. `skip_if_404` keeps a partial build green.
- **Live.** `search.query` returns rows carrying a non-empty `.preview` and a null `.ref` for ordinary records, proving the preview path is alive after the de-aliasing. The ref-vs-inline flip itself is unit-tested rather than smoke-tested — a ≥ 32 KiB synthesised preview is not drivable through a dev smoke, and asserting a mechanism the smoke cannot actually produce is the defect this plan's first draft shipped.
- **Live.** `pause.list` and `memory.list` round-trip 200 on the default boot (`skip_if_404`), so the retargeted wiring at `mux.go:319` is proven to still mount the routes — a nil or zero threshold there leaves them UN-mounted (`transports.go:929`, `pause_list_handler.go:132-134`), which would otherwise be an invisible regression that only looks like a skip.

## Coverage target

Measured on the tree at `plan/v124-wave` with `go test -cover`, not estimated. The master plan's flat 85% for this phase is replaced by per-package targets, because 85% is BELOW measured for two touched packages (sanctioning a regression) and above measured for five (unreachable without work this phase has no reason to do).

| Package | Measured | Target |
|---------|----------|--------|
| `internal/config` | 82.9% | 82.9% — no regression (constant + godoc; no new statements) |
| `internal/search` | 59.3% | 62% — the new preview-bound test covers `RedactAndCapPreview`'s currently-untested branches |
| `internal/runtime/dispatch` | 85.1% | 85.1% — no regression |
| `internal/llm` | 79.1% | 79.1% — no regression |
| `internal/llm/summarizer` | 94.8% | 94.8% — no regression |
| `internal/tui/renderers` | 80.6% | 80.6% — no regression |
| `internal/mcpconsole` | 74.9% | 74.9% — no regression |
| `internal/runtime/serve` | 84.4% | 84.4% — no regression |
| `internal/runtime/assemble` | 83.6% | 83.6% — no regression |

## Dependencies

- **210** — widened `findContextLeak` onto `Messages[].ToolCalls[].Args`. This phase re-targets that arm's comparison value and corrects its godoc, so it must land after.
- **209** — the `artifact_fetch` ceiling fields this phase deliberately leaves alone. The Non-goal is only meaningful once they exist as operator policy.

Phase 212 is NOT a dependency, but it also edits `internal/runtime/dispatch/dispatch_test.go`; the overlap is recorded in Files added or changed and is a merge-order matter for the wave coordinator, not a design edge.

## Risks / open questions

**The leak-guard weakening is real for one of three classes, and the risk is stated per class rather than as a single "designed pair" claim.** The claim in this plan's first draft — that `materializeRequest` runs first at the same threshold and offloads, so the guard and the offloader are a designed pair — is true for exactly one of the three classes `findContextLeak` walks.

1. **Binary `DataURL` parts (Image / Audio / File, any role).** The pair is exact. `materialize.go:53-99` walks `PartImage` / `PartAudio` / `PartFile` and offloads above the threshold; `safety.go:98` compares the MATERIALIZED request. Raising both together is structurally behaviour-preserving.
2. **`RoleTool` message text (`Content.Text` and `PartText`).** `materialize.go` does NOT offload this — its `case PartText:` arm passes text through verbatim, with a comment saying the leak check catches it afterwards. The offload counterpart exists, but one edge earlier: `dispatch.projectForLLM` (`dispatch.go:1333`) promotes a heavy tool result to a stub before the renderer ever builds the message, and it reads the SAME operator field (`assemble.go:1050`). The pair holds; it is dispatch↔LLM-edge, not materialize↔LLM-edge. Naming the wrong partner is what let the first draft state the defence more strongly than the code supports.
3. **`Messages[].ToolCalls[].Args`.** No offloader exists anywhere in the tree. `internal/planner/react/prompt.go:925` writes `Args: json.RawMessage(safeArgs(call.Args))` and `safeArgs` (`:1875-1880`) returns `raw` unchanged; `materialize.go` never walks `ToolCalls`. For this class, raising the threshold is a straight 4× weakening of the only guard that exists.

   **The decision is that it still moves, for three reasons.** (a) The arm is a DETECTOR that fails the request loud with `ErrContextLeak`, and its judgement — "a producer that should have passed a reference didn't" — is only meaningful relative to what the runtime considers heavy. Holding it at 32 KiB while the offload boundary sits at 128 KiB would kill runs over 40 KiB of arguments that no producer in the tree is capable of offloading: a run-terminating false positive on content the runtime itself just declared acceptable everywhere else. (b) Two numbers inside one walk, one of them unsatisfiable by any producer, is the §13 parallel-implementation shape. (c) Args are model-authored and arrive back from the provider; refusing the replay recovers nothing the outbound turn did not already spend.

   **The residual is stated as a property, not an open question:** after this phase, a tool call may carry up to 128 KiB of arguments through the replay path with no offloader and one detector. The successor work — an args offloader stubbing heavy arguments through the same `ArtifactStub` path a tool RESULT takes, which `safety.go:321-329`'s godoc already asserts as the intent — is named in D-358 and deliberately not built here: it needs a decision about how a stubbed argument is re-hydrated for the provider's `tool_calls` block, which is a translator-layer question across every driver.

**An operator who raised the threshold for a Console-rendering reason loses that.** B5–B9 stop tracking `heavy_output_threshold_bytes`, so a deployment that set `262144` to keep some payload inline in the Console reverts to a 32 KiB by-reference bound there. The one documented instance (`docs/decisions.md:5219`, the 86.4 KB studio App) was made obsolete by `appDocumentInlineCap` in phase 109g. **Reopening condition, stated so it need not be re-derived:** if a Console-facing payload other than a `ui://` document is reported as unusably by-reference, the answer is an additive optional `artifacts.console_inline_payload_bytes` field defaulting to `DefaultConsoleInlinePayloadBytes` — NOT re-coupling the two constants.

**A 128 KiB inline result is ~32k tokens, roughly 16% of a 200k window.** That is the deliberate trade and it is bounded: it is paid only by results in the 32–128 KiB band, each of which otherwise cost a full extra planner turn, and the tail above 128 KiB is promoted exactly as before. It is a real regression for a small-context model, which is why the operator field remains the override and why brief 05's range starts at 16 KiB.

**The `heavy_count` reported by `tools.content_stats` becomes non-comparable across the upgrade.** `annotate/metrics.go:139-160` counts historical `mcp.resource_offloaded` events against the CURRENT threshold, so events recorded under 32 KiB are re-classified when the reported threshold moves. This is inherent to reporting a live threshold over a historical event stream and is not introduced here; it is named so an operator reading a discontinuity in that number after upgrading has an explanation.

**Not a risk, recorded because it was asserted as one and is false.** The first draft's search rationale — "a result list of ten previews at the offload threshold would exceed most context windows" — is arithmetically impossible. `PreviewMaxRunes = 256` (`search.go:77`) caps every preview AFTER the heavy check at `:423`, so ten previews are ~10 KB at any threshold. The threshold performs ref-versus-inline SELECTION (`:423` returns `("", true, nil)` — an empty preview plus a ref), never truncation. The search bound still pins, but on the §8 wire-shape argument in row A5 and the honest-representation argument in its godoc, not on a context-budget argument that does not apply.

## Glossary additions

- **Preview bound** — the byte length at which a `SearchResultRow`'s redacted preview is judged too large to be honestly represented by a snippet, so the row ships an empty `Preview` plus a `SearchArtifactRef` instead (`search.HeavyPreviewThreshold`, 32 KiB). It performs selection, not truncation: `PreviewMaxRunes` (256) caps every emitted preview independently, so a row is either a snippet or a reference at any bound. Distinct from the heavy-output threshold, which prices bytes against a model's context window. D-358.
- **Console inline payload bound** — the byte length at or above which a Protocol reply projected for a browser (`pause.list`, `memory.get` / `memory.list`, the flow catalog, `mcp.servers.read_resource`, `mcp.apps.call_tool`, `mcp.apps.tool_context`) ships an artifact reference instead of inline bytes (`config.DefaultConsoleInlinePayloadBytes`, 32 KiB). Decoupled from the heavy-output threshold because the payload never enters a model's context and because the selection is Protocol-visible (§8). D-358.

Existing entries corrected in the same PR: **HeavyOutputThreshold** (`:542` — new default; the stale per-tool-override claim removed), **ImagePart / AudioPart / FilePart** (`:572`), **App-document cap** (`:723`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ the MEASURED per-package target above (not the master plan's flat 85%)
- [ ] Cross-session isolation asserted in the integration test's per-arm identity propagation and its four-tenant concurrency run
- [ ] N/A — this phase builds no new reusable artifact. The existing `internal/runtime/dispatch` D-025 run and `internal/search/concurrent_reuse_test.go` are re-run at the new values.
- [ ] Integration test wires real drivers, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] Glossary updated: two new terms, three corrected entries
- [ ] §18: both skills that hardcode the value, `docs/CONFIG.md`, and the three hand-written `docs/site/concepts/` pages updated in this PR
- [ ] `docs/plans/README.md` phase-213 detail block rewritten (the shipped block states a disproven consumer count and a disproven search rationale) and `Status` flipped
- [ ] D-358 filed, recording: the per-consumer matrix, the per-leak-class qualification, the `ToolCalls[].Args` residual and its successor, and the restated single-sourcing rule
