#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 112b smoke — external consumers on the sdk/ facade + THE
# STANDING EXTERNAL-MODULE COMPILE GATE (RFC §3.6 items 4-5,
# D-204/D-206; docs/plans/phase-112b-external-consumers.md).
#
# This is the gate that keeps the SDK friction audit's headline
# external break (docs/notes/sdk-friction-audit.md §5 — "scaffold
# output cannot compile the moment it declares a tool") from silently
# returning. Assertions:
#
#   1. Template hygiene (static): the scaffold templates emit sdk/
#      imports and zero internal/ imports.
#   2. Doc hygiene (static): the five consumer-facing recipes + the
#      root README instruct no `hurtener/Harbor/internal` import.
#   3. THE COMPILE GATE: `harbor scaffold --from-config` with >=1
#      built-in AND >=1 custom tool, materialised into a temp dir as
#      an EXTERNAL module (replace directive), `go mod tidy` +
#      `go build ./...`. FAIL on compile error. Runtime is measured
#      and bounded (module cache keeps it in single-digit seconds
#      warm; GATE_DEADLINE_SECS caps cold-cache pathology).
#   4. Gate self-test (failure mode): a deliberately-broken Go file
#      injected into a scaffolded module makes the same build step
#      fail — proving the gate detects compile errors loudly.
#   5. The harbortest external probe: a second external module
#      constructs harbortest.Deps (bus via sdk/events + sdk/audit,
#      identity via sdk/identity), runs an agent that publishes a
#      custom event through the sdk/events surface, calls
#      AssertSequence with sdk-typed events, and drives a
#      FaultInjector over a catalog built via sdk/tools — the
#      audit's three "type-poisoned" surfaces, exercised end-to-end
#      with `go test` from OUTSIDE the module.
#
# Note on phase-67: its toolless template build-check stands; the
# TOOL-DECLARING shape (the audit's CI blind spot) is owned by this
# gate rather than duplicated there (§4.3 call, recorded in the
# phase plan + D-206).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# Upper bound for one external `go mod tidy && go build`/`go test`
# leg. Warm module cache: ~2s. The bound exists so a cold CI cache
# degrades loudly instead of hanging preflight.
GATE_DEADLINE_SECS="${HARBOR_PHASE112B_GATE_DEADLINE_SECS:-240}"

# --- 1. Template hygiene -----------------------------------------------------

TPL_DIR="cmd/harbor/scaffold/templates/minimal-react"
assert_grep_present 'github.com/hurtener/Harbor/sdk/tools' \
    "${TPL_DIR}/agent.go.tmpl" \
    'phase 112b: agent.go.tmpl imports the sdk/ facade'
assert_grep_absent 'hurtener/Harbor/internal' \
    "${TPL_DIR}/agent.go.tmpl" \
    'phase 112b: agent.go.tmpl emits no internal/ import'

# --- 2. Doc hygiene — consumer-facing docs instruct no internal/ import ------

for doc in \
    docs/recipes/embed-harbor-headless.md \
    docs/recipes/define-a-tool.md \
    docs/recipes/use-memory-and-skills-from-go.md \
    docs/recipes/steer-and-resume-a-run.md \
    docs/recipes/observe-an-embedded-runtime.md \
    README.md
do
    assert_grep_absent 'hurtener/Harbor/internal' "${doc}" \
        "phase 112b: ${doc} instructs no internal/ import"
done

# --- helpers ------------------------------------------------------------------

# write_probe_yaml <path> — a validator-passing harbor.yaml declaring
# one built-in + one custom tool (the tool-declaring shape the audit
# proved broken).
write_probe_yaml() {
    cat > "$1" <<'YAML'
server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: phase-112b-gate
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: phase-112b-gate
llm:
  provider: openrouter
  model: anthropic/claude-haiku-4-5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  model_profiles:
    anthropic/claude-haiku-4-5:
      context_window_tokens: 200000
tools:
  built_in:
    - clock.now
  custom:
    - name: weather.lookup
      description: Look up current weather by city.
      input:
        city: string
      output:
        summary: string
YAML
}

# with_deadline <cmd...> — portable bounded execution (macOS ships no
# coreutils `timeout`; perl's alarm is the fallback).
with_deadline() {
    if command -v timeout >/dev/null 2>&1; then
        timeout "${GATE_DEADLINE_SECS}" "$@"
    elif command -v perl >/dev/null 2>&1; then
        perl -e 'alarm shift; exec @ARGV' "${GATE_DEADLINE_SECS}" "$@"
    else
        "$@"
    fi
}

