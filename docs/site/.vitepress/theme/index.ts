// Harbor docs-site theme.
//
// Extends the stock VitePress default theme so every docs route (/skills/,
// /recipes/, /protocol/, /reference/) keeps the standard layout, sidebar, and
// search. The ONLY override is the home page: Layout.vue renders the bespoke
// marketing landing when a page declares `layout: landing` in frontmatter, and
// falls through to the default layout otherwise.
import DefaultTheme from "vitepress/theme";
import type { Theme, Directive } from "vitepress/theme";
import Layout from "./Layout.vue";

import "./styles/tokens.css";
import "./styles/docs.css";
import "./styles/landing.css";

// v-reveal: a scroll-into-view fade. SSR-safe — the server-rendered HTML stays
// visible (good for no-JS + SEO); the hidden state is only applied on the
// client for elements that start below the fold, so above-the-fold content
// never flashes. Honors prefers-reduced-motion via CSS in landing.css.
const reveal: Directive<HTMLElement> = {
  mounted(el) {
    if (typeof IntersectionObserver === "undefined") return;
    const reduced =
      typeof matchMedia !== "undefined" &&
      matchMedia("(prefers-reduced-motion: reduce)").matches;
    const rect = el.getBoundingClientRect();
    const belowFold = rect.top > (window.innerHeight || 0) * 0.85;
    if (reduced || !belowFold) {
      el.classList.add("is-visible");
      return;
    }
    el.setAttribute("data-reveal", "");
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting) {
            el.classList.add("is-visible");
            io.disconnect();
          }
        }
      },
      { threshold: 0.12, rootMargin: "0px 0px -8% 0px" },
    );
    io.observe(el);
  },
};

export default {
  extends: DefaultTheme,
  Layout,
  enhanceApp({ app }) {
    app.directive("reveal", reveal);
  },
} satisfies Theme;
