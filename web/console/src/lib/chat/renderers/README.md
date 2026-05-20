# Canonical renderer registry

The shared dispatch table that maps a MIME type onto a `.svelte`
renderer component. Introduced by Phase 73l (D-120) — the Artifacts page
preview pane is the first in-staging consumer.

## The dispatch contract (stable — extend, do not rewrite)

- `RendererProps` — every renderer accepts `{ mime, src, filename }`.
  `src` is a **resolved URL** (a presigned URL per D-026) — renderers
  never receive inline heavy bytes.
- `RendererDescriptor` — `{ source, component }`. `source` is a stable
  id exposed by the host as `data-renderer-source` for tests/telemetry.
- `registerRenderer(predicate, descriptor)` — appends a rule. Rules are
  evaluated in registration order; first match wins.
- `dispatchRenderer(mime)` — returns the winning descriptor, or
  `fallbackRenderer` when no rule matches.

## How to add a renderer (the 73n contract)

Phase 73n (Playground, Stage 2.3) is the **second consumer**. It adds
chat-bubble / tool-call / diff renderers by calling `registerRenderer`
from its own module init — it does **not** edit `index.ts`'s dispatch
core. The dispatch table is open for registration, closed for
modification.

```ts
import { registerRenderer, mimeIs } from '$lib/chat/renderers';
import DiffRenderer from './diff.svelte';

registerRenderer(mimeIs('application/vnd.harbor.diff'), {
  source: 'diff',
  component: DiffRenderer
});
```

## Built-in MIME renderers (Phase 73l first-consumer set)

`markdown`, `code`, `image`, `pdf`, `audio`, `json` — plus the
`fallback` renderer for unrenderable MIME types.
