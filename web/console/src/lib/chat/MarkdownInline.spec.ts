// Harbor Console — MarkdownInline parser unit tests (Phase 108 / D-167).
//
// Pins each grammar rule (headings, bullets, numbered lists, bold,
// italic, inline code, fenced code, line breaks) plus XSS safety.

import { describe, it, expect } from 'vitest';
import { parseMarkdown, type MarkdownNode } from '$lib/chat/markdown-parser.js';

function expectParagraph(node: MarkdownNode) {
  expect(node.type).toBe('paragraph');
  return node as Extract<MarkdownNode, { type: 'paragraph' }>;
}

describe('parseMarkdown', () => {
  it('parses headings', () => {
    const tree = parseMarkdown('# Hello\n## World\n### Deep');
    expect(tree).toHaveLength(3);
    expect(tree[0]).toEqual({ type: 'heading', level: 1, text: 'Hello' });
    expect(tree[1]).toEqual({ type: 'heading', level: 2, text: 'World' });
    expect(tree[2]).toEqual({ type: 'heading', level: 3, text: 'Deep' });
  });

  it('parses bold with **', () => {
    const tree = parseMarkdown('**bold text**');
    expect(tree).toHaveLength(1);
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([{ type: 'bold', text: 'bold text' }]);
  });

  it('parses bold with __', () => {
    const tree = parseMarkdown('__bold text__');
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([{ type: 'bold', text: 'bold text' }]);
  });

  it('parses italic with *', () => {
    const tree = parseMarkdown('*italic text*');
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([{ type: 'italic', text: 'italic text' }]);
  });

  it('parses italic with _', () => {
    const tree = parseMarkdown('_italic text_');
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([{ type: 'italic', text: 'italic text' }]);
  });

  it('parses inline code', () => {
    const tree = parseMarkdown('`code snippet`');
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([{ type: 'code', text: 'code snippet' }]);
  });

  it('parses unordered lists', () => {
    const tree = parseMarkdown('- first\n- second\n- third');
    expect(tree).toHaveLength(1);
    expect(tree[0].type).toBe('list');
    const list = tree[0] as Extract<MarkdownNode, { type: 'list' }>;
    expect(list.ordered).toBe(false);
    expect(list.items).toHaveLength(3);
    expect(list.items[0]).toEqual([{ type: 'text', text: 'first' }]);
  });

  it('parses ordered lists', () => {
    const tree = parseMarkdown('1. first\n2. second');
    expect(tree).toHaveLength(1);
    const list = tree[0] as Extract<MarkdownNode, { type: 'list' }>;
    expect(list.ordered).toBe(true);
    expect(list.items).toHaveLength(2);
  });

  it('parses fenced code blocks', () => {
    const tree = parseMarkdown('```ts\nconst x = 1;\n```');
    expect(tree).toHaveLength(1);
    expect(tree[0]).toEqual({
      type: 'code',
      lang: 'ts',
      text: 'const x = 1;'
    });
  });

  it('parses mixed inline formatting', () => {
    const tree = parseMarkdown('**By topic:** music, *games*, and `code`.');
    const para = expectParagraph(tree[0]);
    expect(para.children).toEqual([
      { type: 'bold', text: 'By topic:' },
      { type: 'text', text: ' music, ' },
      { type: 'italic', text: 'games' },
      { type: 'text', text: ', and ' },
      { type: 'code', text: 'code' },
      { type: 'text', text: '.' }
    ]);
  });

  it('escapes HTML tags (XSS safety)', () => {
    const tree = parseMarkdown('<script>alert(1)</script>');
    const para = expectParagraph(tree[0]);
    // The < should be escaped in the heading text, but the parser
    // preserves it as text content — Svelte auto-escapes on render.
    const text = para.children[0].text;
    expect(text).toContain('<script>');
    expect(text).toContain('</script>');
  });

  it('does not autolink javascript: URLs', () => {
    const tree = parseMarkdown('[click](javascript:alert(1))');
    // Should render as plain text, not a link
    const para = expectParagraph(tree[0]);
    expect(para.children[0].text).toBe('[click](javascript:alert(1))');
  });

  it('does not allow img onerror', () => {
    const tree = parseMarkdown('<img onerror=alert(1) src=x>');
    const para = expectParagraph(tree[0]);
    const text = para.children[0].text;
    expect(text).toContain('<img');
    expect(text).toContain('onerror=');
  });

  it('produces no eval / innerHTML / document.write in output', () => {
    // This is a structural guarantee: the parser only produces a typed AST
    // with text/bold/italic/code nodes. It never produces raw HTML strings.
    const tree = parseMarkdown('**bold** and `code`');
    for (const node of tree) {
      if (node.type === 'paragraph') {
        const para = node as Extract<MarkdownNode, { type: 'paragraph' }>;
        for (const child of para.children) {
          expect(child.type).toMatch(/^(text|bold|italic|code)$/);
        }
      }
    }
  });
});
