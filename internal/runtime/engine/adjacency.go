package engine

import (
	"fmt"
	"strings"
)

// Adjacency is a (From Node, To []Node) pair the engine consumes to
// allocate channels. The full set of adjacencies forms the runtime
// DAG (with cycle opt-in per node). New validates the set:
//
//   - every To node must appear as a From in some adjacency OR be
//     terminal (no children — i.e. an Outlet);
//   - no two adjacencies share a Node.Name;
//   - the graph contains no cycle unless the cycle's nodes have
//     AllowCycle: true.
type Adjacency struct {
	From Node
	To   []Node
}

// nodeIndex is a name -> Node map built from the adjacency set during
// New. Used by validation, the cycle detector, and the engine struct.
// Deduplication: a node may appear as From in one adjacency and as To
// in another; we use the From-side definition (it carries Func +
// Policy + AllowCycle).
//
// Returns ErrDuplicateNodeName when two From entries share a name
// with non-equivalent definitions.
func buildNodeIndex(adjs []Adjacency) (map[string]Node, error) {
	idx := make(map[string]Node, len(adjs))
	for _, adj := range adjs {
		if existing, ok := idx[adj.From.Name]; ok {
			if !sameNodeDefinition(existing, adj.From) {
				return nil, fmt.Errorf("%w: %q appears in adjacencies with conflicting Func/Policy/AllowCycle",
					ErrDuplicateNodeName, adj.From.Name)
			}
			continue
		}
		idx[adj.From.Name] = adj.From
	}
	// Walk children — register them as Outlet candidates if not
	// already in the index (a node that is purely a To-target has
	// no Func of its own but still needs an entry so the cycle
	// detector has a complete vertex list).
	for _, adj := range adjs {
		for _, child := range adj.To {
			if existing, ok := idx[child.Name]; ok {
				if !sameNodeDefinition(existing, child) {
					return nil, fmt.Errorf("%w: %q appears as To with conflicting Func/Policy/AllowCycle vs From",
						ErrDuplicateNodeName, child.Name)
				}
				continue
			}
			idx[child.Name] = child
		}
	}
	return idx, nil
}

// sameNodeDefinition compares two Node values by Name AND AllowCycle.
// Func equality is not checked (Go disallows comparison of function
// values); we trust the caller not to register the same name twice
// with different funcs — the typical mis-use is duplicating From
// entries which the dedup catches by name + AllowCycle.
func sameNodeDefinition(a, b Node) bool {
	return a.Name == b.Name && a.AllowCycle == b.AllowCycle
}

// detectCycle runs a DFS-based cycle detector across the adjacencies.
// Returns ErrCycleDetected wrapping the cycle path when an unintended
// cycle is found. A cycle whose every participating node has
// AllowCycle: true is allowed.
//
// The detector treats each adjacency's From -> To edges as directed.
// Standard 3-color DFS (white = unvisited, gray = on stack, black =
// done); a gray re-visit reports the cycle back through the stack.
func detectCycle(adjs []Adjacency, nodes map[string]Node) error {
	// Build adjacency list: name -> []name.
	out := make(map[string][]string, len(nodes))
	for _, adj := range adjs {
		for _, to := range adj.To {
			out[adj.From.Name] = append(out[adj.From.Name], to.Name)
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	var stack []string

	var visit func(name string) error
	visit = func(name string) error {
		switch color[name] {
		case gray:
			// Found a cycle. The path from where we re-entered
			// `name` on the stack to the current top is the
			// cycle. Slice it out and verify whether every node
			// has AllowCycle.
			cycle := append([]string{}, sliceCycleFromStack(stack, name)...)
			cycle = append(cycle, name)
			if everyNodeAllowsCycle(cycle, nodes) {
				return nil // legitimate cycle
			}
			return fmt.Errorf("%w: %s", ErrCycleDetected, strings.Join(cycle, " -> "))
		case black:
			return nil
		}
		color[name] = gray
		stack = append(stack, name)
		for _, child := range out[name] {
			if err := visit(child); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	// Visit in deterministic order so error messages are stable
	// across runs (helps tests + audit logs).
	for _, name := range sortedKeys(nodes) {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// sliceCycleFromStack returns the suffix of stack starting at the
// first element equal to target. Caller appends target to the result
// to close the cycle.
func sliceCycleFromStack(stack []string, target string) []string {
	for i, n := range stack {
		if n == target {
			return stack[i:]
		}
	}
	return stack // fallback: entire stack
}

// everyNodeAllowsCycle reports true when every name in the cycle is
// registered with AllowCycle: true.
func everyNodeAllowsCycle(cycle []string, nodes map[string]Node) bool {
	for _, name := range cycle {
		if !nodes[name].AllowCycle {
			return false
		}
	}
	return true
}

// sortedKeys returns the map's keys in lexicographic order. Used by
// the cycle detector to make traversal deterministic.
func sortedKeys(m map[string]Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Tiny manual insertion sort — keeps the package dependency-free
	// and is plenty fast for graphs of any realistic V1 size.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// inletNodes returns nodes with no incoming edges in adjs. These are
// the points where external Emit lands. A graph with zero inlets
// would be unreachable from outside; New rejects that case.
func inletNodes(adjs []Adjacency, nodes map[string]Node) []string {
	hasParent := make(map[string]bool, len(nodes))
	for _, adj := range adjs {
		for _, to := range adj.To {
			hasParent[to.Name] = true
		}
	}
	inlets := make([]string, 0)
	for name := range nodes {
		if !hasParent[name] {
			inlets = append(inlets, name)
		}
	}
	// Deterministic order for tests.
	for i := 1; i < len(inlets); i++ {
		for j := i; j > 0 && inlets[j-1] > inlets[j]; j-- {
			inlets[j-1], inlets[j] = inlets[j], inlets[j-1]
		}
	}
	return inlets
}

// outletNodes returns nodes with no outgoing edges in adjs. These
// emit to the synthetic Outlet (the engine's egress queue).
func outletNodes(adjs []Adjacency, nodes map[string]Node) []string {
	hasChild := make(map[string]bool, len(nodes))
	for _, adj := range adjs {
		if len(adj.To) > 0 {
			hasChild[adj.From.Name] = true
		}
	}
	outlets := make([]string, 0)
	for name := range nodes {
		if !hasChild[name] {
			outlets = append(outlets, name)
		}
	}
	for i := 1; i < len(outlets); i++ {
		for j := i; j > 0 && outlets[j-1] > outlets[j]; j-- {
			outlets[j-1], outlets[j] = outlets[j], outlets[j-1]
		}
	}
	return outlets
}
