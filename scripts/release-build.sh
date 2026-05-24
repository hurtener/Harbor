#!/usr/bin/env bash
# scripts/release-build.sh — the Harbor release build (Phase 81, D-139).
#
# This is the SINGLE home of the release version-stamping logic. Both
# the `make release-dryrun` target and the `.github/workflows/release.yml`
# release workflow invoke this script — there is no second copy of the
# `-ldflags -X` incantation anywhere (CLAUDE.md §13 "two parallel
# implementations" smell).
#
# What it does:
#   1. Derives the product release version (RELEASE_VERSION below).
#   2. Builds the CGo-free static `harbor` binary with the version
#      stamped into `main.HarborVersion` via `go build -ldflags -X`.
#   3. Emits the binary plus a SHA-256 checksum into the output dir.
#   4. Verifies the built binary's `harbor version` actually reports
#      the stamped version — a stamp that didn't take is a loud failure.
#
# Version resolution (in priority order):
#   - $HARBOR_RELEASE_VERSION, if set and non-empty (the release
#     workflow sets it from the pushed `v*` tag — `github.ref_name`).
#   - `git describe --tags --always --dirty`, when the working tree is
#     a git checkout with at least one reachable tag.
#   - "v0.0.0-dev", the un-tagged fallback — the load-bearing operator
#     signal that "this is not a release artifact" (CLAUDE.md §5
#     "fail loudly": no silent pretend-release).
#
# Distinct from the Protocol version: HarborVersion is the PRODUCT
# release version of the binary; `internal/protocol/types.ProtocolVersion`
# is the Runtime↔Console wire contract version (RFC §5.3). They move
# independently — this script stamps only the former.
#
# Usage:
#   scripts/release-build.sh [OUTPUT_DIR]
#     OUTPUT_DIR   directory for the artifact + checksum (default: dist/)
#
# Environment:
#   HARBOR_RELEASE_VERSION   override the derived version (the workflow
#                            sets this from the tag ref)
#   GOOS / GOARCH            standard Go cross-build knobs (honoured;
#                            the artifact name carries the os/arch)

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

OUT_DIR="${1:-dist}"

# ---------------------------------------------------------------------------
# 1. Resolve the product release version.
# ---------------------------------------------------------------------------
resolve_version() {
    if [ -n "${HARBOR_RELEASE_VERSION:-}" ]; then
        printf '%s' "${HARBOR_RELEASE_VERSION}"
        return 0
    fi
    if git rev-parse --git-dir >/dev/null 2>&1; then
        local described
        if described="$(git describe --tags --always --dirty 2>/dev/null)" \
            && [ -n "${described}" ]; then
            # `git describe` with no tags falls back to a bare short
            # hash (no leading 'v'); treat that as the dev fallback so
            # an untagged build never masquerades as a release.
            case "${described}" in
                v*) printf '%s' "${described}"; return 0 ;;
            esac
        fi
    fi
    printf 'v0.0.0-dev'
}

RELEASE_VERSION="$(resolve_version)"

# The HOST platform — captured with GOOS / GOARCH explicitly cleared so
# `go env` reports the true runner platform even when this build is a
# cross-build (a set GOOS env var makes `go env GOOS` echo the override,
# which would fool the exec-verification guard in step 4).
HOST_GOOS="$(env -u GOOS go env GOOS)"
HOST_GOARCH="$(env -u GOARCH go env GOARCH)"

# The TARGET platform — the cross-build knobs if set, else the host.
GOOS_VAL="${GOOS:-${HOST_GOOS}}"
GOARCH_VAL="${GOARCH:-${HOST_GOARCH}}"
ARTIFACT_NAME="harbor-${RELEASE_VERSION}-${GOOS_VAL}-${GOARCH_VAL}"

echo "release-build: version=${RELEASE_VERSION} os=${GOOS_VAL} arch=${GOARCH_VAL}"
echo "release-build: output dir=${OUT_DIR}"

# ---------------------------------------------------------------------------
# 2. Rebuild the Console bundle so the binary embeds the FRESH SvelteKit
#    static build (Phase 83k / D-157). Without this, the release artifact
#    embeds whatever `cmd/harbor/consoledist/` happens to hold on the
#    builder — usually empty on CI, so `harbor console` would serve the
#    synthesized "run make console-build" placeholder. Fail loud if the
#    Console build fails: a release without a working Console is not a
#    release artifact (§13 fail-loud posture). Skipped when `web/console/`
#    is absent (the build target's no-op branch covers that path too).
# ---------------------------------------------------------------------------
if [ -d web/console ]; then
    echo "release-build: rebuilding Console bundle (make console-build)"
    make console-build
fi

# ---------------------------------------------------------------------------
# 3. Build the CGo-free static binary with the version stamped in.
#    CGO_ENABLED=0 + -ldflags='-s -w' is the CLAUDE.md §5 invariant; the
#    extra `-X` clause stamps the release version into main.HarborVersion.
# ---------------------------------------------------------------------------
mkdir -p "${OUT_DIR}"
BIN_PATH="${OUT_DIR}/${ARTIFACT_NAME}"

CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X 'main.HarborVersion=${RELEASE_VERSION}'" \
    -o "${BIN_PATH}" \
    ./cmd/harbor

echo "release-build: built ${BIN_PATH}"

# ---------------------------------------------------------------------------
# 4. Emit a SHA-256 checksum alongside the binary.
# ---------------------------------------------------------------------------
CHECKSUM_PATH="${BIN_PATH}.sha256"
(
    cd "${OUT_DIR}"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${ARTIFACT_NAME}" > "${ARTIFACT_NAME}.sha256"
    else
        # macOS / BSD: shasum -a 256 produces the same `<hash>  <file>`
        # two-column format `sha256sum -c` consumes.
        shasum -a 256 "${ARTIFACT_NAME}" > "${ARTIFACT_NAME}.sha256"
    fi
)
echo "release-build: wrote checksum ${CHECKSUM_PATH}"

# ---------------------------------------------------------------------------
# 5. Verify the stamp took. A cross-built binary cannot be exec'd here,
#    so this check runs only for a native build (GOOS/GOARCH unchanged).
#    Fail loudly if `harbor version` does not report the stamped string.
# ---------------------------------------------------------------------------
if [ "${GOOS_VAL}" = "${HOST_GOOS}" ] && [ "${GOARCH_VAL}" = "${HOST_GOARCH}" ]; then
    REPORTED="$("${BIN_PATH}" version --json)"
    case "${REPORTED}" in
        *"\"harbor\":\"${RELEASE_VERSION}\""*)
            echo "release-build: verified — harbor version reports ${RELEASE_VERSION}"
            ;;
        *)
            echo "release-build: FAIL — version stamp did not take." >&2
            echo "  expected harbor=${RELEASE_VERSION}" >&2
            echo "  got: ${REPORTED}" >&2
            exit 1
            ;;
    esac
else
    echo "release-build: cross-build (${GOOS_VAL}/${GOARCH_VAL}) — skipping exec verification"
fi

echo "release-build: done — artifact ${BIN_PATH} (+ .sha256)"
