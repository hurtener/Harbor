/*
 * event-viewer-ts is the TypeScript sibling of the worked `event-viewer`
 * Go client (../event-viewer). It is deliberately dependency-free: pure
 * Node built-ins (global fetch) against the Harbor Protocol wire, with its
 * wire types VENDORED from the generated `harbor-protocol.gen.ts` module —
 * the point this example exists to make: a third-party TypeScript client
 * gets typed wire shapes by copy-vendoring one generated file, no Console
 * import and no hand-transcription.
 *
 * What it does, in order:
 *
 *   1. Resolves a connection token — HARBOR_TOKEN if set, otherwise the
 *      loopback-only dev bootstrap endpoint (`harbor dev` only).
 *   2. Calls `runtime.info`, typed as the generated `RuntimeInfo`, and
 *      checks Protocol-version compatibility (same major) plus the
 *      `events_subscribe` capability. It also compares the runtime's
 *      reported `wire_surface_digest` against the WIRE_SURFACE_DIGEST this
 *      module was generated against, warning on a skew.
 *   3. Opens the SSE event stream for one session and prints every frame:
 *      sequence, type, run, and the payload JSON.
 *
 * Run it (no install step — Node >= 22.6 strips the types):
 *
 *   harbor dev &
 *   node --experimental-strip-types event-viewer.ts
 *
 * Or with a TypeScript runner you already have:
 *
 *   npx tsx event-viewer.ts
 *
 * Bounded modes (used by the phase smoke, handy for scripting):
 *   HARBOR_PROBE=1            do the runtime.info handshake, then exit 0.
 *   HARBOR_STREAM_SECONDS=N   tail the stream for N seconds, then exit 0
 *                             (0 / unset = tail until interrupted).
 */

import type {
  RuntimeInfo,
  HarborMethod,
} from "./harbor-protocol.gen.ts";
import {
  PROTOCOL_VERSION,
  WIRE_SURFACE_DIGEST,
} from "./harbor-protocol.gen.ts";

const baseURL = envOr("HARBOR_BASE_URL", "http://127.0.0.1:18080");
const session = envOr("HARBOR_SESSION", "event-viewer-ts");

// The method names this client calls — typed against the generated
// HarborMethod union, so a renamed or dropped method fails the type-check
// rather than 404ing at runtime.
const RUNTIME_INFO: HarborMethod = "runtime.info";

async function main(): Promise<void> {
  // 1. A token authenticates (tenant, user) + scopes; the session is
  // chosen per-request via the X-Harbor-Session header.
  let token = process.env.HARBOR_TOKEN ?? "";
  if (token === "") {
    const boot = await post<{ token: string }>(
      `${baseURL}/v1/dev/bootstrap.json`,
      "",
      "",
      {},
    );
    token = boot.token;
  }

  // 2. runtime.info — the attach handshake, typed as the generated
  // RuntimeInfo. Pin what you were built against; tolerate what you don't
  // know (see the versioning guide).
  const info = await post<RuntimeInfo>(
    `${baseURL}/v1/control/${RUNTIME_INFO}`,
    token,
    session,
    { identity: {} },
  );
  console.log(
    `runtime ${info.instance_id} (build ${info.build_version}) speaks Protocol ${info.protocol_version}`,
  );
  console.log(`this client was generated against Protocol ${PROTOCOL_VERSION}`);

  const builtAgainstMajor = PROTOCOL_VERSION.split(".")[0];
  if (info.protocol_version.split(".")[0] !== builtAgainstMajor) {
    fail(
      `protocol major version skew: runtime speaks ${info.protocol_version}, this client was built against ${builtAgainstMajor}.x`,
    );
  }
  if (info.wire_surface_digest && info.wire_surface_digest !== WIRE_SURFACE_DIGEST) {
    console.warn(
      `[warn] wire-surface digest skew: runtime=${info.wire_surface_digest} vendored=${WIRE_SURFACE_DIGEST} — re-vendor harbor-protocol.gen.ts`,
    );
  }
  if (!info.capabilities.includes("events_subscribe")) {
    fail("this runtime does not advertise the events_subscribe capability");
  }

  // Probe mode: the handshake proved the generated types match the live
  // runtime — exit before opening the long-lived stream.
  if (process.env.HARBOR_PROBE === "1") {
    console.log("probe OK — runtime.info round-trip succeeded");
    return;
  }

  // 3. The SSE stream. Last-Event-ID: 0 replays the session's retained
  // history before live-tailing. An optional HARBOR_STREAM_SECONDS bounds
  // the tail so the example is scriptable.
  const controller = new AbortController();
  const streamSeconds = Number(process.env.HARBOR_STREAM_SECONDS ?? "0");
  let timer: ReturnType<typeof setTimeout> | undefined;
  if (streamSeconds > 0) {
    timer = setTimeout(() => controller.abort(), streamSeconds * 1000);
  }

  const resp = await fetch(`${baseURL}/v1/events`, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${token}`,
      "X-Harbor-Session": session,
      "Last-Event-ID": "0",
    },
    signal: controller.signal,
  }).catch((err: unknown) => {
    if (controller.signal.aborted) return undefined;
    fail(`open event stream: ${String(err)}`);
  });
  if (resp === undefined) return; // aborted before the response arrived
  if (!resp.ok) {
    fail(`event stream: HTTP ${resp.status}`);
  }
  if (resp.body === null) {
    fail("event stream: empty response body");
  }
  console.log(`tailing session "${session}" — Ctrl-C to stop`);

  // Each SSE frame is `event:` + `id:` + `data:` lines, blank-line
  // terminated. The data object carries the run and the payload.
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventType = "";
  let id = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, nl).replace(/\r$/, "");
        buffer = buffer.slice(nl + 1);
        if (line.startsWith("event: ")) {
          eventType = line.slice("event: ".length);
        } else if (line.startsWith("id: ")) {
          id = line.slice("id: ".length);
        } else if (line.startsWith("data: ")) {
          const data = line.slice("data: ".length);
          let frame: { run?: string; payload?: unknown };
          try {
            frame = JSON.parse(data);
          } catch {
            fail(`malformed data frame: ${data}`); // a broken frame is a wire bug — fail loud
            return;
          }
          console.log(
            `${id.padStart(6)}  ${eventType.padEnd(28)} run=${(frame.run ?? "").padEnd(26)} ${JSON.stringify(frame.payload)}`,
          );
        }
      }
    }
  } catch (err: unknown) {
    if (!controller.signal.aborted) {
      fail(`event stream closed: ${String(err)}`);
    }
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

// post issues an identity-bearing Protocol POST and decodes the JSON
// response into T, failing loudly on any non-200.
async function post<T>(
  url: string,
  token: string,
  sessionID: string,
  body: unknown,
): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  if (token !== "") headers.Authorization = `Bearer ${token}`;
  if (sessionID !== "") headers["X-Harbor-Session"] = sessionID;
  const resp = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  }).catch((err: unknown) => fail(`${url}: ${String(err)}`));
  if (!resp.ok) {
    fail(`${url}: HTTP ${resp.status}`);
  }
  return (await resp.json()) as T;
}

function envOr(key: string, def: string): string {
  const v = process.env[key];
  return v !== undefined && v !== "" ? v : def;
}

function fail(message: string): never {
  console.error(`event-viewer-ts: ${message}`);
  process.exit(1);
}

void main();