# run_bounded <log> <dir> <cmd...> — run cmd in dir under the gate
# deadline; echoes elapsed seconds on stdout, returns cmd's status.
run_bounded() {
    local log_file="$1" dir="$2"
    shift 2
    local start elapsed status=0
    start=$(date +%s)
    (cd "${dir}" && with_deadline "$@") >"${log_file}" 2>&1 || status=$?
    elapsed=$(( $(date +%s) - start ))
    echo "${elapsed}"
    return "${status}"
}

if [[ ! -x "${BIN}" ]]; then
    skip 'phase 112b: bin/harbor not built (preflight build step skipped) — compile gate + probe skipped'
    smoke_summary
    exit 0
fi

TMPDIR="$(mktemp -d -t harbor-phase112b-XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

# --- 3. THE COMPILE GATE — tool-declaring scaffold as an external module -----

write_probe_yaml "${TMPDIR}/harbor.yaml"
GATE_OUT="${TMPDIR}/gate-agent"
if ! (cd "${TMPDIR}" && "${BIN}" scaffold --name gate-agent --output "${GATE_OUT}" --from-config "${TMPDIR}/harbor.yaml") >/dev/null 2>&1; then
    fail 'phase 112b: harbor scaffold --from-config (tool-declaring) failed'
else
    if [[ -f "${GATE_OUT}/tools/weather_lookup.go" && -f "${GATE_OUT}/agent.go" ]]; then
        ok 'phase 112b: tool-declaring scaffold materialised (agent.go + tools/weather_lookup.go)'
    else
        fail 'phase 112b: tool-declaring scaffold missing expected files'
    fi
    {
        printf '\n'
        printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
    } >> "${GATE_OUT}/go.mod"
    build_log="${TMPDIR}/gate-build.log"
    if elapsed=$(run_bounded "${build_log}" "${GATE_OUT}" sh -c 'go mod tidy && go build ./...'); then
        ok "phase 112b: EXTERNAL COMPILE GATE green — tool-declaring scaffold builds as an external module in ${elapsed}s (bound ${GATE_DEADLINE_SECS}s)"
    else
        fail 'phase 112b: EXTERNAL COMPILE GATE RED — tool-declaring scaffold does NOT build externally (the audit-§5 break is back; see tail)'
        tail -40 "${build_log}" | sed 's/^/    /'
    fi
fi

# --- 4. Gate self-test — a broken module must fail the same step --------------

SELFTEST_OUT="${TMPDIR}/selftest-agent"
if ! (cd "${TMPDIR}" && "${BIN}" scaffold --name selftest-agent --output "${SELFTEST_OUT}" --from-config "${TMPDIR}/harbor.yaml") >/dev/null 2>&1; then
    fail 'phase 112b: scaffold for gate self-test failed'
else
    {
        printf '\n'
        printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
    } >> "${SELFTEST_OUT}/go.mod"
    # Deliberately-broken fixture: an undefined identifier at package scope.
    printf 'package selftest_agent\n\nvar _ = definitelyNotDefined\n' > "${SELFTEST_OUT}/broken.go"
    selftest_log="${TMPDIR}/selftest-build.log"
    if elapsed=$(run_bounded "${selftest_log}" "${SELFTEST_OUT}" sh -c 'go mod tidy && go build ./...'); then
        fail 'phase 112b: gate self-test — the build step PASSED a deliberately-broken module (the gate detects nothing)'
    else
        ok 'phase 112b: gate self-test — the build step fails loudly on a broken module (compile errors are detected)'
    fi
fi

# --- 5. The harbortest external probe -----------------------------------------

PROBE_DIR="${TMPDIR}/harbortest-probe"
mkdir -p "${PROBE_DIR}"
cat > "${PROBE_DIR}/go.mod" <<EOF
module github.com/example/harbortest-probe

go 1.26

require github.com/hurtener/Harbor v0.0.0-dev

replace github.com/hurtener/Harbor => ${ROOT}
EOF
cat > "${PROBE_DIR}/probe_test.go" <<'GO'
// probe_test.go — the Phase 112b harbortest external probe (D-206).
// Exercises, from OUTSIDE the Harbor module, the three surfaces the
// SDK friction audit (§5) found type-poisoned: Deps.{Bus,Redactor,
// Identity}, AssertSequence's []events.EventType, and
// NewFaultInjector's tools.ToolCatalog — all satisfied through sdk/
// aliases, no internal/ import anywhere in this module.
package probe_test

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/sdk/drivers/prod"

	"github.com/hurtener/Harbor/harbortest"
	sdkaudit "github.com/hurtener/Harbor/sdk/audit"
	sdkconfig "github.com/hurtener/Harbor/sdk/config"
	sdkevents "github.com/hurtener/Harbor/sdk/events"
	sdkidentity "github.com/hurtener/Harbor/sdk/identity"
	sdktools "github.com/hurtener/Harbor/sdk/tools"
	sdkinproc "github.com/hurtener/Harbor/sdk/tools/inproc"
)

