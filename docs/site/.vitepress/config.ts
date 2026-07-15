// VitePress configuration for the Harbor docs site (Phase 103, D-208).
//
// Static markdown-first generator; the same mechanism the sibling
// Dockyard project uses for its published docs, so the two project
// sites age together. GitHub Pages deploys the built site from
// .github/workflows/docs.yml.
//
// The `base` URL is the GitHub Pages path; we read DOCS_BASE from the
// environment so the CI workflow can pin it to "/Harbor/" without
// hard-coding the repo name in the config.
import { defineConfig } from "vitepress";

const base = process.env.DOCS_BASE ?? "/";

// Canonical in-repo docs (RFC, glossary, decisions, skills, recipes)
// are mirrored into pages via `<!--@include: …-->` stubs — the repo
// stays the single source of truth; the site renders from it. Those
// canonical files legitimately link into the repo tree (Go packages,
// scripts, phase plans, examples) — paths that exist on GitHub but are
// not site pages. The dead-link gate stays ON for everything else:
// a renamed skill, a deleted recipe, or a moved reference page fails
// the build.
const repoTreeLink = (url: string): boolean => {
  // Cross-skill links ("../run-the-dev-loop/SKILL.md") resolve INSIDE the
  // mirrored /skills/ tree — they stay checked, so a renamed skill that a
  // sibling skill still references fails the build.
  if (/^\.\.\/[a-z0-9-]+\/SKILL(\.md)?(#.*)?$/.test(url)) return false;
  if (url.includes("../")) return true; // escapes the site root → repo tree
  if (/\.(go|sh|ya?ml|ts|cjs|mjs|css|svelte|json|tmpl|txt|proto|sql|svg|png)(#|$)/.test(url)) {
    return true; // file artifact, not a page
  }
  // Repo directories the site deliberately does not mirror page-for-page.
  return /^(\.\/)?(docs\/(plans|research|rfc|design|notes|perf)|internal|cmd|scripts|examples|harbortest|sdk|test|web)\//.test(
    url,
  );
};

export default defineConfig({
  base,
  lang: "en-US",
  title: "Harbor",
  description:
    "Harbor — a Go-native runtime for durable, steerable, event-driven AI agents.",
  cleanUrls: true,
  // Default to the dark, navy theme so the docs match the landing's mood; the
  // light/dark toggle stays available for readers who prefer it.
  appearance: "dark",
  // Branding: the favicon + a mask icon, plus Open Graph / Twitter cards so a
  // shared link renders the lighthouse mark and the one-line pitch. Paths are
  // base-prefixed so they resolve under the GitHub Pages "/Harbor/" path.
  head: [
    ["link", { rel: "icon", type: "image/svg+xml", href: `${base}favicon.svg` }],
    ["link", { rel: "mask-icon", href: `${base}favicon.svg`, color: "#2bb6cc" }],
    ["meta", { name: "theme-color", content: "#0d1117" }],
    ["meta", { property: "og:type", content: "website" }],
    ["meta", { property: "og:title", content: "Harbor — durable, steerable AI agents in Go" }],
    [
      "meta",
      {
        property: "og:description",
        content:
          "A Go-native runtime for durable, steerable, event-driven AI agents. One static binary; multi-isolation from day one; a swappable planner on a versioned Protocol.",
      },
    ],
    ["meta", { property: "og:image", content: `${base}harbor_logo.svg` }],
    ["meta", { name: "twitter:card", content: "summary" }],
    ["meta", { name: "twitter:title", content: "Harbor — durable, steerable AI agents in Go" }],
    ["meta", { name: "twitter:image", content: `${base}harbor_logo.svg` }],
  ],
  // The build fails on a dead internal link (modulo the repo-tree
  // carve-out above). Surfacing a 404 before publish is the point of
  // having a docs build step in CI.
  ignoreDeadLinks: [repoTreeLink],
  markdown: {
    // The canonical docs are plain markdown — none uses raw inline HTML —
    // but several write angle-bracket placeholders ("<slug>", "<area>")
    // in prose. With HTML passthrough on, Vue's SFC compiler reads those
    // as unclosed elements and fails the build; rendering them escaped
    // is both safe and correct.
    html: false,
    config(md) {
      // The canonical docs quote Go text/template syntax ("{{ .Args.city }}")
      // in inline code spans. Vue's SFC compiler would read those as
      // interpolations and fail the build; v-pre on every inline code
      // span keeps the quoted syntax literal. Fenced blocks are already
      // v-pre'd by VitePress.
      const defaultRender =
        md.renderer.rules.code_inline ??
        ((tokens, idx, options, _env, self) =>
          `<code${self.renderAttrs(tokens[idx])}>${md.utils.escapeHtml(tokens[idx].content)}</code>`);
      md.renderer.rules.code_inline = (tokens, idx, options, env, self) => {
        tokens[idx].attrSet("v-pre", "");
        return defaultRender(tokens, idx, options, env, self);
      };
    },
  },
  themeConfig: {
    siteTitle: "Harbor",
    logo: "/harbor_logo.svg",
    nav: [
      { text: "Get started", link: "/get-started" },
      { text: "Concepts", link: "/concepts/", activeMatch: "/concepts/" },
      {
        text: "Guides",
        items: [
          { text: "How do I…?", link: "/guides/" },
          { text: "Operator skills", link: "/skills/" },
          { text: "Recipes", link: "/recipes/" },
        ],
      },
      { text: "Protocol", link: "/protocol/", activeMatch: "/protocol/" },
      {
        text: "Reference",
        items: [
          { text: "Overview", link: "/reference/" },
          { text: "RFC-001", link: "/reference/rfc" },
          { text: "Configuration (harbor.yaml)", link: "/reference/config" },
          { text: "Glossary", link: "/reference/glossary" },
          { text: "Decisions log", link: "/reference/decisions" },
          { text: "Master phase plan", link: "/reference/master-plan" },
          { text: "Productionization playbook", link: "/reference/productionization-playbook" },
          { text: "Changelog", link: "/reference/changelog" },
        ],
      },
    ],
    sidebar: {
      // The conceptual track — the mental-model layer between the quickstart
      // and the wire-level reference. Distils the RFC; links down to it.
      "/concepts/": [
        {
          text: "Concepts",
          items: [{ text: "Overview", link: "/concepts/" }],
        },
        {
          text: "Architecture",
          items: [
            { text: "The four-layer architecture", link: "/concepts/architecture" },
            { text: "Runtime and Planner", link: "/concepts/runtime-and-planner" },
          ],
        },
        {
          text: "Isolation & control",
          items: [
            { text: "Identity and multi-isolation", link: "/concepts/identity-and-isolation" },
            { text: "Pause, resume, and steering", link: "/concepts/pause-resume-and-steering" },
          ],
        },
        {
          text: "Capabilities",
          items: [
            { text: "Tools and transports", link: "/concepts/tools" },
            { text: "Memory and skills", link: "/concepts/memory-and-skills" },
            { text: "Sessions, tasks, and events", link: "/concepts/sessions-tasks-and-events" },
            {
              text: "Artifacts and the context-window safety net",
              link: "/concepts/artifacts-and-context-safety",
            },
          ],
        },
        {
          text: "Operations",
          items: [
            { text: "The persistence triad", link: "/concepts/persistence" },
            { text: "Governance and security", link: "/concepts/governance-and-security" },
            { text: "Observability and the Console", link: "/concepts/observability" },
          ],
        },
      ],
      // The task-oriented Guides hub — its own "How do I…?" index plus
      // jump-offs into the two existing practical tracks so a reader who
      // entered via Guides can reach them without backtracking to the nav.
      "/guides/": [
        {
          text: "Guides",
          items: [{ text: "How do I…?", link: "/guides/" }],
        },
        {
          text: "Operator skills",
          items: [{ text: "Browse all skills", link: "/skills/" }],
        },
        {
          text: "Recipes",
          items: [{ text: "Browse all recipes", link: "/recipes/" }],
        },
      ],
      "/skills/": [
        {
          text: "Operator skills",
          items: [{ text: "Index", link: "/skills/" }],
        },
        {
          text: "Start a project",
          items: [
            { text: "scaffold-a-harbor-agent", link: "/skills/scaffold-a-harbor-agent/SKILL" },
            { text: "define-the-agent-yaml", link: "/skills/define-the-agent-yaml/SKILL" },
          ],
        },
        {
          text: "Build the agent",
          items: [
            { text: "add-an-in-process-tool", link: "/skills/add-an-in-process-tool/SKILL" },
            { text: "wire-the-llm-provider", link: "/skills/wire-the-llm-provider/SKILL" },
            { text: "configure-memory-and-skills", link: "/skills/configure-memory-and-skills/SKILL" },
          ],
        },
        {
          text: "Drive it interactively",
          items: [
            { text: "run-the-dev-loop", link: "/skills/run-the-dev-loop/SKILL" },
            { text: "drive-the-playground", link: "/skills/drive-the-playground/SKILL" },
            { text: "drive-the-harbor-tui", link: "/skills/drive-the-harbor-tui/SKILL" },
          ],
        },
        {
          text: "Observe + debug",
          items: [
            { text: "observe-with-the-console", link: "/skills/observe-with-the-console/SKILL" },
          ],
        },
        {
          text: "Ship",
          items: [
            { text: "validate-and-package", link: "/skills/validate-and-package/SKILL" },
            { text: "configure-production-identity", link: "/skills/configure-production-identity/SKILL" },
          ],
        },
        {
          text: "Build a custom frontend",
          items: [
            { text: "use-the-harbor-protocol", link: "/skills/use-the-harbor-protocol/SKILL" },
          ],
        },
      ],
      "/recipes/": [
        {
          text: "Recipes",
          items: [
            { text: "Index", link: "/recipes/" },
            { text: "Scaffold an agent", link: "/recipes/scaffold-an-agent" },
            { text: "Define an in-process tool", link: "/recipes/define-a-tool" },
            { text: "Configure a planner", link: "/recipes/configure-a-planner" },
            { text: "Run the dev loop", link: "/recipes/run-harbor-dev" },
            { text: "Test an agent", link: "/recipes/test-an-agent" },
            { text: "Embed Harbor headless", link: "/recipes/embed-harbor-headless" },
            { text: "Steer and resume a run", link: "/recipes/steer-and-resume-a-run" },
            { text: "Use memory and skills from Go", link: "/recipes/use-memory-and-skills-from-go" },
            { text: "Observe an embedded runtime", link: "/recipes/observe-an-embedded-runtime" },
            { text: "Control attachment disposition", link: "/recipes/control-attachment-disposition" },
            { text: "Provider-native attachments", link: "/recipes/provider-native-attachments" },
            { text: "Embed and retrieve", link: "/recipes/embed-and-retrieve" },
          ],
        },
      ],
      "/protocol/": [
        {
          text: "Protocol",
          items: [
            { text: "Overview", link: "/protocol/" },
            { text: "Quickstart — Speak Protocol in 15 minutes", link: "/protocol/quickstart" },
          ],
        },
        {
          // The four reference pages are GENERATED by
          // cmd/harbor-gen-protocol-docs and drift-gated by
          // `make protocol-docs-gen-check` (Phase 113a, D-209).
          text: "Reference (generated)",
          items: [
            { text: "Methods", link: "/protocol/methods" },
            { text: "Events", link: "/protocol/events" },
            { text: "Errors", link: "/protocol/errors" },
            { text: "Types", link: "/protocol/types" },
          ],
        },
        {
          text: "Choreographies",
          items: [
            { text: "1 — Auth & identity", link: "/protocol/auth-and-identity" },
            { text: "2 — Streaming semantics", link: "/protocol/streaming-semantics" },
            { text: "3 — Task control", link: "/protocol/task-control" },
            { text: "4 — The pause model", link: "/protocol/pause-model" },
            {
              text: "5 — Versioning & compatibility",
              link: "/protocol/versioning-and-compatibility",
            },
          ],
        },
        {
          text: "Adopt",
          items: [
            { text: "Build a client", link: "/protocol/build-a-client" },
            {
              text: "Production identity setup",
              link: "/protocol/production-identity-setup",
            },
            {
              text: "Conformance certification",
              link: "/protocol/conformance-certification",
            },
          ],
        },
      ],
      "/reference/": [
        {
          text: "Reference",
          items: [
            { text: "Overview", link: "/reference/" },
            { text: "RFC-001 — Harbor", link: "/reference/rfc" },
            { text: "Configuration (harbor.yaml)", link: "/reference/config" },
            { text: "Glossary", link: "/reference/glossary" },
            { text: "Decisions log", link: "/reference/decisions" },
            { text: "Master phase plan", link: "/reference/master-plan" },
            { text: "Productionization playbook", link: "/reference/productionization-playbook" },
            { text: "Changelog", link: "/reference/changelog" },
          ],
        },
      ],
    },
    socialLinks: [
      { icon: "github", link: "https://github.com/hurtener/Harbor" },
    ],
    footer: {
      message: "Apache-2.0 licensed — see LICENSE.",
      copyright: "© Harbor contributors. Generated by VitePress (Phase 103).",
    },
    outline: { level: [2, 3] },
    search: { provider: "local" },
  },
});
