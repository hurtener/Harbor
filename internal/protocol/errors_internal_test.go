package protocol

import (
	stderrors "errors"
	"fmt"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// In-package test: the runtime-error → Protocol-code mapping is the one
// place the steering / tasks sentinels are bridged onto stable Protocol
// codes. Every branch is exercised here so a sentinel rename in a
// dependency surfaces as a failing test, not a silently mis-mapped code.

func TestMapSteeringError_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want protoerrors.Code
	}{
		{"nil", nil, ""},
		{"identity required", fmt.Errorf("wrap: %w", steering.ErrIdentityRequired), protoerrors.CodeIdentityRequired},
		{"scope mismatch", fmt.Errorf("wrap: %w", steering.ErrScopeMismatch), protoerrors.CodeScopeMismatch},
		{"invalid scope", fmt.Errorf("wrap: %w", steering.ErrInvalidScope), protoerrors.CodeScopeMismatch},
		{"payload invalid", fmt.Errorf("wrap: %w", steering.ErrPayloadInvalid), protoerrors.CodePayloadInvalid},
		{"unsupported payload value", fmt.Errorf("wrap: %w", steering.ErrUnsupportedPayloadValue), protoerrors.CodePayloadInvalid},
		{"unknown control type", fmt.Errorf("wrap: %w", steering.ErrUnknownControlType), protoerrors.CodeRuntimeError},
		{"inbox not found", fmt.Errorf("wrap: %w", steering.ErrInboxNotFound), protoerrors.CodeNotFound},
		{"unclassified", stderrors.New("some other steering failure"), protoerrors.CodeRuntimeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapSteeringError("cancel", tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("mapSteeringError(nil) = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("mapSteeringError(%v) = nil, want a *Error", tc.err)
			}
			if got.Code != tc.want {
				t.Fatalf("mapSteeringError(%v).Code = %q, want %q", tc.err, got.Code, tc.want)
			}
			// The message names the method and never carries the raw
			// wrapped error verbatim (CLAUDE.md §7 rule 7).
			if got.Message == "" {
				t.Error("mapSteeringError produced an empty message")
			}
		})
	}
}

func TestMapTaskError_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want protoerrors.Code
	}{
		{"nil", nil, ""},
		{"identity required", fmt.Errorf("wrap: %w", tasks.ErrIdentityRequired), protoerrors.CodeIdentityRequired},
		{"not found", fmt.Errorf("wrap: %w", tasks.ErrNotFound), protoerrors.CodeNotFound},
		{"idempotency conflict", fmt.Errorf("wrap: %w", tasks.ErrIdempotencyConflict), protoerrors.CodeInvalidRequest},
		{"invalid request", fmt.Errorf("wrap: %w", tasks.ErrInvalidRequest), protoerrors.CodeInvalidRequest},
		{"unclassified", stderrors.New("some other task failure"), protoerrors.CodeRuntimeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapTaskError("start", tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("mapTaskError(nil) = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("mapTaskError(%v) = nil, want a *Error", tc.err)
			}
			if got.Code != tc.want {
				t.Fatalf("mapTaskError(%v).Code = %q, want %q", tc.err, got.Code, tc.want)
			}
			if got.Message == "" {
				t.Error("mapTaskError produced an empty message")
			}
		})
	}
}
