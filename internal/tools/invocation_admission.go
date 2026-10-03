package tools

import (
	"context"
	"errors"
	"fmt"
)

// InvocationAdmission is the runtime-owned acceptance lifecycle for a direct
// tool invocation. Native pause owners may park it only after establishing their
// own pause, then must reacquire before letting the original descriptor proceed.
// It grants no approval, OAuth credential, or execution authority itself.
type InvocationAdmission interface {
	ParkForNative(context.Context) error
	ResumeAfterNative(context.Context) error
	EffectStarted(context.Context) error
}

// ErrInvocationAdmission identifies a runtime-owned native handoff refusal.
var ErrInvocationAdmission = errors.New("tools: invocation admission refused")

type invocationAdmissionKey struct{}
type invocationAdmissionValue struct{ admission InvocationAdmission }

// WithInvocationAdmission attaches an immutable per-invocation lifecycle handle.
// This is a trusted runtime operation, never a wire or model-provided value.
func WithInvocationAdmission(ctx context.Context, admission InvocationAdmission) context.Context {
	return context.WithValue(ctx, invocationAdmissionKey{}, invocationAdmissionValue{admission: admission})
}

// ParkInvocationAdmission releases acceptance after a native pause is established.
// Ordinary task invocations without a direct-admission handle are unchanged.
func ParkInvocationAdmission(ctx context.Context) error {
	if value, ok := ctx.Value(invocationAdmissionKey{}).(invocationAdmissionValue); ok && value.admission != nil {
		admission := value.admission
		if err := admission.ParkForNative(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrInvocationAdmission, err)
		}
		return nil
	}
	return nil
}

// ResumeInvocationAdmission rechecks current authority before a natively approved
// invocation proceeds. A rejected or uncertain reacquisition must stop execution.
func ResumeInvocationAdmission(ctx context.Context) error {
	if value, ok := ctx.Value(invocationAdmissionKey{}).(invocationAdmissionValue); ok && value.admission != nil {
		admission := value.admission
		if err := admission.ResumeAfterNative(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrInvocationAdmission, err)
		}
		return nil
	}
	return nil
}

// MarkInvocationEffectStarted marks a concrete transport dispatch. Once any
// attempt may have had external effects, a later native challenge cannot release
// the same admission as if it were a pre-invocation pause.
func MarkInvocationEffectStarted(ctx context.Context) error {
	if value, ok := ctx.Value(invocationAdmissionKey{}).(invocationAdmissionValue); ok && value.admission != nil {
		admission := value.admission
		if err := admission.EffectStarted(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrInvocationAdmission, err)
		}
	}
	return nil
}
