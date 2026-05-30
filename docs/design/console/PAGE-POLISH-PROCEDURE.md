# Console Page-Polish Procedure — verbatim-vs-mock, wire-by-wire

> **Binding for the Phase 108 page-polish series.** Phase 108 opens a page-by-page
> pass that brings every Console page to **verbatim parity with its mock** — in
> look & feel AND in functionality — with **every datum wired to real Protocol
> data, verified end-to-end against a live Runtime.** This document is the gate
> each page wave (108b nav, 108c overview, … one wave per page) passes before it
> is "done." It encodes the exact component-by-component, wire-by-wire method the
> Playground (108 / 108a) was finished with.
>
> Authority chain: `RFC-001` > the page's `docs/design/console/page-*.md` >
> `docs/design/console/CONVENTIONS.md` > this procedure. A page wave's phase plan
> MUST cite this procedure in its "Console consistency" section.

---

## 0. The acceptance bar (what "done" means)

A page is done when ALL of the following hold — each one demonstrated, not asserted:

1. **Every component in the mock is present** and pixel-faithful: spacing, type
   scale, colour, radius, motion — all via design tokens (`tokens.css`), zero raw
   literals (`stylelint` enforced).
2. **Every datum the page shows is real-wired** — traced end-to-end to a Protocol
   method or event and **verified against live Runtime data** (§3). No fabricated,
   placeholder, or synthesised values fill the UI.
3. **Every interactive element performs its action** against the real Runtime and
   the resulting state change is observed (§4).
4. **All four `PageState` branches render** correctly — Loading / Loaded / Empty /
   Error — each one forced and seen (§5).
5. **The shell is correct** — viewport-locked, internal scroll only, no full-page
   scroll, no white bleed, responsive (§6).
6. **Browser truth confirms it** — Playwright renders the page with **zero console
   errors**, the DOM matches the mock, and **hydration** works (reload shows the
   persisted state, not an empty page) (§7).
7. **The wave-end checkpoint audit is FAIL-free** (§9).

If any one fails, the page is not done. Partial is not done.

---

## 1. The non-negotiable rules

- **No fabrication.** Never display, claim, or test a value the Runtime did not
  actually produce. If the Runtime exposes no source for something the mock shows,
  that is a finding — surface it (it becomes a Protocol-surface gap or a deferred
  item), never a hand-invented value. "It looks right" is not "it is wired."
- **Verify against real data, not against the code.** Reading the decoder and
  concluding "it should work" is insufficient. Probe the live Runtime, capture the
  real payload, and confirm the rendered value equals it (§3).
- **Live source, never the embedded build.** Test against the **vite dev server**
  (`npm run dev` in `web/console/`). The `harbor console` embedded build is only as
  fresh as the last binary rebuild and will hide your changes. (§2)
- **Fix what you find, wherever it lives (§17.6).** When a pass surfaces a wiring,
  functional, or depth bug — even one rooted in the Runtime, the Protocol, or
  another component/phase — fix it in the same wave. Do not defer with "that's a
  different phase."
- **Commit through the gate.** Each meaningful change is its own commit through the
  pre-commit preflight: `svelte-check --fail-on-warnings` 0/0, `npm run lint`
  clean (tokens, no raw literals), the page's unit specs, `make drift-audit`, the
  phase smoke. No `--no-verify`.

---

## 2. Environment setup (once per session)

1. **Boot the Runtime against a real agent with real data** — a configured LLM, real
   tools, a real state/memory backend. Not a mock provider. Capture the boot log
   (`driver_* ...`) to confirm the drivers you expect.
