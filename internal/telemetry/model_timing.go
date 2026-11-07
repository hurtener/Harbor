package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// ModelAttemptCoordinates distinguish Harbor retry, downgrade, and failover
// attempts. They contain no grant, credential, or route identifiers.
type ModelAttemptCoordinates struct {
	Attempt, Retry, Downgrade, FallbackHop int
}

// ModelCallTiming carries only content-free measurements for one driver call.
// Nil token counts and FirstDelta mean unreported, never an inferred zero.
type ModelCallTiming struct {
	Provider, Model, ReasoningEffort string
	Role                             string
	Total                            time.Duration
	FirstDelta                       *time.Duration
	InputTokens, OutputTokens        *int
	Failed, Canceled                 bool
	Attempt                          *ModelAttemptCoordinates
}

// RecordModelCall writes the shared completion/embedding timing schema.
// Role and outcome vocabularies are bounded here; callers supply identifiers
// and measurements only, never errors, inputs, responses, or endpoint values.
func RecordModelCall(ctx context.Context, logger *slog.Logger, call ModelCallTiming) {
	if logger == nil {
		logger = slog.Default()
	}
	role := call.Role
	switch role {
	case "planner", "repair", "compaction", "title", "skill_draft", "agent_proposal", "embedding":
	default:
		role = "completion"
	}
	outcome := "ok"
	if call.Failed {
		outcome = "error"
	}
	if call.Canceled {
		outcome = "canceled"
	}
	attrs := []slog.Attr{
		slog.String("step_kind", "model"), slog.String("role", role),
		slog.String("provider", modelTimingIdentifier(call.Provider)), slog.String("model", modelTimingIdentifier(call.Model)),
		slog.Float64("total_ms", float64(max(call.Total, 0))/float64(time.Millisecond)),
		slog.String("outcome", outcome),
	}
	switch call.ReasoningEffort {
	case "off", "low", "medium", "high":
		attrs = append(attrs, slog.String("reasoning_effort", call.ReasoningEffort))
	}
	if call.FirstDelta != nil {
		attrs = append(attrs, slog.Float64("ttft_ms", float64(max(*call.FirstDelta, 0))/float64(time.Millisecond)))
	}
	if call.InputTokens != nil && *call.InputTokens > 0 {
		attrs = append(attrs, slog.Int("input_tokens", *call.InputTokens))
	}
	if call.OutputTokens != nil && *call.OutputTokens > 0 {
		attrs = append(attrs, slog.Int("output_tokens", *call.OutputTokens))
	}
	if call.Attempt != nil {
		attrs = append(attrs, slog.Int("attempt", call.Attempt.Attempt), slog.Int("retry", call.Attempt.Retry),
			slog.Int("downgrade", call.Attempt.Downgrade), slog.Int("fallback_hop", call.Attempt.FallbackHop))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "llm.call.completed", appendCtxIdentity(ctx, attrs)...)
}

// Model and provider identifiers may contain routing slashes or colons, but
// never URL syntax, query parameters, whitespace, or unbounded opaque values.
func modelTimingIdentifier(value string) string {
	if len(value) > 256 || strings.Contains(value, "://") {
		return "redacted"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/:@", r) {
			continue
		}
		return "redacted"
	}
	return value
}
