#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83u — Console DB chicken-and-egg fix (closes walkthrough F3).
# D-163.
#
# The Connected Runtimes add-form on the Settings page must work pre-
# attach. The fix splits the form's two effects:
#   1) attachConnection() writes `harbor.runtime.base_url` to
#      localStorage — always works; no DB.
#   2) DB upsert into `runtime_registry` is best-effort.
# An address-book catch-up routine runs on Console DB load to promote
# the active connection into the registry on the next page mount.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# connection.ts gains the attachConnection() helper.
# ----------------------------------------------------------------------------
assert_grep_present 'export function attachConnection' \
    "web/console/src/lib/connection.ts" \
    "connection.ts exports attachConnection()"
assert_grep_present 'AttachConnectionOptions' \
    "web/console/src/lib/connection.ts" \
    "AttachConnectionOptions interface is declared"
assert_grep_present 'STORAGE_KEYS\.baseURL' \
    "web/console/src/lib/connection.ts" \
    "attachConnection writes the canonical base_url storage key"

# ----------------------------------------------------------------------------
# console_db.svelte.ts::addRuntime rewires through attachConnection.
# ----------------------------------------------------------------------------
assert_grep_present "import \{ attachConnection, resolveConnection \}" \
    "web/console/src/lib/settings/console_db.svelte.ts" \
    "SettingsDBController imports attachConnection"
# Phase 105 widened the call to `attachConnection(baseURL, { token, identity, scopes })`
# (the connection resolver requires all five fields). The 83u invariant —
# that the active-connection write happens BEFORE the address-book write —
# is still expressed by calling attachConnection at the head of addRuntime;
# the grep just accepts either signature shape.
assert_grep_present 'attachConnection\(baseURL[,)]' \
    "web/console/src/lib/settings/console_db.svelte.ts" \
    "addRuntime calls attachConnection(baseURL, ...) before attempting the DB write"
assert_grep_present 'addWarning' \
    "web/console/src/lib/settings/console_db.svelte.ts" \
    "addRuntime surfaces a non-fatal warning instead of throwing on DB-write deferral"
assert_grep_present '#catchUpAddressBook' \
    "web/console/src/lib/settings/console_db.svelte.ts" \
    "load() invokes the address-book catch-up routine"

# Pre-83u, addRuntime threw 'Console DB not open — attach to a Runtime
# first' synchronously when the form was used on first attach. The
# F3 fix removes that throw entirely — the message no longer exists
# as a `throw new Error(...)` in addRuntime.
if grep -nE "addRuntime\(.*\).*Promise" "web/console/src/lib/settings/console_db.svelte.ts" >/dev/null 2>&1; then
    if grep -nE "throw new Error\('Console DB not open — attach to a Runtime first'\)" \
        "web/console/src/lib/settings/console_db.svelte.ts" >/dev/null 2>&1; then
        fail "addRuntime still throws the pre-83u chicken-and-egg error"
    else
        ok "addRuntime no longer throws the pre-83u chicken-and-egg error"
    fi
fi

# ----------------------------------------------------------------------------
# ConnectedRuntimesCard accepts the addWarning + onaddsuccess props and
# the Settings page wires them to a page reload.
# ----------------------------------------------------------------------------
assert_grep_present 'addWarning' \
    "web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte" \
    "ConnectedRuntimesCard accepts the addWarning prop"
assert_grep_present 'onaddsuccess' \
    "web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte" \
    "ConnectedRuntimesCard fires onaddsuccess on successful submit"
assert_grep_present 'window\.location\.reload' \
    "web/console/src/routes/(console)/settings/+page.svelte" \
    "Settings page reloads after a successful Add (new connection takes effect)"

# ----------------------------------------------------------------------------
# Tests — connection.spec.ts covers attachConnection; settings-page.spec
# extends the disconnected-add-form test through to a connected state.
# ----------------------------------------------------------------------------
assert_grep_present 'attachConnection \(Phase 83u / D-163\)' \
    "web/console/src/lib/tests/connection.spec.ts" \
    "connection.spec.ts has the attachConnection unit-test block"
assert_grep_present 'Phase 83u / D-163' \
    "web/console/tests/settings-page.spec.ts" \
    "settings-page.spec.ts has the 83u end-to-end follow-through test"

smoke_summary
