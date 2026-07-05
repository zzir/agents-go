// Markdown facade for the main thread.
//
// Two render paths:
//  - renderMarkdownLite: synchronous, no hljs/KaTeX — for streaming text that
//    changes every frame (cheap enough to run per animation frame).
//  - renderMarkdownAsync / useAsyncMarkdown: full pipeline (marked + hljs +
//    KaTeX) in a Web Worker; the main thread only sanitizes (DOMPurify needs
//    a DOM) and serves an LRU cache. History re-renders are cache hits.
import { useState, useEffect } from 'react';
import DOMPurify from 'dompurify';
import { renderLiteCore } from './markdownShared';

const SANITIZE_OPTS = { ADD_TAGS: ['math-inline'], ADD_ATTR: ['data-code', 'aria-label'] };

const MD_CACHE_MAX = 500;
const mdCache = new Map<string, string>();

function lruGet(key: string): string | undefined {
  const hit = mdCache.get(key);
  if (hit !== undefined) {
    mdCache.delete(key);
    mdCache.set(key, hit);
  }
  return hit;
}

function lruSet(key: string, value: string): void {
  if (mdCache.size >= MD_CACHE_MAX) {
    const first = mdCache.keys().next().value;
    if (first !== undefined) mdCache.delete(first);
  }
  mdCache.set(key, value);
}

// Streaming path: synchronous and cheap (no highlighting, no math).
export function renderMarkdownLite(text: string): string {
  if (!text) return '';
  return DOMPurify.sanitize(renderLiteCore(text), SANITIZE_OPTS);
}

/* ---------- worker client ---------- */

let worker: Worker | null = null;
let workerBroken = false;
let seq = 0;
const inflight = new Map<number, (rawHtml: string) => void>();

function ensureWorker(): Worker | null {
  if (workerBroken) return null;
  if (worker) return worker;
  try {
    worker = new Worker(new URL('./markdown.worker.ts', import.meta.url), { type: 'module' });
    worker.onmessage = (e: MessageEvent<{ id: number; html: string }>) => {
      const cb = inflight.get(e.data.id);
      if (cb) {
        inflight.delete(e.data.id);
        cb(e.data.html);
      }
    };
    worker.onerror = () => {
      // Fall back to lite rendering permanently; resolve anything in flight.
      workerBroken = true;
      const cbs = Array.from(inflight.values());
      inflight.clear();
      worker?.terminate();
      worker = null;
      for (const cb of cbs) cb('');
    };
  } catch {
    workerBroken = true;
    worker = null;
  }
  return worker;
}

export function renderMarkdownAsync(text: string): Promise<string> {
  if (!text) return Promise.resolve('');
  const hit = lruGet(text);
  if (hit !== undefined) return Promise.resolve(hit);
  const w = ensureWorker();
  if (!w) {
    const html = renderMarkdownLite(text);
    lruSet(text, html);
    return Promise.resolve(html);
  }
  return new Promise((resolve) => {
    const id = ++seq;
    inflight.set(id, (rawHtml) => {
      const clean = rawHtml
        ? DOMPurify.sanitize(rawHtml, SANITIZE_OPTS)
        : renderMarkdownLite(text); // worker died mid-flight
      lruSet(text, clean);
      resolve(clean);
    });
    w.postMessage({ id, text });
  });
}

// React hook: returns cached HTML synchronously when available, otherwise ''
// until the worker responds. Callers render into dangerouslySetInnerHTML.
export function useAsyncMarkdown(text: string): string {
  const [html, setHtml] = useState<string>(() => (text ? lruGet(text) ?? '' : ''));
  useEffect(() => {
    if (!text) { setHtml(''); return; }
    const hit = lruGet(text);
    if (hit !== undefined) { setHtml(hit); return; }
    let cancelled = false;
    renderMarkdownAsync(text).then((h) => { if (!cancelled) setHtml(h); });
    return () => { cancelled = true; };
  }, [text]);
  return html;
}

/* ---------- SVG / mermaid helpers (unchanged) ---------- */

export function sanitizeSVG(svg: string): string {
  const clean = DOMPurify.sanitize(svg, { USE_PROFILES: { svg: true, svgFilters: true }, ADD_TAGS: ['foreignObject'] });
  return normalizeMermaidFontSize(clean);
}

function normalizeMermaidFontSize(svg: string): string {
  return svg.replace(/\bstyle="([^"]*)"/g, (_m: string, styles: string) => {
    const cleaned = styles.replace(/\bfont-size:\s*(?!14px)[^;"]+(;?\s*)/g, '');
    return cleaned ? `style="${cleaned}"` : '';
  });
}

interface MermaidSegment {
  type: 'md' | 'mermaid';
  text: string;
}

export function splitMermaidBlocks(text: string): MermaidSegment[] {
  const re = /^```mermaid\s*\n([\s\S]*?)^```\s*$/gm;
  const segments: MermaidSegment[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) segments.push({ type: 'md', text: text.slice(last, m.index) });
    segments.push({ type: 'mermaid', text: m[1].trim() });
    last = m.index + m[0].length;
  }
  if (last < text.length) segments.push({ type: 'md', text: text.slice(last) });
  return segments.length ? segments : [{ type: 'md', text }];
}
