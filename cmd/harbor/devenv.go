// cmd/harbor/devenv.go — dev-only dotenv loading for `harbor dev`.
//
// The dev loop's configs reference secrets as `env.NAME` (resolved via
// os.Getenv at driver construction, fail-closed). Operators keep those
// keys in a local `.env` next to harbor.yaml; this file lets
// `harbor dev` load that file into the process environment before
// config load / boot, so the operator does not have to `source .env`
// manually.
//
// The posture is deliberate and narrow:
//
//   - Dev-only. loadDevEnv is called ONLY from runDev. `harbor serve`
//     and every other subcommand never touch a .env file — production
//     environments are provisioned by the operator's real secret
//     machinery, not a working-directory file.
//   - Loud, never silent. A successful load prints one stderr line
//     naming the file and the VARIABLE NAMES loaded — never values.
//     A malformed file fails the boot with file:line precision; bad
//     lines are never skipped silently.
//   - The environment always wins. A variable already present in the
//     process environment is never overridden; skips are counted and
//     named in the same stderr line.
//   - `./.env` relative to the working directory only — the same place
//     harbor.yaml resolves by default. No search-up-parents magic.
//   - Opt-out via the `--no-env-file` flag on `harbor dev`.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// devEnvFile is the fixed dotenv path `harbor dev` loads, relative to
// the working directory (where harbor.yaml also lives by default).
const devEnvFile = ".env"

// errDevEnvMalformed is the typed sentinel every .env parse failure
// wraps. Tests compare via errors.Is; operators see the file:line
// message.
var errDevEnvMalformed = errors.New("malformed .env entry")

// maxDevEnvBytes bounds how large a .env the loader will read: a dotenv is a
// handful of assignments, and anything beyond this is a mistake worth naming
// rather than swallowing into memory.
const maxDevEnvBytes = 1 << 20

