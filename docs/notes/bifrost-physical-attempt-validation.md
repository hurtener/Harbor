# Bifrost physical-attempt bound correction

## Prior-source failure

The local race test `TestPinnedBifrostTransportPhysicalPOSTBound` ran against
Harbor commit `74dde5ec11282d5e05bbe00380f341dd4200a0e2`, Bifrost core v1.9.0 and
fasthttp v1.74.0. Both buffered and streaming transport modes recorded four fully
consumed loopback POST bodies from one SDK request while Harbor declared three
attempts at `MaxRetries=1`. The server recorded consumption before dropping
response headers. No SDK attempt metadata supplied the count, and no paid
service, real credential or production configuration was used.

## Bound proof

- OpenAI and Anthropic constructors create a fasthttp client with the physical
  attempt field unset, installing `ConfigureDialer`'s context transport. The
  unset field selects `fasthttp.DefaultMaxIdemponentCallAttempts`, currently five
- Streaming and large-response client clones preserve the physical attempt
  field. The admitted text Chat handlers invoke one `Do`; they do not use the
  follow-redirect helper. Files/media and opaque parameters are refused
- `contextTransport` writes and flushes the POST before response-header failure
  can request replay on a reused socket. The stale callback currently allows
  three retries, but the conservative reservation uses the outer five-send cap
- `executeRequestWithRetries` makes at most `MaxRetries + 2` logical calls. Its
  extra encrypted-reasoning allowance is guarded to fire once. Every logical
  call can enter the physical loop, so the limits multiply
- Harbor's translator emits no `Fallbacks`, raw body or arbitrary extra params
  for admitted requests. The driver uses ChatCompletion, preserving the explicit
  request output cap; no alternative provider branch is covered by a guessed cap
- Selected routes expose a credential-free provider/model before reservation.
  Only OpenAI/Anthropic selections and their valid endpoint shapes are admitted.
  The leaf exact-matches fresh resolution before transport; both route account
  factories set SDK retries to zero. Custom providers require the actual OpenAI
  base type. Other SDK/net/http provider families remain unproved and refuse

The result is `5 * (MaxRetries + 2)`, subject to the existing 1,000-attempt
ceiling and checked token/money arithmetic. Exact endpoint/model/version pricing
authority and the narrower monetary allowlist are unchanged.

## Corrected-source checks

The corrected focused race selection passed 11 test events with no skips or
failures using Go 1.27.1 and serial package execution. It covers provider/route
refusal, cap overflow, exact request output limits, no emitted fallback/raw body,
the original physical replay regression, and the real native OpenAI factory.
The native server independently consumed four POST bodies for one logical call
and six when transport replay composed with a logical SDK retry. Only the final
response reported its ordinary 35-token usage. Each case reserved the multiplied
16,500-token envelope, retained 16,465 tokens as unknown, and refused a subsequent
call before transport. Existing monetary retry tests retained the complete
105-micro-USD synthetic quote with no charged spend claim.

```sh
go test -race -p 1 ./internal/llm/drivers/bifrost -count=1 -v \
  -run '^Test(PinnedBifrostTransportPhysicalPOSTBound|AllocationBound_|Allocation_|MonetaryAllocation_Bifrost)'
```

The first minimal reproduction invocation stopped on an int/int64 test comparison
at compile time. After that test-only cast correction, both modes failed against
the old production bound for the intended four-versus-three reason. The same
reproduction passed against the multiplied bound, declaring 15 for the observed
four consumed requests. Logs and exact source manifests are retained separately.

## Settlement and release limits

The complete offline Bifrost driver race suite subsequently passed 241 test
events. Its three paid live probes were explicitly disabled and skipped;
all ordinary driver tests remained selected. Scoped golangci-lint reported
zero issues, and canonical Markdown lint passed 611 files with zero errors.
These checks qualify the corrected source; combined binary and consumer gates
remain separate.

The upper bound is never an actual-attempt count. SDK logical trails and final
response usage do not prove intermediate physical work free. Reported final
usage may settle its known tokens; all remaining possible liability stays held.
The correction intentionally changes no settlement, store, tariff or credential
contract. Closed allocations with unknown usage still cannot release capacity.
This can retain most of a successful call's allocation indefinitely.

Already accepted ceilings remain immutable. The corrected larger envelope can
refuse subsequent work; it cannot replenish a task or certify earlier calls made
under an undercounted bound. Ordinary unallocated provider compatibility remains.

This note records local candidate evidence. Complete consumer/preflight gates,
qualified binaries and the required independent release review remain separate
release-owner gates. No stable release readiness is claimed.
