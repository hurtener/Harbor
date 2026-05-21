/**
 * Background Jobs page — the `AwaitTask` orphan detector (Phase 73h /
 * D-128).
 *
 * # What an orphan is
 *
 * A planner spawns a background task via `SpawnTask` (D-047) and is
 * expected to join it via `AwaitTask`. The §13 binding rule pairs the
 * two — `SpawnTask` + `AwaitTask` emission MUST land in the same phase
 * (Phase 47 / D-056 closed this for ReAct). An *orphan* is a background
 * job whose `parent_task_id` names a task that is no longer in the
 * runtime's active-task set: the parent finished / failed / was
 * cancelled while the child kept running. The detector surfaces, AT THE
 * UI, the property that rule guarantees — it is the observability
 * surface for the binding, NOT a re-implementation of the runtime join.
 *
 * # Why the detector lives Console-side (D-128)
 *
 * The detector is a pure cross-check over a single `tasks.list`
 * snapshot — it adds NO Protocol field and issues NO Protocol call. A
 * row whose `parent_task_id` is non-empty and ABSENT from the same
 * snapshot's id set is flagged. This is a deliberate D-128 call: a
 * runtime-side `parent_alive` boolean would be the obvious post-V1 lift
 * if the per-render cost ever bites, but at V1 the Console-side `O(N)`
 * cross-check needs no Protocol surface change.
 *
 * # Performance
 *
 * The detector is `O(N)` — one pass to build the id set, one pass to
 * test each row, each test a single hashed `Set.has` lookup. It is
 * sort-invariant (the result `Set` does not depend on row order) and
 * pure (it never mutates its input and holds no state), so it is
 * trivially safe to call on every render and from concurrent contexts.
 */

import type { TaskRow } from '../protocol/tasks.js';

/**
 * Compute the set of orphaned task IDs in a `tasks.list` snapshot.
 *
 * A row is an orphan iff its `parent_task_id` is non-empty AND that
 * parent ID is NOT itself present as a row `id` in the same snapshot.
 * A row with no `parent_task_id` (a top-level task) is never an orphan.
 * A row whose parent IS in the snapshot is a healthy child.
 *
 * The function is pure: it reads only `rows`, allocates a fresh `Set`,
 * and never mutates its argument. The returned `Set` is independent of
 * the order of `rows` (sort-invariant).
 *
 * @param rows the `tasks.list` snapshot to cross-check.
 * @returns the set of `id`s flagged as orphans (possibly empty).
 */
export function detectOrphans(rows: readonly TaskRow[]): Set<string> {
  // One pass to index every row id present in this snapshot.
  const present = new Set<string>();
  for (const r of rows) {
    present.add(r.id);
  }
  // One pass to flag a row whose declared parent is absent.
  const orphans = new Set<string>();
  for (const r of rows) {
    const parent = r.parent_task_id;
    if (parent !== undefined && parent !== '' && !present.has(parent)) {
      orphans.add(r.id);
    }
  }
  return orphans;
}

/**
 * A convenience predicate over a precomputed orphan set — keeps the
 * per-row render path a single hashed lookup. Pass the `Set` from one
 * {@link detectOrphans} call and re-use it across every row's render.
 *
 * @param orphans the set returned by {@link detectOrphans}.
 * @param row the row to test.
 * @returns true iff `row` is flagged as an orphan.
 */
export function isOrphan(orphans: ReadonlySet<string>, row: TaskRow): boolean {
  return orphans.has(row.id);
}
