import { useState, useEffect, useCallback } from 'react';
import { CopyIcon, CheckIcon, CodeIcon, EyeIcon } from '@primer/octicons-react';
import { sanitizeSVG } from '@/lib/markdown';
import { useCopy } from '@/lib/hooks';
import { ZoomOverlay } from '@/features/chat/ZoomOverlay';

/* ---------- mermaid helpers ---------- */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let mermaidMod: any = null;
let mermaidTheme: string | null = null;
let mermaidIdSeq = 0;

function getColorModeEl(): Element {
  return document.querySelector('[data-color-mode]') || document.documentElement;
}

function getColorMode(): string {
  return getColorModeEl().getAttribute('data-color-mode') || 'light';
}

function primerThemeVars(): Record<string, string> {
  const el = document.querySelector('.app-layout') || document.documentElement;
  const s = getComputedStyle(el);
  const v = (n: string) => s.getPropertyValue(n).trim();
  return {
    fontFamily: v('--fontStack-sansSerif'),
    fontSize: '14px',

    primaryColor: v('--bgColor-muted'),
    primaryBorderColor: v('--borderColor-default'),
    primaryTextColor: v('--fgColor-default'),
    secondaryColor: v('--bgColor-neutral-muted'),
    secondaryBorderColor: v('--borderColor-default'),
    secondaryTextColor: v('--fgColor-default'),
    tertiaryColor: v('--bgColor-success-muted'),
    tertiaryBorderColor: v('--borderColor-default'),
    tertiaryTextColor: v('--fgColor-default'),

    lineColor: v('--fgColor-muted'),
    textColor: v('--fgColor-default'),
    mainBkg: v('--bgColor-muted'),
    nodeBorder: v('--borderColor-default'),
    nodeTextColor: v('--fgColor-default'),
    clusterBkg: v('--bgColor-default'),
    clusterBorder: v('--borderColor-muted'),
    titleColor: v('--fgColor-default'),
    edgeLabelBackground: v('--bgColor-default'),

    actorBkg: v('--bgColor-muted'),
    actorBorder: v('--borderColor-default'),
    actorTextColor: v('--fgColor-default'),
    signalColor: v('--fgColor-default'),
    signalTextColor: v('--fgColor-default'),
    labelBoxBkgColor: v('--bgColor-muted'),
    labelBoxBorderColor: v('--borderColor-default'),
    labelTextColor: v('--fgColor-default'),
    noteBkgColor: v('--bgColor-attention-muted'),
    noteBorderColor: v('--borderColor-default'),
    noteTextColor: v('--fgColor-default'),
    activationBorderColor: v('--borderColor-default'),
    activationBkgColor: v('--bgColor-muted'),
  };
}

async function ensureMermaid() {
  if (!mermaidMod) {
    mermaidMod = (await import('mermaid')).default;
  }
  const cur = getColorMode();
  if (mermaidTheme !== cur) {
    mermaidTheme = cur;
    mermaidMod.initialize({
      startOnLoad: false,
      theme: 'base',
      themeVariables: primerThemeVars(),
      // Labels as SVG text, not HTML in a foreignObject: the sanitizer strips
      // the HTML wrappers (a foreignObject is no HTML integration point for
      // DOMPurify), which left long labels overflowing and off-center.
      htmlLabels: false,
      flowchart: { useMaxWidth: false },
      sequence: { useMaxWidth: false },
      gantt: { useMaxWidth: false },
      journey: { useMaxWidth: false },
      class: { useMaxWidth: false },
      state: { useMaxWidth: false },
      er: { useMaxWidth: false },
      pie: { useMaxWidth: false },
      git: { useMaxWidth: false },
    });
  }
  return mermaidMod;
}

const MERMAID_CACHE_MAX = 100;
const mermaidCache = new Map<string, string>();
function mermaidCacheSet(key: string, value: string) {
  if (mermaidCache.size >= MERMAID_CACHE_MAX) {
    const first = mermaidCache.keys().next().value;
    if (first !== undefined) mermaidCache.delete(first);
  }
  mermaidCache.set(key, value);
}

