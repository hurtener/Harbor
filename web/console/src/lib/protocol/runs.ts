/**
 * Runs wire types — the `runs.set_overrides` Protocol shape the Console
 * Playground page's Controls card records (Phase 73n / D-130).
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * This module is the wire-type surface only: the override payload the
 * `RunsNamespace.setOverrides` method (in `client.ts`) consumes. It
 * mirrors `internal/protocol/types/runs.go` (`RunOverrides`)
 * field-for-field — the Go side is the single source (D-002 / D-093).
 *
 * # Why this type is NAMED (D-223 blind-spot closure)
 *
 * `RunOverrides` was previously on the untyped-wire-type allowlist, so
 * `setOverrides(overrides: Record<string, unknown>)` accepted any key
 * with ZERO compile-time check. A phantom `top_p` key sailed through the
 * typed client, was serialized onto the wire, and the runtime's
 * `DisallowUnknownFields()` decoder 400'd the WHOLE `runs.set_overrides`
 * request — silently failing every real override. Promoting the type to
 * a named interface (and typing `setOverrides` against it) makes `tsc`
 * reject an unknown key at author time. The snake_case keys ARE the wire
 * keys — the Playground adapter maps its camelCase composer state onto
 * this shape.
 */

/**
 * The next-message override payload `runs.set_overrides` records. Mirrors
 * `types.RunOverrides`.
 *
 * Every tuning field is optional: an absent field leaves the runtime
 * default in place; a present field applies to the NEXT message in the
 * session only (session-scoped, one-shot). There is intentionally NO
 * `top_p` field — the runtime does not support nucleus sampling as a
 * per-message override, and a phantom key fails the whole request at the
 * `DisallowUnknownFields()` decoder.
 */
export interface RunOverrides {
  /** The session the override applies to. Mandatory. */
  session_id: string;
  /** The LLM reasoning-effort hint (`low` / `medium` / `high`). */
  reasoning_effort?: string;
  /** The sampling temperature, in the closed range [0, 2]. */
  temperature?: number;
  /** The per-message MaxTokens governance ceiling (positive integer). */
  max_tokens?: number;
  /** A one-message system-prompt override (empty string clears it). */
  system_prompt_override?: string;
  /** A one-message model swap (must have a configured ModelProfile). */
  model?: string;
}
