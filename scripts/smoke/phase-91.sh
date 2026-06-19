#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 91 smoke — Console-driven LLM provider key rotation
# (admin `governance.rotate_key`). Planning skeleton: SKIPs until the
# surface lands. At implementation, assert (static): the method declared;
# the atomic key holder in the bifrost account; the governance.key_rotated
# event registered. Live: admin rotate → 200 + fingerprint (NO key echoed);
# non-admin → 403; empty-key body → 400.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers. The new-key value is a SECRET — the
# smoke NEVER asserts key identity over the wire (that is the integration
# test's job with a mock driver); the live check asserts only status codes
# + that the response carries a fingerprint, never the key.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if grep -rqE '"governance\.rotate_key"' internal/protocol/methods/methods.go 2>/dev/null; then
    ok 'phase 91: governance.rotate_key Protocol method declared'
else
    skip 'phase 91: governance.rotate_key method not yet declared'
fi

if grep -rqiE 'atomic\.Pointer|RotateKey' internal/llm/drivers/bifrost/account.go 2>/dev/null; then
    ok 'phase 91: atomic key holder present in bifrost account'
else
    skip 'phase 91: atomic key holder not yet present'
fi

if grep -rqiE 'key_rotated|KeyRotated' internal/governance/ 2>/dev/null; then
    ok 'phase 91: governance.key_rotated event present'
else
    skip 'phase 91: governance.key_rotated event not yet present'
fi

smoke_summary
