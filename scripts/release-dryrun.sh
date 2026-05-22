#!/usr/bin/env bash
# scripts/release-dryrun.sh — exercise the release build WITHOUT a tag.
#
# The master plan's Phase 81 acceptance criterion is "`git tag v1.0.0-rc.1`
# produces a release artifact". This script proves the tooling that makes
# that true — it runs the exact `scripts/release-build.sh` path the
# release workflow runs, but with HARBOR_RELEASE_VERSION forced to a
# synthetic dry-run tag, so a contributor (and CI) can verify the release
# build end-to-end without pushing a real `v*` tag.
#
# It is the `make release-dryrun` target's body and the
# `.github/workflows/release.yml` `workflow_dispatch` path's body.
#
# What it asserts:
#   1. release-build.sh produces the binary artifact + its .sha256.
#   2. The checksum file verifies against the binary.
#   3. The built binary's `harbor version` reports the stamped version
#      (both human and --json renderings).
#   4. An un-stamped `go build` (plain `make build`) still reports the
#      "v0.0.0-dev" sentinel — i.e. the stamp is opt-in, never silently
#      applied.
#
# Usage: scripts/release-dryrun.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

# A synthetic, obviously-not-real version so a dry-run artifact can
# never be mistaken for a genuine release build.
DRYRUN_VERSION="v0.0.0-dryrun"
DRYRUN_DIR="dist/dryrun"

echo "release-dryrun: cleaning ${DRYRUN_DIR}"
rm -rf "${DRYRUN_DIR}"

# ---------------------------------------------------------------------------
# 1. Run the real release build with the synthetic version.
# ---------------------------------------------------------------------------
echo "release-dryrun: invoking scripts/release-build.sh (version=${DRYRUN_VERSION})"
HARBOR_RELEASE_VERSION="${DRYRUN_VERSION}" \
    bash scripts/release-build.sh "${DRYRUN_DIR}"

GOOS_VAL="$(go env GOOS)"
GOARCH_VAL="$(go env GOARCH)"
ARTIFACT="harbor-${DRYRUN_VERSION}-${GOOS_VAL}-${GOARCH_VAL}"
BIN_PATH="${DRYRUN_DIR}/${ARTIFACT}"

fail() { echo "release-dryrun: FAIL — $1" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 2. Artifact + checksum exist.
# ---------------------------------------------------------------------------
[ -f "${BIN_PATH}" ]          || fail "binary artifact missing: ${BIN_PATH}"
[ -f "${BIN_PATH}.sha256" ]   || fail "checksum missing: ${BIN_PATH}.sha256"
echo "release-dryrun: OK — artifact + checksum present"

# ---------------------------------------------------------------------------
# 3. Checksum verifies.
# ---------------------------------------------------------------------------
(
    cd "${DRYRUN_DIR}"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c "${ARTIFACT}.sha256" >/dev/null
    else
        shasum -a 256 -c "${ARTIFACT}.sha256" >/dev/null
    fi
) || fail "checksum verification failed"
echo "release-dryrun: OK — checksum verifies against the binary"

# ---------------------------------------------------------------------------
# 4. The stamped binary reports the dry-run version.
# ---------------------------------------------------------------------------
HUMAN="$("${BIN_PATH}" version)"
case "${HUMAN}" in
    *"harbor ${DRYRUN_VERSION}"*) ;;
    *) fail "human version output missing 'harbor ${DRYRUN_VERSION}': ${HUMAN}" ;;
esac
JSON="$("${BIN_PATH}" version --json)"
case "${JSON}" in
    *"\"harbor\":\"${DRYRUN_VERSION}\""*) ;;
    *) fail "json version output missing harbor=${DRYRUN_VERSION}: ${JSON}" ;;
esac
echo "release-dryrun: OK — stamped binary reports ${DRYRUN_VERSION} (human + json)"

# ---------------------------------------------------------------------------
# 5. An un-stamped build still carries the v0.0.0-dev sentinel — proving
#    the stamp is opt-in and a plain `go build` never pretends to be a
#    release.
# ---------------------------------------------------------------------------
UNSTAMPED="${DRYRUN_DIR}/harbor-unstamped"
CGO_ENABLED=0 go build -ldflags='-s -w' -o "${UNSTAMPED}" ./cmd/harbor
UNSTAMPED_JSON="$("${UNSTAMPED}" version --json)"
case "${UNSTAMPED_JSON}" in
    *'"harbor":"v0.0.0-dev"'*)
        echo "release-dryrun: OK — un-stamped build keeps the v0.0.0-dev sentinel"
        ;;
    *)
        fail "un-stamped build did not report v0.0.0-dev: ${UNSTAMPED_JSON}"
        ;;
esac

echo "release-dryrun: PASS — release tooling produces a verified, version-stamped artifact"
