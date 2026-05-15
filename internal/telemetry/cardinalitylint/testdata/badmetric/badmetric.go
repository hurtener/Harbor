// Package badmetric is a NEGATIVE fixture for the cardinality-lint
// checker — it deliberately contains the two cardinality violations
// ScanMetricsTree must catch. It lives under testdata/ so it is NOT
// compiled into the Harbor build; cardinalitylint_violation_test.go
// points ScanMetricsTree at this directory and asserts both violations
// are reported.
//
// DO NOT "fix" this file — the violations are the point.
package badmetric

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Identity mimics the shape of identity.Quadruple — the field names are
// what identityFieldSelector matches on.
type Identity struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
}

// Event mimics the shape of events.Event for the fixture.
type Event struct {
	Type     string
	Identity Identity
}

// recordBad is the violating code: a metric counter Add whose label set
// contains BOTH a forbidden literal label key ("run_id") AND a label
// value sourced from an Identity field (ev.Identity.RunID). The checker
// must report one KindForbiddenLabelKey and one KindIdentitySourcedLabel.
func recordBad(ctx context.Context, counter metric.Int64Counter, ev Event) {
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event_type", ev.Type),
		// Violation 1 — KindForbiddenLabelKey: "run_id" literal key.
		attribute.String("run_id", "whatever"),
		// Violation 2 — KindIdentitySourcedLabel: value off Identity.RunID.
		attribute.String("ok_key", ev.Identity.RunID),
	))
}

// spanLikeIsFine proves the checker does NOT flag a forbidden label
// inside a NON-metric.WithAttributes context — this would be a span
// attribute list, which legitimately carries run_id. It is here so the
// negative test can also assert "exactly two violations, not three".
func spanLikeIsFine(ev Event) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("run_id", ev.Identity.RunID),
	}
}
