package skillpkg_test

import (
	"sync"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// TestConcurrentReuse pins the concurrent-reuse contract for the
// semantic core: every function in the package is pure with respect
// to its arguments, so N goroutines sharing the same package value
// (and the same package-level tables) must never race and must never
// observe each other's work. The gate is `go test -race`.
func TestConcurrentReuse_HashValidateURIParallel(t *testing.T) {
	p := testPackage()
	z := buildZip(t, []zipEntry{
		{name: "SKILL.md", data: "---\ntrigger: demo\n---\n## Steps\n- do it\n"},
		{name: "examples/demo.json", data: `{"demo": true}`},
	})
	wantHash, err := skillpkg.PackageHash(p)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Independent per-goroutine copies; the underlying data
			// is read-only shared.
			q := p
			h, err := skillpkg.PackageHash(q)
			if err != nil {
				errs <- err
				return
			}
			if h != wantHash {
				errs <- &mismatchError{h, wantHash}
				return
			}
			if err := skillpkg.VerifyPackageHash(q, wantHash); err != nil {
				errs <- err
				return
			}
			u, err := skillpkg.NewURI(h, q.Name)
			if err != nil {
				errs <- err
				return
			}
			if _, err := skillpkg.ParseURI(u.String()); err != nil {
				errs <- err
				return
			}
			cb, err := skillpkg.CanonicalBytes(q)
			if err != nil {
				errs <- err
				return
			}
			if _, err := skillpkg.FromCanonicalBytes(cb); err != nil {
				errs <- err
				return
			}
			if _, err := skillpkg.ValidateArchive(z, skillpkg.ArchiveLimits{}); err != nil {
				errs <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent invocation: %v", err)
	}
}

type mismatchError struct {
	got, want string
}

func (e *mismatchError) Error() string { return "hash mismatch: " + e.got + " != " + e.want }
