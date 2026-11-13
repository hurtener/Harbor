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

/*
 * Release and count facts are derived at build time (.vitepress/changelog.mjs
 * and .vitepress/facts.mjs) and injected by config.ts, so the page cannot
 * quote a stale version, method count, or skill/recipe count.
 */
const release = __HARBOR_RELEASE__;
const facts = __HARBOR_FACTS__;

export interface CTA {
  label: string;
  link: string;
  kind: "primary" | "secondary";
}

export const GITHUB = "https://github.com/hurtener/Harbor";
export const LICENSE_URL = `${GITHUB}/blob/main/LICENSE`;

export const hero = {
  eyebrow: release.eyebrow,
  name: "Harbor",
  tagline: "Durable, steerable, event-driven AI agents in Go.",
  lead:
    "Most agent loops live in a goroutine and die with it. Harbor persists run state, so a paused run survives a restart, and isolates every (tenant, user, session) from the first line. Around that core: standard SKILL.md skills, on-demand MCP tools and MCP Apps, delegated work with per-run limits, and usage accounting with provenance — in one CGo-free static binary with a swappable Planner.",
  install: "go install github.com/hurtener/Harbor/cmd/harbor@latest",
  ctas: [
    { label: "Get started", link: "/get-started", kind: "primary" },
    { label: "Read the design RFC", link: "/reference/rfc", kind: "secondary" },
  ] as CTA[],
  terminal: {
    file: "your-laptop",
    lines: [
      "$ go install github.com/hurtener/Harbor/cmd/harbor@latest",
      "$ harbor init        # tiered harbor.yaml + companion docs",
      "$ harbor validate ./harbor.yaml",
      "  ./harbor.yaml: ok",
      "$ harbor dev --config ./harbor.yaml",
      "  HARBOR_DEV_TOKEN=eyJhbGciOiJFUzI1NiIs…",
      "  HARBOR_DEV_BOUND=127.0.0.1:18080",
    ],
  },
};

export const announcement = {
  text: release.announcement,
  link: "/reference/changelog",
};

export const navLinks = [
  { text: "Get started", link: "/get-started" },
  { text: "Concepts", link: "/concepts/" },
  { text: "Guides", link: "/guides/" },
  { text: "Protocol", link: "/protocol/" },
  { text: "Reference", link: "/reference/" },
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
harbor dev --config ./harbor.yaml # local runtime + Protocol server on :18080`,
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
      detail: `One CGo-free static binary with ${facts.cliCommands} subcommands that drives all of the above.`,
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
  sub: `The seam most agent frameworks never draw: a canonical, versioned contract between the Runtime and anything observing or controlling it — ${facts.protocolMethods} canonical methods, over SSE + REST.`,
  families: [
    { name: "Task control", detail: "start · cancel · pause · resume · redirect · inject_context · approve · reject · prioritize" },
    { name: "events.subscribe", detail: "identity-scoped streaming event subscription" },
    { name: "state.history", detail: "tail-first windowed event replay" },
    { name: "topology.snapshot", detail: "the live run/agent topology" },
    { name: "search.* · tasks.* · tools.*", detail: "semantic search, task lifecycle, tool catalog" },
    { name: "agent_config.* · governance.*", detail: "durable config tiers, skills and packs, cost ceilings, rate limits" },
    { name: "mcp.* · observability.query", detail: "MCP servers and Apps, usage rollups by minute, model and session" },
  ],
  adoption:
    "Harbor ships a Protocol adoption track for client authors: a quickstart executed in CI, a generated method/event/error/type reference, choreography guides, and a ~150-line stdlib-only example event-viewer client. A connect-time wire_surface_digest (a sha256: on runtime.info) lets a client detect when the surface it was built against has drifted.",
  cta: { label: "Build a Protocol client", link: "/protocol/" },
};

