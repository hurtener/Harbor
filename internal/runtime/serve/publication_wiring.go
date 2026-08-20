package serve

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills/publication"
	"github.com/hurtener/Harbor/internal/state"
)

// ErrPublicationWiringMisconfigured reports a missing authority required to
// mount the same-runtime publication surface. A publication body can only be
// composed on a run whose effective Agent reach was durably admitted, so a
// stack without the restart-stable admission authority keeps this surface
// unavailable rather than manufacturing a second authority.
var ErrPublicationWiringMisconfigured = errors.New("serve: skill publication wiring is misconfigured")

// NewSkillPublicationStore constructs the one StateStore-backed publication
// store shared by Protocol and run-start composition. The returned store is
// wrapped with the caller-provided signed Agent-reach authorizer and is bound
// to the immutable runtime/deployment identifier supplied by the serve
// assembly. The caller owns its Close lifecycle.
func NewSkillPublicationStore(st state.StateStore, runtimeID string, reach auth.AgentReachAuthorizer) (publication.Store, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: StateStore is nil", ErrPublicationWiringMisconfigured)
	}
	runtimeID = strings.TrimSpace(runtimeID)
	if runtimeID == "" {
		return nil, fmt.Errorf("%w: runtime/deployment ID is empty", ErrPublicationWiringMisconfigured)
	}
	if reach == nil {
		return nil, fmt.Errorf("%w: signed Agent-reach authorizer is nil", ErrPublicationWiringMisconfigured)
	}
	durable, err := publication.NewStateStore(st, runtimeID)
	if err != nil {
		return nil, fmt.Errorf("%w: StateStore publication store: %w", ErrPublicationWiringMisconfigured, err)
	}
	authorized, err := publication.NewAuthorizedStore(durable, reach)
	if err != nil {
		return nil, fmt.Errorf("%w: authorized publication store: %w", ErrPublicationWiringMisconfigured, err)
	}
	return authorized, nil
}
