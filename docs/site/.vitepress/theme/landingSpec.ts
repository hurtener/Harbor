/*
 * Harbor landing-page content — the single source for the bespoke home layout.
 *
 * Copy here is fact-checked: every claim was verified against the repo by an
 * adversarial review pass (no download counts, no logo wall, no testimonials,
 * no unmeasured performance numbers). Edit copy here, not in the components.
 *
 * Internal links are site-relative ("/skills/…"); components apply the
 * VitePress base prefix via withBase() so they resolve under "/Harbor/".
 */

export interface CTA {
  label: string;
  link: string;
  kind: "primary" | "secondary";
}

export const GITHUB = "https://github.com/hurtener/Harbor";
export const LICENSE_URL = `${GITHUB}/blob/main/LICENSE`;

export const hero = {
  eyebrow: "Go-native agent runtime · v1.6",
  name: "Harbor",
  tagline: "Durable, steerable, event-driven AI agents in Go.",
  lead:
    "Most agent loops live in a goroutine and die with it. Harbor persists run state, so a paused run survives a process restart. Multi-isolation is mandatory from the first line — one user can hold many concurrent, isolated sessions. The Planner is swappable; the Runtime owns the mechanism. It all ships as one CGo-free static binary.",
  install: "go install github.com/hurtener/Harbor/cmd/harbor@latest",
  ctas: [
    { label: "Get started", link: "/skills/scaffold-a-harbor-agent/SKILL", kind: "primary" },
    { label: "Read the design RFC", link: "/reference/rfc", kind: "secondary" },
  ] as CTA[],
  terminal: {
    file: "your-laptop",
    lines: [
      "$ go install github.com/hurtener/Harbor/cmd/harbor@latest",
      "$ harbor init        # tiered harbor.yaml + companion docs",
      "$ harbor validate ./harbor.yaml",
      "  ✓ config valid",
      "$ harbor dev         # runtime + Protocol server",
      "  → listening on http://127.0.0.1:18080",
      "  → healthz ok · ReAct planner · 3 drivers registered",
    ],
  },
};

export const announcement = {
  text: "v1.6.0 just shipped — session-state history + user-scope agent configuration",
  link: "/reference/changelog",
};

export const navLinks = [
  { text: "Docs", link: "/skills/" },
  { text: "Recipes", link: "/recipes/" },
  { text: "Protocol", link: "/protocol/" },
  { text: "RFC", link: "/reference/rfc" },
  { text: "Changelog", link: "/reference/changelog" },
];

/* ── Code showcase (Scaffold & run / Embed in Go) ───────────────────────── */
export const codeShowcase = {
  eyebrow: "Code is the proof",
  headline: "Five minutes to a working agent",
  sub: "No broker to stand up, no service mesh, no Python toolchain. Install the binary, scaffold a project, point it at one LLM provider — or skip the CLI and embed the runtime in your own Go program.",
  tabs: [
    {
      key: "cli",
      label: "Scaffold & run",
      file: "terminal",
      lang: "bash",
      code: `go install github.com/hurtener/Harbor/cmd/harbor@latest

mkdir my-agent && cd my-agent
harbor init                       # tiered harbor.yaml + AGENTS.md / CLAUDE.md / README.md
# edit harbor.yaml — uncomment one LLM provider block + set its API key env var
harbor validate ./harbor.yaml     # fail-loud config check, file:line precision
harbor scaffold --name my-agent   # the Go project + a worked agent + a test
harbor dev                        # local runtime + Protocol server on :18080`,
    },
    {
      key: "embed",
      label: "Embed in Go",
      file: "main.go",
      lang: "go",
      code: `import (
    _ "github.com/hurtener/Harbor/sdk/drivers/prod" // production driver registrations

    "github.com/hurtener/Harbor/sdk/assemble"
    "github.com/hurtener/Harbor/sdk/config"
)

cfg := config.Defaults()
cfg.LLM.Provider = "openrouter"
cfg.LLM.Model = "anthropic/claude-sonnet-4"
cfg.LLM.APIKey = "env.OPENROUTER_API_KEY" // env-var indirection — never inline a key

if err := cfg.ValidateCore(); err != nil {
    return fmt.Errorf("config: %w", err) // fail loud before anything boots
}

stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
if err != nil {
    return fmt.Errorf("assemble: %w", err)
}
defer stack.Close(ctx)`,
    },
  ],
  footnote:
    "The sdk/ facade is the supported public surface — importable from any external Go module behind a standing external-module compile smoke that runs in CI. For tests, harbortest/ is a public kit: RunOnce, AssertSequence, AssertNoLeaks, SimulateFailure, and EventLog.RecordedEvents.",
};

