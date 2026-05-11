# Phase 27 — HTTP tool driver

## Summary

Land Harbor's HTTP transport driver — the first out-of-process `ToolProvider` riding the Phase 26 unified tool surface. The driver ships two registration paths (inline `RegisterHTTPTool` for dev-loop ergonomics + a UTCP-style YAML manifest loader for operator deployments), three static auth modes (API key, bearer, cookie), `Retry-After`-aware rate-limit handling, and a `ToolsConfig.HTTPManifests` config hook. Every HTTP tool gets the same `ToolPolicy` reliability shell as in-process tools (D-024) — no double-wrapping; identity flows through `ctx` (D-001); secrets stay in operator config and never bleed into URL templates or request bodies.

## RFC anchor

- RFC §6.4
- RFC §4

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- **brief 03 §1 (one `Tool` regardless of source).** HTTP tools are the same `tools.Tool` struct as in-process tools; only `Transport = TransportHTTP` and `Source = ToolSourceID` differ. Dispatch is still one switch.
- **brief 03 §3 (registration ergonomics).** Inline `RegisterHTTPTool(name, method, urlTemplate, opts...)` mirrors `inproc.RegisterFunc` — schemas are either operator-supplied JSON or derived from the manifest. Manifest entries declare the same shape in YAML; both paths converge on a single `buildDescriptor` helper so a tool registered via either path is indistinguishable in the catalog.
- **brief 03 §3 (UTCP-style manifest).** "A plain HTTP endpoint described by an external manifest." Harbor's minimal manifest schema covers `tools[]: {name, method, url_template, description, args_schema?, out_schema?, headers?, body_template?, auth_ref?, side_effect?, tags?, loading?, policy?}` plus a top-level `auth:` map keyed on `auth_ref`. The auth values are environment variable references (`${HARBOR_TOOL_AUTH_FOO}`), never literal secrets — operators store the actual secret in their env / secret manager.
- **brief 03 §4 (validation at the catalog edge).** Manifest-supplied `args_schema` is compiled at load time; failures yield `ErrToolInvalidArgs` at dispatch via the existing policy shell. No new error path.
- **brief 03 §6 (rate-limit handling).** "Respect `Retry-After` headers on 429/503." The driver's per-attempt classifier reads the `Retry-After` header on 429/503 responses and emits a synthetic `errHTTPRateLimit` whose backoff hint is sleep-honoured before the next attempt. The policy shell's exponential backoff still runs; `Retry-After` is honoured as an additional floor for that attempt's sleep.
- **brief 07 §1 (code-level tool calling).** HTTP tools are dispatched by Harbor's runtime, not by an LLM provider's native tool-calling. The driver is a transport, full stop.

## Findings I'm departing from (if any)

- **brief 03 §7 ("HTTP tool registration uses `urlTemplate` substitution from `args`").** Adopted but tightened — substitution uses `text/template` with mandatory `urlquery` escaping of substituted values, applied AFTER the auth headers are stamped. Templates may NOT reference the `Auth` namespace; static auth values live in `auth_ref` lookups exclusively. The brief did not specify the credential boundary; this phase makes it binding (AGENTS.md §7: no credential passthrough by default).
- **Manifest loader is YAML-only (not JSON).** Brief 03 §3 left the format open; Harbor convention is YAML for operator config (consistent with `examples/harbor.yaml`). JSON manifests can be added as a follow-up if a use case appears.

## Goals

