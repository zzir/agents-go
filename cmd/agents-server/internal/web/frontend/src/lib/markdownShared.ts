// Markdown pipeline core shared by the main thread (lite, for streaming) and
// the worker (full, with hljs + KaTeX). This module must stay free of heavy
// imports — no highlight.js, no katex — so the main bundle stays lean.
import { Marked, type Tokens, type TokenizerExtension, type RendererExtension } from 'marked';

// Inline SVGs for HTML-string templates (React components use
// @primer/octicons-react directly; these are the same official octicon paths
// — copy-16 and check-16 — usable inside the worker where React isn't).
export const COPY_ICON = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16" aria-hidden="true"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>';
export const CHECK_ICON = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16" aria-hidden="true"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>';

// Code blocks longer than this render collapsed (CSS-clipped, not scrollable)
// with a "Show all N lines" button — content budget, Claude-Code style.
export const CODE_COLLAPSE_LINES = 40;

export function escapeHTML(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export interface RenderHooks {
  // Returns highlighted HTML for a code block; absent in the lite renderer.
  highlight?: (code: string, lang: string | undefined) => string;
  // KaTeX render functions; absent in the lite renderer (raw TeX shown).
  mathBlock?: (tex: string) => string;
  mathInline?: (tex: string) => string;
}

export function createMarked(hooks: RenderHooks): Marked {
  const mathBlock: TokenizerExtension & RendererExtension = {
    name: 'mathBlock',
    level: 'block',
    start(src: string) { return src.indexOf('$$'); },
    tokenizer(src: string) {
      const match = src.match(/^\$\$([\s\S]+?)\$\$/);
      if (match) return { type: 'mathBlock', raw: match[0], text: match[1].trim() };
    },
    renderer(token: Tokens.Generic) {
      if (hooks.mathBlock) return hooks.mathBlock(token.text);
      return `<div class="math-block"><code>${escapeHTML(token.text)}</code></div>`;
    },
  };

  const mathInline: TokenizerExtension & RendererExtension = {
    name: 'mathInline',
    level: 'inline',
    start(src: string) { return src.indexOf('$'); },
    tokenizer(src: string) {
      const match = src.match(/^\$([^\$\s](?:[^\$]*[^\$\s])?)\$/);
      if (match) return { type: 'mathInline', raw: match[0], text: match[1].trim() };
    },
    renderer(token: Tokens.Generic) {
      if (hooks.mathInline) return hooks.mathInline(token.text);
      return `<code class="inline-code">${escapeHTML(token.raw)}</code>`;
    },
  };

  const m = new Marked({
    renderer: {
      code({ text, lang }: { text: string; lang?: string }) {
        let highlighted: string;
        if (lang === 'mermaid' || !hooks.highlight) {
          highlighted = escapeHTML(text);
        } else {
          highlighted = hooks.highlight(text, lang);
        }
        const escaped = escapeHTML(text).replace(/"/g, '&quot;');
        const lineCount = text.split('\n').length;
        const collapsed = lineCount > CODE_COLLAPSE_LINES;
        const wrapCls = 'code-block-wrapper' + (collapsed ? ' code-collapsed' : '');
        const expandBtn = collapsed
          ? `<button class="btn-code-expand">Show all ${lineCount} lines</button>`
          : '';
        return `<div class="${wrapCls}"><button class="btn-octicon btn-copy" data-code="${escaped}" aria-label="Copy">${COPY_ICON}</button><pre class="hljs-code-block"><code class="hljs">${highlighted}</code></pre>${expandBtn}</div>`;
      },
      codespan({ text }: { text: string }) {
        return `<code class="inline-code">${text}</code>`;
      },
      html({ text }: { text: string }) {
        return escapeHTML(text);
      },
    },
    breaks: true,
    gfm: true,
  });
  m.use({ extensions: [mathBlock, mathInline] });
  return m;
}

// Lite instance for the main thread: no syntax highlighting, no KaTeX.
// Used for streaming text where content changes every frame.
const liteMarked = createMarked({});

export function renderLiteCore(text: string): string {
  return liteMarked.parse(text) as string;
}
