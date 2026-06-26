#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 65 smoke — `harbor dev` hot-reload (D-099).
#
# The preflight harness boots `./bin/harbor dev` with hot-reload enabled
# by default (the loader's CLI.DevHotReload defaults: Enabled=true,
# Policy=drain, DrainTimeout=5s, WatchRoots=[".harbor/agents"]). This
# script asserts the watcher is wired:
#
#   1. The `harbor dev` boot log mentions "hot-reload: watcher started"
#      (the supervisor's Info log line). This is the production-wiring
#      proof — if the boot omits the line, the supervisor never started.
#   2. The canonical `dev.hot_reload.triggered` / `completed` event
#      types are present in the binary (a strings probe — the
#      registered event-type names land in the binary's read-only data
#      because `RegisterEventType` consumes them at init time).
#   3. The `--no-hot-reload` flag is accepted by the binary (a `--help`
#      probe lists the flag).
#
# This smoke deliberately does NOT mutate a watched file and assert the
# hot-reload fires end-to-end: the supervisor's rebuild path takes
# 0.5–2s and would tear down the long-running preflight server. The
# end-to-end behaviour is covered by:
#   - cmd/harbor/cmd_dev_hot_reload_test.go (in-package, real
#     bootDevStack + real fsnotify + real subscriber).
# Per CLAUDE.md §17.2 the in-package shape IS the integration test for
# this wiring boundary.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------
# Assertion 1 — the supervisor's "watcher started" log line lands in
# the preflight server log. Confirms hot-reload is on by default.
# ----------------------------------------------------------------------
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
    if grep -q "hot-reload: watcher started" "${HARBOR_DATA_DIR}/server.log" 2>/dev/null; then
        ok "harbor dev: hot-reload supervisor started (watcher log line present)"
    else
        # SKIP rather than FAIL: pre-Phase-65 builds don't have the
        # supervisor wired and the 404/405/501-equivalent skip
        # convention for log-line probes is "log line absent => surface
        # not yet implemented." Per CLAUDE.md §4.2 "the 404/405/501 →
        # SKIP convention is sacred."
        skip "harbor dev: hot-reload supervisor log line absent (Phase 65 not yet shipped)"
    fi
else
    skip "harbor dev: hot-reload log probe (HARBOR_DATA_DIR/server.log not reachable)"
fi

# ----------------------------------------------------------------------
# Assertion 2 — the `--no-hot-reload` CLI flag is accepted. A `--help`
# probe lists the flag; pre-Phase-65 builds don't have it.
# ----------------------------------------------------------------------
if [ -x "${ROOT}/bin/harbor" ]; then
    if "${ROOT}/bin/harbor" dev --help 2>&1 | grep -q -- "--no-hot-reload"; then
        ok "harbor dev: --no-hot-reload flag present (operator escape hatch)"
    else
        skip "harbor dev: --no-hot-reload flag absent (Phase 65 not yet shipped)"
    fi
else
    skip "harbor dev: --no-hot-reload flag probe (bin/harbor not built)"
fi

# ----------------------------------------------------------------------
# Assertion 3 — the canonical bus event types are registered in the
# binary. `strings` greps the binary for the event-type names; the
# strings appear in the binary's read-only data because
# `RegisterEventType` consumes them at init time. Pre-Phase-65 builds
# won't contain the strings.
# ----------------------------------------------------------------------
if [ -x "${ROOT}/bin/harbor" ] && command -v strings >/dev/null 2>&1; then
    # Substring match (NOT exact line) — the strings(1) output sometimes
    # concatenates short read-only-data entries on one line. The two
    # event-type names landing in the binary's data segment IS the
    # build-time proof we need. Capture strings into a tempfile to
    # avoid the SIGPIPE-shaped failure that `set -o pipefail` turns into
    # a script abort when `grep -q` closes the pipe early.
    strings_tmp=$(mktemp -t harbor-smoke65-XXXXXX)
    strings "${ROOT}/bin/harbor" > "${strings_tmp}" 2>/dev/null || true
    if grep -q "dev\.hot_reload\.triggered" "${strings_tmp}" \
       && grep -q "dev\.hot_reload\.completed" "${strings_tmp}"; then
        ok "harbor dev: canonical bus events dev.hot_reload.{triggered,completed} present in binary"
    else
        skip "harbor dev: canonical bus events not present in binary (Phase 65 not yet shipped)"
    fi
    rm -f "${strings_tmp}"
