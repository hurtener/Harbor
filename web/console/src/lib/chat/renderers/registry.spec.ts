// Vitest unit test for the canonical renderer registry dispatch table
// (Phase 73l / D-120). Exercises the dispatch contract — first-match
// wins, the six built-in MIME renderers, the fallback, and the
// `registerRenderer` extension seam Phase 73n uses.

import { describe, expect, it } from 'vitest';

import {
  dispatchRenderer,
  fallbackRenderer,
  mimeIs,
  mimePrefix,
  registerRenderer,
  registeredSources
} from './index.js';

describe('canonical renderer registry', () => {
  it('registers the six built-in MIME renderers', () => {
    const sources = registeredSources();
    for (const expected of ['image', 'pdf', 'audio', 'json', 'markdown', 'code']) {
      expect(sources).toContain(expected);
    }
  });

  it('dispatches image/* to the image renderer', () => {
    expect(dispatchRenderer('image/png').source).toBe('image');
    expect(dispatchRenderer('image/jpeg').source).toBe('image');
  });

  it('dispatches application/pdf to the pdf renderer', () => {
    expect(dispatchRenderer('application/pdf').source).toBe('pdf');
  });

  it('dispatches audio/* to the audio renderer', () => {
    expect(dispatchRenderer('audio/mpeg').source).toBe('audio');
  });

  it('dispatches application/json to the json renderer', () => {
    expect(dispatchRenderer('application/json').source).toBe('json');
  });

  it('dispatches text/markdown to the markdown renderer (not code)', () => {
    // markdown registers before the broad text/* code rule, so the more
    // specific rule wins — first-match-wins on registration order.
    expect(dispatchRenderer('text/markdown').source).toBe('markdown');
  });

  it('dispatches plain text/* to the code renderer', () => {
    expect(dispatchRenderer('text/plain').source).toBe('code');
    expect(dispatchRenderer('text/x-go').source).toBe('code');
  });

  it('returns the fallback renderer for an unrenderable MIME type', () => {
    expect(dispatchRenderer('application/octet-stream').source).toBe('fallback');
    expect(dispatchRenderer('').source).toBe('fallback');
    expect(dispatchRenderer('video/mp4').source).toBe(fallbackRenderer.source);
  });

  it('normalises MIME case before dispatch', () => {
    expect(dispatchRenderer('IMAGE/PNG').source).toBe('image');
    expect(dispatchRenderer('  application/PDF  ').source).toBe('pdf');
  });

  it('supports extension via registerRenderer without editing the dispatch core', () => {
    // This is exactly the seam Phase 73n (Playground) uses to add
    // chat-bubble / tool-call / diff renderers. A new rule is appended;
    // the dispatch core is untouched.
    const before = dispatchRenderer('application/vnd.harbor.test-diff');
    expect(before.source).toBe('fallback');

    registerRenderer(mimeIs('application/vnd.harbor.test-diff'), {
      source: 'test-diff',
      component: fallbackRenderer.component // placeholder component for the test
    });

    const after = dispatchRenderer('application/vnd.harbor.test-diff');
    expect(after.source).toBe('test-diff');
  });

  it('mimeIs matches exact MIME types case-insensitively', () => {
    const pred = mimeIs('application/json', 'text/json');
    expect(pred('application/json')).toBe(true);
    expect(pred('text/json')).toBe(true);
    expect(pred('application/xml')).toBe(false);
  });

  it('mimePrefix matches MIME type prefixes', () => {
    const pred = mimePrefix('image/', 'audio/');
    expect(pred('image/gif')).toBe(true);
    expect(pred('audio/wav')).toBe(true);
    expect(pred('text/plain')).toBe(false);
  });
});
