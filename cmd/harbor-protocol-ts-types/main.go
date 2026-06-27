// Command harbor-protocol-ts-types emits a vendorable, external-client
// TypeScript wire-type module from the canonical Harbor Protocol single
// sources (RFC §5):
//
//   - internal/protocol/singlesource.CanonicalWireTypes — the wire-type
//     inventory, rendered interface-by-interface via reflection over the
//     typeInstanceIndex.
//   - internal/protocol/methods — the canonical method registry, rendered
//     as a string-union type.
//   - internal/protocol/errors — the canonical error-code set, rendered as
//     a string-union type.
//   - internal/events — the canonical event-type wire strings, read by a
//     textual tree scan (no driver imports), rendered as a string-union
//     type.
//
// The output is a single self-contained `.ts` module a third-party
// Protocol client can copy-vendor: TypeScript interfaces for every
// canonical wire type, plus string-union types for the method, error-code,
// and event-type name sets, plus the pinned Protocol version and the
// wire-surface digest. It is consumed by the worked event-viewer-ts
// example and is the answer to "where do I get typed wire shapes for a
// non-Console client?"
//
// This tool is DISTINCT from the Console wire-manifest generator
// (harbor-protocol-ts-lockstep) and from the reserved, not-yet-built
// full Console TypeScript-client generator. It does not touch the
// Console's hand-maintained per-page TypeScript modules, the committed
// wire manifest, or the Console lockstep gate — it only emits the
// external-client types module. The reserved harbor-gen-protocol-ts name
// stays unused for the full Console generator.
//
// The output is deterministic (sorted type / method / error / event sets)
// so the `make protocol-ts-types-gen-check` git-diff gate is byte-stable.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts", "output path for the generated external-client TypeScript wire-type module")
	root := flag.String("root", ".", "repository root scanned for canonical event-type declarations")
	flag.Parse()

	if err := run(*out, *root); err != nil {
		fmt.Fprintf(os.Stderr, "harbor-protocol-ts-types: %v\n", err)
		os.Exit(1)
	}
}

// run builds the TypeScript module from the canonical sources rooted at
// root and writes it to outPath. It is the whole program behind the flag
// surface, split out so tests drive it against a t.TempDir().
func run(outPath, root string) error {
	mod, err := BuildModule(root)
	if err != nil {
		return err
	}
	data := render(mod)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("create output dir for %q: %w", outPath, err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("write module %q: %w", outPath, err)
	}
	fmt.Printf("harbor-protocol-ts-types: wrote %s (%d interfaces, %d methods, %d errors, %d events)\n",
		outPath, len(mod.Types), len(mod.Methods), len(mod.Errors), len(mod.Events))
	return nil
}
