#!/usr/bin/env bash
set -euo pipefail

# HA-59 static posture checks; live child-run assertions land with the runtime
# surface and use the shared smoke helpers.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
grep -q "D-415" "$root/docs/decisions.md"
grep -q "ScopedArtifacts" "$root/docs/plans/phase-236f-child-artifact-reference-reuse.md"
grep -q "no model-authored schema" "$root/docs/plans/phase-236f-child-artifact-reference-reuse.md"
printf '%s\n' 'phase-236f: OK (3 static assertions)'
