# Recipe: control attachment disposition

How an uploaded attachment reaches the model is **declared policy**,
not a runtime hardcode (Phase 84b, D-189). The disposition enum is
`ref` / `inline` / `provider_native` / `tool:<name>`, resolved with
explicit precedence:

> per-attachment caller hint > per-agent policy map > runtime default

The runtime default is byte-for-byte the pre-84b behaviour: `image/*`
inlines as a DataURL (the sub-threshold vision fast path); everything
else is emitted as an `ArtifactStub` **by reference** with a
`Fetch.Tool` hint, so the planner fetches and processes the bytes
through a tool — the developer-controllable path.

The **layers are semantic; the carriers are adapters**. The Protocol
`disposition` field and the `harbor.yaml` block below are thin
carriers over a planner-homed policy core — a Go program embedding the
runtime headless authors the same policy directly, with no Protocol
and no config file. That headless path is this recipe's main course.

> **Import paths.** Go snippets use the public `sdk/` facade
> (`github.com/hurtener/Harbor/sdk/...` — D-204).

## The headless path 1 — you ARE the top precedence layer

A consumer that builds its own `planner.InputArtifactView` slice
(hand-rolled run loop, custom `RunContext`) sets `Disposition`
directly. Nothing else is consulted — the view's value is final:

```go
import "github.com/hurtener/Harbor/sdk/planner"

views := []planner.InputArtifactView{
    {
        ID:        "art_q3_report",
        MIME:      "application/pdf",
        SizeBytes: 882_113,
        Filename:  "q3-report.pdf",
        // Force the catalog's pdf.extract tool — the emitted
        // ArtifactStub carries Fetch.Tool: "pdf.extract" and the
        // planner calls it instead of shipping the PDF anywhere.
        Disposition: planner.DispositionTool("pdf.extract"),
    },
    {
        ID:          "art_screenshot",
        MIME:        "image/png",
        SizeBytes:   24_576,
        Filename:    "screenshot.png",
        Bytes:       screenshotBytes, // inline needs the bytes
        Disposition: planner.DispositionInline,
    },
}
// Hand the views to the planner turn as usual:
rc := planner.RunContext{ /* ... */ InputArtifacts: views}
```

A zero `Disposition` ("") selects the runtime default for that view —
existing code is unchanged.

## The headless path 2 — programmatic policy + the pure resolver

When the hint comes from elsewhere (your own wire surface, a job
queue), build a `DispositionPolicy` and call the exported pure
resolver — the same function the `harbor dev` run loop calls:

```go
import "github.com/hurtener/Harbor/sdk/planner"

policy := planner.DispositionPolicy{
    ByMIME: map[string]planner.AttachmentDisposition{
        "application/pdf": planner.DispositionTool("pdf.extract"),
        "audio/*":         planner.DispositionTool("audio.transcribe"),
    },
    Default: planner.DispositionRef,
}

// hint = "" → the policy map / default decide. The second return
// names the layer that won (caller_hint / agent_policy /
// runtime_default) — log it so the resolution is auditable.
resolved, layer := planner.ResolveDisposition("", policy, "application/pdf")

// Normalise against runtime capability: provider_native degrades to
// ref until 84c ships; an unknown tool:<name> degrades to ref when a
// catalog view is supplied. The degradation comes back as a TYPED
// fact — the resolver never logs; YOU log/emit it (fail-loud).
effective, degradation := planner.EffectiveDisposition(resolved, "application/pdf", catalogView)
if degradation != nil {
    logger.Warn("disposition degraded",
        "from", degradation.From, "to", degradation.To,
        "reason", degradation.Reason, "tool", degradation.Tool)
}
view.Disposition = effective
_ = layer
```

Consumers that drive the shared run-loop helper get all of this in
one call — `runctx.ResolveInputArtifacts` (sdk/runctx) accepts an
`InputArtifactOptions{Hints, Policy, Catalog, Emit}` and resolves,
fetches inline bytes, logs the winning layer, and emits one
`task.input_disposition.resolved` event per attachment.

## The config carrier — `harbor.yaml`

The per-agent policy map decodes from `multimodal.disposition`
(`planner.DispositionPolicyFromConfig` is the projection):

```yaml
multimodal:
  disposition:
    "application/pdf": "tool:pdf.extract" # exact media type
    "image/*": inline                     # family wildcard
    "*": ref                              # agent-wide default
```

## The Protocol carrier — per-attachment `disposition` hint

The Playground composer and any Protocol client override per upload
via the optional `input_artifact_dispositions` map on `start`:

```json
{
  "query": "summarise the attached report",
  "input_artifact_ids": ["art_q3_report"],
  "input_artifact_dispositions": { "art_q3_report": "tool:pdf.extract" }
}
```

`tasks.get` reflects the hint back on `input_artifacts[].disposition`,
and every resolution (including hint-less defaults) is observable on
the `task.input_disposition.resolved` event stream — disposition,
winning layer, and any degradation fact.

## The enum, precisely

| Value | Meaning | Notes |
|-------|---------|-------|
| `ref` | `ArtifactStub` + `Fetch.Tool` hint; the planner processes bytes via a tool | The runtime default for non-image MIMEs |
| `inline` | DataURL inline | `image/*` only at V1.1; the runtime default for images. Non-image `inline` degrades to `ref` with a returned fact |
| `provider_native` | The provider's own document/vision understanding | Phase 84c mechanism; until it ships, resolves but degrades to `ref` with a logged notice — never a silent no-op |
| `tool:<name>` | Force the named catalog tool via `Fetch.Tool` | An unknown name degrades to `ref` + warning (resilience over hard failure) |
