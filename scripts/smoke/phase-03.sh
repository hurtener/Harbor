#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 03 — Audit redactor smoke checks.
# This phase ships a Go package only (no HTTP / Protocol surface). Validation is via
# `go test ./internal/audit/...`. Smoke records the surface state explicitly.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 03: audit redactor — Go package only; validated by go test ./internal/audit/..."

smoke_summary