/* ── Capabilities surface ───────────────────────────────────────────────── */
// Every tile names a shipped capability and links to the page that documents
// it; verify against the code and CHANGELOG before adding one.
export const capabilities = {
  eyebrow: "The runtime surface",
  headline: "Skills. Tools. Delegation. Context. Usage.",
  sub: "What a Harbor runtime does today beyond the core loop — each piece configured in harbor.yaml or the agent config, never a fork.",
  tiles: [
    {
      kicker: "Skills",
      title: "Standard SKILL.md packages",
      body: "Import Agent Skills files with bundled scripts, data and templates — stored as data, never executed. Reviewed two-phase import, drafts, and ZIP export for personal skills.",
      link: "/concepts/memory-and-skills#the-runtime-skills-subsystem",
    },
    {
      kicker: "Skills",
      title: "Agent packs and publications",
      body: "Boot and revision packs give each agent its shared skills. Admins inspect and copy packs between agents, support files included, and publish immutable organization skills.",
      link: "/concepts/memory-and-skills#agent-pack-inspect-and-copy-stay-runtime-local",
    },
    {
      kicker: "Tools",
      title: "On-demand MCP tools",
      body: "With deferred loading, a large MCP catalog leaves the per-call schema: the model sees a compact name index and loads a tool through tool_search / tool_get when it needs it.",
      link: "/reference/config#tools-mcp-default-loading-mode",
    },
    {
      kicker: "Tools",
      title: "MCP Apps",
      body: "A tool that declares a ui:// App renders in the Console's sandboxed iframe. App calls stay confined to their own server and pass every identity, approval and OAuth gate.",
      link: "/reference/config#tools-mcp-app-host",
    },
    {
      kicker: "Runs",
      title: "Delegated background work",
      body: "One planner step can spawn several background tasks at once. Virtual-agent profiles narrow a child's model, skills and tools; a run can cancel only its own descendants.",
      link: "/reference/config#virtual-agents",
    },
    {
      kicker: "Runs",
      title: "Per-run limits",
      body: "Cap a run's active time or total tokens per runtime, per agent, or per start. At the limit the run says what it did and what remains, and finishes as limit_reached — not failed.",
      link: "/reference/config#runtime-run-limits-max-duration",
    },
    {
      kicker: "Context",
      title: "Cumulative session memory",
      body: "A rolling checkpoint plus a bounded recent tail, compacted to a token budget by its own summarizer route. An oversized exchange becomes a preview with a retained reference.",
      link: "/concepts/memory-and-skills#cumulative-session-memory",
    },
    {
      kicker: "Context",
      title: "Session artifact tools",
      body: "artifact_list, artifact_search and artifact_note let the model page, grep and annotate earlier large results instead of running the tool again.",
      link: "/reference/config#tools-built-in",
    },
    {
      kicker: "Identity",
      title: "Signed agent reach",
      body: "Bearer claims name the agents a caller may address. agent_read_reach adds read-only access to those agents' conversation turns without letting the caller start or steer them.",
      link: "/reference/glossary",
    },
    {
      kicker: "Usage",
      title: "Usage with provenance",
      body: "Rollups of cost, tokens, latency and task outcomes by minute, model and session, served by observability.query. Each turn's cost says whether it is exact or estimated.",
      link: "/reference/config#observability",
    },
    {
      kicker: "Persistence",
      title: "One database, verify mode",
      body: "Point every store at one Postgres database. Migrations apply once over a direct connection; replicas then start with migration_mode: verify through a transaction pooler.",
      link: "/concepts/persistence#migrations-are-forward-only",
    },
    {
      kicker: "LLM",
      title: "Any provider, governed",
      body: "OpenAI, Anthropic, Google, OpenRouter, or any OpenAI-compatible endpoint, behind per-tenant cost ceilings, rate limits and a mandatory audit redactor.",
      link: "/concepts/governance-and-security",
    },
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
    { cmd: "harbor dev", body: "Local Runtime, Protocol server, and config reload on your laptop — edit harbor.yaml and the dev stack rebuilds in place. The Console is served separately by harbor console." },
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
    { value: String(facts.cliCommands), label: "CLI subcommands" },
    { value: String(facts.protocolMethods), label: "canonical Protocol methods" },
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
// Fed by the same CHANGELOG parse as the announcement bar: the newest
// release's lead entries, then one-line summaries of the releases before it.
const [latest, ...earlier] = release.recent;
export const whatsNew = {
  eyebrow: "Recently shipped",
  headline: `v${latest.version}`,
  date: latest.date,
  bullets: latest.items.slice(0, 4),
  earlier: earlier
    .filter((r) => r.summary)
    .map((r) => ({ version: `v${r.version}`, date: r.date, summary: r.summary })),
  cta: { label: "Read the full changelog", link: "/reference/changelog" },
};

/* ── Final CTA ──────────────────────────────────────────────────────────── */
export const finalCta = {
  headline: "Boot the whole runtime locally with one command",
  sub: `Then read the design RFC to see how it's built, or browse the ${facts.operatorSkills} operator skills and ${facts.recipes} recipes that walk you from scaffold to production.`,
  install: "go install github.com/hurtener/Harbor/cmd/harbor@latest",
  run: "harbor dev",
  ctas: [
    { label: "Get started", link: "/get-started", kind: "primary" },
    { label: "Explore the concepts", link: "/concepts/", kind: "secondary" },
  ] as CTA[],
};

/* ── Footer ─────────────────────────────────────────────────────────────── */
export const footerNav = [
  {
    group: "Learn",
    links: [
      { text: "Get started", link: "/get-started" },
      { text: "Concepts", link: "/concepts/" },
      { text: "How do I…?", link: "/guides/" },
      { text: "Operator skills", link: "/skills/" },
      { text: "Recipes", link: "/recipes/" },
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
