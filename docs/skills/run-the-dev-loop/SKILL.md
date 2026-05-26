---
name: run-the-dev-loop
description: "Use `harbor dev` + `harbor console` to run an agent locally with the Console attached. Use when iterating on a Harbor project — single-process or multi-process attach posture, the `HARBOR_DEV_TOKEN` handshake, hot reload on yaml changes, and which posture to pick when."
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: "dev console"
---

# Run the Harbor dev loop

Harbor's local-iteration loop is two processes: `harbor dev` (the Runtime — Protocol server on `:18080`, the agent yaml's tools / LLM / planner all wired) and `harbor console` (the Svelte SPA on `:18790` that attaches to the Runtime over the Protocol). They're separate binaries' worth of work but the same `harbor` static binary — pick by subcommand. Choose between two postures:

- **Single-process** — Runtime + Console both at the same workstation, no auth ceremony. Easiest. Default for development.
- **Multi-process** — Runtime on a workstation or VM; Console on a different machine, browser tab, or laptop. Production posture. Needs the CORS allowlist (`server.allowed_origins`) configured in the yaml.

## 1. Single-process dev

In one terminal:

```bash
cd ~/my-agent
harbor dev
```

You see:

```text
time=... msg="harbor dev: starting Protocol server" bind=127.0.0.1:18080
HARBOR_DEV_TOKEN=eyJhbGc...
HARBOR_DEV_BOUND=127.0.0.1:18080
time=... msg="harbor dev: listener bound" bind=127.0.0.1:18080
```

In a second terminal:

```bash
harbor console --port 18790
```

Open `http://127.0.0.1:18790`. In the browser DevTools console:

```js
localStorage.clear();
localStorage.setItem('harbor.runtime.base_url', 'http://127.0.0.1:18080');
localStorage.setItem('harbor.runtime.token', '<the HARBOR_DEV_TOKEN from the first terminal>');
localStorage.setItem('harbor.runtime.tenant', 'dev');
localStorage.setItem('harbor.runtime.user', 'dev');
localStorage.setItem('harbor.runtime.session', 'dev');
localStorage.setItem('harbor.runtime.scopes', 'admin console:fleet');
```

Reload the page. The Console's connection footer shows "Connected <http://127.0.0.1:18080>" and every page reads from your live Runtime.

**The token rotates per `harbor dev` boot.** Restart Runtime → reseed localStorage. Tokens expire 24h after `iat`.

## 2. Multi-process dev (attach from a remote machine)

Edit the agent's `harbor.yaml`:

```yaml
server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
  # Allow the Console origin to attach via CORS (default-deny posture).
  allowed_origins:
    - http://127.0.0.1:18790
    - http://10.0.0.42:18790        # if Console runs on another host
```

`harbor dev` honours the binding (note: `bind_addr` is honoured only by `harbor serve` — `harbor dev` always binds `:18080`; setting `:8080` in the yaml is operator convenience for the production path, not the dev path).

The CORS allowlist gates origin access. A request from an origin NOT in the list gets a default-deny — the wire is reachable but the browser rejects the response.

In a second terminal (or on a different machine):

```bash
harbor console --port 18790
```

Then seed localStorage the same way, with the Runtime's base_url pointing at wherever Runtime is reachable from the browser. Identity stays `(dev, dev, dev)`.

## 3. Hot reload

`harbor dev` runs an fsnotify watcher over the project directory:

- **`harbor.yaml`** changes → the Runtime drains in-flight runs, re-reads the config, re-wires the LLM / tools / memory, and restarts the Protocol server. The watcher debounces — a flurry of saves coalesces to one reload.
- **In-process tool `.go` file** changes → `harbor dev` does NOT recompile your binary automatically. You re-run `go build && harbor dev` for code changes. Yaml-only changes (provider model swap, new MCP server entry, memory budget tweak) flow through the hot-reload path.

The watcher policy is `drain` with a 5s timeout — in-flight tasks are given 5s to settle; longer-running runs are cancelled at the 5s mark. Drainage timing is hot-reloadable via `server.shutdown_grace_period` in the yaml.

**Watch out for the SQLite-WAL feedback loop.** `state.dsn: ./harbor-state.sqlite` writes a `.sqlite-wal` sibling file the watcher sees as a change — infinite reboot loop. Move the DSN OUTSIDE the project dir: `state.dsn: /tmp/harbor-validation/<project>-state.sqlite` or `~/.harbor/<project>.sqlite`. The init template puts it under `/tmp/harbor-validation/` for this reason.

## 4. Token re-seed (the 24h expiry trap)

`HARBOR_DEV_TOKEN` is signed with an in-memory ephemeral ES256 key minted per `harbor dev` boot. Every restart mints a new key + a new token:

- The OLD token is still in your browser's localStorage.
- The NEW token's `kid` doesn't match the one the OLD signed with.
- Every Protocol call from the Console fails 401.

When this happens you see the Console footer flipping to "Disconnected"; DevTools shows a wall of `401 Unauthorized` browser errors. Fix: copy the new token from the Runtime's stderr and reseed `localStorage.setItem('harbor.runtime.token', ...)`. Reload.

Tokens also expire after 24h — a Console session left open overnight needs the same reseed.

## 5. Logs — where to look

`harbor dev`'s stderr is the operator log. JSON-structured by default (`telemetry.log_format: json`); switch to text for human-readable dev with:

```yaml
telemetry:
  log_format: text
  log_level: debug  # bumps to debug for the noisy traces
```

Per-task events ALSO go to the Console's Events page in real time (assuming `events.driver: inmem` — the dev default — keeps events in memory while the Console is attached). Use the Events page when you want a live stream; use stderr when you want grep-able history.

## Common failure modes

- **Console shows "Disconnected" after I restart `harbor dev`.** Token rotated. Reseed localStorage. See §4.
- **Browser DevTools floods with `401 Unauthorized`.** Same root cause — stale token. Reseed.
- **`harbor dev` reboots in a loop with `fsnotify` events.** The SQLite WAL trap (see §3). Move `state.dsn` outside the project dir.
- **CORS preflight failing on multi-process Console.** Your `server.allowed_origins` doesn't list the Console's origin. The Runtime defaults to default-deny — explicitly add the Console URL.
- **Port conflict on `:18080` / `:18790`.** Another `harbor dev` is already running. `lsof -nP -iTCP:18080,18790 -sTCP:LISTEN | awk 'NR>1 {print $2}' | xargs -r kill`.

## See also

- [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) — get to the point where `harbor dev` can boot.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — what to do once the Console is attached.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the 14-page Console tour.
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) — if you're attaching a NON-bundled UI to the Runtime.
- Sibling project: Dockyard's [`run-the-dev-loop`](https://github.com/hurtener/dockyard) — the same hot-reload posture for MCP-server projects.
