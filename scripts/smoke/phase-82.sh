#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 82 — V1 cut. Phase 82 ships no Protocol method, REST endpoint,
# or CLI subcommand — it is the v1.0.0 release: the rewritten root
# README, the CHANGELOG [1.0.0] roll, the launch announcement scaffold,
# and (operator-run) the `v1.0.0` git tag. There is no live surface for
# this smoke to hit; it statically asserts the release artifacts are in
# place. The `harbor version == v1.0.0` acceptance check is verified at
# tag time, not at PR time (the tag does not exist when this runs).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The CHANGELOG carries a dated [1.0.0] section (not a bare [Unreleased]).
if grep -qE '^## \[1\.0\.0\] — [0-9]{4}-[0-9]{2}-[0-9]{2}' CHANGELOG.md; then
	ok "CHANGELOG.md carries a dated [1.0.0] release section"
else
	fail "CHANGELOG.md is missing a dated [1.0.0] section"
fi

# The launch announcement scaffold exists.
if [ -f docs/announcements/v1.0.0.md ]; then
	ok "docs/announcements/v1.0.0.md launch announcement scaffold present"
else
	fail "docs/announcements/v1.0.0.md is missing"
fi

# Public release surfaces carry no internal "phase" development jargon.
if grep -qiE '\bphase[ -][0-9]' CHANGELOG.md; then
	fail "CHANGELOG.md mentions internal phase numbering (public surfaces must not)"
else
	ok "CHANGELOG.md carries no internal phase jargon"
fi

smoke_summary
