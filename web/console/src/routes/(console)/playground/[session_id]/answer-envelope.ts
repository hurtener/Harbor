// Phase 106 (V1.2) — answer envelope parsing for the Playground.
//
// Extracted from `+page.svelte` so the JSON-envelope shape contract can
// be unit-tested in Vitest (no Svelte renderer needed). The wire shape
// is documented on `tasks.TaskResult` (internal/tasks/tasks.go) and
// `prototypes.TaskDetail.ResultInline` (internal/protocol/types/tasks.go).
//
// `result_inline` lives at the TOP LEVEL of the `TaskDetail` JSON
// response — it is NOT nested inside the `task` field. A previous
// implementation read `detail.task.result_inline` and silently never
// found the answer because `TaskRow` (the value of `.task`) has no such
// field. Test pin: parseAnswerEnvelope({task:{...}, result_inline:'...'})
// MUST return the answer.

// Phase 107a (V1.3) — reasoning step type matching the wire projection.
// Structurally identical to `$lib/chat/types.js::ReasoningStep`.
export interface ReasoningStep {
	index: number;
	reasoning_trace: string;
}

/** The TaskDetail subset the Playground reads. */
export interface TaskDetailLike {
	result_inline?: string;
	trajectory?: { steps?: ReasoningStep[] };
}

/**
 * parseAnswerFromDetail extracts the LLM's natural-language answer from
 * a TaskDetail JSON response. It returns:
 *   - the envelope's `.answer` string when result_inline is non-empty
 *     and parses as the Phase 106 envelope shape;
 *   - the empty string when result_inline is absent / empty;
 *   - the explicit fallback "(failed to parse answer payload)" when
 *     result_inline is present but is not valid JSON (no silent
 *     degradation per CLAUDE.md §5).
 */
export function parseAnswerFromDetail(detail: TaskDetailLike | null | undefined): string {
	if (!detail || !detail.result_inline) {
		return '';
	}
	try {
		const envelope = JSON.parse(detail.result_inline) as { answer?: unknown };
		const a = envelope.answer;
		return typeof a === 'string' ? a : '';
	} catch {
		return '(failed to parse answer payload)';
	}
}

/**
 * parseReasoningSteps extracts the reasoning-trace projection from a
 * TaskDetail JSON response. Steps with an empty ReasoningTrace are
 * already filtered on the Go side; the TS layer asserts the invariant in
 * tests but does not re-filter in production. Returns an empty array
 * when the trajectory is absent, nil, or has no non-empty-reasoning steps.
 */
export function parseReasoningSteps(detail: TaskDetailLike | null | undefined): ReasoningStep[] {
	if (!detail?.trajectory?.steps) {
		return [];
	}
	return detail.trajectory.steps.filter((s) => s.reasoning_trace.length > 0);
}

/**
 * normalizeLifecycleType converts an EventSource event-name into the
 * suffix the lifecycle handler branches on. The Console SSE stream
 * emits 'task.completed' / 'task.failed' / 'task.cancelled' on the
 * named-event channel; the default-message channel may carry the same
 * events with a `type` field that includes the `task.` prefix.
 * Normalising once at the dispatch boundary keeps the handler's
 * `if (eventType === 'completed')` branches correct regardless of
 * delivery channel.
 */
export function normalizeLifecycleType(t: string): string {
	return t.replace(/^task\./, '');
}
