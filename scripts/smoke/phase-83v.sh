#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 83v smoke — Runtime CORS middleware (D-162).
#
# Static-only assertions: the smoke script pins the load-bearing
# declarations in source files. The live cross-origin behavior is
# covered by `test/integration/phase83v_cors_test.go` (real preflight +
# allow / deny / dev-any branches against the assembled devstack).
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (not used here — static-only).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Config fields declared.
assert_grep_present \
    'AllowedOrigins[[:space:]]+\[\]string[[:space:]]+`yaml:"allowed_origins,omitempty"`' \
    "internal/config/config.go" \
    "phase 83v: ServerConfig.AllowedOrigins field declared"

assert_grep_present \
    'CORSDevAllowAny[[:space:]]+bool[[:space:]]+`yaml:"cors_dev_allow_any,omitempty"`' \
    "internal/config/config.go" \
    "phase 83v: ServerConfig.CORSDevAllowAny field declared"

# 2. CORS middleware package exists.
assert_file "internal/protocol/transports/cors/cors.go" \
    "phase 83v: cors middleware package"
assert_file "internal/protocol/transports/cors/cors_test.go" \
    "phase 83v: cors middleware unit tests"

# 3. The middleware is wired into the dev boot path.
assert_grep_present 'cors\.Wrap\(router' \
    "cmd/harbor/cmd_dev.go" \
    "phase 83v: cors.Wrap mounted around dev mux"

# 4. D-094 mirror — devstack.Assemble wraps the same way as production.
assert_grep_present 'cors\.Wrap\(router' \
    "harbortest/devstack/devstack.go" \
    "phase 83v: cors.Wrap mirrored in devstack (D-094)"

# 5. Validator rejects `*` without the dev flag.
assert_grep_present 'cors_dev_allow_any' \
    "internal/config/validate.go" \
    "phase 83v: validator references cors_dev_allow_any"

# 6. docs/CONFIG.md documents both new fields.
assert_grep_present '^### server\.allowed_origins' \
    "docs/CONFIG.md" \
    "phase 83v: docs/CONFIG.md documents server.allowed_origins"
assert_grep_present '^### server\.cors_dev_allow_any' \
    "docs/CONFIG.md" \
    "phase 83v: docs/CONFIG.md documents server.cors_dev_allow_any"

# 7. Wildcard never emitted as Access-Control-Allow-Origin in production
# paths (security invariant — grep the middleware source). The literal
# string `"*"` MUST NOT appear as the value the middleware writes.
if grep -nE 'Allow-?Origin.*"\*"' internal/protocol/transports/cors/cors.go 2>/dev/null \
        | grep -v '_test\.go' \
        | grep -v '//' >/dev/null; then
    fail "phase 83v: middleware emits Access-Control-Allow-Origin: * — forbidden"
else
    ok "phase 83v: middleware never emits Access-Control-Allow-Origin: *"
fi

smoke_summary
