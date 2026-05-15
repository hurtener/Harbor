// Package scaffold implements the `harbor scaffold` engine — the
// template registry, the renderer, and the on-disk writer.
//
// The package is BINARY-INTERNAL. Although it lives at
// `cmd/harbor/scaffold/` (so `cmd/harbor/cmd_scaffold.go` can import
// it), nothing outside `cmd/harbor` consumes it; in particular no
// `internal/...` package depends on this surface. The decision to live
// here rather than under `cmd/harbor/internal/scaffold/` is
// stylistic — Go's `internal/` rules already restrict reachability to
// the parent's import tree, which for `cmd/harbor/` is just the binary
// itself.
//
// # Public entry point
//
// scaffold.Scaffold(opts Options) (Result, error) takes:
//
//   - Options.Name      — the project name (validated via validateName).
//   - Options.Template  — the template to render (default
//     DefaultTemplate = "minimal-react").
//   - Options.OutputDir — where to write the project tree. The
//     directory MUST NOT exist; Scaffold creates it.
//
// Returns Result{Name, OutputDir (absolute path), Files (relative
// paths)} on success; otherwise one of the package's sentinel errors:
//
//   - ErrInvalidName       — Options.Name failed validateName.
//   - ErrOutputDirExists   — Options.OutputDir already exists.
//   - ErrUnknownTemplate   — Options.Template is not in Templates().
//   - ErrRender            — a template render or filesystem write
//     failed (wrapped with the offending file path).
//
// # Template registry
//
// Templates are embedded at compile time via Go's embed package
// (templates/*). The registry is a write-once package-level map
// populated in init(); Templates() returns the keys in deterministic
// order. Phase 67 ships exactly one template (`minimal-react`).
//
// # Concurrency
//
// Scaffold is a pure function — no shared state, no goroutines, no
// long-lived artifacts. The D-025 concurrent-reuse contract is
// vacuous here; the package is safe for concurrent invocation by
// construction.
package scaffold
