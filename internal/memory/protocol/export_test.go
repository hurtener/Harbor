package protocol

// Test-only exports — only compiled into _test binaries (CLAUDE.md §13
// "Test stubs as production defaults" — a test seam in an _test.go file
// is the sanctioned shape). Used by the same-package leak test in
// leak_internal_test.go to exercise the D-026 defence-in-depth branch
// in buildDetail that the public Get path cannot reach (the public
// path's snapshotTurns sets HeavyContent from the same bytes it would
// inline, so the leak branch is unreachable through Get).

import (
	"context"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// BuildDetailLeakProbe runs buildDetail against a deliberately
// mis-classified row: HeavyContent=false but a value whose byte length
// meets/exceeds threshold. This models a future driver / projection bug
// that would let a heavy value reach the inline path; buildDetail MUST
// fail loudly with ErrContextLeak (D-026). Returns the buildDetail
// error so the test can assert errors.Is(err, ErrContextLeak).
func BuildDetailLeakProbe(ctx context.Context, store artifacts.ArtifactStore, threshold int, heavyValue []byte, id identity.Quadruple) error {
	row := projectedTurn{
		item: prototypes.MemoryItem{
			Key:          "mem_leakprobe",
			Strategy:     string(prototypes.MemoryStrategyTruncation),
			Scope:        string(prototypes.MemoryScopeSession),
			HeavyContent: false, // deliberately wrong — the bug shape
			SizeBytes:    int64(len(heavyValue)),
		},
		value: heavyValue,
	}
	_, err := buildDetail(ctx, GetDeps{
		Artifacts:      store,
		HeavyThreshold: threshold,
	}, row, id)
	return err
}
