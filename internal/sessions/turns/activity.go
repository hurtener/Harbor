package turns

// The optional activity-read surface (ActivityReader / PageActivity /
// activity cursor) has been REMOVED: the v1.28 Protocol surface is
// exactly `sessions.turns.list` / `sessions.turns.get`, and there is
// no third activity / analytics subresource. An over-budget turn's
// inline activity window overflows honestly (More + Dropped + Partial)
// and the exact turn-level totals survive in Activity.Totals; the
// projection never reads `state.history` from a consumer. This file is
// retained as a pointer to that removal.