/* ---------- SVG viewer overlay ---------- */

function SvgOverlay({ svg, onClose }: { svg: string; onClose: () => void }) {
  return (
    <ZoomOverlay onClose={onClose}>
      <div dangerouslySetInnerHTML={{ __html: svg }} />
    </ZoomOverlay>
  );
}

/* ---------- rendering hook ---------- */

// useMermaidSvg renders a mermaid source to sanitized SVG, cached per source
// and color mode, re-rendered when the app's color mode flips. svg is null
// until the first render lands; failed reports a source mermaid rejected.
export function useMermaidSvg(source: string): { svg: string | null; failed: boolean } {
  const [colorMode, setColorMode] = useState(getColorMode);
  const cacheKey = source + '\0' + colorMode;
  const cached = mermaidCache.get(cacheKey);
  const [svg, setSvg] = useState<string | null>(() => cached || null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    const target = getColorModeEl();
    const obs = new MutationObserver(() => setColorMode(getColorMode()));
    obs.observe(target, { attributes: true, attributeFilter: ['data-color-mode'] });
    return () => obs.disconnect();
  }, []);

  useEffect(() => {
    const c = mermaidCache.get(cacheKey);
    if (c) { setSvg(c); setFailed(false); return; }
    let cancelled = false;
    setFailed(false);
    (async () => {
      const id = `m${++mermaidIdSeq}`;
      try {
        const mermaid = await ensureMermaid();
        // `source` is the raw fenced ```mermaid``` body from the un-escaped
        // markdown — it must be fed to mermaid verbatim. Decoding entities here
        // corrupted diagrams that legitimately contain "&", "<" or ">".
        const { svg: rendered } = await mermaid.render(id, source);
        if (!cancelled) {
          const safe = sanitizeSVG(rendered);
          mermaidCacheSet(cacheKey, safe);
          setSvg(safe);
        }
      } catch {
        // mermaid leaves its scratch container ("d" + id) behind on failure,
        // and may leave the svg itself.
        document.getElementById('d' + id)?.remove();
        document.getElementById(id)?.remove();
        if (!cancelled) setFailed(true);
      }
    })();
    return () => { cancelled = true; };
  }, [source, cacheKey]);

  return { svg, failed };
}

/* ---------- MermaidBlock ---------- */

export function MermaidBlock({ source }: { source: string }) {
  const { svg, failed } = useMermaidSvg(source);
  const [viewerOpen, setViewerOpen] = useState(false);
  const [mode, setMode] = useState<'code' | 'svg'>('svg');
  const { copied, copy } = useCopy();
  const handleCopy = useCallback(() => copy(source), [source, copy]);

  const showSvg = mode === 'svg' && svg && !failed;

  return (
    <div className="code-block-wrapper">
      <div className="code-block-actions">
        {svg && !failed && (
          <button
            className="btn-octicon btn-toggle-mermaid"
            aria-label={mode === 'svg' ? 'Show code' : 'Show diagram'}
            onClick={() => setMode(m => m === 'svg' ? 'code' : 'svg')}
          >
            {mode === 'svg' ? <CodeIcon size={16} /> : <EyeIcon size={16} />}
          </button>
        )}
        <button
          className={'btn-octicon btn-copy-react' + (copied ? ' copied' : '')}
          aria-label={copied ? 'Copied!' : 'Copy'}
          onClick={handleCopy}
        >
          {copied ? <CheckIcon size={16} /> : <CopyIcon size={16} />}
        </button>
      </div>
      {showSvg ? (
        <>
          <div className="mermaid-preview" onClick={() => setViewerOpen(true)} dangerouslySetInnerHTML={{ __html: svg }} />
          {viewerOpen && <SvgOverlay svg={svg} onClose={() => setViewerOpen(false)} />}
        </>
      ) : (
        <pre className="hljs-code-block"><code className="hljs">{source}</code></pre>
      )}
    </div>
  );
}
