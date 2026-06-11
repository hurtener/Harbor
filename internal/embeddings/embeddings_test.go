package embeddings

// Phase 84d (D-191) — unit tests for the Embedder seam: registry
// dispatch, the identity-mandatory fail-closed edge, input
// validation, and the shared Cosine ranking primitive.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// fakeDriver is a registry-test driver. Lives in _test.go only —
// never a production default (AGENTS.md §13).
type fakeDriver struct {
	calls  atomic.Int64
	closed atomic.Bool
}

func (f *fakeDriver) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (f *fakeDriver) Close(_ context.Context) error {
	f.closed.Store(true)
	return nil
}

func identityCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestOpen_UnknownDriver_ListsRegistered(t *testing.T) {
	_, err := Open(context.Background(), ConfigSnapshot{Driver: "nope"}, Deps{})
	if !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("err=%v, want ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "registered:") {
		t.Errorf("error must list registered drivers, got %q", err.Error())
	}
}

func TestOpen_RejectsInvalidSnapshot(t *testing.T) {
	if _, err := Open(context.Background(), ConfigSnapshot{Dimensions: -1}, Deps{}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Dimensions=-1: err=%v, want ErrInvalidConfig", err)
	}
	if _, err := Open(context.Background(), ConfigSnapshot{Timeout: -1}, Deps{}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Timeout=-1: err=%v, want ErrInvalidConfig", err)
	}
}

func TestOpen_DispatchesRegisteredDriver_AndGuards(t *testing.T) {
	fake := &fakeDriver{}
	Register("fake-dispatch", func(_ ConfigSnapshot, _ Deps) (Embedder, error) {
		return fake, nil
	})
	emb, err := Open(context.Background(), ConfigSnapshot{Driver: "fake-dispatch"}, Deps{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Run("Embed_FailsClosedOnMissingIdentity", func(t *testing.T) {
		_, err := emb.Embed(context.Background(), []string{"hello"})
		if !errors.Is(err, ErrIdentityMissing) {
			t.Fatalf("err=%v, want ErrIdentityMissing", err)
		}
		if fake.calls.Load() != 0 {
			t.Error("driver was reached despite missing identity — the edge must fail closed BEFORE the driver")
		}
	})

	t.Run("Embed_RejectsEmptyInput", func(t *testing.T) {
		ctx := identityCtx(t)
		if _, err := emb.Embed(ctx, nil); !errors.Is(err, ErrEmptyInput) {
			t.Errorf("nil texts: err=%v, want ErrEmptyInput", err)
		}
		if _, err := emb.Embed(ctx, []string{"ok", ""}); !errors.Is(err, ErrEmptyInput) {
			t.Errorf("empty element: err=%v, want ErrEmptyInput", err)
		}
		if fake.calls.Load() != 0 {
			t.Error("driver was reached with empty input")
		}
	})

	t.Run("Embed_HappyPath", func(t *testing.T) {
		vecs, err := emb.Embed(identityCtx(t), []string{"a", "b"})
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(vecs) != 2 {
			t.Fatalf("got %d vectors, want 2", len(vecs))
		}
	})

	t.Run("Embed_HonoursQuadrupleIdentity", func(t *testing.T) {
		ctx, err := identity.WithRun(context.Background(),
			identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "r1")
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		if _, err := emb.Embed(ctx, []string{"a"}); err != nil {
			t.Fatalf("Embed with quadruple identity: %v", err)
		}
	})

	t.Run("Embed_HonoursCancelledCtx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(identityCtx(t))
		cancel()
		if _, err := emb.Embed(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, want context.Canceled", err)
		}
	})

	t.Run("Close_Delegates", func(t *testing.T) {
		if err := emb.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !fake.closed.Load() {
			t.Error("Close did not reach the driver")
		}
	})
}

func TestOpen_RejectsShapeMismatchFromDriver(t *testing.T) {
	Register("fake-short", func(_ ConfigSnapshot, _ Deps) (Embedder, error) {
		return shortDriver{}, nil
	})
	emb, err := Open(context.Background(), ConfigSnapshot{Driver: "fake-short"}, Deps{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = emb.Embed(identityCtx(t), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "2 texts") {
		t.Fatalf("err=%v, want a loud vectors-for-texts mismatch", err)
	}
}

// shortDriver returns one vector regardless of input — exercises the
// guard's shape check.
type shortDriver struct{}

func (shortDriver) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}
func (shortDriver) Close(_ context.Context) error { return nil }

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	Register("fake-dup", func(_ ConfigSnapshot, _ Deps) (Embedder, error) { return &fakeDriver{}, nil })
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	Register("fake-dup", func(_ ConfigSnapshot, _ Deps) (Embedder, error) { return &fakeDriver{}, nil })
}

func TestCosine_MathAndErrors(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Cosine(tc.a, tc.b)
			if err != nil {
				t.Fatalf("Cosine: %v", err)
			}
			if math.Abs(got-tc.want) > 1e-6 {
				t.Errorf("Cosine=%v, want %v", got, tc.want)
			}
		})
	}

	if _, err := Cosine([]float32{1, 2}, []float32{1}); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("dim mismatch: err=%v, want ErrDimensionMismatch", err)
	}
	if _, err := Cosine(nil, []float32{1}); !errors.Is(err, ErrEmptyInput) {
		t.Errorf("empty: err=%v, want ErrEmptyInput", err)
	}
	got, err := Cosine([]float32{0, 0}, []float32{1, 1})
	if err != nil || got != 0 {
		t.Errorf("zero-norm: got (%v, %v), want (0, nil)", got, err)
	}
}

func TestRegisteredDrivers_SortedAndContainsFakes(t *testing.T) {
	Register("fake-z", func(_ ConfigSnapshot, _ Deps) (Embedder, error) { return &fakeDriver{}, nil })
	Register("fake-a", func(_ ConfigSnapshot, _ Deps) (Embedder, error) { return &fakeDriver{}, nil })
	names := RegisteredDrivers()
	var za, aa int
	for i, n := range names {
		if n == "fake-z" {
			za = i
		}
		if n == "fake-a" {
			aa = i
		}
	}
	if aa > za {
		t.Errorf("RegisteredDrivers not sorted: %v", names)
	}
	if !sortedStrings(names) {
		t.Errorf("RegisteredDrivers not sorted: %v", names)
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func TestHasIdentity(t *testing.T) {
	if HasIdentity(context.Background()) {
		t.Error("bare ctx reported an identity")
	}
	if !HasIdentity(identityCtx(t)) {
		t.Error("identity-stamped ctx reported no identity")
	}
}

// TestGuard_ErrorsPassThrough pins that driver errors surface
// unwrapped-compatible (errors.Is) through the guard.
func TestGuard_ErrorsPassThrough(t *testing.T) {
	sentinel := errors.New("driver boom")
	Register("fake-err", func(_ ConfigSnapshot, _ Deps) (Embedder, error) {
		return errDriver{err: sentinel}, nil
	})
	emb, err := Open(context.Background(), ConfigSnapshot{Driver: "fake-err"}, Deps{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := emb.Embed(identityCtx(t), []string{"a"}); !errors.Is(err, sentinel) {
		t.Errorf("err=%v, want the driver sentinel", err)
	}
}

type errDriver struct{ err error }

func (d errDriver) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, fmt.Errorf("embed: %w", d.err)
}
func (d errDriver) Close(_ context.Context) error { return nil }
