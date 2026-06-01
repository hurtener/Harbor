// Harbor Console — Tasks page per-task (RUN-scoped) event projections
// (Phase 108i / D-181).
//
// Pure, DOM-free projections of the shipped `events.subscribe` SSE stream
// into the per-task bottom-dock tab views (Events / Control History /
// Interventions / Group / Cost). The Tasks detail dock opens ONE
// subscription scoped to the task's parent session (the runtime scopes
// the SSE server-side by session — stream.go) and narrows it to the
// task's RUN with {@link eventBelongsToRun}. There is no new Protocol
// method — these are read-only folds over the stream the Events,
// Sessions and Overview pages already consume.
//
// # The run-match is `payload.TaskID`, not just `e.run` (live-wire finding)
//
// The PAGE-POLISH §3 live-wire pass (2026-06-01) found the top-level
// `run` field is populated on `llm.cost.recorded` / `planner.decision`
// events but is NULL on the `task.*` lifecycle events (`task.spawned` /
// `task.started` / `task.completed`), which carry the id in the
// PascalCase `payload.TaskID` instead. A naive `e.run === taskID` filter
// would therefore silently DROP every lifecycle event from the Events
// tab. {@link eventBelongsToRun} matches all three shapes; the
// `run-events.test.ts` suite locks it against a captured real frame.
//
// Extracted as a pure module so it is unit-testable against captured real
// wire frames (the SSE payload is PascalCase Go field names, not json
// tags — PAGE-POLISH §3.3).

import type { Event } from '$lib/protocol/events.js';

/** Reads a record from an unknown payload, or null when not an object. */
function asRecord(v: unknown): Record<string, unknown> | null {
  return v !== null && typeof v === 'object' ? (v as Record<string, unknown>) : null;
}

/** Reads a number from a record under any candidate key (finite only). */
function readNumber(obj: Record<string, unknown>, keys: string[]): number {
  for (const k of keys) {
    const v = obj[k];
    if (typeof v === 'number' && Number.isFinite(v)) {
      return v;
    }
  }
  return 0;
}

/** Reads a string from a record under any candidate key (non-empty only). */
function readString(obj: Record<string, unknown>, keys: string[]): string | null {
  for (const k of keys) {
    const v = obj[k];
    if (typeof v === 'string' && v.length > 0) {
      return v;
    }
  }
  return null;
}

/**
 * True when an event belongs to the given task's run. Matches the three
 * shapes the runtime emits (live-wire verified):
 *   - `e.run === taskID` — `llm.cost.recorded`, `planner.decision`, etc.
 *   - `payload.TaskID === taskID` — the `task.*` lifecycle events (whose
 *     top-level `run` is null).
 *   - `payload.Identity.RunID === taskID` — cost/decision events also
 *     carry the run inside the identity quadruple; checked as a fallback.
 * A task's run id IS the task id (a control verb targets `identity.run =
 * taskID` — D-072), so this is the per-task event filter.
 */
export function eventBelongsToRun(e: Event, taskID: string): boolean {
  if (taskID === '') return false;
  if (e.run === taskID) return true;
  const p = asRecord(e.payload);
  if (p === null) return false;
  if (readString(p, ['TaskID', 'task_id']) === taskID) return true;
  const id = asRecord(p['Identity'] ?? p['identity']);
  if (id !== null && readString(id, ['RunID', 'run_id', 'run']) === taskID) {
    return true;
  }
  return false;
}

/** Narrows an event page to the events belonging to one task's run. */
export function filterRunEvents(events: readonly Event[], taskID: string): Event[] {
  return events.filter((e) => eventBelongsToRun(e, taskID));
}

