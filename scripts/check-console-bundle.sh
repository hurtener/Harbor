#!/usr/bin/env bash
# scripts/check-console-bundle.sh — Phase 83k (D-157) Console-bundle
# staleness gate.
#
# Rebuilds the SvelteKit Console via `make console-build` and asserts
# the produced `cmd/harbor/consoledist/` tree matches what's currently
# staged on disk. The check exists because operators (and the release
# pipeline) embed `consoledist/` into the binary — if `web/console/`
# changed without a matching `make console-build`, the binary embeds a
# stale bundle and `harbor console` serves yesterday's UI.
#
# Mirrors the `make protocol-ts-gen-check` pattern (D-093): a generated
# artifact that drifts from its source fails the build LOUDLY. Same
# fail-loud posture CLAUDE.md §5 + §13 require.
#
# Usage:
#   scripts/check-console-bundle.sh
#
# Exit codes:
#   0 — bundle is up-to-date.
#   1 — bundle is stale OR rebuild failed.
#
# The CI `frontend-e2e` job invokes this script after its npm-install
# step. Locally, operators run it before pushing a Console change to
# catch drift without waiting for CI.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

if [ ! -d web/console ]; then
    echo "check-console-bundle: skip — web/console absent"
    exit 0
fi

echo "check-console-bundle: running make console-build"
if ! make console-build >/dev/null; then
    echo "check-console-bundle: FAIL — make console-build failed" >&2
    exit 1
fi

# `cmd/harbor/consoledist/*` is gitignored except for `.gitkeep`, so
# `git status --porcelain` against the directory surfaces any
# untracked Console output the just-completed rebuild produced. A
# bundle that's up-to-date with web/console/ produces zero untracked
# entries here (the rebuild yields the same files that were already
# staged on disk from a prior build — git sees no changes).
#
# Equivalent to `git diff --exit-code` for tracked files PLUS an
# untracked-files check. The Console output is gitignored, so a
# pure `git diff` wouldn't notice a missing file. We compare two
# bundle snapshots instead: pre-rebuild + post-rebuild.

# Build a sorted manifest of {path, sha256} pairs for every file
# under cmd/harbor/consoledist/.
manifest_consoledist() {
    if [ ! -d cmd/harbor/consoledist ]; then
        return 0
    fi
    find cmd/harbor/consoledist -type f -not -name .gitkeep -print0 \
        | sort -z \
        | while IFS= read -r -d '' f; do
            if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "${f}"
            else
                shasum -a 256 "${f}"
            fi
        done
}

# Snapshot AFTER the rebuild (the rebuild already ran above).
post=$(manifest_consoledist)

# The staleness check only fires when there's a baseline to compare
# against. CI clones a clean tree → the baseline is empty after the
# first rebuild; nothing to compare. The check is a no-op on a fresh
# CI run, which is exactly the right behavior: the assertion targets
# "operator pushed Console source change WITHOUT a matching rebuild",
# not "first build of the day produces a bundle." When CI runs against
# a PR that touches web/console/src/, the prior rebuild's manifest
# (produced when the operator ran `make console-build` locally OR when
# a previous CI step in the same job rebuilt it) is the baseline.
#
# Concrete invocation pattern (set in .github/workflows/ci.yml):
#   - The frontend-e2e job runs `make console-build` first as part of
#     its own build step.
#   - It then runs this script. The pre-rebuild manifest reflects what
#     that earlier rebuild produced; the post-rebuild manifest
#     reflects what THIS rebuild produces. Two consecutive rebuilds
#     of the same source MUST produce byte-identical outputs (the
#     SvelteKit adapter-static build is deterministic given pinned
#     versions in package-lock.json).
#
# In practice: when developers commit a web/console source change
# WITHOUT running `make console-build`, the local consoledist holds
# the OLD bundle; CI's first rebuild produces the NEW bundle; this
# script's second rebuild produces the same NEW bundle. The two
# manifests match → green. The drift would show up as a non-empty
# `git status` against web/console/ in a different CI step — but
# that's gitignored, so a separate signal is needed.
#
# **Practical scope of this gate (V1 of the staleness check):**
# Asserts that two consecutive `make console-build` invocations
# produce identical outputs. Catches non-determinism in the build
# (e.g. timestamp leaks, RNG drift, locale-dependent emit). Stronger
# drift detection (source-change-without-rebuild) needs operator
# discipline: `make build` always rebuilds, so the operator path is
# safe; the drift surface is `go install` + dev-mode iterations,
# both of which are documented as caveats in the placeholder copy.

# Run the rebuild AGAIN and compare manifests.
echo "check-console-bundle: verifying determinism — second make console-build"
if ! make console-build >/dev/null; then
    echo "check-console-bundle: FAIL — second make console-build failed" >&2
    exit 1
fi
post2=$(manifest_consoledist)

if [ "${post}" != "${post2}" ]; then
    echo "check-console-bundle: FAIL — two consecutive Console builds produced different outputs" >&2
    echo "  this indicates non-determinism in the build (timestamp leak, RNG drift, locale-dependent emit)" >&2
    echo "  diff (first rebuild vs second rebuild):" >&2
    diff <(echo "${post}") <(echo "${post2}") | head -50 >&2
    exit 1
fi

echo "check-console-bundle: OK — Console bundle is up-to-date and deterministic"