2. **Serve the Console from live source:** `npm run dev -- --port <P> --strictPort`
   in `web/console/`. Reuse the same origin/port across the session so the browser's
   `localStorage` connection (and the Runtime's CORS allowance) persist.
3. **Seed the connection.** Dev token from `POST /v1/dev/bootstrap.json` (24h JWT —
   re-mint when expired; a stale token surfaces as `401` / disconnected). Settings →
   Connected Runtimes, or seed `localStorage` directly for Playwright:
   `harbor.runtime.{base_url,token,tenant,user,session,scopes}` (session **blank** —
   per D-171 the token no longer pins a session).
4. **Open the mock(s) side-by-side:** the page's `docs/design/console/page-*.md` plus
   the mock images. The mock is the spec for both look and behaviour.

---

## 3. The wire-by-wire pass (the core)

For **every** piece of data the page renders, walk the full chain and prove each hop
with real data:

1. **Name the source.** Which Protocol method (`POST /v1/<area>/<verb>`) or event
   (`GET /v1/events` SSE) supplies this datum? If none exists, STOP — that is a
   Protocol-surface gap (a finding), not a UI problem to paper over.
2. **Probe the Runtime directly.** `curl` the method (with the dev token +
   `X-Harbor-Session` when session-scoped) or read the SSE frame. Capture the **real
   payload**. Confirm the datum actually exists in it.
3. **Pin the wire shape.** RPC request/response bodies use **snake_case json tags**.
   The `GET /v1/events` SSE projection is a flat `wireEvent` whose nested `payload`
   is marshalled WITHOUT json tags — its keys are the Go struct field names in
   **PascalCase** (`payload.TaskID`, `payload.Usage.TotalTokens`). Decoders that read
   the wrong casing silently drop every value. Unit-test each decoder against a
   captured real frame.
4. **Trace decode → state → render.** Confirm the decoder reads the correct shape,
   the state holds the real value, and the rendered DOM shows it — equal to what the
   `curl`/SSE probe returned.
5. **Hydration.** If the datum is conversation/session state, reload the page and
   confirm it re-renders from the Runtime (not an empty start). A reload-then-act
   race that discards hydrated state is a bug (it happened in 108a).

Record, per datum: source method/event · real payload snippet · rendered value ·
PASS/finding.

---

## 4. The functional pass

For every interactive element (button, selector, slider, composer, toggle, nav link):

1. Perform the action in the browser against the live Runtime.
2. Confirm the round-trip: the Protocol call fires (check the Runtime log / network),
   the Runtime acts, and the **resulting state change is observed** in the UI.
3. Cover the meaningful variants (e.g. send / queue / steer; switch / new; apply /
   reset; approve / reject). A control that renders but does nothing — or fakes a
   local-only effect — is a finding.

---

## 5. The four-state pass (`PageState`)

Force and observe each branch (CONVENTIONS.md §"PageState"):

- **Loading** — the skeleton matches the shape of the loaded view; it resolves (a
  skeleton that never resolves is a bug — happened in 108a).
- **Loaded** — the real data path.
- **Empty** — force it (a fresh session / no rows) — the empty copy is intentional,
  not a blank panel.
- **Error** — force it (a bad request / a down dependency) — a real error surface,
  never a silent blank or a fabricated success.

---

## 6. The shell / layout pass

- Viewport-locked: the document does NOT scroll; only the page's own regions scroll
  internally. No full-page scroll that moves the header/sidebar (happened in 108a).
- No white bleed: `html`/`body` carry the dark token background; zero default margin.
- Responsive at the supported widths; the right rail / panels collapse per the mock.
- Tokens only: no raw colour / spacing / type literals in `.svelte` (`stylelint`).

---

## 7. The browser-truth pass (Playwright)

- Navigate to the page; take an accessibility **snapshot** and a **screenshot** for
  the visual diff against the mock.
- **Console errors MUST be zero** (after a clean hard-reload with a valid token —
  distinguish live errors from stale-token/historical noise).
- Confirm hydration (§3.5) and each `PageState` branch (§5) in the actual browser.
- Keep the screenshots with the wave for the before/after record.

---

## 8. Per-datum / per-component ledger

Each page wave produces a short ledger (in the PR description or the phase plan) — one
row per mock component and per datum: `component/datum · source · verified-real?
(payload) · functional? · state-branches? · PASS/finding`. This is the artifact that
proves §0 was actually walked, not asserted.

---

## 9. The wave-end checkpoint audit (§17.5)

After the page is built, a **read-only fork** audits it component-by-component,
wire-by-wire, hunting for: wiring gaps (a datum shown but not actually sourced),
fabrication, silent degradation, weak/absent verification, fabricated state branches,
token violations, and hygiene/drift. It produces a categorised **FAIL / WARN / NIT**
punch list with `file:line` + a one-line fix directive. The coordinator lands the
fixes as one `chore(checkpoint): <page> audit fixes` commit. **A FAIL blocks the page;
this audit gates the next page wave.**

---

## 10. Authoring a page wave (workflow)

1. Read the page's `docs/design/console/page-*.md` + mock images + `CONVENTIONS.md`.
2. Copy the phase template; the plan's "Console consistency" section cites this
   procedure and lists the page's component inventory + the per-datum source map.
3. Build to the mock; run §3–§7 continuously against the live Runtime as you go.
4. Produce the §8 ledger; fix every finding in-wave (§1, §17.6).
5. Run the §9 checkpoint audit; land its punch list.
6. The page is done only when §0's bar is met with evidence.

---

## Appendix — techniques used to finish the Playground (108 / 108a), as reference

- **Probe methods directly:** `POST /v1/dev/bootstrap.json` (token), `/v1/tasks/list`,
  `/v1/tasks/get`, `/v1/control/<verb>`, `/v1/tools/list`, `/v1/artifacts/list`,
  `GET /v1/events?...&access_token=...&session=...` (SSE). A `runtime_error` like
  "no steering control type mapping" means you hit the wrong surface for that method.
- **SSE casing gotcha:** the flat `wireEvent` payload is PascalCase Go field names,
  not json tags — the first streaming cut read snake_case and dropped every chunk.
- **Tool lifecycle:** the Runtime emits `tool.invoked` / `tool.completed` /
  `tool.failed` — use them for live tool status; do not blanket-mark "succeeded."
- **Latency vs hang:** a 2-minute "unresponsive" turn was a tool timing out 4×30s,
  not a hang — read the Runtime log; the fix was a per-tool policy (26b), not the UI.
- **Embedded vs live:** swap the stale `harbor console` embed for `npm run dev` on the
  same port to see committed work; hard-reload to clear a cached bundle.