/** The per-task cost rollup, broken down by TOKEN TYPE (D-181 sign-off). */
export interface RunCost {
  /** Σ of `llm.cost.recorded` input-token cost, USD. */
  inputUSD: number;
  /** Σ output-token cost, USD. */
  outputUSD: number;
  /** Σ reasoning-token cost, USD. */
  reasoningUSD: number;
  /** Σ total cost, USD (authoritative — not the sum of the three above). */
  totalUSD: number;
  /** Σ prompt (input) tokens. */
  promptTokens: number;
  /** Σ completion (output) tokens. */
  outputTokens: number;
  /** Σ reasoning tokens. */
  reasoningTokens: number;
  /** Σ total tokens. */
  totalTokens: number;
  /** The count of `llm.cost.recorded` events folded in. */
  events: number;
  /** The distinct model labels seen (e.g. `openai/gpt-5.4`). */
  models: string[];
}

const EMPTY_RUN_COST: RunCost = {
  inputUSD: 0,
  outputUSD: 0,
  reasoningUSD: 0,
  totalUSD: 0,
  promptTokens: 0,
  outputTokens: 0,
  reasoningTokens: 0,
  totalTokens: 0,
  events: 0,
  models: []
};

/**
 * Folds the run's `llm.cost.recorded` events into a token-type cost
 * rollup. Reads the PascalCase wire shape defensively
 * (`payload.Cost.{Input,Output,Reasoning}TokensCost` / `TotalCost`,
 * `payload.Usage.{PromptTokens,CompletionTokens,ReasoningTokens,
 * TotalTokens}`, `payload.Model`) — an event whose payload does not parse
 * is skipped, never counted as zero (CLAUDE.md §13). Verified live: the
 * cost crosses the wire as `{"Cost":{"TotalCost":0.001794,...},"Usage":
 * {"TotalTokens":2351,...},"Model":"openai/gpt-5.4"}`.
 */
export function projectRunCost(events: readonly Event[]): RunCost {
  const out: RunCost = { ...EMPTY_RUN_COST, models: [] };
  const models = new Set<string>();
  for (const e of events) {
    if (e.type !== 'llm.cost.recorded') continue;
    const p = asRecord(e.payload);
    if (p === null) continue;
    const cost = asRecord(p['Cost'] ?? p['cost']);
    const usage = asRecord(p['Usage'] ?? p['usage']);
    if (cost === null && usage === null) continue;
    out.events += 1;
    if (cost !== null) {
      out.inputUSD += readNumber(cost, ['InputTokensCost', 'input_tokens_cost']);
      out.outputUSD += readNumber(cost, ['OutputTokensCost', 'output_tokens_cost']);
      out.reasoningUSD += readNumber(cost, ['ReasoningTokensCost', 'reasoning_tokens_cost']);
      out.totalUSD += readNumber(cost, ['TotalCost', 'total_cost']);
    }
    if (usage !== null) {
      out.promptTokens += readNumber(usage, ['PromptTokens', 'prompt_tokens']);
      out.outputTokens += readNumber(usage, ['CompletionTokens', 'completion_tokens']);
      out.reasoningTokens += readNumber(usage, ['ReasoningTokens', 'reasoning_tokens']);
      out.totalTokens += readNumber(usage, ['TotalTokens', 'total_tokens']);
    }
    const model = readString(p, ['Model', 'model']);
    if (model !== null) models.add(model);
  }
  out.models = [...models];
  return out;
}

/** The control-instruction events for a run (`control.*`), newest-first. */
export function filterControlEvents(events: readonly Event[]): Event[] {
  return events.filter((e) => e.type.startsWith('control.'));
}

/** The HITL / pause intervention events for a run, newest-first. */
export function filterInterventionEvents(events: readonly Event[]): Event[] {
  return events.filter(
    (e) =>
      e.type.startsWith('pause.') ||
      e.type.startsWith('tool.approval_') ||
      e.type.startsWith('tool.auth_')
  );
}

/** The TaskGroup lifecycle events for a run (`task.group_*`). */
export function filterGroupEvents(events: readonly Event[]): Event[] {
  return events.filter((e) => e.type.startsWith('task.group_'));
}
