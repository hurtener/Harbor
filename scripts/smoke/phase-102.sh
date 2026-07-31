#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 102 smoke — godoc hygiene: no internal phase jargon in
# godoc-visible comments.
#
# pkg.go.dev renders every non-test comment under internal/ and cmd/.
# "Phase NN" / "phase-NN" / inline "D-NNN" / "brief NN" / wave-band
# references ("Wave 8", "Round 3", "Stage A") are contributor concepts
# that must not reach the public docs surface. This smoke runs the four
# acceptance greps from the phase plan and asserts each returns zero
# matches in non-test Go source. Test files (_test.go) are
# contributor-internal and exempt.
#
# The exemption is ANCHORED ON THE PATH FIELD (`^[^:]*_test\.go:`), never
# on the line body. A bare `grep -v '_test\.go'` discards any VIOLATING
# line that merely MENTIONS a _test.go filename — and a production
# comment citing an integration test by its `phaseNN_*_test.go` name is
# exactly that shape, so the unanchored form was blind to 26 real hits.
#
# The patterns are intentionally narrow (numbering forms only) so
# legitimate runtime vocabulary — e.g. "three phases: pending, running,
# completed" or a domain "stage" — never trips the scan.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# One assertion per acceptance grep (plan §Acceptance criteria).
check_jargon() {
    local pattern="$1" label="$2" hits
    hits=$(grep -rE "$pattern" --include='*.go' internal/ cmd/ 2>/dev/null | grep -vE '^[^:]*_test\.go:' || true)
    if [ -z "$hits" ]; then
        ok "phase 102: no '${label}' jargon in non-test Go source under internal/ + cmd/"
    else
        fail "phase 102: '${label}' jargon found in non-test Go source:"
        printf '%s\n' "$hits" | head -10 | sed 's/^/       /'
    fi
}

check_jargon '[Pp]hase[ -]?[0-9]+' 'Phase NN / phase-NN'
check_jargon '\bD-[0-9]+' 'D-NNN'
check_jargon '\b[Bb]rief [0-9]+' 'brief NN'
check_jargon '\b(Wave|Round|Stage)[ -][0-9A-Z]+' 'Wave/Round/Stage band'

smoke_summary
