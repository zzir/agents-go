import { Marked } from 'marked';
import hljs from 'highlight.js/lib/core';
import javascript from 'highlight.js/lib/languages/javascript';
import typescript from 'highlight.js/lib/languages/typescript';
import python from 'highlight.js/lib/languages/python';
import go from 'highlight.js/lib/languages/go';
import rust from 'highlight.js/lib/languages/rust';
import java from 'highlight.js/lib/languages/java';
import c from 'highlight.js/lib/languages/c';
import cpp from 'highlight.js/lib/languages/cpp';
import csharp from 'highlight.js/lib/languages/csharp';
import swift from 'highlight.js/lib/languages/swift';
import kotlin from 'highlight.js/lib/languages/kotlin';
import ruby from 'highlight.js/lib/languages/ruby';
import php from 'highlight.js/lib/languages/php';
import lua from 'highlight.js/lib/languages/lua';
import perl from 'highlight.js/lib/languages/perl';
import r from 'highlight.js/lib/languages/r';
import scala from 'highlight.js/lib/languages/scala';
import shell from 'highlight.js/lib/languages/shell';
import bash from 'highlight.js/lib/languages/bash';
import sql from 'highlight.js/lib/languages/sql';
import json from 'highlight.js/lib/languages/json';
import yaml from 'highlight.js/lib/languages/yaml';
import xml from 'highlight.js/lib/languages/xml';
import css from 'highlight.js/lib/languages/css';
import scss from 'highlight.js/lib/languages/scss';
import markdown from 'highlight.js/lib/languages/markdown';
import dockerfile from 'highlight.js/lib/languages/dockerfile';
import makefile from 'highlight.js/lib/languages/makefile';
import nginx from 'highlight.js/lib/languages/nginx';
import ini from 'highlight.js/lib/languages/ini';
import properties from 'highlight.js/lib/languages/properties';
import diff from 'highlight.js/lib/languages/diff';
import protobuf from 'highlight.js/lib/languages/protobuf';
import graphql from 'highlight.js/lib/languages/graphql';
import katex from 'katex';
import DOMPurify from 'dompurify';

const langs = {
  javascript, js: javascript, jsx: javascript,
  typescript, ts: typescript, tsx: typescript,
  python, py: python,
  go, rust, java, c, cpp, 'c++': cpp,
  csharp, 'c#': csharp, cs: csharp,
  swift, kotlin, ruby, rb: ruby,
  php, lua, perl, r, scala,
  shell, bash, sh: bash, zsh: bash,
  sql, json, yaml, yml: yaml,
  xml, html: xml, svg: xml,
  css, scss, sass: scss,
  markdown, md: markdown,
  dockerfile, docker: dockerfile,
  makefile, make: makefile,
  nginx, ini, toml: ini, conf: ini,
  properties, diff, patch: diff,
  protobuf, proto: protobuf,
  graphql, gql: graphql,
};

for (const [name, lang] of Object.entries(langs)) {
  hljs.registerLanguage(name, lang);
}

export { hljs };

const COPY_ICON = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>';
const CHECK_ICON = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>';

function wrapLines(html) {
  return html.split('\n').map((line, i) =>
    `<span class="code-line" data-ln="${i + 1}">${line || ' '}</span>`
  ).join('');
}

const MAX_VISIBLE_LINES = 20;

const mathBlock = {
  name: 'mathBlock',
  level: 'block',
  start(src) {
    return src.indexOf('$$');
  },
  tokenizer(src) {
    const match = src.match(/^\$\$([\s\S]+?)\$\$/);
    if (match) {
      return { type: 'mathBlock', raw: match[0], text: match[1].trim() };
    }
  },
  renderer(token) {
    try {
      return `<div class="math-block">${katex.renderToString(token.text, { displayMode: true, throwOnError: false })}</div>`;
    } catch {
      return `<div class="math-block"><code>${token.text}</code></div>`;
    }
  },
};

const mathInline = {
  name: 'mathInline',
  level: 'inline',
  start(src) {
    return src.indexOf('$');
  },
  tokenizer(src) {
    const match = src.match(/^\$([^\$\s](?:[^\$]*[^\$\s])?)\$/);
    if (match) {
      return { type: 'mathInline', raw: match[0], text: match[1].trim() };
    }
  },
  renderer(token) {
    try {
      return katex.renderToString(token.text, { displayMode: false, throwOnError: false });
    } catch {
      return `<code>${token.text}</code>`;
    }
  },
};

const marked = new Marked({
  renderer: {
    code({ text, lang }) {
      let highlighted;
      if (lang === 'mermaid') {
        highlighted = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      } else if (lang && hljs.getLanguage(lang)) {
        highlighted = hljs.highlight(text, { language: lang }).value;
      } else {
        highlighted = hljs.highlightAuto(text).value;
      }
      const escaped = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      const lineCount = text.split('\n').length;
      const cls = 'hljs-code-block' + (lineCount > MAX_VISIBLE_LINES ? ' scrollable' : '');
      return `<div class="code-block-wrapper"><button class="btn-octicon btn-copy" data-code="${escaped}" aria-label="Copy">${COPY_ICON}</button><pre class="${cls}"><code class="hljs">${highlighted}</code></pre></div>`;
    },
    codespan({ text }) {
      return `<code class="inline-code">${text}</code>`;
    },
  },
  breaks: true,
  gfm: true,
});

marked.use({ extensions: [mathBlock, mathInline] });

export function renderMarkdown(text) {
  if (!text) return '';
  return DOMPurify.sanitize(marked.parse(text), { ADD_TAGS: ['math-inline'], ADD_ATTR: ['data-code', 'data-ln', 'aria-label'] });
}

export function sanitizeSVG(svg) {
  return DOMPurify.sanitize(svg, { USE_PROFILES: { svg: true, svgFilters: true }, ADD_TAGS: ['foreignObject'] });
}

export function splitMermaidBlocks(text) {
  const re = /^```mermaid\s*\n([\s\S]*?)^```\s*$/gm;
  const segments = [];
  let last = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) segments.push({ type: 'md', text: text.slice(last, m.index) });
    segments.push({ type: 'mermaid', text: m[1].trim() });
    last = m.index + m[0].length;
  }
  if (last < text.length) segments.push({ type: 'md', text: text.slice(last) });
  return segments.length ? segments : [{ type: 'md', text }];
}