// loadDevEnv loads the dotenv file at path into the process
// environment. It is the `harbor dev` dotenv entry point — dev-only,
// loud, environment-wins:
//
//   - A missing file is a silent no-op (absence is the normal case,
//     not an error).
//   - A malformed file returns an error carrying file:line of the
//     first bad entry — the boot fails loudly, bad lines are never
//     skipped.
//   - Variables already present in the process environment are NEVER
//     overridden; they are counted and named as kept.
//   - On any load (or skip) activity, ONE line is printed to stderr
//     naming the file and the variable NAMES involved — never values.
//
// Parser scope (minimal by design, no third-party dependency):
//
//   - `KEY=value` assignments, one per line; an optional `export `
//     prefix is stripped.
//   - Keys must match [A-Za-z_][A-Za-z0-9_]*; anything else is an
//     error.
//   - Blank lines and full-line `#` comments are skipped; a trailing
//     `#` comment is stripped after both quoted and unquoted values
//     (for unquoted values only when preceded by whitespace).
//   - `KEY=` assigns an EMPTY value — meaningful for os.LookupEnv-style
//     overrides, where set-but-empty differs from unset.
//   - Single- or double-quoted values have their surrounding quotes
//     stripped; no escape-sequence processing happens inside quotes.
//     Only whitespace or a `#` comment may follow the closing quote.
//   - No multiline values, no `${VAR}` interpolation, no `\n`
//     expansion. Values needing those belong in the real environment.
//   - A key repeated within the file resolves last-assignment-wins
//     (shell `source` semantics) and is reported once.
func loadDevEnv(path string, stderr io.Writer) error {
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > maxDevEnvBytes {
		return fmt.Errorf("read %s: file is %d bytes; a .env should be a few assignments (limit %d)", path, info.Size(), maxDevEnvBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.HasPrefix(string(data), "\ufeff") {
		// The most common Windows-editor artifact; without this check it
		// surfaces as a baffling "invalid key" on line 1.
		return fmt.Errorf("%s:1: %w: file starts with a UTF-8 byte-order mark; save the file without a BOM", path, errDevEnvMalformed)
	}

	var loaded []string // insertion-ordered, unique
	loadedSet := map[string]bool{}
	var kept []string // insertion-ordered, unique
	keptSet := map[string]bool{}

	for n, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		key, value, ok, perr := parseDevEnvLine(line)
		if perr != nil {
			return fmt.Errorf("%s:%d: %w", path, n+1, perr)
		}
		if !ok {
			continue
		}
		// Environment-wins: skip keys the process environment already
		// carries — UNLESS this load set them (duplicate key within the
		// file resolves last-assignment-wins).
		if _, exists := os.LookupEnv(key); exists && !loadedSet[key] {
			if !keptSet[key] {
				kept = append(kept, key)
				keptSet[key] = true
			}
			continue
		}
		if sErr := os.Setenv(key, value); sErr != nil {
			return fmt.Errorf("%s:%d: set %s: %w", path, n+1, key, sErr)
		}
		if !loadedSet[key] {
			loaded = append(loaded, key)
			loadedSet[key] = true
		}
	}

	if len(loaded) == 0 && len(kept) == 0 {
		// Nothing to announce (file was all comments/blank lines).
		return nil
	}
	_, _ = fmt.Fprintln(stderr, devEnvSummary(path, loaded, kept))
	return nil
}

// devEnvSummary renders the single names-only stderr line loadDevEnv
// prints. Examples:
//
//	harbor dev: loaded 2 vars from .env (OPENROUTER_API_KEY, FOO)
//	harbor dev: loaded 1 var from .env (FOO; 1 already set, kept environment: BAR)
//	harbor dev: loaded 0 vars from .env (1 already set, kept environment: BAR)
func devEnvSummary(path string, loaded, kept []string) string {
	var b strings.Builder
	unit := "vars"
	if len(loaded) == 1 {
		unit = "var"
	}
	fmt.Fprintf(&b, "harbor dev: loaded %d %s from %s", len(loaded), unit, path)
	var parts []string
	if len(loaded) > 0 {
		parts = append(parts, strings.Join(loaded, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, fmt.Sprintf("%d already set, kept environment: %s", len(kept), strings.Join(kept, ", ")))
	}
	if len(parts) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString(")")
	}
	return b.String()
}

// parseDevEnvLine parses one .env line. ok=false means the line is
// skippable (blank or full-line comment). A non-nil error means the
// line is malformed; the caller attaches file:line context.
func parseDevEnvLine(line string) (key, value string, ok bool, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false, nil
	}
	// Optional `export ` prefix — only when followed by whitespace, so a
	// key literally named "export" ("export=1") still parses.
	if rest, found := strings.CutPrefix(trimmed, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		trimmed = strings.TrimSpace(rest)
	}
	eq := strings.IndexByte(trimmed, '=')
	if eq < 0 {
		// The line's content is deliberately absent from the error: a wrapped
		// secret pasted across lines is exactly what lands here, and an error
		// message is still a log line. path:line is enough to find it.
		return "", "", false, fmt.Errorf("%w: missing '='", errDevEnvMalformed)
	}
	key = strings.TrimSpace(trimmed[:eq])
	if !validDevEnvKey(key) {
		// Same posture: the "key" on a malformed line can be secret material
		// (base64 bodies end in '='), so it is never echoed.
		return "", "", false, fmt.Errorf("%w: invalid key (must match [A-Za-z_][A-Za-z0-9_]*)", errDevEnvMalformed)
	}
	raw := strings.TrimSpace(trimmed[eq+1:])
	value, err = parseDevEnvValue(raw)
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

// parseDevEnvValue resolves the value portion of an assignment: quote
// stripping for single/double-quoted values (no escape processing),
// trailing-comment stripping for unquoted values.
func parseDevEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if q := raw[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(raw[1:], q)
		if end < 0 {
			return "", fmt.Errorf("%w: unterminated %c-quoted value", errDevEnvMalformed, q)
		}
		value := raw[1 : 1+end]
		rest := strings.TrimSpace(raw[2+end:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			// The trailing fragment is part of the value's line — never echoed.
			return "", fmt.Errorf("%w: unexpected content after closing quote", errDevEnvMalformed)
		}
		return value, nil
	}
	// Unquoted: a '#' preceded by whitespace (or starting the value)
	// begins a trailing comment.
	value := raw
	for i := 0; i < len(value); i++ {
		if value[i] == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
			value = strings.TrimRight(value[:i], " \t")
			break
		}
	}
	return value, nil
}

// validDevEnvKey reports whether key matches [A-Za-z_][A-Za-z0-9_]*.
func validDevEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		c := key[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
