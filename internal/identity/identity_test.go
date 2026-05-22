package identity

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidate_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		id      Identity
		wantErr bool
	}{
		{"all-empty", Identity{}, true},
		{"empty-tenant", Identity{UserID: "u", SessionID: "s"}, true},
		{"empty-user", Identity{TenantID: "t", SessionID: "s"}, true},
		{"empty-session", Identity{TenantID: "t", UserID: "u"}, true},
		{"populated", Identity{TenantID: "t", UserID: "u", SessionID: "s"}, false},
		{"whitespace-passes", Identity{TenantID: " ", UserID: "\t", SessionID: "\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate(%+v) returned nil, want error", tc.id)
				}
				if !errors.Is(err, ErrIdentityIncomplete) {
					t.Fatalf("Validate err=%v, want errors.Is ErrIdentityIncomplete", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%+v) returned %v, want nil", tc.id, err)
			}
		})
	}
}

func TestWith_FailsClosedOnInvalid(t *testing.T) {
	in := context.Background()
	out, err := With(in, Identity{})
	if err == nil {
		t.Fatalf("With(empty) returned nil error, want ErrIdentityIncomplete")
	}
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("With err=%v, want errors.Is ErrIdentityIncomplete", err)
	}
	if out != in {
		t.Fatalf("With on invalid input returned a new ctx; want unchanged input ctx")
	}
}

func TestWith_RoundTrip(t *testing.T) {
	want := Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	ctx, err := With(context.Background(), want)
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	got, ok := From(ctx)
	if !ok {
		t.Fatalf("From returned ok=false")
	}
	if got != want {
		t.Fatalf("From returned %+v, want %+v", got, want)
	}
}

func TestFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	id, ok := From(context.Background())
	if ok {
		t.Fatalf("From on bare ctx returned ok=true")
	}
	if id != (Identity{}) {
		t.Fatalf("From on bare ctx returned %+v, want zero", id)
	}
}

func TestMustFrom_PanicsWithSentinelOnAbsence(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("MustFrom did not panic on bare ctx")
		}
		err, ok := r.(error)
		if !ok {
			t.Fatalf("recovered value %v is not an error", r)
		}
		if !errors.Is(err, ErrIdentityMissing) {
			t.Fatalf("MustFrom panicked with %v, want errors.Is ErrIdentityMissing", err)
		}
	}()
	_ = MustFrom(context.Background())
}

func TestMustFrom_ReturnsIdentityWhenPresent(t *testing.T) {
	want := Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := With(context.Background(), want)
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	if got := MustFrom(ctx); got != want {
		t.Fatalf("MustFrom returned %+v, want %+v", got, want)
	}
}

func TestWithRun_FailsClosedOnInvalidIdentity(t *testing.T) {
	_, err := WithRun(context.Background(), Identity{}, "run-1")
	if err == nil {
		t.Fatalf("WithRun(empty id) returned nil error")
	}
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("WithRun err=%v, want errors.Is ErrIdentityIncomplete", err)
	}
}

func TestWithRun_FailsClosedOnEmptyRunID(t *testing.T) {
	id := Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	_, err := WithRun(context.Background(), id, "")
	if err == nil {
		t.Fatalf("WithRun(empty run_id) returned nil error")
	}
	if !errors.Is(err, ErrIdentityIncomplete) {
		t.Fatalf("WithRun err=%v, want errors.Is ErrIdentityIncomplete", err)
	}
}

func TestWithRun_RoundTrip(t *testing.T) {
	id := Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := WithRun(context.Background(), id, "run-42")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	q, ok := QuadrupleFrom(ctx)
	if !ok {
		t.Fatalf("QuadrupleFrom returned ok=false")
	}
	want := Quadruple{Identity: id, RunID: "run-42"}
	if q != want {
		t.Fatalf("QuadrupleFrom=%+v, want %+v", q, want)
	}
}

func TestQuadrupleFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	q, ok := QuadrupleFrom(context.Background())
	if ok {
		t.Fatalf("QuadrupleFrom on bare ctx returned ok=true")
	}
	if q != (Quadruple{}) {
		t.Fatalf("QuadrupleFrom returned %+v, want zero", q)
	}
}

func TestMustQuadrupleFrom_PanicsWithSentinelOnAbsence(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("MustQuadrupleFrom did not panic on bare ctx")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrIdentityMissing) {
			t.Fatalf("MustQuadrupleFrom panicked with %v, want ErrIdentityMissing", r)
		}
	}()
	_ = MustQuadrupleFrom(context.Background())
}

func TestMustQuadrupleFrom_ReturnsQuadrupleWhenPresent(t *testing.T) {
	id := Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := WithRun(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	want := Quadruple{Identity: id, RunID: "run-1"}
	if got := MustQuadrupleFrom(ctx); got != want {
		t.Fatalf("MustQuadrupleFrom=%+v, want %+v", got, want)
	}
}

func TestKeysIndependent_WithDoesNotSatisfyQuadrupleFrom(t *testing.T) {
	ctx, err := With(context.Background(), Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	if _, ok := QuadrupleFrom(ctx); ok {
		t.Fatalf("With-derived ctx satisfied QuadrupleFrom; keys are not independent")
	}
}

func TestKeysIndependent_WithRunDoesNotSatisfyFrom(t *testing.T) {
	ctx, err := WithRun(context.Background(), Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run-1")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	if _, ok := From(ctx); ok {
		t.Fatalf("WithRun-derived ctx satisfied From; keys are not independent")
	}
}

func TestKeysIndependent_ComposedSatisfiesBoth(t *testing.T) {
	id := Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := With(context.Background(), id)
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	ctx, err = WithRun(ctx, id, "run-1")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	if _, ok := From(ctx); !ok {
		t.Fatalf("composed ctx did not satisfy From")
	}
	if _, ok := QuadrupleFrom(ctx); !ok {
		t.Fatalf("composed ctx did not satisfy QuadrupleFrom")
	}
}

func TestIdentity_RaceFreeConcurrentDerivedCtx(t *testing.T) {
	const goroutines = 1024
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var mismatches atomic.Int64
	root := context.Background()
	start := make(chan struct{})

	wg.Add(goroutines)
	for i := range goroutines {

		go func() {
			defer wg.Done()
			<-start
			want := Identity{
				TenantID:  itoa("t", i%17),
				UserID:    itoa("u", i%41),
				SessionID: itoa("s", i),
			}
			ctx, err := With(root, want)
			if err != nil {
				mismatches.Add(1)
				return
			}
			got, ok := From(ctx)
			if !ok || got != want {
				mismatches.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d/%d goroutines observed cross-talk", n, goroutines)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

func itoa(prefix string, i int) string {
	const digits = "0123456789"
	if i == 0 {
		return prefix + "-0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return prefix + "-" + string(buf[pos:])
}
