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
    nav: [
      { text: "Operator skills", link: "/skills/" },
      { text: "Recipes", link: "/recipes/" },
      {
        text: "Reference",
        items: [
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
          ],
        },
      ],
      "/reference/": [
        {
          text: "Reference",
          items: [
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
