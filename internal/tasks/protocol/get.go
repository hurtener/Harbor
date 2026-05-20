package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Get implements the `tasks.get` Protocol method. It validates
// identity, then resolves the enriched single-task detail from the
// Projector. A TaskID outside the caller's tenant returns
// ErrTaskNotFound — existence is never revealed across tenants
// (CLAUDE.md §6; same posture `tasks.TaskRegistry.Get` already
// enforces). Heavy result content is referenced via ArtifactRef
// (D-026) — the Projector never inlines bytes above the heavy-content
// threshold.
func (s *Service) Get(ctx context.Context, req prototypes.TaskGetRequest) (prototypes.TaskDetail, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.TaskDetail{}, err
	}
	if strings.TrimSpace(req.ID) == "" {
		return prototypes.TaskDetail{}, fmt.Errorf("%w: task id is empty", ErrInvalidRequest)
	}
	detail, err := s.projector.GetTask(ctx, id, req.ID)
	if err != nil {
		return prototypes.TaskDetail{}, mapProjectorErr(err)
	}
	return detail, nil
}

// mapProjectorErr maps a Projector error onto the Service sentinel set
// so the wire handler can branch on a stable error. ErrTaskNotFound
// passes through unchanged; any other error is wrapped as a runtime
// failure.
func mapProjectorErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTaskNotFound) {
		return ErrTaskNotFound
	}
	return fmt.Errorf("tasks/protocol: projector: %w", err)
}
