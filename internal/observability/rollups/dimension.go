package rollups

import (
	"fmt"
)

// Dimension is a closed rollup dimension. All values are AUTHORITATIVE: the
// tenant / user / session axes come from the event's identity triple (the
// isolation principal — never from payload fields a producer could choose),
// and model comes from the source event's model field. Agent is populated
// ONLY when the source event carries an authoritative agent id; none of the
// V1 canonical payloads do, so the agent axis is empty by construction until
// a payload that carries one lands. agent_id is NOT an isolation principal —
// it is a closed rollup axis like model, never a scoping key (CLAUDE.md §6).
type Dimension string

const (
	// DimensionTenant is the event's tenant_id.
	DimensionTenant Dimension = "tenant"
	// DimensionUser is the event's user_id.
	DimensionUser Dimension = "user"
	// DimensionSession is the event's session_id.
	DimensionSession Dimension = "session"
	// DimensionModel is the source payload's model (LLM completions only;
	// empty for events with no authoritative model).
	DimensionModel Dimension = "model"
	// DimensionAgent is the source payload's authoritative agent id when
	// one exists. No V1 canonical payload carries one — the axis is closed
	// and empty by construction (see the package doc).
	DimensionAgent Dimension = "agent"
)

// AllDimensions is the closed dimension set in canonical order.
var AllDimensions = [...]Dimension{
	DimensionTenant,
	DimensionUser,
	DimensionSession,
	DimensionModel,
	DimensionAgent,
}

// Validate reports whether d is a closed dimension.
func (d Dimension) Validate() error {
	switch d {
	case DimensionTenant, DimensionUser, DimensionSession, DimensionModel, DimensionAgent:
		return nil
	default:
		return fmt.Errorf("%w: dimension %q (allowed: %v)", ErrQueryInvalid, d, allDimensions())
	}
}

// ValidateDimensions validates a query's GroupBy: every member is closed and
// no member repeats. Returns a wrapped ErrQueryInvalid otherwise.
func ValidateDimensions(dims []Dimension) error {
	seen := make(map[Dimension]struct{}, len(dims))
	for _, d := range dims {
		if err := d.Validate(); err != nil {
			return err
		}
		if _, dup := seen[d]; dup {
			return fmt.Errorf("%w: GroupBy repeats dimension %q", ErrQueryInvalid, d)
		}
		seen[d] = struct{}{}
	}
	return nil
}

// DimensionValues maps a dimension to its value for a grouped result row.
// The map carries exactly the query's GroupBy dimensions; a missing entry
// means the empty value (a grouped row that aggregates un-attributed events,
// e.g. model "" for task outcomes).
type DimensionValues map[Dimension]string

// valueIn returns the row's value for a group dimension ("" when absent).
func (v DimensionValues) valueIn(d Dimension) string {
	if v == nil {
		return ""
	}
	return v[d]
}

// Less reports whether v sorts before w. Both rows carry the same GroupBy
// dimensions, compared in AllDimensions order — the comparison is total
// (every value is a Go string, so lexicographic order is total).
func (v DimensionValues) Less(w DimensionValues) bool {
	for _, d := range AllDimensions {
		if _, ok := v[d]; ok {
			if v[d] != w[d] {
				return v[d] < w[d]
			}
		}
	}
	return false
}

func allDimensions() []string {
	out := make([]string, 0, len(AllDimensions))
	for _, d := range AllDimensions {
		out = append(out, string(d))
	}
	return out
}
