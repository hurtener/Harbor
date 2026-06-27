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
#   3b. THE EXECUTION GATE (D-267): `go test ./...` on the SAME
#      tool-declaring scaffold. The scaffolded agent_test.go carries a
#      register-and-dispatch test that, when tools are declared, calls
#      RegisterTools AND drives >=1 tool THROUGH the catalog/executor,
#      asserting an observable dispatch signal — not merely that
#      RegisterTools is defined (Go does not flag an unused exported
#      func). A tools-declaring scaffold that registers nothing fails
#      here. Closes the §1 CLI false-green: a tools-declaring agent that
#      compiles + passes while no tool is ever invoked.
#   3c. Execution-gate self-test (failure mode): the registration name
#      in a scaffolded agent.go is rewritten so RegisterTools registers
#      the tool under the WRONG name. The module still COMPILES (proving
#      this is a dispatch failure, distinct from the §3 compile gate)
#      but `go test ./...` now FAILS — proving the execution gate bites.
#   4. Compile-gate self-test (failure mode): a deliberately-broken Go
#      file injected into a scaffolded module makes the build step
#      fail — proving the compile gate detects compile errors loudly.
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

# write_builtin_only_yaml <path> — a validator-passing harbor.yaml
# declaring a built-in tool and NO custom tool. This renders the
# scaffold test template's built-in-only branch (`{{if and .BuiltIns
# (not .CustomTools)}}`), which carries its own uniquely-conditional
# `errors` import and built-in dispatch path — a branch the
# tool-declaring probe above (which always declares a custom tool)
# never exercises.
write_builtin_only_yaml() {
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

        # 3b. THE EXECUTION GATE (D-267) — the module is already tidied;
        # `go test ./...` runs the scaffolded register-and-dispatch test
        # that drives a declared tool through the catalog/executor.
        test_log="${TMPDIR}/gate-test.log"
        if elapsed=$(run_bounded "${test_log}" "${GATE_OUT}" sh -c 'go test ./...'); then
            ok "phase 112b: EXTERNAL EXECUTION GATE green — tool-declaring scaffold's register-and-dispatch test passes (a tool is registered AND invoked through the catalog) in ${elapsed}s"
        else
            fail 'phase 112b: EXTERNAL EXECUTION GATE RED — the tool-declaring scaffold compiles but its register-and-dispatch test fails (the §1 CLI false-green; see tail)'
            tail -40 "${test_log}" | sed 's/^/    /'
        fi
    else
        fail 'phase 112b: EXTERNAL COMPILE GATE RED — tool-declaring scaffold does NOT build externally (the audit-§5 break is back; see tail)'
        tail -40 "${build_log}" | sed 's/^/    /'
    fi
fi

# --- 3c. Execution-gate self-test — a register/dispatch-name mismatch must
#         compile but FAIL `go test` (proving the D-267 gate bites) ------------

DISPATCH_OUT="${TMPDIR}/dispatch-selftest-agent"
if ! (cd "${TMPDIR}" && "${BIN}" scaffold --name dispatch-selftest --output "${DISPATCH_OUT}" --from-config "${TMPDIR}/harbor.yaml") >/dev/null 2>&1; then
    fail 'phase 112b: scaffold for execution-gate self-test failed'
else
    {
        printf '\n'
        printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
    } >> "${DISPATCH_OUT}/go.mod"
    # Break dispatch WITHOUT breaking the build: register the custom tool
    # under a different name so the scaffolded test's Resolve finds
    # nothing. The standalone registration-name arg line is unique (the
    # error-wrap occurrence keeps its closing paren, not a trailing
    # comma), so only the registration name is rewritten.
    sed -i.bak 's/^\([[:space:]]*\)"weather.lookup",$/\1"weather.lookup.DISPATCH_BROKEN",/' "${DISPATCH_OUT}/agent.go"
    if grep -q 'DISPATCH_BROKEN' "${DISPATCH_OUT}/agent.go"; then
        ok 'phase 112b: execution-gate self-test — registration name rewritten (dispatch deliberately broken)'
    else
        fail 'phase 112b: execution-gate self-test — could not apply the break (template formatting drifted; the self-test cannot prove the gate bites)'
    fi
    # The module must still COMPILE (proves this is a dispatch failure,
    # not a build failure) but `go test` must FAIL.
    dispatch_build_log="${TMPDIR}/dispatch-build.log"
    if elapsed=$(run_bounded "${dispatch_build_log}" "${DISPATCH_OUT}" sh -c 'go mod tidy && go build ./...'); then
        dispatch_test_log="${TMPDIR}/dispatch-test.log"
        if elapsed=$(run_bounded "${dispatch_test_log}" "${DISPATCH_OUT}" sh -c 'go test ./...'); then
            fail 'phase 112b: execution-gate self-test — `go test` PASSED a scaffold whose tool is registered under the wrong name (the dispatch gate detects nothing)'
        else
            ok 'phase 112b: execution-gate self-test — the register-and-dispatch test fails loudly when RegisterTools dispatches nothing under the expected name (D-267 gate bites)'
        fi
    else
        fail 'phase 112b: execution-gate self-test — the broken-dispatch module did NOT compile; the self-test cannot isolate a dispatch failure from a build failure (see tail)'
        tail -40 "${dispatch_build_log}" | sed 's/^/    /'
    fi
fi

# --- 3d. Built-in-only branch coverage (D-267) — a scaffold declaring a
#         built-in and NO custom tool renders the template's `{{- else}}`
#         branch (its uniquely-conditional `errors` import + built-in
#         dispatch). The custom-tool probe above never renders it, so
#         absent this leg the branch could silently regress to an
#         uncompilable scaffold for built-in-only adopters. -----------------

write_builtin_only_yaml "${TMPDIR}/builtin-only.yaml"
BUILTIN_OUT="${TMPDIR}/builtin-only-agent"
if ! (cd "${TMPDIR}" && "${BIN}" scaffold --name builtin-only --output "${BUILTIN_OUT}" --from-config "${TMPDIR}/builtin-only.yaml") >/dev/null 2>&1; then
    fail 'phase 112b: harbor scaffold --from-config (built-in-only) failed'
else
    {
        printf '\n'
        printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
    } >> "${BUILTIN_OUT}/go.mod"
    builtin_log="${TMPDIR}/builtin-only.log"
    if elapsed=$(run_bounded "${builtin_log}" "${BUILTIN_OUT}" sh -c 'go mod tidy && go test ./...'); then
        ok "phase 112b: built-in-only branch green — a built-in-only scaffold compiles AND its register-and-dispatch test drives the built-in through the catalog in ${elapsed}s"
    else
        fail 'phase 112b: built-in-only branch RED — the built-in-only scaffold (template `{{- else}}` branch) does not compile or its register-and-dispatch test fails (see tail)'
        tail -40 "${builtin_log}" | sed 's/^/    /'
    fi
fi

# --- 4. Compile-gate self-test — a broken module must fail the build step -----

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

# --- 6. The embed one-call runner — Stack.RunOnce (phase 132, D-265) ----------
#
# The production one-call runner ships with a checked-in worked example
# (examples/embed-runonce/, sdk/-facade only) and the mandatory tests:
# the NewRunContext projection-parity table and the N>=100 concurrent-
# reuse -race stress against ONE shared Stack (D-025). This leg builds
# the real example file (a heredoc would be gameable) and asserts both
# test surfaces exist and pass.

# 6a — the example is a real, checked-in, sdk-facade-only program.
assert_grep_present 'github.com/hurtener/Harbor/sdk/assemble' \
    "${ROOT}/examples/embed-runonce/main.go" \
    'phase 132: embed-runonce example imports the sdk/ facade'
assert_grep_present 'RunOnce' \
    "${ROOT}/examples/embed-runonce/main.go" \
    'phase 132: embed-runonce example calls Stack.RunOnce'
assert_grep_absent 'hurtener/Harbor/internal' \
    "${ROOT}/examples/embed-runonce/main.go" \
    'phase 132: embed-runonce example emits no internal/ import'

runonce_build_log="${TMPDIR}/runonce-build.log"
if elapsed=$(run_bounded "${runonce_build_log}" "${ROOT}" sh -c 'go build -o /dev/null ./examples/embed-runonce/'); then
    ok "phase 132: embed-runonce example compiles (go build, ${elapsed}s)"
else
    fail 'phase 132: embed-runonce example does NOT compile (see tail)'
    tail -40 "${runonce_build_log}" | sed 's/^/    /'
fi

# 6b — the mandatory tests exist (grep) AND pass under -race. The N>=100
# concurrent-reuse stress + the projection-parity table are the D-265 /
# §11 gate; a SKIP-that-should-be-OK here would be a false green.
assert_grep_present 'TestRunOnce_ConcurrentReuse_NoBleedNoLeak' \
    "${ROOT}/internal/runtime/assemble/runonce_test.go" \
    'phase 132: N>=100 concurrent-reuse RunOnce -race test exists'
assert_grep_present 'TestNewRunContext_MemoryParity' \
    "${ROOT}/internal/runtime/runctx/newruncontext_test.go" \
    'phase 132: NewRunContext projection-parity test exists'

runonce_test_log="${TMPDIR}/runonce-test.log"
if elapsed=$(run_bounded "${runonce_test_log}" "${ROOT}" sh -c 'go test -race -run "TestRunOnce|TestNewRunContext" ./internal/runtime/assemble/ ./internal/runtime/runctx/'); then
    ok "phase 132: RunOnce + NewRunContext parity/concurrency tests pass under -race (${elapsed}s)"
else
    fail 'phase 132: RunOnce + NewRunContext tests FAILED under -race (see tail)'
    tail -40 "${runonce_test_log}" | sed 's/^/    /'
fi

# --- 7. The WithStream streaming sink — RunOnce (phase 132-stream, D-266) -----
#
# The streaming sink ships on the SAME blocking RunOnce. Its gate lives
# here (no new smoke file for the assertion): the §13 primitive-with-
# consumer e2e test asserts ordered token/step chunks arrive BEFORE the
# final envelope (deterministic — the OnChunk seam fires synchronously on
# the run goroutine), and the N>=100 concurrent-reuse -race test is
# extended to assert NO cross-run chunk bleed (run A's chunks never reach
# run B's sink). The grep legs pin existence so a deletion fails loud;
# the -race run executes the streaming + extended-concurrency tests.

assert_grep_present 'func WithStream' \
    "${ROOT}/internal/runtime/assemble/stream.go" \
    'phase 132-stream: WithStream sink on RunOnce exists'
assert_grep_present 'TestRunOnce_WithStream_ChunksArriveBeforeEnvelope' \
    "${ROOT}/internal/runtime/assemble/runonce_stream_test.go" \
    'phase 132-stream: ordered-chunks-before-envelope e2e test exists'
assert_grep_present 'CROSS-RUN CHUNK BLEED' \
    "${ROOT}/internal/runtime/assemble/runonce_test.go" \
    'phase 132-stream: concurrent-reuse test asserts no cross-run chunk bleed'
assert_grep_present 'WithStream' \
    "${ROOT}/sdk/assemble/assemble.go" \
    'phase 132-stream: sdk/assemble re-exports WithStream'

stream_test_log="${TMPDIR}/runonce-stream-test.log"
if elapsed=$(run_bounded "${stream_test_log}" "${ROOT}" sh -c 'go test -race -run "TestRunOnce_WithStream|TestRunOnce_StreamEventKinds|TestRunOnce_ConcurrentReuse" ./internal/runtime/assemble/'); then
    ok "phase 132-stream: WithStream e2e + kind-mapping + no-cross-run-bleed tests pass under -race (${elapsed}s)"
else
    fail 'phase 132-stream: WithStream streaming tests FAILED under -race (see tail)'
    tail -40 "${stream_test_log}" | sed 's/^/    /'
fi

smoke_summary
