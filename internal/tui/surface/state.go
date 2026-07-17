// Package surface defines honest loading and availability state for TUI views.
package surface

// Status distinguishes unavailable data from an authoritative empty result.
type Status string

const (
	Loading     Status = "loading"
	Loaded      Status = "loaded"
	Partial     Status = "partial"
	Unavailable Status = "unavailable"
	Failed      Status = "error"
)

// State describes one independently refreshed Protocol surface.
type State struct {
	Status Status
	Reason string
}

// Ready reports whether canonical rows may be rendered. Partial data is useful
// but must retain its partial label.
func (s State) Ready() bool { return s.Status == Loaded || s.Status == Partial }

// Label returns a non-empty honest operator label.
func (s State) Label() string {
	if s.Status == "" {
		return string(Loading)
	}
	if s.Reason == "" {
		return string(s.Status)
	}
	return string(s.Status) + ": " + s.Reason
}
