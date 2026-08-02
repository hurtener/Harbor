package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestParseAgentReach_StrictBoundedShape(t *testing.T) {
	tooLong := strings.Repeat("a", maxAgentReachIDBytes+1)
	cases := []struct {
		name string
		raw  any
		want []string
		bad  bool
	}{
		{name: "absent", raw: nil},
		{name: "empty", raw: []any{}, want: []string{}},
		{name: "valid", raw: []any{"agent-a", "agent-b"}, want: []string{"agent-a", "agent-b"}},
		{name: "not array", raw: "agent-a", bad: true},
		{name: "non string", raw: []any{1}, bad: true},
		{name: "blank", raw: []any{" "}, bad: true},
		{name: "duplicate", raw: []any{"agent-a", "agent-a"}, bad: true},
		{name: "too long", raw: []any{tooLong}, bad: true},
	}
	tooMany := make([]any, maxAgentReachIDs+1)
	for i := range tooMany {
		tooMany[i] = "agent"
	}
	cases = append(cases, struct {
		name string
		raw  any
		want []string
		bad  bool
	}{name: "too many", raw: tooMany, bad: true})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentReach(tc.raw)
			if tc.bad {
				if !errors.Is(err, ErrAgentReachMalformed) {
					t.Fatalf("ParseAgentReach() error = %v, want ErrAgentReachMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAgentReach() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseAgentReach() = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseAgentReach() = %#v, want %#v", got, tc.want)
				}
			}
		})
	}
}

func TestReachAuthorizer_FailsClosedAndDoesNotBleed(t *testing.T) {
	gate := NewAgentReachAuthorizer()
	allowed := WithAgentReach(context.Background(), []string{"agent-a"})
	if err := gate.AuthorizeAgentReach(allowed, "agent-a"); err != nil {
		t.Fatalf("allowed target: %v", err)
	}
	for _, ctx := range []context.Context{context.Background(), WithAgentReach(context.Background(), nil), WithAgentReach(context.Background(), []string{})} {
		if err := gate.AuthorizeAgentReach(ctx, "agent-a"); !errors.Is(err, ErrAgentReachDenied) {
			t.Fatalf("missing or empty authority error = %v, want ErrAgentReachDenied", err)
		}
	}
	if err := gate.AuthorizeAgentReach(allowed, "agent-b"); !errors.Is(err, ErrAgentReachDenied) {
		t.Fatalf("excluded target error = %v, want ErrAgentReachDenied", err)
	}
	if err := gate.AuthorizeAgentReach(allowed, ""); !errors.Is(err, ErrAgentReachDenied) {
		t.Fatalf("empty target error = %v, want ErrAgentReachDenied", err)
	}
}

func TestReachAuthorizer_ConcurrentIsolation_N100(t *testing.T) {
	gate := NewAgentReachAuthorizer()
	var wg sync.WaitGroup
	errCh := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "agent-a"
			if i%2 == 1 {
				id = "agent-b"
			}
			ctx := WithAgentReach(context.Background(), []string{id})
			if err := gate.AuthorizeAgentReach(ctx, id); err != nil {
				errCh <- err
				return
			}
			if err := gate.AuthorizeAgentReach(ctx, "agent-a"); id == "agent-b" && !errors.Is(err, ErrAgentReachDenied) {
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
