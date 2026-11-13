package protocol

import (
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrInvalidRunLimits rejects a run-limit section with a non-positive or
// out-of-range value.
var ErrInvalidRunLimits = errors.New("agentcfg/protocol: invalid run limits")

// ValidateRunLimits checks an agent-config run-limit section: set values must
// be positive, and the duration at most the runtime's maximum.
func ValidateRunLimits(l *prototypes.AgentConfigRunLimits) error {
	if l == nil {
		return nil
	}
	maxSeconds := int64(config.MaxRunLimitDuration.Seconds())
	if l.MaxDurationSeconds != nil && (*l.MaxDurationSeconds <= 0 || *l.MaxDurationSeconds > maxSeconds) {
		return fmt.Errorf("%w: run_limits.max_duration_seconds must be between 1 and %d (omit it for no limit)", ErrInvalidRunLimits, maxSeconds)
	}
	if l.MaxTokens != nil && *l.MaxTokens <= 0 {
		return fmt.Errorf("%w: run_limits.max_tokens must be positive (omit it for no limit)", ErrInvalidRunLimits)
	}
	return nil
}

// runLimitsDiffToWire projects a changed run-limit delta; an unchanged delta
// is omitted from the wire.
func runLimitsDiffToWire(d agentcfg.RunLimitsDiff) *prototypes.AgentConfigRunLimitsDiff {
	if !d.Changed() {
		return nil
	}
	return &prototypes.AgentConfigRunLimitsDiff{
		MaxDurationChanged: d.MaxDurationChanged, MaxDurationFrom: d.MaxDurationFrom, MaxDurationTo: d.MaxDurationTo,
		MaxTokensChanged: d.MaxTokensChanged, MaxTokensFrom: d.MaxTokensFrom, MaxTokensTo: d.MaxTokensTo,
	}
}
