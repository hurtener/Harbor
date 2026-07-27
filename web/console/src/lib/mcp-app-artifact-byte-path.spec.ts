// The MCP-App host's BYTE path, driven by a CAPTURED transcript of the real
// Runtime on its DEFAULT artifact driver.
//
// # Why this file exists
//
// `fetchArtifactText` — the one read that turns a heavy tool payload into the
// data a rendered `ui://` App receives — used to resolve `artifacts.get_ref`
// and fetch the presigned URL it returned. `get_ref` type-asserts the OPTIONAL
// `artifacts.Presigner` capability, which exactly one of five shipped drivers
// implements and which is NOT `artifacts.DefaultDriver`. So on a stock Harbor
// the call threw, and every heavy payload reached an App as the host's
// "unavailable" stub rather than its data.
//
// NOTHING CAUGHT IT. The adapter's own spec asserted the presigned round trip
// against a fake whose `getRef` always succeeded, so it pinned the broken route
// as if it were the contract. The Console's byte fetch against the default
// driver had no coverage at all; the bug was found by reading D-347.
//
// # The fixture (CLAUDE.md §17.8)
//
// `testdata/artifacts-get-inmem-transcript.json` is a CAPTURED TRANSCRIPT, not
// a hand-authored blob: an artifact was `artifacts.put` into a live
// `harbor dev` (examples/dev.yaml, default `inmem` store) and then paged back
// out through `artifacts.get`, and every request/response pair was recorded
// verbatim — including the real `artifacts.get_ref` refusal on that same store,
// which is the asymmetry this whole path exists to close. A hand fixture would
// encode our own reading of the wire shape and could not tell a right field
// from a wrong one.
//
// The transcript's payload carries multi-byte runes and its windows are sized
// to SPLIT one across a boundary, because that is the defect a per-window
// decode produces and a whole-blob fetch never could.

import { describe, expect, it, vi } from 'vitest';

import { makeMCPAppHostClient, ARTIFACT_READ_WINDOW_BYTES } from './mcp-app-host-client.js';
import type { ProtocolClient } from './protocol/client.js';
import { ProtocolError } from './protocol/errors.js';
import transcript from './testdata/artifacts-get-inmem-transcript.json' with { type: 'json' };

interface GetResponse {
  content?: string;
  offset: number;
  returned_bytes: number;
  total_size_bytes: number;
  truncated: boolean;
}

interface RecordedWindow {
  request: { id: string; offset: number; max_bytes: number };
  status: number;
  response: GetResponse;
}

const WINDOWS = transcript.get_windows as RecordedWindow[];
const ARTIFACT_ID = transcript.artifact.id;
const EXPECTED_TEXT = transcript.artifact.text;

/**
 * A `ProtocolClient` that REPLAYS the recorded transcript: `artifacts.get`
 * answers with the window the real Runtime returned for that offset, and
 * `artifacts.get_ref` rejects with the refusal the real Runtime returned on the
 * same store. Both halves are the server's own words.
 */
function replayClient(): {
  client: ProtocolClient;
  get: ReturnType<typeof vi.fn>;
  getRef: ReturnType<typeof vi.fn>;
} {
  const get = vi.fn(async (req: { id: string; offset?: number }) => {
    const hit = WINDOWS.find((w) => w.response.offset === (req.offset ?? 0));
    if (!hit) throw new ProtocolError('not_found', `no recorded window at ${req.offset}`, 404);
    return hit.response;
  });
  const refusal = transcript.get_ref_refusal;
  const getRef = vi.fn(async () => {
    throw new ProtocolError(refusal.response.code, refusal.response.message, refusal.status);
  });
  const client = { artifacts: { get, getRef } } as unknown as ProtocolClient;
  return { client, get, getRef };
}

