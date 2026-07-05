// Full markdown rendering in a Web Worker: marked + highlight.js + KaTeX all
// run off the main thread. The main thread (markdown.ts) sanitizes the HTML
// with DOMPurify (needs a DOM) and caches it.
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
import type { LanguageFn } from 'highlight.js';
import { createMarked, escapeHTML } from './markdownShared';

const langs: Record<string, LanguageFn> = {
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

const fullMarked = createMarked({
  highlight(code, lang) {
    if (lang && hljs.getLanguage(lang)) {
      return hljs.highlight(code, { language: lang }).value;
    }
    return hljs.highlightAuto(code).value;
  },
  mathBlock(tex) {
    try {
      // output: 'html' skips the parallel MathML tree — half the DOM nodes.
      return `<div class="math-block">${katex.renderToString(tex, { displayMode: true, throwOnError: false, output: 'html' })}</div>`;
    } catch {
      return `<div class="math-block"><code>${escapeHTML(tex)}</code></div>`;
    }
  },
  mathInline(tex) {
    try {
      return katex.renderToString(tex, { displayMode: false, throwOnError: false, output: 'html' });
    } catch {
      return `<code>${escapeHTML(tex)}</code>`;
    }
  },
});

self.onmessage = (e: MessageEvent<{ id: number; text: string }>) => {
  const { id, text } = e.data;
  let html: string;
  try {
    html = fullMarked.parse(text) as string;
  } catch {
    html = `<pre>${escapeHTML(text)}</pre>`;
  }
  self.postMessage({ id, html });
};
