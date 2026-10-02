package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// BeginOutputInvocation persists a fence before any remote tool invocation.
// Replaying a pending/settled slot never grants another external invocation.
func (e *Engine) BeginOutputInvocation(ctx context.Context, id tasks.TaskID, intent tasks.OutputInvocationIntent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.closed.Load() {
		return "", tasks.ErrRegistryClosed
	}
	if err := tasks.ValidateOutputInvocation(intent); err != nil {
		return "", err
	}
	q, ok := identity.QuadrupleFrom(ctx)
	bound, boundOK := tasks.OutputTaskFromContext(ctx)
	if !ok || !boundOK || bound != id {
		return "", tasks.ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return "", err
	}
	if q.Identity != t.Identity.Identity {
		return "", tasks.ErrNotFound
	}
	m := tasks.CloneOutputManifest(t.OutputManifest)
	if m == nil || m.Version != 1 {
		return "", tasks.ErrOutputProvenance
	}
	if intent.Position < m.Position {
		return "", tasks.ErrOutputInvocationSettled
	}
	for _, prior := range m.Invocations {
		if prior.Intent.Position == intent.Position && prior.Intent.Branch == intent.Branch {
			if prior.Intent != intent {
				return "", tasks.ErrIdempotencyConflict
			}
			if prior.State == "pending" {
				return prior.ID, tasks.ErrOutputInvocationUnknown
			}
			return prior.ID, tasks.ErrOutputInvocationSettled
		}
	}
	if t.Status != tasks.StatusRunning || m.Sealed || m.Uncertain {
		return "", tasks.ErrInvalidTransition
	}
	if intent.Position > m.Position {
		for _, prior := range m.Invocations {
			if prior.State == "pending" {
				return "", tasks.ErrOutputInvocationUnknown
			}
		}
		m.Position = intent.Position
		m.Invocations = nil
	}
	if len(m.Invocations) >= tasks.MaxOutputInvocationBranches {
		return "", tasks.ErrOutputProvenance
	}
	raw, err := json.Marshal(struct {
		Tenant, User, Session string
		TaskID                tasks.TaskID
		Position              int64
		Branch                int
	}{q.TenantID, q.UserID, q.SessionID, id, intent.Position, intent.Branch})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	invocation := hex.EncodeToString(sum[:])
	m.Invocations = append(m.Invocations, tasks.OutputInvocation{ID: invocation, Intent: intent, State: "pending"})
	t.OutputManifest = m
	t.UpdatedAt = time.Now().UnixNano()
	if err = e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		// Even an ambiguous failed write is a fence in this process. A durable
		// reopen fails interrupted tasks and never grants re-invocation.
		m.Uncertain = true
		return "", fmt.Errorf("persist output invocation admission: %w", err)
	}
	return invocation, nil
}

// FinishOutputInvocation seals one successful native materialization batch or
// a known tool error. It never converts failed/error-content refs into outputs.
func (e *Engine) FinishOutputInvocation(ctx context.Context, id tasks.TaskID, invocation string, artifacts []tasks.ProducedArtifact, successful bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed.Load() {
		return tasks.ErrRegistryClosed
	}
	q, ok := identity.QuadrupleFrom(ctx)
	bound, boundOK := tasks.OutputTaskFromContext(ctx)
	if !ok || !boundOK || bound != id {
		return tasks.ErrNotFound
	}
	if !successful && len(artifacts) != 0 {
		return tasks.ErrOutputProvenance
	}
	for _, ref := range artifacts {
		if ref.InvocationID != invocation || tasks.ValidateProducedArtifact(ref) != nil {
			return tasks.ErrOutputProvenance
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if q.Identity != t.Identity.Identity {
		return tasks.ErrNotFound
	}
	m := tasks.CloneOutputManifest(t.OutputManifest)
	if m == nil || m.Version != 1 || m.Sealed || m.Uncertain {
		return tasks.ErrOutputProvenance
	}
	index := -1
	for i, prior := range m.Invocations {
		if prior.ID == invocation {
			index = i
			break
		}
	}
	if index < 0 {
		return tasks.ErrOutputProvenance
	}
	desired := "failed"
	if successful {
		desired = "succeeded"
	}
	if m.Invocations[index].State != "pending" {
		if m.Invocations[index].State != desired {
			return tasks.ErrIdempotencyConflict
		}
		prior := []tasks.ProducedArtifact{}
		for _, ref := range m.Artifacts {
			if ref.InvocationID == invocation {
				prior = append(prior, ref)
			}
		}
		if !slices.Equal(prior, artifacts) {
			return tasks.ErrIdempotencyConflict
		}
		return nil
	}
	if t.Status != tasks.StatusRunning {
		return tasks.ErrInvalidTransition
	}
	if len(m.Artifacts)+len(artifacts) > tasks.MaxProducedArtifacts {
		return tasks.ErrOutputProvenance
	}
	m.Invocations[index].State = desired
	m.Artifacts = append(m.Artifacts, artifacts...)
	prior := t.OutputManifest
	t.OutputManifest = m
	if err = tasks.ValidateOutputManifest(t); err != nil {
		t.OutputManifest = prior
		return err
	}
	t.UpdatedAt = time.Now().UnixNano()
	if err = e.persistTaskLocked(ctx, t, e.contentHashLocked(t)); err != nil {
		m.Uncertain = true
		return fmt.Errorf("persist output invocation outcome: %w", err)
	}
	return nil
}
