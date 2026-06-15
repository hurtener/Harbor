// Command harbor-protocol-ts-lockstep emits the committed wire-surface
// manifest (web/console/src/lib/protocol/wire-manifest.gen.json) from the
// canonical Harbor Protocol single sources (RFC §5):
//
//   - internal/protocol/singlesource.CanonicalWireTypes — the wire-type
//     inventory, rendered field-by-field via reflection over the
//     typeInstanceIndex.
//   - internal/protocol/methods — the canonical method registry.
//   - internal/protocol/errors — the canonical error-code set.
//   - internal/events — the canonical event-type wire strings, read by a
//     textual tree scan (no driver imports — the manifest stays a pure
//     contract reader).
//
// The manifest is the machine-checkable contract the hand-maintained
// Console TypeScript client is lockstep-gated against: the Go-side
// generator emits the manifest, a TypeScript-source scan
// (web/console/scripts/check-protocol-ts-lockstep.mjs) verifies the
// hand-written interfaces declare every manifest type's fields, and the
// `make protocol-ts-gen-check` git-diff gate forces a regeneration
// whenever a Go wire shape changes. The Console TypeScript client is NOT
// generated from this manifest — emitting the per-domain TypeScript type
// modules is a deferred future deliverable, and the
// cmd/harbor-gen-protocol-ts name is reserved for it.
//
// The output is deterministic (sorted map keys, sorted method/error/event
// sets) so the gate is byte-stable.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "web/console/src/lib/protocol/wire-manifest.gen.json", "output path for the generated wire-surface manifest")
	root := flag.String("root", ".", "repository root scanned for canonical event-type declarations")
	flag.Parse()

	if err := run(*out, *root); err != nil {
		fmt.Fprintf(os.Stderr, "harbor-protocol-ts-lockstep: %v\n", err)
		os.Exit(1)
	}
}

// run builds the manifest from the canonical sources rooted at root and
// writes it to outPath. It is the whole program behind the flag surface,
// split out so tests drive it against a t.TempDir().
func run(outPath, root string) error {
	m, err := BuildManifest(root)
	if err != nil {
		return err
	}
	data, err := render(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("create output dir for %q: %w", outPath, err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("write manifest %q: %w", outPath, err)
	}
	fmt.Printf("harbor-protocol-ts-lockstep: wrote %s (%d types, %d methods, %d errors, %d events)\n",
		outPath, len(m.Types), len(m.Methods), len(m.Errors), len(m.Events))
	return nil
}
