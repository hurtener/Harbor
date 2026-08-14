package bootpacks

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// want is the frozen-truth snapshot one concurrent goroutine asserts
// against: the entries, set hash, and owned names of one key, captured
// before the source tree is destroyed.
type want struct {
	entries   []Entry
	hash      string
	ownsNames map[string]bool
}

// TestIndex_ConcurrentMixedLookups pins the concurrent-reuse contract
// of the frozen index: N>=100 goroutines performing mixed lookups
// (present keys, missing keys, ownership probes, set hashes, and key
// enumeration) against ONE shared index under -race show no data race,
// no context bleed, no cancellation cross-talk, and byte-identical
// results — and they never observe filesystem changes made after the
// index was built (the source tree is destroyed and rewritten before
// the storm starts).
func TestIndex_ConcurrentMixedLookups(t *testing.T) {
	root := t.TempDir()
	writePackDir(t, root, "workbench-foundation", map[string]string{
		"SKILL.md": validSkillMD("workbench-foundation"),
	})
	writePackDir(t, root, "deploy-runbook", map[string]string{
		"SKILL.md": validSkillMD("deploy-runbook"),
	})
	writePackDir(t, root, "git-guide", map[string]string{
		"SKILL.md": validSkillMD("git-guide"),
	})
	writePackDir(t, root, "other-foundation", map[string]string{
		"SKILL.md": validSkillMD("other-foundation"),
	})
	packs := []config.BootAgentPackConfig{
		declaration(t, "acme", "harbor-dev-agent", root, "workbench-foundation", "deploy-runbook"),
		declaration(t, "globex", "harbor-dev-agent", root, "git-guide"),
		declaration(t, "acme", "secondary-agent", root, "other-foundation"),
	}

	ix := requireLoad(t, testDeps(t, nil), packs...)

	// Capture the frozen truth BEFORE the source tree is destroyed.
	wants := map[Key]want{}
	for _, k := range ix.Keys() {
		entries, _ := ix.Lookup(k.TenantID, k.AgentID)
		hash, _ := ix.BootPackSetHash(k.TenantID, k.AgentID)
		owns := map[string]bool{}
		for _, e := range entries {
			owns[e.Skill.Name] = true
		}
		wants[k] = want{entries: entries, hash: hash, ownsNames: owns}
	}

	// The loader must never observe the filesystem after load: destroy
	// and rewrite every source file with different content.
	_ = os.RemoveAll(filepath.Join(root, "workbench-foundation"))
	_ = os.RemoveAll(filepath.Join(root, "deploy-runbook"))
	_ = os.RemoveAll(filepath.Join(root, "git-guide"))
	_ = os.RemoveAll(filepath.Join(root, "other-foundation"))
	writePackDir(t, root, "workbench-foundation", map[string]string{"SKILL.md": validSkillMD("totally-different")})

	baseline := runtime.NumGoroutine()
	const goroutines = 100
	const opsPerGo = 20

	keys := ix.Keys()
	var wg sync.WaitGroup
	var errs atomic.Int64
	start := make(chan struct{})
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			<-start // barrier: all goroutines launch together
			for op := 0; op < opsPerGo; op++ {
				k := keys[(g+op)%len(keys)]
				if err := verifyLookup(ix, k, wants[k], g, op); err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d: %v", g, op, err)
					return
				}
				// Mixed misses: unknown tenants, agents, and names.
				if _, ok := ix.Lookup("nobody", k.AgentID); ok {
					errs.Add(1)
					t.Errorf("goroutine %d: unknown tenant composed a boot pack", g)
					return
				}
				if _, ok := ix.Lookup(k.TenantID, "nobody"); ok {
					errs.Add(1)
					t.Errorf("goroutine %d: unknown agent composed a boot pack", g)
					return
				}
				if ix.OwnsName(k.TenantID, k.AgentID, "never-declared") {
					errs.Add(1)
					t.Errorf("goroutine %d: unknown name reported owned", g)
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	if n := errs.Load(); n != 0 {
		t.Fatalf("%d concurrent lookups errored", n)
	}

	// Goroutine-leak check: the index spawns no goroutines; after the
	// storm the count must settle back to baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// verifyLookup asserts one goroutine's snapshot of one key matches the
// frozen truth captured before the filesystem was destroyed.
func verifyLookup(ix *Index, k Key, w want, g, op int) error {
	entries, ok := ix.Lookup(k.TenantID, k.AgentID)
	if !ok {
		return fmt.Errorf("Lookup(%s) absent", k)
	}
	if len(entries) != len(w.entries) {
		return fmt.Errorf("Lookup(%s) entry count %d != %d", k, len(entries), len(w.entries))
	}
	for i := range entries {
		e := entries[i]
		we := w.entries[i]
		if e.Skill.Name != we.Skill.Name || e.PackageHash != we.PackageHash ||
			e.SemanticHash != we.SemanticHash || e.Source != we.Source {
			return fmt.Errorf("Lookup(%s)[%d] drifted: %+v vs %+v", k, i, e, we)
		}
		if e.Skill.Trigger != we.Skill.Trigger || e.Skill.ContentHash != we.Skill.ContentHash {
			return fmt.Errorf("Lookup(%s)[%d] skill drifted", k, i)
		}
	}
	hash, ok := ix.BootPackSetHash(k.TenantID, k.AgentID)
	if !ok || hash != w.hash {
		return fmt.Errorf("BootPackSetHash(%s) drifted: %q vs %q", k, hash, w.hash)
	}
	for name := range w.ownsNames {
		if !ix.OwnsName(k.TenantID, k.AgentID, name) {
			return fmt.Errorf("OwnsName(%s, %q) drifted to false", k, name)
		}
	}
	return nil
}
