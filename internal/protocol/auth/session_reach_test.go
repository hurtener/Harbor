package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestParseSessionReach_StrictBoundedShape(t *testing.T) {
	tooLong := strings.Repeat("a", maxSessionReachIDBytes+1)
	cases := []struct {
		name string
		raw  any
		want []string
		bad  bool
	}{
		{name: "signed null", raw: nil, bad: true},
		{name: "empty", raw: []any{}, want: []string{}},
		{name: "valid", raw: []any{"sess-a", "sess-b"}, want: []string{"sess-a", "sess-b"}},
		{name: "not array", raw: "sess-a", bad: true},
		{name: "non string", raw: []any{1}, bad: true},
		{name: "blank", raw: []any{" "}, bad: true},
		{name: "duplicate", raw: []any{"sess-a", "sess-a"}, bad: true},
		{name: "too long", raw: []any{tooLong}, bad: true},
	}
	tooMany := make([]any, maxSessionReachIDs+1)
	for i := range tooMany {
		tooMany[i] = "sess"
	}
	cases = append(cases, struct {
		name string
		raw  any
		want []string
		bad  bool
	}{name: "too many", raw: tooMany, bad: true})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSessionReach(tc.raw)
			if tc.bad {
				if !errors.Is(err, ErrSessionReachMalformed) {
					t.Fatalf("ParseSessionReach() error = %v, want ErrSessionReachMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSessionReach() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseSessionReach() = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseSessionReach() = %#v, want %#v", got, tc.want)
				}
			}
		})
	}
}

// TestSessionReachGate_AbsencePreservesDynamicSelection pins the D-409
// core semantic: an ABSENT session_reach claim must not restrict the
// effective session — D-171's per-request dynamic selection is preserved
// exactly. Presence (even empty) restricts; an empty present set grants
// no session.
func TestSessionReachGate_AbsencePreservesDynamicSelection(t *testing.T) {
	gate := NewSessionReachAuthorizer()

	// No reach authority on ctx (the absent-claim shape) — any
	// nonblank effective session passes.
	if err := gate.AuthorizeSessionReach(context.Background(), "any-session"); err != nil {
		t.Fatalf("absent claim must preserve dynamic selection: %v", err)
	}
	// A context that never established JWT authority fails closed for
	// an empty target (nothing is ever resolvable without a session).
	if err := gate.AuthorizeSessionReach(context.Background(), ""); !errors.Is(err, ErrSessionReachDenied) {
		t.Fatalf("empty effective session error = %v, want ErrSessionReachDenied", err)
	}

	// Present reach: member passes, non-member denies.
	allowed := WithSessionReach(context.Background(), []string{"sess-a"})
	if err := gate.AuthorizeSessionReach(allowed, "sess-a"); err != nil {
		t.Fatalf("member session: %v", err)
	}
	if err := gate.AuthorizeSessionReach(allowed, "sess-b"); !errors.Is(err, ErrSessionReachDenied) {
		t.Fatalf("excluded session error = %v, want ErrSessionReachDenied", err)
	}

	// Explicit empty present set grants no session at all.
	for _, ctx := range []context.Context{
		WithSessionReach(context.Background(), nil),
		WithSessionReach(context.Background(), []string{}),
	} {
		if err := gate.AuthorizeSessionReach(ctx, "sess-a"); !errors.Is(err, ErrSessionReachDenied) {
			t.Fatalf("explicit empty reach error = %v, want ErrSessionReachDenied", err)
		}
	}

	// A blank target is never a member, even when the claim is absent.
	if err := gate.AuthorizeSessionReach(allowed, " "); !errors.Is(err, ErrSessionReachDenied) {
		t.Fatalf("blank target error = %v, want ErrSessionReachDenied", err)
	}
}

func TestSessionReachGate_ConcurrentIsolation_N100(t *testing.T) {
	gate := NewSessionReachAuthorizer()
	var wg sync.WaitGroup
	errCh := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-a"
			if i%2 == 1 {
				id = "sess-b"
			}
			ctx := WithSessionReach(context.Background(), []string{id})
			if err := gate.AuthorizeSessionReach(ctx, id); err != nil {
				errCh <- err
				return
			}
			// Cross-talk probe: a goroutine granted sess-b must not
			// authorize sess-a.
			if err := gate.AuthorizeSessionReach(ctx, "sess-a"); id == "sess-b" && !errors.Is(err, ErrSessionReachDenied) {
				errCh <- err
			}
			// Absence probe: the same shared gate with no authority on
			// ctx must stay permissive (dynamic selection preserved).
			if err := gate.AuthorizeSessionReach(context.Background(), id); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent gate result = %v", err)
	}
}