else
    skip "harbor dev: canonical bus event type probe (bin/harbor not built or strings unavailable)"
fi

# ----------------------------------------------------------------------
# Assertion 4 (Phase 138, D-268) — live `.go`-edit honesty.
#
# A static `strings` grep CANNOT distinguish this case: the
# `dev.hot_reload.completed` string stays in the binary for the YAML
# rebuild path, so the false-success can only be caught by a LIVE
# edit-and-observe. The preflight server boots with `--config
# examples/dev.yaml`, so `examples/` is a hot-reload watch root (the
# config file's parent dir). We write a `.go` file directly into
# `examples/` (the watcher is non-recursive), wait past the debounce
# window, and assert:
#
#   (a) the honest WARN line fires ("Go source change detected"), AND
#   (b) NO in-process rebuild fired for the edit — the rebuild path
#       logs "hot-reload: file change observed" at its start, so the
#       count of that line is UNCHANGED across the .go edit. That is the
#       observable proxy for "no dev.hot_reload.completed{Success=true}"
#       (completed is emitted ONLY on the reboot path).
#
# The probe file name is `_`-prefixed so the Go toolchain ignores it
# (it never enters a build) while fsnotify still observes it; it is
# removed immediately after. A `.go` edit is SAFE to perform against the
# long-running preflight server precisely because Phase 138 makes it a
# no-reboot WARN — it does not tear the server down (unlike a YAML edit,
# which this smoke deliberately does NOT perform; the YAML rebuild path
# is covered by cmd/harbor/cmd_dev_hot_reload_test.go).
# ----------------------------------------------------------------------
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ] \
   && grep -q "hot-reload: watcher started" "${HARBOR_DATA_DIR}/server.log" 2>/dev/null; then
    log_file="${HARBOR_DATA_DIR}/server.log"
    probe="${ROOT}/examples/_hotreload_probe_138.go"
    # Self-clean even if the script is SIGKILLed in the write/sleep window
    # (preflight runs inside the pre-commit hook): an EXIT trap guarantees
    # no stray untracked probe file is left in the working tree.
    trap 'rm -f "${probe}"' EXIT
    # Snapshot the rebuild-path log count BEFORE the .go edit. The
    # repo-standard `$(...) || var=0` form keeps grep -c's "0"-and-exit-1
    # from leaking a non-zero status under `set -e`.
    reboot_before=$(grep -c "hot-reload: file change observed" "${log_file}" 2>/dev/null) || reboot_before=0
    # Write a .go file directly under the watched examples/ dir, then
    # remove it (cleanup even if a later assertion fails).
    printf 'package examples\n\n// hot-reload honesty probe %s\n' "$(date +%s)" > "${probe}"
    sleep 2
    rm -f "${probe}"
    if grep -q "hot-reload: Go source change detected" "${log_file}" 2>/dev/null; then
        ok "harbor dev: live .go edit logs honest WARN + rebuild guidance"
    else
        fail "harbor dev: live .go edit did NOT log the honest WARN (false-success guard)"
    fi
    reboot_after=$(grep -c "hot-reload: file change observed" "${log_file}" 2>/dev/null) || reboot_after=0
    if [ "${reboot_after}" -eq "${reboot_before}" ]; then
        ok "harbor dev: live .go edit did NOT trigger an in-process rebuild (no false dev.hot_reload.completed)"
    else
        fail "harbor dev: live .go edit triggered a rebuild (observed ${reboot_before} -> ${reboot_after}); false-success regression"
    fi
else
    skip "harbor dev: live .go-edit honesty probe (server log / watcher not reachable; Phase 65/138 not yet shipped)"
fi

smoke_summary
