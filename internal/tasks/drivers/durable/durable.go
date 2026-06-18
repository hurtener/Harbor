// Package durable is Harbor's StateStore-backed TaskRegistry driver.
// Task, group, and patch records are persisted through the runtime's
// state.StateStore — each in its own per-record slot — so they survive
// a runtime restart. On open the driver replays every record into the
// shared task engine and runs a recovery sweep over tasks left Running
// by a crash.
//
// The driver wraps the same internal/tasks/engine state machine the
// in-process driver wraps; it differs only in its persistence backend
// (per-record, replayed on open) and the open-time recovery sweep. It
// is opt-in via tasks.driver: durable; the in-process driver remains
// the default.
//
// Scope. The driver delivers single-instance restart-survival of task
// RECORDS. It recovers the record, not execution: a task left Running
// by a crash is transitioned to a terminal failed state, not re-driven
// (auto-re-drive of recovered work is a separate, deferred concern).
// Cross-process durability requires a durable StateStore
// (state.driver: sqlite | postgres); paired with an in-memory state
// store the records survive only an in-process driver reopen, not a
// process restart. Multi-instance coordination (two runtimes sharing
// one store) is out of scope.
package durable

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// New constructs the durable TaskRegistry. It fails loudly when no
// StateStore is wired: an operator who selected tasks.driver: durable
// asked for durability and must get it, so the driver refuses to fall
// back to non-durable behavior (CLAUDE.md §13 "no silent stub
// default"). The runtime always wires its shared StateStore into
// tasks.Open, so this guard fires only on a mis-wired embedding.
//
// After constructing the engine (which hydrates persisted records), New
// runs the open-time recovery sweep so tasks interrupted by a restart
// reach a terminal state before the registry serves any request.
func New(deps tasks.Dependencies) (tasks.TaskRegistry, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf(
			"tasks/durable: the durable task driver requires a StateStore but none was wired; " +
				"configure a durable store via state.driver (sqlite or postgres — see examples/) " +
				"and ensure it is passed to tasks.Open")
	}
	eng, err := engine.New(deps.Bus, deps.Redactor, &backend{store: deps.Store})
	if err != nil {
		return nil, fmt.Errorf("tasks/durable: %w", err)
	}
	if _, err := eng.RecoverInterruptedTasks(context.Background()); err != nil {
		return nil, fmt.Errorf("tasks/durable: open-time recovery sweep: %w", err)
	}
	return eng, nil
}

func init() {
	tasks.Register("durable", New)
}