- Ship `internal/tools/drivers/http/` with three files: `http.go` (inline `RegisterHTTPTool` + the per-tool descriptor), `manifest.go` (UTCP-style YAML loader), `auth.go` (static-auth helpers — API key / bearer / cookie).
- Inline path + manifest path converge: both produce a `tools.ToolDescriptor` with `Transport = tools.TransportHTTP` and identical Invoke semantics. A regression test asserts identity across the two paths for the same logical tool.
- Static auth modes (API key as header or query param, bearer token, named cookie) with the secret value loaded from operator-supplied config — never from request payload.
- `Retry-After` header on 429 / 503 honoured: the per-attempt classifier marks the error class as `ErrClassTransient` (so it's retryable) AND surfaces the `Retry-After` floor via `errHTTPRateLimit` so the next attempt sleeps at least that long.
- URL-template substitution: `text/template` with mandatory URL-escaping of substituted values; missing template variables fail loudly with a typed error (NOT silent empty string).
- Identity (`(tenant, user, session)` triple via `identity.MustFrom(ctx)`) is read from ctx on every invocation; the transport itself does NOT propagate user-identity bearer tokens — that's tool-side OAuth (Phase 30).
- `ToolsConfig.HTTPManifests []string` (paths to manifest files) added to `internal/config`; validator coverage; `examples/harbor.yaml` entry.
- Concurrent-reuse contract (D-025): a single HTTP `ToolDescriptor` is safe under N ≥ 100 concurrent invocations against a shared `httptest.Server`; no races, no goroutine leaks.

## Non-goals

- **No OAuth / token-exchange flows.** Phase 30 owns tool-side OAuth via the unified pause/resume primitive. Static auth only at Phase 27.
- **No tenant-credential lookup.** Per-tenant secret routing is post-V1 (operator stores one credential per tool at config time).
- **No HTTP-server-side feature.** This is a southbound driver; Harbor calling external HTTP APIs.
- **No streaming HTTP responses.** Phase 27 handles unary request/response; SSE / chunked streaming is post-V1.
- **No response-body-shaped artifacts.** Heavy outputs from HTTP tools route through the existing `ArtifactStore` at later phases (LLM-edge enforcement). Phase 27 returns the body bytes as `ToolResult.Value`.

## Acceptance criteria

- [ ] `internal/tools/drivers/http/http.go` exports `RegisterHTTPTool(cat, name, method, urlTemplate, opts...) error`, the descriptor builder, and the per-attempt classifier.
- [ ] `internal/tools/drivers/http/manifest.go` exports `LoadManifest(path string) (*Manifest, error)` + `RegisterManifest(cat tools.ToolCatalog, m *Manifest) error`. Manifest schema documented in package godoc.
- [ ] `internal/tools/drivers/http/auth.go` exports `AuthSpec`, `AuthKind`, sentinel `ErrAuthMissing`, and the apply-auth helper.
- [ ] HTTP tools register with `Transport = tools.TransportHTTP` and a non-empty `Source`.
- [ ] Argument-schema source: operator-supplied raw JSON via `WithArgsSchema` / `WithOutSchema`, or manifest's `args_schema` / `out_schema` field. Validation runs at the catalog edge (via the policy shell).
- [ ] URL-template substitution uses `text/template` with mandatory `urlquery` escaping on substituted values; missing variables return `ErrTemplateRender` wrapped via `%w`.
- [ ] Header / body templates support the same substitution; body content-type defaults to `application/json`.
- [ ] Static auth: `api_key` (header OR query, configurable), `bearer`, `cookie`. Secret value is loaded from `Manifest.Auth[auth_ref]`; the loader expands `${ENV_VAR}` references at load time but a manifest may NOT inline a literal secret value (validation rejects bare strings that don't match the `${...}` form).
- [ ] `Retry-After` on 429 / 503: classifier returns `errHTTPRateLimit` (transient-class) carrying the parsed delay; the policy shell sleeps at least that delay before the next attempt. Both seconds-integer and HTTP-date forms parsed.
- [ ] `RunWithPolicy` is called exactly ONCE per Invoke — driver does NOT loop independently. A regression test asserts the per-failed-invocation retry budget is consumed by the policy shell, not by the driver (D-024 no-double-wrap).
- [ ] Identity: every `Invoke` reads `identity.From(ctx)` and stamps it into the per-attempt hooks; missing identity fails the invocation with a typed error.
- [ ] **Concurrent-reuse test (D-025)**: `TestHTTPTool_ConcurrentReuse_NoRace` runs N=128 concurrent `Invoke`s against a single shared HTTP `ToolDescriptor` (and shared `httptest.Server`) under `-race`. No races, no context bleed (per-invocation correlation IDs unique), no goroutine leaks (baseline restored within 2s).
- [ ] **Integration test**: `TestHTTPTool_Integration_RetryAfter` against a flaky `httptest.Server` that returns 429 with `Retry-After: 1` for the first attempt, 200 with the expected body on retry. Asserts: success after one retry, total elapsed ≥ 1s, attempts hook fired exactly twice.
- [ ] **Integration test**: `TestHTTPTool_InlineEqualsManifest` — register the same logical tool via both `RegisterHTTPTool` and `RegisterManifest` against the same `httptest.Server` route; assert the two descriptors produce byte-identical `ToolResult.Value` for the same input.
- [ ] **Integration test**: `TestHTTPTool_AuthHeaderApplied` — fire requests through each of the three auth modes; assert the test server observes the correct header / cookie / query parameter exactly once per request.
- [ ] **Security test**: a manifest that attempts to interpolate `{{ .Auth.Token }}` (i.e. tries to pull a secret into a URL template) returns `ErrTemplateSecretLeak` at load time.
- [ ] `internal/config/config.go` declares `ToolsConfig { HTTPManifests []string }`; `Config.Tools` field added; `validate.go` validates the paths are non-empty strings.
- [ ] `examples/harbor.yaml` shows the new `tools:` section with a commented example.
- [ ] `scripts/smoke/phase-27.sh` runs `go test -race -count=1 -timeout 120s ./internal/tools/drivers/http/...` and reports OK ≥ 1.
- [ ] `docs/plans/README.md` Phase 27 row flips to `Shipped`.
- [ ] `README.md` Status table updated with the Phase 27 entry.
- [ ] `docs/glossary.md` updated with `RegisterHTTPTool`, `UTCP manifest`, `AuthSpec`.

## Files added or changed

- `internal/tools/drivers/http/http.go` (new) — inline `RegisterHTTPTool` + descriptor builder + classifier.
- `internal/tools/drivers/http/manifest.go` (new) — manifest schema + loader + `RegisterManifest`.
- `internal/tools/drivers/http/auth.go` (new) — static-auth helpers.
- `internal/tools/drivers/http/http_test.go` (new) — unit + integration tests.
- `internal/tools/drivers/http/manifest_test.go` (new) — manifest-loader tests.
- `internal/tools/drivers/http/auth_test.go` (new) — auth-helper tests.
- `internal/tools/drivers/http/concurrent_test.go` (new) — concurrent-reuse test.
- `internal/tools/drivers/http/testdata/manifest_valid.yaml` (new) — fixture.
- `internal/tools/drivers/http/testdata/manifest_secret_leak.yaml` (new) — negative fixture.
- `internal/config/config.go` (modified) — add `Tools ToolsConfig` field.
- `internal/config/validate.go` (modified) — add `validateTools` validator.
- `internal/config/validate_test.go` (modified) — add validator coverage.
- `examples/harbor.yaml` (modified) — add `tools:` section.
- `scripts/smoke/phase-27.sh` (new) — smoke test.
- `docs/plans/phase-27-tools-http.md` (this file).
- `docs/plans/README.md` (modified) — flip Phase 27 to `Shipped`.
- `README.md` (modified) — add Phase 27 row.
- `docs/glossary.md` (modified) — new vocabulary.

## Public API surface

```go
package http // import "github.com/hurtener/Harbor/internal/tools/drivers/http"

func RegisterHTTPTool(
    cat tools.ToolCatalog,
    name, method, urlTemplate string,
    opts ...HTTPOption,
) error

type HTTPOption func(*httpToolConfig)

func WithClient(c *http.Client) HTTPOption
func WithAuth(spec AuthSpec, secret string) HTTPOption
func WithHeaders(h map[string]string) HTTPOption
func WithBodyTemplate(tmpl string) HTTPOption
func WithHTTPSource(id tools.ToolSourceID) HTTPOption
func WithArgsSchema(schema []byte) HTTPOption
func WithOutSchema(schema []byte) HTTPOption
func WithDescription(s string) HTTPOption
func WithPolicy(p tools.ToolPolicy) HTTPOption
func WithTags(tags ...string) HTTPOption
func WithSideEffect(s tools.SideEffect) HTTPOption
func WithLoading(m tools.LoadingMode) HTTPOption

type AuthSpec struct {
    Kind       AuthKind
    HeaderName string
    QueryParam string
    CookieName string
}

type AuthKind string

const (
    AuthKindAPIKey AuthKind = "api_key"
    AuthKindBearer AuthKind = "bearer"
    AuthKindCookie AuthKind = "cookie"
)

func LoadManifest(path string) (*Manifest, error)
func RegisterManifest(cat tools.ToolCatalog, m *Manifest) error

type Manifest struct {
    Tools []ManifestTool
    Auth  map[string]ManifestAuth
}

var (
    ErrTemplateRender     = errors.New("http: URL/body template render failure")
    ErrTemplateSecretLeak = errors.New("http: template attempted to interpolate auth/secret reference")
    ErrManifestInvalid    = errors.New("http: manifest invalid")
    ErrAuthMissing        = errors.New("http: auth secret missing or empty")
    ErrUnsupportedMethod  = errors.New("http: HTTP method not supported")
    ErrIdentityMissing    = errors.New("http: identity missing from ctx")
)
```

```go
// internal/config/config.go (additions)

type ToolsConfig struct {
    HTTPManifests []string `yaml:"http_manifests,omitempty"`
}

// Config gets a new field:
//   Tools     ToolsConfig     `yaml:"tools,omitempty"`
```

## Test plan

- **Unit:**
  - `http_test.go`: URL-template render with valid + missing variables; `urlquery` escaping of `?&/` in substituted values; method allowlist (`POST`, `GET`, `PUT`, `DELETE`, `PATCH`); body template render; error classifier handles 200, 4xx (permanent), 5xx (5xx class), 429 (transient with Retry-After), 503 (transient with Retry-After), connection-refused (transient).
  - `manifest_test.go`: load valid YAML; reject unknown fields; reject literal secret values; expand `${ENV}` refs from `os.Getenv`; reject missing referenced env vars; reject duplicate tool names within one manifest; reject template that interpolates `.Auth`.
  - `auth_test.go`: each kind produces the right `*http.Request` shape; missing secret → `ErrAuthMissing`.
- **Integration:**
  - `TestHTTPTool_Integration_Roundtrip`: `httptest.NewServer` echoes back a JSON body; tool registered inline with raw JSON schemas; identity-bearing ctx; assert body sent correctly, response parsed back.
  - `TestHTTPTool_Integration_RetryAfter`: flaky server returns `429 Retry-After: 1` once then `200`; assert success after one retry with elapsed ≥ 1s.
  - `TestHTTPTool_Integration_5xxTransient`: flaky server returns `503` twice then `200`; assert success after two retries with exponential backoff.
  - `TestHTTPTool_Integration_4xxPermanent`: server returns `400`; assert non-retryable failure on the first attempt; attempts hook fires exactly once.
  - `TestHTTPTool_InlineEqualsManifest`: two equivalent registrations produce indistinguishable results.
  - `TestHTTPTool_AuthHeaderApplied`: each of the three auth kinds verified end-to-end.
  - `TestHTTPTool_NoDoubleRetry_PolicyShellExclusiveOwner`: a single-Invoke run consumes ONE policy retry budget (not driver × policy).
- **Conformance:** N/A — the conformance suite is transport-agnostic; the in-process driver test already runs it. Phase 27 inherits the catalog-level contract for free (HTTP descriptors register the same way; the test would just be inproc-flavoured against a server stub).
- **Concurrency / leak:** `TestHTTPTool_ConcurrentReuse_NoRace` (N=128 goroutines under `-race`; per-goroutine correlation IDs verified; baseline goroutine count restored within 2s).

## Smoke script additions

- `scripts/smoke/phase-27.sh` runs `go test -race -count=1 -timeout 120s ./internal/tools/drivers/http/...` and reports `OK` on pass. The HTTP/Protocol surface stub is intentionally skipped (mirrors Phase 26 — no boot-time HTTP surface yet).

## Coverage target

- `internal/tools/drivers/http`: 85%.

## Dependencies

- Phase 26 (tool catalog + ToolPolicy + ToolProvider interfaces).
- Phase 02 (config loader — `ToolsConfig` plugs in here).
- Phase 01 (identity ctx propagation).

## Risks / open questions

- **Manifest schema vs UTCP spec drift.** UTCP itself is an evolving spec; Harbor's manifest is "UTCP-style," not "UTCP-conformant." If/when UTCP v1 stabilizes, a converter is a one-off post-V1 add. Documenting the schema in package godoc keeps the contract local.
- **`Retry-After` honouring vs policy backoff floor.** The policy's exponential backoff is the floor; `Retry-After` is the additional sleep. A very long `Retry-After` blowing the run's ctx deadline is correct behaviour: ctx cancellation fires, attempt aborts loudly. Tested.
- **Idempotency on retry of write-shaped tools.** Same as in-process tools: `SideEffect = write/external` is a hint, NOT a retry-gate. Operators set `RetryOn` appropriately. Documented inline.

## Glossary additions

- `RegisterHTTPTool` — inline registration helper for HTTP tools; mirrors `inproc.RegisterFunc`. Phase 27.
- `UTCP manifest` — UTCP-style YAML schema describing HTTP endpoints as Harbor tools. Phase 27. Operator deployment shape; the inline helper is the dev-loop shape.
- `AuthSpec` — static-auth specification attached to an HTTP tool; `Kind` ∈ {`api_key`, `bearer`, `cookie`}; secret value loaded from `Manifest.Auth[auth_ref]`, never from request payload. RFC §6.4, AGENTS.md §7.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: identity propagation tested via the integration tests
- [ ] **Concurrent-reuse test passes** — `TestHTTPTool_ConcurrentReuse_NoRace` runs N≥100 concurrent invocations against a single shared `ToolDescriptor` under `-race` (D-025).
- [ ] **Integration test exists** — `TestHTTPTool_Integration_*` test family wires real `httptest.Server` + the policy shell + identity ctx end-to-end (AGENTS.md §17).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (or N/A justified — the URL-template tightening is documented in this plan and may receive a D-NNN entry if reviewers request)
