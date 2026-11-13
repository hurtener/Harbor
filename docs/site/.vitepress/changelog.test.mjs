// Run by `npm test` (and before every `npm run build`): the landing
// page's release metadata must match the newest released CHANGELOG heading.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { CHANGELOG_PATH, SUMMARY_MAX, loadRelease, parseRelease } from "./changelog.mjs";

test("derived version equals the newest released CHANGELOG heading", () => {
  const text = readFileSync(CHANGELOG_PATH, "utf8");
  const newest = text
    .split("\n")
    .find((l) => l.startsWith("## [") && !l.startsWith("## [Unreleased]"));
  assert.ok(newest, "no released heading in CHANGELOG.md");
  const expected = /^## \[([^\]]+)\]/.exec(newest)[1];

  const r = loadRelease();
  assert.equal(r.version, expected);
  assert.equal(r.eyebrow, `Go-native agent runtime · v${expected}`);
  assert.ok(r.announcement.startsWith(`v${expected} just shipped — `));
  assert.ok(r.summary.length > 0 && r.summary.length <= SUMMARY_MAX + 1);
  assert.doesNotMatch(r.announcement, /[*`]|\(D-\d+\)/);
});

test("summary joins bold lead phrases and strips markdown", () => {
  const r = parseRelease(
    [
      "## [Unreleased]",
      "",
      "- **Not yet.** ignored",
      "",
      "## [2.3.4] — 2030-01-02",
      "",
      "### Added",
      "",
      "- **`foo.bar` keeps state across",
      "  restarts (D-12).** Details.",
      "- **Faster [rollups](x.md).** More.",
      "",
      "## [2.3.3] — 2030-01-01",
      "- **Older.** ignored",
    ].join("\n"),
  );
  assert.equal(r.version, "2.3.4");
  assert.equal(r.summary, "foo.bar keeps state across restarts; faster rollups");
  assert.equal(r.announcement, "v2.3.4 just shipped — foo.bar keeps state across restarts; faster rollups");
});

test("falls back when no lead phrase exists", () => {
  const r = parseRelease("## [1.0.0] — 2030-01-01\n\n- plain bullet\n");
  assert.equal(r.announcement, "v1.0.0 just shipped — see what changed");
});

test("fails loudly without a released heading", () => {
  assert.throws(() => parseRelease("## [Unreleased]\n\n- **x.** y\n"), /no released/);
});
