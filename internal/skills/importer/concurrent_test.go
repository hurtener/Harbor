package importer_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

var (
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
)

// TestConcurrent_NEqual128_SharedImporter exercises the D-025
// concurrent-reuse contract on the importer. N=128 goroutines each
// build a distinct in-memory Skills.md payload, import + export it
// against a single shared *Importer, and assert:
//
//   - No data races (the race detector is the gate).
//   - No context bleed (the produced Skill's Name encodes the
//     goroutine's index — every goroutine sees its own bytes back).
//   - No cross-cancellation (pre-cancelled ctxes on i%5==0 return
//     ctx.Err() without affecting siblings).
//   - No goroutine leak (the runtime.NumGoroutine baseline is
//     restored within 500ms of WaitGroup.Wait).
func TestConcurrent_NEqual128_SharedImporter(t *testing.T) {
	const N = 128

	imp, _ := newImporter(t)

	baselineG := runtime.NumGoroutine()

	var (
		wg              sync.WaitGroup
		successes       atomic.Int64
		canceledRejects atomic.Int64
	)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx := context.Background()
			if idx%5 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel() // pre-cancel
			}
			src := buildPayload(idx)
			skill, imports, err := imp.Import(ctx, importer.ImportSource{
				Bytes:    src,
				PathHint: fmt.Sprintf("goroutine-%d.md", idx),
				Scope: artifacts.ArtifactScope{
					TenantID:  fmt.Sprintf("t-%d", idx),
					UserID:    fmt.Sprintf("u-%d", idx),
					SessionID: fmt.Sprintf("s-%d", idx),
					TaskID:    fmt.Sprintf("task-%d", idx),
				},
			})
			if err != nil {
				if ctx.Err() != nil {
					canceledRejects.Add(1)
					return
				}
				t.Errorf("[%d] Import: %v", idx, err)
				return
			}
			// Per-goroutine identity assertion: the Skill's Name
			// encodes idx so cross-goroutine bleed would show up
			// as a mismatch.
			wantName := fmt.Sprintf("goroutine-%d", idx)
			if skill.Name != wantName {
				t.Errorf("[%d] Name = %q, want %q (context bleed?)", idx, skill.Name, wantName)
				return
			}
			exported, err := imp.Export(ctx, skill, imports)
			if err != nil {
				if ctx.Err() != nil {
					canceledRejects.Add(1)
					return
				}
				t.Errorf("[%d] Export: %v", idx, err)
				return
			}
			if !bytes.Equal(src, exported) {
				t.Errorf("[%d] round-trip drifted", idx)
				return
			}
			successes.Add(1)
		}(i)
	}
	wg.Wait()

	// Goroutine leak check: baseline-restored within 500ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baselineG+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > baselineG+2 {
		t.Errorf("goroutine leak: started %d, ended %d", baselineG, runtime.NumGoroutine())
	}

	// At least the non-cancelled fraction must have succeeded.
	expectedSuccess := int64(N) - int64(N/5+1)
	if successes.Load() < expectedSuccess-1 {
		t.Errorf("only %d successes (want >= %d, cancels=%d)",
			successes.Load(), expectedSuccess-1, canceledRejects.Load())
	}
}

// buildPayload returns a Skills.md whose `name` encodes idx so the
// concurrent test detects context-bleed by name-mismatch.
func buildPayload(idx int) []byte {
	return []byte(fmt.Sprintf(`---
name: goroutine-%d
trigger: when goroutine %d runs
---
Payload for goroutine %d.

## Steps

- step from goroutine %d
`, idx, idx, idx, idx))
}

// TestConcurrent_PathSafety_NoRace exercises the path-safety helper
// across goroutines (read-only — the helper carries no mutable
// state).
func TestConcurrent_PathSafety_NoRace(t *testing.T) {
	const N = 64
	root := t.TempDir()
	src := []byte(`---
name: pathsafe
trigger: yes
---

![attach](attachments/file.txt)

## Steps

- s
`)
	// Write a dummy attachment so the upload succeeds.
	if err := writeFile(filepath.Join(root, "attachments", "file.txt"), []byte("hello")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	imp, _ := newImporter(t)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := imp.Import(context.Background(), importer.ImportSource{
				Bytes:       src,
				PathHint:    "pathsafe.md",
				AllowedRoot: root,
				Scope: artifacts.ArtifactScope{
					TenantID:  fmt.Sprintf("t-%d", idx),
					UserID:    "u",
					SessionID: "s",
					TaskID:    fmt.Sprintf("task-%d", idx),
				},
			})
			if err != nil {
				t.Errorf("[%d] Import: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
}

// writeFile mkdirs the parent + writes data.
func writeFile(p string, data []byte) error {
	dir := filepath.Dir(p)
	if err := osMkdirAll(dir, 0o755); err != nil {
		return err
	}
	return osWriteFile(p, data, 0o600)
}