/* ── Architecture ───────────────────────────────────────────────────────── */
export const architecture = {
  eyebrow: "Four-layer architecture",
  headline: "Runtime, Protocol, Console, CLI — separated on purpose",
  sub: "Harbor is not one monolith with a UI bolted on. Because observability rides the same wire contract a third party would use, the Runtime never imports the Console and the Console never reads internal Runtime objects. The same surface powers harbor dev, a remote-attached dashboard, and a third-party IDE or TUI client.",
  layers: [
    {
      name: "Runtime",
      role: "Orchestration kernel",
      detail: "Tasks, planner runtime, tools, memory, sessions, events, skills, artifacts, and the unified pause/resume primitive.",
    },
    {
      name: "Protocol",
      role: "Versioned wire contract",
      detail: "The canonical event/state contract, pinned at 0.1.0 and versioned independently of the product release.",
    },
    {
      name: "Console",
      role: "Just a Protocol client",
      detail: "A SvelteKit UI baked into the binary, served only by harbor console — it reads canonical events, never internals.",
    },
    {
      name: "CLI",
      role: "The harbor binary",
      detail: "One CGo-free static binary with 11 subcommands that drives all of the above.",
    },
  ],
  clients: ["Console", "Remote dashboard", "IDE / TUI client"],
};

/* ── Three non-negotiables ──────────────────────────────────────────────── */
export const nonNegotiables = {
  eyebrow: "By construction, not by config",
  headline: "Three non-negotiable properties",
  sub: "These are not features you enable. They are invariants the codebase is built to enforce.",
  cards: [
    {
      title: "Multi-isolation from V1",
      body: "Identity is the mandatory triple (tenant, user, session). Every identity-scoped storage method filters on it, and one conformance harness proves all six identity-scoped subsystems hold the boundary under concurrent load. The runtime fails closed on a missing component — no opt-out knob. One user can hold many concurrent sessions with zero cross-talk.",
    },
    {
      title: "The Console is a Protocol client",
      body: "All data flows through canonical events and state snapshots. There is no debug shortcut that leaks raw internals — the same contract a third-party client would consume is the only contract the Console gets.",
    },
    {
      title: "The Planner is swappable",
      body: "The Runtime owns mechanism — tasks, tools, memory, events, artifacts, pause/resume — and the Planner owns reasoning policy behind one interface. Two concretes ship on identical primitives today: the reference ReAct planner (the default) and a Deterministic planner that proves the seam is real.",
    },
  ],
};

/* ── Durable + steerable ────────────────────────────────────────────────── */
export const durable = {
  eyebrow: "Durable + steerable runs",
  headline: "Pause, redirect, resume — without losing context",
  sub: "Run state is persisted, not held in a goroutine. With a durable checkpoint store configured, a paused run survives a process restart via trajectory checkpoints, with a max-park sweeper for parked work.",
  tiles: [
    {
      title: "One pause primitive",
      body: "HITL approval, tool-side OAuth, A2A AUTH_REQUIRED, and operator pause all back onto the same unified pause/resume mechanism — not three reinventions.",
    },
    {
      title: "Explicit steering surface",
      body: "CANCEL · REDIRECT · INJECT_CONTEXT · PAUSE · RESUME · APPROVE · REJECT · PRIORITIZE · USER_MESSAGE — a named control instruction for each.",
    },
    {
      title: "Fails loudly by design",
      body: "ErrUnserializable when a pause can't be serialized, ErrToolContextLost on a handle miss — never a silent nil, nil. No try/catch that returns None.",
    },
    {
      title: "Guarded at the LLM edge",
      body: "ErrContextLeak and ErrContextWindowExceeded stop oversized or leaked context before it reaches the provider, not after.",
    },
  ],
};

