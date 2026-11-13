// Build-time repo facts for the landing page (protocol method count, CLI
// subcommands, operator skills, recipes), so the counts it quotes cannot
// drift from the tree. config.ts injects the result as `__HARBOR_FACTS__`.
// Any fact that cannot be read throws and fails the docs build.
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = fileURLToPath(new URL("../../../", import.meta.url));

function need(name, n) {
  if (!Number.isInteger(n) || n <= 0) throw new Error(`site facts: could not derive ${name}`);
  return n;
}

/** @returns {{ protocolMethods: number, cliCommands: number, operatorSkills: number, recipes: number }} */
export function loadFacts(root = REPO_ROOT) {
  const methods = readFileSync(join(root, "docs/site/protocol/methods.md"), "utf8");
  const root_go = readFileSync(join(root, "cmd/harbor/root.go"), "utf8");
  const skillsDir = join(root, "docs/skills");
  const recipesDir = join(root, "docs/recipes");
  return {
    protocolMethods: need("protocolMethods", Number(/The (\d+) canonical Harbor Protocol methods/.exec(methods)?.[1])),
    cliCommands: need("cliCommands", (root_go.match(/^\s*root\.AddCommand\(/gm) ?? []).length),
    operatorSkills: need(
      "operatorSkills",
      readdirSync(skillsDir, { withFileTypes: true }).filter(
        (d) => d.isDirectory() && existsSync(join(skillsDir, d.name, "SKILL.md")),
      ).length,
    ),
    recipes: need(
      "recipes",
      readdirSync(recipesDir).filter((f) => f.endsWith(".md") && f !== "README.md").length,
    ),
  };
}
