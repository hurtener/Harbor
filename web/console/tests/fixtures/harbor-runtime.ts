// Harbor Console e2e harness — Runtime + `harbor console` fixture (Phase 75 / D-115).
//
// This fixture boots, per Playwright worker, a Harbor Runtime + a
// `harbor console` instance serving the static Console build on an ephemeral
// port (the D-104 preflight pattern: bind `:0`, read the bound port back from
// the process stderr). It yields the Console base URL + a JWT scoped to the
// seeded test identity, and tears the child process down on worker teardown.
//
// `harbor console` (D-091) — NOT `harbor dev` — is the subcommand under test.
// The Console static build is served exclusively by `harbor console`; the
// Runtime ships headless. The subcommand itself lands in Phase 73m (Stage
// 2.3). The harness baseline (Phase 75) therefore degrades gracefully: when
// `bin/harbor console --help` exits non-zero (the subcommand is absent), the
// fixture yields `available: false` and the meta-test SKIPs. Once 73m merges,
// `available` flips to `true` and per-page specs run for real.

import { test as base } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { existsSync } from "node:fs";
import { resolve } from "node:path";
import {
  DEFAULT_TEST_IDENTITY,
  type IdentityTriple,
} from "../helpers/identity";

/** Repository root, derived from this file's location at runtime. */
const REPO_ROOT = resolve(import.meta.dirname, "..", "..", "..", "..");

/** The built Harbor binary the harness boots. `make build` produces it. */
const HARBOR_BIN = resolve(REPO_ROOT, "bin", "harbor");

/**
 * The Runtime fixture surface every per-page spec consumes.
 */
export type RuntimeFixture = {
  /**
   * False when `bin/harbor` or the `harbor console` subcommand is absent
   * (pre-Phase-73m, or `make build` has not run). Specs MUST gate on this and
   * `test.skip()` when false, so the harness baseline stays green before the
   * `harbor console` subcommand lands.
   */
  available: boolean;
  /** The base URL `harbor console` serves on (ephemeral port). */
  baseURL: string;
  /** A JWT scoped to the seeded test identity, for auth-storage seeding. */
  token: string;
  /** Seed an identity triple into the Runtime fixture. */
  seedIdentity(triple: IdentityTriple): Promise<void>;
};

/**
 * Probe whether `bin/harbor console` exists and is invokable. Synchronous so
 * specs can gate their `test.describe` on it at collection time — Playwright
 * instantiates the `page` fixture (which launches the browser) before a test
 * body runs, so a body-level `test.skip()` cannot prevent a browser launch on
 * a runner with no browser installed. Gating the describe block is the only
 * way to keep the harness baseline green pre-Phase-73m.
 */
export function consoleSubcommandAvailable(): boolean {
  if (!existsSync(HARBOR_BIN)) {
    return false;
  }
  const probe = spawnSync(HARBOR_BIN, ["console", "--help"], {
    timeout: 10_000,
    stdio: "ignore",
  });
  return probe.status === 0;
}

/** Parse the `HARBOR_DEV_BOUND=<host:port>` line `harbor` emits on stderr (D-104). */
function parseBoundURL(stderr: string): string | null {
  const match = stderr.match(/HARBOR_(?:DEV_)?BOUND=([^\s]+)/);
  return match ? `http://${match[1]}` : null;
}

/**
 * The extended Playwright `test` carrying the `runtime` fixture. Worker-scoped:
 * one Runtime + one `harbor console` per worker, reused across that worker's
 * specs. The harness runs `workers: 1` for V1 (see playwright.config.ts).
 */
// eslint-disable-next-line @typescript-eslint/no-empty-object-type
export const test = base.extend<{}, { runtime: RuntimeFixture }>({
  runtime: [
    // eslint-disable-next-line no-empty-pattern -- Playwright fixture fn requires the first arg; this fixture consumes none.
    async ({}, use) => {
      if (!consoleSubcommandAvailable()) {
        // Pre-73m (or no `make build`): yield an unavailable fixture so the
        // meta-test and per-page specs SKIP cleanly instead of failing.
        await use({
          available: false,
          baseURL: "",
          token: "",
          async seedIdentity() {
            /* no-op — fixture unavailable */
          },
        });
        return;
      }

      // Boot `harbor console` on an ephemeral port. `--bind 127.0.0.1:0`
      // lets the OS pick a free port; the bound address is read back from
      // the `HARBOR_*_BOUND=` stderr line (D-104).
      const child: ChildProcess = spawn(
        HARBOR_BIN,
        ["console", "--bind", "127.0.0.1:0"],
        { cwd: REPO_ROOT, stdio: ["ignore", "pipe", "pipe"] },
      );

      let stderr = "";
      child.stderr?.on("data", (chunk: Buffer) => {
        stderr += chunk.toString();
      });

      // Wait for the bound-port line, bounded by a real-time timeout (no
      // sleep-as-synchronisation — CLAUDE.md §17.4).
      const baseURL = await new Promise<string>((resolveURL, rejectURL) => {
        const deadline = setTimeout(() => {
          rejectURL(
            new Error(
              `harbor console did not emit a bound-port line within 15s; stderr:\n${stderr}`,
            ),
          );
        }, 15_000);
        const poll = setInterval(() => {
          const url = parseBoundURL(stderr);
          if (url) {
            clearTimeout(deadline);
            clearInterval(poll);
            resolveURL(url);
          }
        }, 100);
        child.once("exit", (code) => {
          clearTimeout(deadline);
          clearInterval(poll);
          rejectURL(
            new Error(
              `harbor console exited early (code ${code}); stderr:\n${stderr}`,
            ),
          );
        });
      });

      const token = parseDevToken(stderr);

      await use({
        available: true,
        baseURL,
        token,
        // eslint-disable-next-line @typescript-eslint/no-unused-vars -- baseline seam; first seeding consumer is Phase 73a Overview.
        async seedIdentity(_triple: IdentityTriple) {
          // Per-page specs seed deterministic Runtime state through the typed
          // Protocol client (helpers/protocol.ts). The harness baseline ships
          // the seam; the first real seeding consumer is Phase 73a Overview.
        },
      });

      // Teardown: graceful SIGTERM, then SIGKILL after a 500ms grace so a
      // crashed-spec run cannot leak the child process.
      child.kill("SIGTERM");
      await new Promise<void>((done) => {
        const grace = setTimeout(() => {
          child.kill("SIGKILL");
          done();
        }, 500);
        child.once("exit", () => {
          clearTimeout(grace);
          done();
        });
      });
    },
    { scope: "worker" },
  ],
});

/** Parse the `HARBOR_DEV_TOKEN=<jwt>` line `harbor` prints at boot. */
function parseDevToken(stderr: string): string {
  const match = stderr.match(/HARBOR_DEV_TOKEN=([^\s]+)/);
  return match ? match[1] : "";
}

export { DEFAULT_TEST_IDENTITY };
export { expect } from "@playwright/test";
