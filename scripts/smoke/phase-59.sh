#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 59 smoke — Protocol versioning + deprecation policy
# (RFC §5.3; CLAUDE.md §8; master-plan Phase 59 detail block; D-077).
#
# Phase 59 turns the Harbor Protocol version *pin* (the ProtocolVersion
# string Phase 54 placed in internal/protocol/types/version.go) into a
# versioning *discipline*: a parsed comparable Version value with a
# same-major Compatible check, a settled Deprecation note format with a
# Deprecations() registry, and a Capability set + VersionHandshake wire
# shape for capability negotiation. It bumps NO version — it ships the
# mechanism, all inside the single canonical home internal/protocol/types.
#
# Phase 59 ships NO HTTP / Protocol-wire surface — the SSE+REST transport
# binding is Phase 60, and the version constant is returned on `harbor
# version` only after Phase 63. The wire/CLI assertions skip per the
# 404/405/501 -> SKIP convention; the versioning surface is exercised
# in-process via `go test`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROTO_PKG="internal/protocol"
TYPES_PKG="${PROTO_PKG}/types"
SS_PKG="${PROTO_PKG}/singlesource"
VERSION_FILE="${TYPES_PKG}/version.go"

# The Phase 59 versioning discipline unit tests: run the types package
# under -race. Covers ParseVersion round-trip + fail-loud rejection, the
# CurrentVersion <-> ProtocolVersion pin, Version.Compare ordering +
# Version.Compatible same-major rule, the Deprecation note format +
# Validate + the (empty) Deprecations() registry, the Capability set +
# IsValidCapability, and the VersionHandshake capability-negotiation
# shape + JSON round-trip.
if go test -race -count=1 -timeout 120s "./${TYPES_PKG}/..." >/dev/null 2>&1; then
    ok 'phase 59: internal/protocol/types tests pass under -race (Version/ParseVersion/Compatible + Deprecation/Deprecations + Capability/VersionHandshake)'
else
    fail 'phase 59: versioning discipline tests failed (run `go test -race ./internal/protocol/types/...` for detail)'
fi

# The Phase 58 single-source checker re-run: Phase 59 adds three new
# exported wire structs (Version, Deprecation, VersionHandshake) to
# internal/protocol/types, so singlesource.CanonicalWireTypes must record
# them under home "types" in lockstep (D-075 §4). This proves the lockstep
# map was updated and the tree stays single-source-clean.
if go test -race -count=1 -timeout 120s "./${SS_PKG}/..." >/dev/null 2>&1; then
    ok 'phase 59: internal/protocol/singlesource tests pass — the 3 new wire structs are registered in the checker lockstep map and the Protocol tree stays single-source-clean (D-075)'
else
    fail 'phase 59: single-source checker tests failed — likely the Phase 59 wire structs are missing from singlesource.CanonicalWireTypes (run `go test -race ./internal/protocol/singlesource/...`)'
fi

# Static guard: the Phase 59 versioning surface is present in the
# canonical home.
if [[ -f "${VERSION_FILE}" ]] \
    && grep -q 'type Version struct' "${VERSION_FILE}" \
    && grep -q 'type Deprecation struct' "${VERSION_FILE}" \
    && grep -q 'type VersionHandshake struct' "${VERSION_FILE}"; then
    ok 'phase 59: the versioning discipline surface (Version + Deprecation + VersionHandshake) is present in internal/protocol/types/version.go'
else
    fail "phase 59: ${VERSION_FILE} missing the Version / Deprecation / VersionHandshake types — Phase 59 must ship the versioning discipline surface"
fi

# Static guard (CLAUDE.md §8): the ProtocolVersion constant is
# single-sourced. It must be declared in exactly one file under
# internal/protocol/ — internal/protocol/types/version.go. A second
# `const ProtocolVersion` definition site anywhere else is a violation.
proto_version_decls="$(
    grep -rIln --include='*.go' -e 'const ProtocolVersion' "${PROTO_PKG}/" 2>/dev/null || true
)"
if [[ "${proto_version_decls}" == "${VERSION_FILE}" ]]; then
    ok 'phase 59: ProtocolVersion is single-sourced in internal/protocol/types/version.go (CLAUDE.md §8 — bumping it is an RFC change)'
else
    fail "phase 59: ProtocolVersion declared in: ${proto_version_decls:-<none>} — must be single-sourced in ${VERSION_FILE} only (CLAUDE.md §8)"
fi

# Phase 59 ships no Protocol/HTTP wire surface — the SSE+REST transport
# binding lands in Phase 60, and the version constant reaches the
# `harbor version` CLI surface only after Phase 63. Skip the wire/CLI
# assertions per the 404/405/501 -> SKIP convention.
skip "phase 59: Phase 59 ships the transport-agnostic versioning discipline (Version / Deprecation / Capability / VersionHandshake values); the wire transport lands in Phase 60 and the \`harbor version\` CLI surface in Phase 63 (RFC §5.3, §5.4, D-077)"

smoke_summary
