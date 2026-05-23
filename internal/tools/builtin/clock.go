package builtin

import (
	"context"
	"time"
)

// ClockNowArgs is the input shape for `clock.now`. The tool takes no
// arguments — the empty struct keeps the schema deriver
// (`inproc.DeriveSchema`) happy without forcing the planner to
// fabricate a payload.
type ClockNowArgs struct{}

// ClockNowOut is the result shape for `clock.now`. Both
// representations are returned so callers that need a string format
// (logging, prompt injection) and callers that need integer math
// (deduplication windows, freshness checks) get the value in the
// shape they want without re-parsing.
type ClockNowOut struct {
	RFC3339  string `json:"rfc3339"`
	EpochMS  int64  `json:"epoch_ms"`
	Timezone string `json:"timezone"`
}

// ClockNow returns the current UTC time. Pure / read-only — no
// dependency on identity, no side effect, safe for concurrent
// invocation.
func ClockNow(_ context.Context, _ ClockNowArgs) (ClockNowOut, error) {
	now := time.Now().UTC()
	return ClockNowOut{
		RFC3339:  now.Format(time.RFC3339),
		EpochMS:  now.UnixMilli(),
		Timezone: "UTC",
	}, nil
}
