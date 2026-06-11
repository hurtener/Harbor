# Recipe: provider-native attachments

Hand an attachment to the **provider's own understanding** — vision
for over-threshold images, native audio/video/document ingestion —
instead of degrading it to an `ArtifactStub` text reference the model
cannot see into. The driver uploads the bytes to the provider's file
surface (`file_id`) **inside `Complete`**, so `LLMClient` stays one
method and the whole mechanism is reachable with zero planner, zero
run loop, zero config file, zero Protocol.

This is the `provider_native` attachment disposition
([control-attachment-disposition](control-attachment-disposition.md))
— always **opt-in**, never the runtime default.

> **Import paths.** Go snippets use the public `sdk/` facade
> (`github.com/hurtener/Harbor/sdk/...` — D-204).

## The headless path — `llm.Open` + the `ProviderNative` flag

A consumer building a `CompleteRequest` by hand sets the part-level
`ProviderNative` flag directly. No `harbor dev` anywhere:

```go
import (
    "context"

    // The bifrost driver self-registers via the production aggregator.
    _ "github.com/hurtener/Harbor/sdk/drivers/prod"

    "github.com/hurtener/Harbor/sdk/artifacts"
    "github.com/hurtener/Harbor/sdk/config"
    "github.com/hurtener/Harbor/sdk/events"
    "github.com/hurtener/Harbor/sdk/identity"
    "github.com/hurtener/Harbor/sdk/llm"
)

// 1. The LLM client needs its two deps: an artifact store (the
//    upload pass reads attachment bytes from it) and the event bus
//    (the upload emits `llm.provider_file.uploaded`).
store, _ := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
bus, _ := events.Open(ctx, eventsCfg, redactor)

client, err := llm.Open(ctx, llm.ConfigSnapshot{
    Driver:   "bifrost",
    Provider: "openai",
    APIKey:   "env.OPENAI_API_KEY", // env-var indirection — never inline a key
    Model:    "gpt-5.3-chat",
    ModelProfiles: map[string]llm.ModelProfile{
        "gpt-5.3-chat": {ContextWindowTokens: 128_000},
    },
}, llm.Deps{Artifacts: store, Bus: bus})
if err != nil {
    return err
}
defer client.Close(ctx) // sweeps the provider-side files (see lifecycle)

// 2. Identity is mandatory at the LLM edge (§6) — stamp the triple.
ctx, err = identity.With(ctx, identity.Identity{
    TenantID: "acme", UserID: "u-42", SessionID: "s-1",
})

// 3. Store the attachment and flag the part. The part carries the
//    canonical ArtifactStub; the driver fetches the bytes, uploads
//    them, and rewrites the part to the provider's `file_id` before
//    the chat call.
scope := artifacts.ArtifactScope{TenantID: "acme", UserID: "u-42", SessionID: "s-1"}
ref, _ := store.PutBytes(ctx, scope, screenshotBytes, artifacts.PutOpts{MimeType: "image/png"})

prompt := "What does this screenshot show?"
resp, err := client.Complete(ctx, llm.CompleteRequest{
    Messages: []llm.ChatMessage{{
        Role: llm.RoleUser,
        Content: llm.Content{Parts: []llm.ContentPart{
            {Type: llm.PartText, Text: prompt},
            {Type: llm.PartImage, Image: &llm.ImagePart{
                Artifact: &llm.ArtifactStub{
                    Ref:       ref.ID,
                    MIME:      "image/png",
                    SizeBytes: int64(len(screenshotBytes)),
                },
                MIME:           "image/png",
                ProviderNative: true, // ← the whole opt-in
            }},
        }},
    }},
})
```

Sub-threshold inline content works too — a `DataURL`-supplied part
with `ProviderNative: true` uploads the decoded bytes directly, no
artifact store involved. (Over-threshold `DataURL`s are auto-
materialized to `Artifact` form by the safety net first — D-026.)

## Per-modality dispatch

| Content | Part shape | Notes |
|---------|-----------|-------|
| `image/*` | `ImagePart` | Priority 1 — restores vision the stub path loses |
| `audio/*` | `AudioPart` | End-to-end audio in |
| `video/*` | `FilePart` | Provider-native video understanding where supported (e.g. Gemini) |
| `application/pdf`, documents | `FilePart` + `DocumentType` (`"pdf"`, `"csv"`, …) | Last on purpose — the `ref`/`tool:<name>` route plus retrieval is usually the better document path |

A part can also arrive with `ProviderFileID` pre-set (a file you
uploaded out of band): the driver skips the upload and emits the
reference as-is.

## The `file_id` lifecycle — driver-owned, end to end

- **Cache.** Uploads are cached per
  `(tenant, user, session, content)` — re-attaching the same artifact
  next turn reuses the `file_id` instead of re-uploading. A `file_id`
  never crosses a session boundary (§6 isolation).
- **TTL + LRU.** Entries expire (default 1 hour) and the cache is
  capacity-bounded (default 128); both paths best-effort delete the
  remote file (`FileDeleteRequest`).
- **`Close`.** `client.Close(ctx)` sweeps every remaining cached file
  — a headless consumer who never runs a dev loop does not leak
  provider-side files.

## Observability — the event, not a task field

Every upload emits **`llm.provider_file.uploaded`** on the bus
(artifact ref, provider, modality, `file_id`, size, identity). That
event is the whole observability surface — no task field carries the
`file_id`, and the run loop never pre-uploads.

```go
sub, _ := bus.Subscribe(ctx, events.Filter{
    Tenant: "acme", User: "u-42", Session: "s-1",
    Types: []events.EventType{"llm.provider_file.uploaded"},
})
```

## Degradation — loud, never silent

A provider without file-upload support for a modality (bifrost
reports `unsupported_operation`) keeps the part's canonical
`ArtifactStub` rendering — the universal degradation (RFC §6.5) — and
the driver logs a Warn naming the provider and modality. Any other
upload failure fails the `Complete` call outright.

## The managed path — policy routes here

On the full runtime (`harbor dev`, the Protocol, the Playground) you
rarely set the flag yourself: declare the disposition instead and the
materializer flags the part for you.

```yaml
# harbor.yaml — opt a MIME family in, per agent
multimodal:
  disposition:
    "image/*": provider_native
```

…or per attachment from any Protocol client:

```json
{
  "query": "what is in this image?",
  "input_artifact_ids": ["art_abc123"],
  "input_artifact_dispositions": { "art_abc123": "provider_native" }
}
```

The resolution is observable on `task.input_disposition.resolved`;
the upload on `llm.provider_file.uploaded`. See
[control-attachment-disposition](control-attachment-disposition.md)
for the full precedence model.