/* ── Protocol ───────────────────────────────────────────────────────────── */
export const protocol = {
  eyebrow: "The Harbor Protocol",
  headline: "A versioned wire contract, not an internal function call",
  sub: "The seam most agent frameworks never draw: a canonical, versioned contract between the Runtime and anything observing or controlling it — 109 canonical methods at v1.6, over SSE + REST.",
  families: [
    { name: "Task control", detail: "start · cancel · pause · resume · redirect · inject_context · approve · reject · prioritize" },
    { name: "events.subscribe", detail: "identity-scoped streaming event subscription" },
    { name: "state.history", detail: "tail-first windowed event replay" },
    { name: "topology.snapshot", detail: "the live run/agent topology" },
    { name: "search.* · tasks.* · tools.*", detail: "semantic search, task lifecycle, tool catalog" },
    { name: "agent_config.* · governance.*", detail: "durable config tiers, cost ceilings, rate limits" },
  ],
  adoption:
    "Harbor ships a Protocol adoption track for client authors: a quickstart executed in CI, a generated method/event/error/type reference, choreography guides, and a ~150-line stdlib-only example event-viewer client. A connect-time wire_surface_digest (a sha256: on runtime.info) lets a client detect when the surface it was built against has drifted.",
  cta: { label: "Build a Protocol client", link: "/protocol/" },
};

/* ── Capabilities surface ───────────────────────────────────────────────── */
export const capabilities = {
  eyebrow: "The runtime surface",
  headline: "Tasks. Tools. Memory. Sessions. Skills. Artifacts.",
  sub: "Harbor reads like an API surface, not a grab-bag.",
  tiles: [
    { title: "Tools", body: "Transport-agnostic across in-process, HTTP, MCP, and A2A — all registering into one catalog." },
    { title: "Memory", body: "Session-scoped by default, with an opt-in Embedder seam for semantic memory and skill retrieval." },
    { title: "Tasks & events", body: "A durable TaskService plus a durable EventBus that rehydrates its sequence counter across a restart." },
    { title: "Skills", body: "A DB-backed, token-savvy catalog with a Skills.md importer (harbor skill import / rm)." },
    { title: "Governance", body: "Per-tenant cost ceilings, rate limits, and max-tokens, with a mandatory audit redactor on all telemetry." },
    { title: "LLM", body: "Provider-agnostic client for OpenAI, Anthropic, Google, OpenRouter, and any OpenAI-compatible endpoint (NIM, vLLM, ollama, lm-studio)." },
  ],
};

/* ── Persistence + deploy ───────────────────────────────────────────────── */
export const persistence = {
  eyebrow: "Persistence triad · deploy your way",
  headline: "in-memory / SQLite / Postgres — at conformance parity",
  sub: "Three storage drivers ship for StateStore and MemoryStore — all held to a single conformance suite. No Postgres-only or SQLite-only features. ArtifactStore adds fs and s3 drivers on top.",
  matrix: {
    columns: ["in-memory", "SQLite", "Postgres"],
    rows: [
      { feature: "StateStore", cells: [true, true, true] },
      { feature: "MemoryStore", cells: [true, true, true] },
      { feature: "Single conformance suite", cells: [true, true, true] },
      { feature: "CGo-free", cells: [true, true, true] },
      { feature: "Best for", cells: ["dev / embed", "single-node", "multi-node"] },
    ],
  },
  deploy: [
    { cmd: "harbor dev", body: "Local Runtime, Protocol server, and hot reload on your laptop. The Console is served separately by harbor console." },
    { cmd: "harbor serve", body: "The headless production sibling: verifies JWTs against a JWKS source (asymmetric algorithms only), mints no dev token, embeds no Console." },
  ],
  note: "Same binary, no CGo, one file to ship.",
};

/* ── Planner self-select ────────────────────────────────────────────────── */
export const planner = {
  eyebrow: "Choose your planner",
  headline: "ReAct today, your policy tomorrow — same runtime",
  sub: "The swappable Planner is a real seam, not a slide. Two concretes ship on identical primitives, and the interface is the contract for everything that comes after.",
  cards: [
    {
      title: "ReAct",
      lead: "For agents that should think, call a tool, observe, and iterate.",
      body: "The reference reasoning loop, and the default planner out of the box — the only self-registering, harbor.yaml-selectable driver.",
    },
    {
      title: "Deterministic",
      lead: "For predictable, testable control flow.",
      body: "A no-LLM, decision-tree planner you compose from typed steps via the SDK — and the on-disk proof that the Planner interface isn't biased toward an LLM-driven concrete.",
    },
  ],
  roadmap:
    "Both implement the same Planner.Next interface on the same Runtime, RunContext, and Decision sum. Additional concretes (Plan-Execute, Graph, Workflow, Supervisor, MultiAgent, HumanApproval) are on the post-V1 roadmap — built against the same interface these two already prove.",
};