describe('fetchArtifactText — the driver-independent byte path (D-347 consumer 1 / D-353)', () => {
  it('reads the artifact through artifacts.get and NEVER through the presign route', async () => {
    // The regression guard. `get_ref` in this fake rejects with the refusal the
    // REAL inmem store returns, so any route back through the presign path
    // fails here exactly as it fails on a stock deployment.
    const { client, get, getRef } = replayClient();
    const host = makeMCPAppHostClient(client);
    await expect(host.fetchArtifactText(ARTIFACT_ID)).resolves.toBe(EXPECTED_TEXT);
    expect(getRef).not.toHaveBeenCalled();
    expect(get).toHaveBeenCalled();
  });

  it('the recorded refusal is the real one — presign_unsupported / 501 on the default store', () => {
    // Non-vacuity for the guard above: if the captured refusal were a success,
    // routing through `getRef` would pass and the test would guard nothing.
    expect(transcript.driver).toBe('inmem');
    expect(transcript.get_ref_refusal.status).toBe(501);
    expect(transcript.get_ref_refusal.response.code).toBe('presign_unsupported');
  });

  it('pages at the response-reported offset + returned_bytes until truncated is false', async () => {
    const { client, get } = replayClient();
    const host = makeMCPAppHostClient(client);
    await host.fetchArtifactText(ARTIFACT_ID);

    // One call per recorded window, at exactly the offsets the RESPONSES named
    // (never a locally-guessed cursor), each asking for the documented window.
    expect(get.mock.calls.length).toBe(WINDOWS.length);
    expect(get.mock.calls.map((c) => (c[0] as { offset: number }).offset)).toEqual(
      WINDOWS.map((w) => w.response.offset),
    );
    for (const [req] of get.mock.calls) {
      expect((req as { id: string }).id).toBe(ARTIFACT_ID);
      expect((req as { max_bytes: number }).max_bytes).toBe(ARTIFACT_READ_WINDOW_BYTES);
    }
    // The last window is the one that terminates the loop.
    expect(WINDOWS[WINDOWS.length - 1].response.truncated).toBe(false);
    expect(WINDOWS.length).toBeGreaterThan(1);
  });

  it('concatenates BYTES before decoding — a rune split across a window survives', async () => {
    const { client } = replayClient();
    const host = makeMCPAppHostClient(client);
    const text = await host.fetchArtifactText(ARTIFACT_ID);
    expect(text).toBe(EXPECTED_TEXT);
    // The payload really is multi-byte, and the parsed JSON round-trips.
    expect(text).toContain('Ökonomische');
    expect((JSON.parse(text) as { summary: string }).summary).not.toContain('�');
  });

  it('the transcript genuinely splits a rune (non-vacuity for the case above)', () => {
    // Decoding the FIRST window on its own — what a naive per-window decode
    // does — mangles the trailing partial rune. If this ever stopped holding,
    // the reassembly test above would pass on a broken implementation.
    const first = WINDOWS[0].response;
    const bytes = Uint8Array.from(atob(first.content ?? ''), (c) => c.charCodeAt(0));
    expect(new TextDecoder().decode(bytes)).toContain('�');
    expect(first.truncated).toBe(true);
  });

  it('refuses to page forever when the runtime reports truncated with no progress', async () => {
    // Fail loud rather than spin: a `truncated` response returning zero bytes
    // would advance the cursor nowhere.
    const get = vi.fn(async () => ({
      content: '',
      offset: 0,
      returned_bytes: 0,
      total_size_bytes: 100,
      truncated: true,
    }));
    const client = { artifacts: { get, getRef: vi.fn() } } as unknown as ProtocolClient;
    const host = makeMCPAppHostClient(client);
    await expect(host.fetchArtifactText('art_stuck')).rejects.toThrow(/refusing to page forever/);
    expect(get).toHaveBeenCalledTimes(1);
  });

  it('propagates a Protocol failure instead of returning empty text', async () => {
    const get = vi.fn(async () => {
      throw new ProtocolError('not_found', 'artifact "art_gone" not found in scope', 404);
    });
    const client = { artifacts: { get, getRef: vi.fn() } } as unknown as ProtocolClient;
    const host = makeMCPAppHostClient(client);
    await expect(host.fetchArtifactText('art_gone')).rejects.toThrow(/not found in scope/);
  });

  it('handles a single-window artifact with no paging at all', async () => {
    const last = WINDOWS[WINDOWS.length - 1].response;
    const whole = {
      content: last.content,
      offset: 0,
      returned_bytes: last.returned_bytes,
      total_size_bytes: last.returned_bytes,
      truncated: false,
    };
    const get = vi.fn(async () => whole);
    const client = { artifacts: { get, getRef: vi.fn() } } as unknown as ProtocolClient;
    const host = makeMCPAppHostClient(client);
    await host.fetchArtifactText(ARTIFACT_ID);
    expect(get).toHaveBeenCalledTimes(1);
  });
});
