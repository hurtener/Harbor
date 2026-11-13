// The landing page's quoted counts come from the tree, not from copy.
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { REPO_ROOT, loadFacts } from "./facts.mjs";

test("site facts match the repository tree", () => {
  const f = loadFacts();
  for (const [k, v] of Object.entries(f)) assert.ok(v > 0, k);
  // Every operator skill and recipe has a site page (phase-103 mirror rule).
  const siteSkills = readdirSync(join(REPO_ROOT, "docs/site/skills"), { withFileTypes: true })
    .filter((d) => d.isDirectory()).length;
  assert.equal(f.operatorSkills, siteSkills);
  const siteRecipes = readdirSync(join(REPO_ROOT, "docs/site/recipes"))
    .filter((n) => n.endsWith(".md") && n !== "index.md").length;
  assert.equal(f.recipes, siteRecipes);
  const methodsRows = readFileSync(join(REPO_ROOT, "docs/site/protocol/methods.md"), "utf8")
    .match(/^\| `[a-z_]+(?:\.[a-z_]+)*` \| `(?:GET|POST|PUT|DELETE|PATCH) /gm);
  assert.equal(f.protocolMethods, methodsRows?.length ?? 0);
});