/* ── Rigor / stat band ──────────────────────────────────────────────────── */
export const rigor = {
  eyebrow: "Engineering rigor you can audit",
  headline: "The trust signal a solo OSS project can actually stand behind",
  sub: "No download counts, no logo wall, no testimonials. Just facts you can verify in the repo.",
  stats: [
    { value: "1", label: "static CGo-free binary" },
    { value: "11", label: "CLI subcommands" },
    { value: "109", label: "canonical Protocol methods" },
    { value: "3", label: "conformance-equal stores" },
    { value: "2", label: "planners on one interface" },
    { value: "0.1.0", label: "Protocol, versioned apart" },
  ],
  discipline: [
    "Doc snippets executed in CI",
    "Per-phase live smoke gate",
    "Drift-audited Protocol reference",
    "External-module SDK compile gate",
    "Published master phase plan",
    "Apache-2.0 · SHA-256 release checksums",
  ],
};

/* ── What's new ─────────────────────────────────────────────────────────── */
export const whatsNew = {
  eyebrow: "Recently shipped",
  headline: "v1.6.0 — session hydration + user-scope agent config",
  date: "2026-06-25",
  bullets: [
    {
      title: "Session-state history",
      body: "POST /v1/state/history serves identity-scoped, tail-first windowed event replay, with heavy payloads surfaced by artifact reference. Cross-tenant reads 404 with no existence leak; unidentified reads 401.",
    },
    {
      title: "User-scope durable agent configuration",
      body: "A per-user config tier between the admin config and the session overlay — set / list / diff / rollback, layered prompt precedence, and a grow-only tool-exposure union (narrow the palette, never re-widen).",
    },
    {
      title: "Wire-surface drift detection",
      body: "runtime.info now returns a sha256: wire_surface_digest, so a client built against a different Protocol surface raises a loud drift signal at connect time.",
    },
    {
      title: "Durable event-bus rehydration",
      body: "The durable EventBus restores its monotonic sequence counter from the persisted log on restart, so a session's event sequence stays gap-free across a restart boundary.",
    },
  ],
  cta: { label: "Read the full changelog", link: "/reference/changelog" },
};

/* ── Final CTA ──────────────────────────────────────────────────────────── */
export const finalCta = {
  headline: "Boot the whole runtime locally with one command",
  sub: "Then read the design RFC to see how it's built, or browse the 10 operator skills and 12 recipes that walk you from scaffold to production.",
  install: "go install github.com/hurtener/Harbor/cmd/harbor@latest",
  run: "harbor dev",
  ctas: [
    { label: "Get started", link: "/skills/scaffold-a-harbor-agent/SKILL", kind: "primary" },
    { label: "Browse recipes", link: "/recipes/", kind: "secondary" },
  ] as CTA[],
};

/* ── Footer ─────────────────────────────────────────────────────────────── */
export const footerNav = [
  {
    group: "Product",
    links: [
      { text: "Scaffold an agent", link: "/skills/scaffold-a-harbor-agent/SKILL" },
      { text: "Run the dev loop", link: "/skills/run-the-dev-loop/SKILL" },
      { text: "Operator skills", link: "/skills/" },
      { text: "Recipes", link: "/recipes/" },
      { text: "Changelog", link: "/reference/changelog" },
    ],
  },
  {
    group: "Protocol",
    links: [
      { text: "Overview", link: "/protocol/" },
      { text: "Quickstart", link: "/protocol/quickstart" },
      { text: "Methods", link: "/protocol/methods" },
      { text: "Events", link: "/protocol/events" },
      { text: "Build a client", link: "/protocol/build-a-client" },
    ],
  },
  {
    group: "Reference",
    links: [
      { text: "RFC-001", link: "/reference/rfc" },
      { text: "Configuration", link: "/reference/config" },
      { text: "Glossary", link: "/reference/glossary" },
      { text: "Decisions log", link: "/reference/decisions" },
      { text: "Master phase plan", link: "/reference/master-plan" },
    ],
  },
  {
    group: "Community",
    links: [
      { text: "View source on GitHub", link: GITHUB, external: true },
      { text: "License (Apache-2.0)", link: LICENSE_URL, external: true },
    ],
  },
];
