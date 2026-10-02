package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactcontent"
)

func (e *toolExecutor) outputInvocationBranches(rc planner.RunContext) parallel.ExecuteOption {
	return parallel.WithDescriptorDecorator(func(index int, desc tools.ToolDescriptor) tools.ToolDescriptor {
		return e.withOutputInvocation(rc, index, desc)
	})
}

// withOutputInvocation is the native task's durable pre-I/O fence. The exact
// append-only trajectory position and branch slot survive process recovery;
// provider-authored call IDs cannot authorize a second invocation. A settled
// capture without a persisted observation requires attention, never a replay.
func (e *toolExecutor) withOutputInvocation(rc planner.RunContext, branch int, desc tools.ToolDescriptor) tools.ToolDescriptor {
	if desc.Tool.Form != tools.ToolFormTool {
		return desc
	}
	out := desc
	out.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		taskID, native := tasks.OutputTaskFromContext(ctx)
		if !native || steering.IsTrustedCompletionHook(ctx) {
			return desc.Invoke(ctx, args)
		}
		fail := func(err error) (tools.ToolResult, error) {
			return tools.ToolResult{}, fmt.Errorf("%w: output provenance: %w", tools.ErrInvocationCleanupFailed, err)
		}
		if e.tasks == nil || rc.Trajectory == nil {
			return fail(tasks.ErrOutputProvenance)
		}
		scope := artifacts.ArtifactScope{TenantID: rc.Quadruple.TenantID, UserID: rc.Quadruple.UserID, SessionID: rc.Quadruple.SessionID, TaskID: rc.Quadruple.RunID}
		var mu sync.Mutex
		var attempted, finished bool
		var invocation string
		var invocationCtx context.Context
		var innerErr, admissionErr error
		decorated := tools.DecorateInvocation(desc, func(invoke tools.Invocation) tools.Invocation {
			return func(callCtx context.Context, callArgs json.RawMessage) (tools.ToolResult, error) {
				mu.Lock()
				if attempted {
					admissionErr = tasks.ErrOutputInvocationUnknown
					mu.Unlock()
					return fail(tasks.ErrOutputInvocationUnknown)
				}
				attempted = true
				admit := func() error {
					if err := tools.CheckInvocationFence(callCtx); err != nil {
						return err
					}
					task, err := e.tasks.Get(callCtx, taskID)
					if err != nil {
						return err
					}
					q, ok := identity.QuadrupleFrom(callCtx)
					if !ok || q != rc.Quadruple || task.Identity.Identity != q.Identity {
						return tasks.ErrNotFound
					}
					request, err := json.Marshal(struct {
						Tool     string
						Args     json.RawMessage
						Revision uint64
					}{desc.Tool.Name, callArgs, task.AppliedInputRevision})
					if err != nil {
						return err
					}
					sum := sha256.Sum256(request)
					invocation, err = e.tasks.BeginOutputInvocation(callCtx, taskID, tasks.OutputInvocationIntent{Position: int64(len(rc.Trajectory.Steps)), Branch: branch, ToolName: desc.Tool.Name, RequestSHA256: hex.EncodeToString(sum[:])})
					if err != nil {
						return err
					}
					invocationCtx, err = artifactcontent.WithInvocation(callCtx, invocation, string(taskID), desc.Tool.Name, scope)
					return err
				}
				admissionErr = admit()
				if admissionErr != nil {
					err := admissionErr
					mu.Unlock()
					if errors.Is(err, tools.ErrInvocationSuperseded) || errors.Is(err, context.Canceled) {
						return tools.ToolResult{}, err
					}
					return fail(err)
				}
				bound := invocationCtx
				mu.Unlock()
				result, err := invoke(bound, callArgs)
				mu.Lock()
				innerErr = err
				finished = true
				mu.Unlock()
				return result, err
			}
		})
		// Run the actual outer wrapper chain. A veto/simulation cannot be bypassed,
		// and a post-invocation error cannot certify otherwise successful inner refs.
		result, invokeErr := decorated.Invoke(ctx, args)
		mu.Lock()
		admittedID, bound, completed, innerFailure, admitFailure := invocation, invocationCtx, finished, innerErr, admissionErr
		mu.Unlock()
		if admitFailure != nil {
			if errors.Is(admitFailure, tools.ErrInvocationSuperseded) || errors.Is(admitFailure, context.Canceled) {
				return tools.ToolResult{}, admitFailure
			}
			return fail(admitFailure)
		}
		if admittedID == "" {
			return result, invokeErr
		}
		if !completed {
			return fail(tasks.ErrOutputInvocationUnknown)
		}
		for _, err := range []error{innerFailure, invokeErr} {
			if errors.Is(err, tools.ErrToolResultMaterialization) || errors.Is(err, tools.ErrInvocationCleanupFailed) {
				return fail(err)
			}
		}
		successful := invokeErr == nil && innerFailure == nil
		var refs []tasks.ProducedArtifact
		if successful {
			value, witness, err := artifactcontent.MaterializeWithWitness(bound, e.artifacts, scope, result.Value, desc.Tool.Name)
			if err != nil {
				return fail(err)
			}
			result.Value = value
			if !witness.Present() {
				if carrier, ok := value.(artifactcontent.WitnessCarrier); ok {
					witness = carrier.MaterializedArtifactWitness()
				}
			}
			if witness.Present() {
				verified, err := witness.References(bound)
				if err != nil {
					return fail(err)
				}
				for _, ref := range verified {
					refs = append(refs, tasks.ProducedArtifact{ID: ref.ID, SHA256: ref.SHA256, MIMEType: ref.MIMEType, SizeBytes: ref.SizeBytes, InvocationID: admittedID, ContentIndex: ref.ContentIndex})
				}
			}
		}
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(bound), 5*time.Second)
		defer cancel()
		if err := e.tasks.FinishOutputInvocation(settleCtx, taskID, admittedID, refs, successful); err != nil {
			return fail(err)
		}
		return result, invokeErr
	}
	return out
}
