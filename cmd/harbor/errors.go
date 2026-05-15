// Package main hosts the `harbor` CLI binary.
//
// This file defines CLIError — the structured-error type the binary
// surfaces to operators (stderr + non-zero exit code). It is the
// single sink for CLI exit errors; subcommand bodies never hand-roll
// JSON.
//
// Distinct from internal/protocol/errors.Error: that type is the
// Protocol *wire* error code consumed by Protocol clients over
// REST/SSE; CLIError is the *operator-facing* exit surface and lives
// in its own home so the two surfaces evolve independently
// (CLAUDE.md §8 — Protocol error codes are single-sourced under
// internal/protocol/errors; this file does NOT add a Protocol code).
//
// Wire shape (--json mode), pinned by D-084:
//
//	{"error":"<message>","code":"<code>","hint":"<hint>"}
//
// Human shape (default), pinned by D-084:
//
//	Error: harbor <subcommand>: <message> [(<hint>)]
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Stable CLI error codes. Each value is a fixed string a smoke test
// can assert against. New CLI subcommands ADD codes here — they do
// NOT reach into internal/protocol/errors (the Protocol wire surface
// is a different concern; CLAUDE.md §8).
const (
	// CodeNotImplemented is emitted by every Phase 63 stub
	// subcommand. The §13 amendment requires stubs to exit non-zero
	// with a structured error pointing to the implementing phase;
	// this is that code.
	CodeNotImplemented = "not_implemented"
)

// CLIError is the structured error every CLI exit path emits. It
// implements the standard error interface so cobra can surface it via
// the usual RunE return value, and MarshalJSON pins the wire field
// names so the --json contract is stable.
//
// CLIError is a value type — safe to copy and compare. No pointers, no
// mutable state.
type CLIError struct {
	// Subcommand names the binary path the error originated from —
	// e.g. "dev", "scaffold", "version". Empty if the error
	// originates from the root command itself. The human-mode
	// rendering prefixes the message with "harbor <subcommand>:".
	Subcommand string `json:"-"`
	// Message is the human-readable error message. Required.
	Message string `json:"error"`
	// Code is the stable error code (e.g. CodeNotImplemented). A
	// caller / smoke test asserts against this.
	Code string `json:"code"`
	// Hint is an optional follow-up — e.g. "see phase 64 — harbor
	// dev v1". Empty hint OK; the JSON field is omitted when empty
	// to keep the wire shape compact.
	Hint string `json:"hint,omitempty"`
}

// Error implements the error interface. Rendering is the human-mode
// form pinned by D-084:
//
//	harbor <subcommand>: <message> [(<hint>)]
//
// When Subcommand is empty, the "harbor <subcommand>:" prefix
// collapses to "harbor:" — a sensible default for root-level errors.
func (e CLIError) Error() string {
	var b strings.Builder
	b.WriteString("harbor")
	if e.Subcommand != "" {
		b.WriteByte(' ')
		b.WriteString(e.Subcommand)
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.Hint != "" {
		b.WriteString(" (")
		b.WriteString(e.Hint)
		b.WriteByte(')')
	}
	return b.String()
}

// PrintCLIError is the single sink for CLI errors. Subcommand bodies
// return a CLIError via cobra's RunE; the root command's
// PersistentPostRunE hook (set in NewRootCmd) calls this with the
// resolved jsonMode flag. Tests invoke PrintCLIError directly to
// assert the wire shape.
//
// jsonMode true: writes a single-line JSON object on the form
//
//	{"error":"<message>","code":"<code>"[,"hint":"<hint>"]}
//
// followed by a trailing newline.
//
// jsonMode false: writes the human form returned by err.Error()
// prefixed with "Error: " and followed by a trailing newline. The
// "Error: " prefix matches cobra's default human-mode behaviour for
// command errors and is the form smoke scripts and operators see.
func PrintCLIError(w io.Writer, jsonMode bool, err CLIError) error {
	if jsonMode {
		// Encode with a fresh encoder so JSON marshalling failures
		// surface loudly (no silent degradation per CLAUDE.md §5).
		// We cannot return early — a hand-rolled fallback would
		// drop fields silently, which the §13 amendment forbids on
		// operator-facing seams.
		buf, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			return fmt.Errorf("cli: marshal structured error: %w", marshalErr)
		}
		buf = append(buf, '\n')
		if _, writeErr := w.Write(buf); writeErr != nil {
			return fmt.Errorf("cli: write structured error: %w", writeErr)
		}
		return nil
	}
	if _, writeErr := fmt.Fprintf(w, "Error: %s\n", err.Error()); writeErr != nil {
		return fmt.Errorf("cli: write human error: %w", writeErr)
	}
	return nil
}
