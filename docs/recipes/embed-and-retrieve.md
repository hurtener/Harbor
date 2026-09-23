# Recipe: embed and retrieve

Harbor's embedding client (`Embedder`, Phase 84d — D-191) turns text
into vectors. It is a **standalone, factory-constructible primitive**:
semantic skill retrieval is a built-in consumer, not its gatekeeper. This recipe walks the à-la-carte
headless path — `embeddings.Open` + `Embed` + cosine ranking over your
own corpus, with **no memory subsystem, no config file, no Protocol**
— then shows the one-knob opt-in for skill retrieval.

This is the primitive that makes the `ref` / `tool:<name>` document
path powerful: keep a document as an artifact reference (see
[Control attachment disposition](control-attachment-disposition.md)),
process it with your own tool, and retrieve over its chunks
semantically.

> **Import paths.** Go snippets use the public `sdk/` facade
> (`github.com/hurtener/Harbor/sdk/...` — D-204).

## 1. Open the embedder

The `Embedder` is a §4.4 driver seam. The production driver is
`bifrost` (the provider gateway); register it via the production
aggregator and open through the factory. The embedding model is its
own choice — nothing falls back to your chat `llm` settings:

```go
import (
    _ "github.com/hurtener/Harbor/sdk/drivers/prod" // driver registrations

    "github.com/hurtener/Harbor/sdk/embeddings"
)

emb, err := embeddings.Open(ctx, embeddings.ConfigSnapshot{
    Driver:   "bifrost", // also the default when empty
    Provider: "openai",
    Model:    "text-embedding-3-small",
    APIKey:   "env.OPENAI_API_KEY", // literal or env.NAME, same convention as chat
}, embeddings.Deps{})
if err != nil {
    return err // misconfiguration fails loudly and names the field
}
defer emb.Close(ctx)
```

The same snapshot decodes from `harbor.yaml`'s `embeddings:` block
(`embeddings.SnapshotFromConfig`) — config and programmatic
construction are the same code path.

## 2. Identity is mandatory at the Embed edge

Embedding calls are billable provider traffic; the edge fails closed
without an identity, exactly like the chat edge. Headless consumers
stamp one with `identity.With` (or `identity.WithRun`):

```go
import "github.com/hurtener/Harbor/sdk/identity"

ctx, err := identity.With(ctx, identity.Identity{
    TenantID: "acme", UserID: "ana", SessionID: "sess-1",
})
if err != nil {
    return err
}
```

A bare `context.Background()` gets `embeddings.ErrIdentityMissing` —
there is no opt-out knob.

## 3. Embed your corpus and rank with Cosine

One batched `Embed` call returns one vector per input, in input
order. Rank with the shared `embeddings.Cosine` primitive — don't
hand-roll a second cosine:

```go
corpus := []string{
    "the harbor pier accepts boats up to twelve meters",
    "the cafeteria opens at seven",
    "quarterly revenue grew eight percent",
}

vecs, err := emb.Embed(ctx, corpus)
if err != nil {
    return err
}

qv, err := emb.Embed(ctx, []string{"where can I moor my boat?"})
if err != nil {
    return err
}

best, bestScore := -1, -1.0
for i, v := range vecs {
    score, err := embeddings.Cosine(qv[0], v)
    if err != nil {
        return err // dimension mismatch = embedding-model drift; re-embed
    }
    if score > bestScore {
        best, bestScore = i, score
    }
}
fmt.Println(corpus[best]) // → the pier sentence
```

Two rules worth keeping:

- **Vectors are derived data.** Persist the model name alongside any
  vectors you store; vectors from different models (or different
  `Dimensions`) are not comparable — `Cosine` fails loudly with
  `ErrDimensionMismatch` instead of silently mis-ranking.
- **Brute force is the V1 design.** At conversation/catalog scale a
  linear cosine scan is the honest implementation; reach for an ANN
  index only when your corpus actually demands it.

## 4. Session memory is not a vector index

Native session-memory semantic retrieval was removed under D-477.
Use cumulative `memory.strategy: rolling_summary` for short-term session
continuity. External long-term retrieval stays in your integration or capability;
it is not another Harbor memory store.

## 5. Consumer opt-in: semantic skill retrieval

Same pattern on the skills store: `skill_search` (and any direct
`Search`) ranks the identity-scoped catalog by similarity instead of
the FTS5 → regex → exact ladder, reporting result path `"semantic"`.
Capability filtering, redaction, and the budgeter apply unchanged:

```go
import "github.com/hurtener/Harbor/sdk/skills"

store, err := skills.Open(ctx, skills.ConfigSnapshot{
    Driver:    "localdb",
    DSN:       "/var/lib/harbor/skills.sqlite",
    Retrieval: skills.RetrievalSemantic,
}, skills.Deps{Bus: bus, Embedder: emb})
```

Config carrier: `skills.retrieval: semantic`.

## 6. Failure modes you should expect (and want)

| Situation | Behaviour |
|---|---|
| Semantic mode enabled, no `Deps.Embedder` | `Open` errors naming `Deps.Embedder` (skills) |
| Removed `memory.retrieval*` settings | Configuration fails; remove the obsolete settings |
| `Embed` without identity in ctx | `ErrIdentityMissing` — fail closed, like the chat edge |
| Embedding provider down mid-search | the search errors loudly; **never** a silent fallback to lexical ranking |
| Vector dimension mismatch (model changed) | `ErrDimensionMismatch` — re-embed; vectors are derived, not source-of-truth |

## Related

- [Control attachment disposition](control-attachment-disposition.md)
  — the `ref` / `tool:<name>` document path this primitive empowers.
- [Use memory and skills from Go](use-memory-and-skills-from-go.md) —
  the broader headless memory/skills surface.
- [Embed Harbor headless](embed-harbor-headless.md) — full-stack
  assembly (where `assemble.Assemble` opens the embedder from the
  config block for you, on `Stack.Embedder`).
