package auth

import "context"

type runtimeIDContextKey struct{}

// WithRuntimeID carries runtime authority from a verified token or sealed admission.
// Callers must never supply a runtime ID from request bodies or configuration.
func WithRuntimeID(ctx context.Context, runtimeID string) context.Context {
	return context.WithValue(ctx, runtimeIDContextKey{}, runtimeID)
}

// RuntimeIDFrom returns the runtime identity from verified bearer authority.
func RuntimeIDFrom(ctx context.Context) string {
	runtimeID, ok := ctx.Value(runtimeIDContextKey{}).(string)
	if !ok {
		return ""
	}
	return runtimeID
}