const probeEventType = sdkevents.EventType("probe.agent_ran")

func init() { sdkevents.RegisterEventType(probeEventType) }

type probePayload struct {
	sdkevents.SafeSealed
	Note string `json:"note"`
}

// probeAgent proves an EXTERNAL agent can read its identity and emit
// events through the sdk/ surface (the audit found both impossible).
type probeAgent struct{}

func (probeAgent) Run(ctx context.Context, input any) (any, error) {
	q := sdkidentity.MustQuadrupleFrom(ctx)
	bus := sdkevents.MustFrom(ctx)
	if err := bus.Publish(ctx, sdkevents.Event{
		Type:     probeEventType,
		Identity: q,
		Payload:  probePayload{Note: "external probe"},
	}); err != nil {
		return nil, err
	}
	return input, nil
}

type probeEchoIn struct {
	S string `json:"s"`
}

type probeEchoOut struct {
	S string `json:"s"`
}

func TestExternalProbe_DepsSequenceFaultInjector(t *testing.T) {
	ctx := context.Background()

	// Deps construction through sdk/ (audit finding: "internal types
	// with no external constructors"). Open resolves the default
	// "patterns" production driver.
	red, err := sdkaudit.Open(ctx, sdkconfig.AuditConfig{})
	if err != nil {
		t.Fatalf("sdk/audit.Open: %v", err)
	}
	bus, err := sdkevents.Open(ctx, sdkconfig.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         32,
	}, red)
	if err != nil {
		t.Fatalf("sdk/events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()

	id := sdkidentity.Identity{TenantID: "probe-tenant", UserID: "probe-user", SessionID: "probe-session"}
	out, log, err := harbortest.RunOnce(ctx, probeAgent{}, "ping", harbortest.Deps{
		Bus:      bus,
		Redactor: red,
		Identity: &id,
		RunID:    "probe-run-1",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if out != "ping" {
		t.Fatalf("RunOnce output = %v, want ping", out)
	}

	// AssertSequence with sdk-typed events; the agent's emit must be
	// observed (the audit found external EventLogs structurally empty).
	if !harbortest.AssertSequence(t, log, []sdkevents.EventType{probeEventType}) {
		t.Fatal("AssertSequence missed the external agent's event")
	}
	harbortest.AssertNoLeaks(t, log)

	// FaultInjector over a catalog built via sdk/tools.
	cat := sdktools.NewCatalog()
	if err := sdkinproc.RegisterFunc(cat, "probe.echo",
		func(_ context.Context, in probeEchoIn) (probeEchoOut, error) {
			return probeEchoOut{S: in.S}, nil
		}); err != nil {
		t.Fatalf("RegisterFunc: %v", err)
	}
	inj := harbortest.NewFaultInjector(cat)
	harbortest.SimulateFailure(inj, "probe.echo", sdktools.ErrClassTransient, 1)
	desc, found := inj.Catalog().Resolve("probe.echo")
	if !found {
		t.Fatal("Resolve(probe.echo) not found")
	}
	if _, err := desc.Invoke(ctx, []byte(`{"s":"x"}`)); !errors.Is(err, harbortest.ErrSimulatedFailure) {
		t.Fatalf("first invoke: want ErrSimulatedFailure, got %v", err)
	}
	if _, err := desc.Invoke(ctx, []byte(`{"s":"x"}`)); err != nil {
		t.Fatalf("second invoke (counter popped): %v", err)
	}
}
GO

probe_log="${TMPDIR}/probe-test.log"
if elapsed=$(run_bounded "${probe_log}" "${PROBE_DIR}" sh -c 'go mod tidy && go test ./...'); then
    ok "phase 112b: harbortest external probe green — Deps/AssertSequence/FaultInjector satisfied via sdk/ from an external module (go test, ${elapsed}s)"
else
    fail 'phase 112b: harbortest external probe RED — the kit is not externally usable (see tail)'
    tail -40 "${probe_log}" | sed 's/^/    /'
fi

smoke_summary
