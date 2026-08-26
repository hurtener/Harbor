# Phase 264 — Optional grant-free provider route resolver

## Summary

Add one Bifrost-specific route resolver that is invoked only for an explicit
opaque route after ordinary JWT identity and effective-Agent reach admission.
It is independent of external grants and leaves runtime-default execution fully
latent and unchanged.

## Goals

- Preserve nil-resolver/no-route execution with no boot requirement, worker,
  timer, cache, store read, or network call.
- Bind resolution to verified tenant/user/session/run, effective Agent,
  runtime/task/call, opaque route/connection generations, and model selector.
- Confine an exact-bound expiring credential to one Bifrost attempt.
- Permit only the finite Bifrost v1.7.4 chat-capable route set.
- Map Azure, vLLM, Ollama, SGLang, and OpenAI-compatible typed endpoints without
  accepting a generic credential or endpoint object.
- Extend protected validation/discovery with opaque route observations.

## Non-goals

- Run authorization, usage metering, quotas, policy grants, generic provider
  endpoints, background reconciliation, or `ExternalGrant` reuse.
- Automatic fallback after an explicit route fails.
- A public remove-provider method; the resolver stores no runtime provider
  assignment and therefore needs no uninstall lifecycle.

## Acceptance

- Runtime-default regression and zero-work tests.
- Missing resolver, malformed/mismatched/expired/revoked generation refusal.
- Concurrent two-tenant/same-runtime/same-provider credentials never bleed.
- Resolver transport refuses redirects, unsafe endpoints, oversized replies,
  and raw coordinator bodies in errors. Boot-pinned endpoints accept HTTPS (or
  loopback HTTP) only and refuse userinfo, fragments, all query strings,
  non-http(s) schemes, and control characters; request/response bytes and
  duration are bounded.
- Bifrost-internal retry is disabled for routed calls; Harbor retry/downgrade
  attempts resolve within their own trusted call context and cannot reuse a
  revoked or foreign route secret.
- OpenAI-compatible endpoint clients are bounded and isolated by exact tenant,
  runtime, route/connection/credential generations, provider, and normalized
  endpoint digest; eviction and Close drain them.
- Route-aware validate/discover requires admin plus effective-Agent reach and
  returns only generations/readiness.
- Protocol source/index/client/manifest/reference and operator skills remain in
  lockstep; hosted CI owns all Go, race, lint, build, conformance, and smoke
  execution for this change.
- Two independent adversarial reviews report P0=0/P1=0 at exact candidate.

## Public surface

- `sdk/llm.ProviderRouteResolver` and related opaque request/result aliases.
- Optional `llm.provider_route` stock authenticated transport config.
- Additive provider-route fields on `control.start` and `llm.posture` provider
  operations. Protocol version remains `0.1.0`.

## Security invariants

- Protocol requests carry no provider, endpoint, secret, tenant, or user route
  authority.
- The runtime installs route context only after Agent-reach restoration.
- A response must exact-match every opaque ID/generation and be unexpired.
- Credentials and endpoint values are never logged/emitted/persisted;
  credentials are not cached and endpoint pool keys contain only the digest.
- Explicit-route failure is loud and cannot reach the LiveKey fallback.

## Evidence status

Implementation candidate only. No local Go/test/build/lint command is run by
owner instruction; hosted web CI is authoritative. No merge, tag, release,
deployment, or downstream acceptance is claimed by this phase document.
