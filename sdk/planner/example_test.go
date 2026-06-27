// example_test.go — runnable godoc example for the sdk/planner facade:
// the swappable-planner driver registry (CLAUDE.md §1 property 3) and
// the canonical terminal-reason vocabulary. The example body imports
// only the public sdk/planner facade plus a blank import that seats a
// planner driver — the copyable code never reaches into internal/.
package planner_test

import (
	"fmt"

	"github.com/hurtener/Harbor/sdk/planner"
	// Blank-importing the reference ReAct planner facade seats its
	// "react" driver registration, so `planner.driver: react` resolves
	// through the registry. sdk/drivers/prod seats it (and the rest of
	// the production set) too.
	_ "github.com/hurtener/Harbor/sdk/planner/react"
)

// Example shows the planner facade's primary entry surface: the driver
// registry that makes the Planner swappable. Blank-importing a planner
// concrete seats its registration; RegisteredDrivers lists the seated
// names a `planner.driver:` config can resolve. The example also checks
// a terminal reason against the canonical FinishReason vocabulary.
func Example() {
	// The seated planner drivers a config can resolve by name.
	fmt.Println(planner.RegisteredDrivers())

	// FinishGoal is the canonical "goal achieved" terminal reason.
	fmt.Println(planner.IsValidFinishReason(planner.FinishGoal))

	// Output:
	// [react]
	// true
}
