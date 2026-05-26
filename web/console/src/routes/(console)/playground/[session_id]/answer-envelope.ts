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

/** The TaskDetail subset the Playground reads. */
export interface TaskDetailLike {
	result_inline?: string;
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
