import './terminal.css';
import '@xterm/xterm/css/xterm.css';
import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import { Terminal, type ITheme } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { Unicode11Addon } from '@xterm/addon-unicode11';
import { getToken } from '@/lib/api';
import { EV } from '@/lib/protocol';

// ANSI palettes matching GitHub's light/dark themes; Primer primitives expose
// no ANSI variables, so these are fixed per color mode while background /
// foreground track the live CSS variables.
const ANSI_LIGHT: Partial<ITheme> = {
  black: '#24292f', red: '#cf222e', green: '#116329', yellow: '#4d2d00',
  blue: '#0969da', magenta: '#8250df', cyan: '#1b7c83', white: '#6e7781',
  brightBlack: '#57606a', brightRed: '#a40e26', brightGreen: '#1a7f37',
  brightYellow: '#633c01', brightBlue: '#218bff', brightMagenta: '#a475f9',
  brightCyan: '#3192aa', brightWhite: '#8c959f',
};
const ANSI_DARK: Partial<ITheme> = {
  black: '#484f58', red: '#ff7b72', green: '#3fb950', yellow: '#d29922',
  blue: '#58a6ff', magenta: '#bc8cff', cyan: '#39c5cf', white: '#b1bac4',
  brightBlack: '#6e7681', brightRed: '#ffa198', brightGreen: '#56d364',
  brightYellow: '#e3b341', brightBlue: '#79c0ff', brightMagenta: '#d2a8ff',
  brightCyan: '#56d4dd', brightWhite: '#f0f6fc',
};

// xtermTheme derives the terminal theme from the live Primer CSS variables
// plus the fixed ANSI palette for the current color mode.
function xtermTheme(): ITheme {
  const style = getComputedStyle(document.documentElement);
  const v = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  const dark = document.documentElement.getAttribute('data-color-mode') === 'dark';
  return {
    background: v('--bgColor-default', dark ? '#0d1117' : '#ffffff'),
    foreground: v('--fgColor-default', dark ? '#e6edf3' : '#1f2328'),
    cursor: v('--fgColor-default', dark ? '#e6edf3' : '#1f2328'),
    selectionBackground: dark ? 'rgba(56,139,253,0.4)' : 'rgba(84,174,255,0.4)',
    // xterm 6 renders its own scrollbar; align it with the app-wide native
    // scrollbar colors (thumb: borderColor-muted, hover: bgColor-neutral-muted).
    scrollbarSliderBackground: v('--borderColor-muted', dark ? '#30363d' : '#d8dee4'),
    scrollbarSliderHoverBackground: v('--bgColor-neutral-muted', dark ? 'rgba(110,118,129,0.4)' : 'rgba(175,184,193,0.2)'),
    scrollbarSliderActiveBackground: v('--bgColor-neutral-muted', dark ? 'rgba(110,118,129,0.4)' : 'rgba(175,184,193,0.2)'),
    ...(dark ? ANSI_DARK : ANSI_LIGHT),
  };
}

export type TermStatus = 'connecting' | 'connected' | 'exited' | 'error';

// TerminalViewHandle is the imperative surface the panel uses to pull the
// active tab's selection when quoting it into the chat composer.
export interface TerminalViewHandle {
  getSelection(): string;
}

interface TerminalViewProps {
  sandboxId: string;
  // The project whose (sandbox, project) container this shell opens into —
  // required by terminal.open; a bound session's terminal follows its binding.
  projectId: string;
  // Hidden tabs stay mounted so their session (xterm buffer + WebSocket)
  // survives tab switches; only the active tab is displayed.
  hidden: boolean;
  onStatus?: (status: TermStatus) => void;
  onSelection?: (hasSelection: boolean) => void;
}

// TerminalView hosts one interactive sandbox terminal over /ws/terminal. The
// session's lifetime is the component's: unmounting (closing the tab) ends
// the shell.
export const TerminalView = forwardRef<TerminalViewHandle, TerminalViewProps>(function TerminalView(
  { sandboxId, projectId, hidden, onStatus, onSelection }: TerminalViewProps,
  ref,
) {
  const mountRef = useRef<HTMLDivElement>(null);
  // Keep the latest callbacks and visibility out of the session effect's deps:
  // a re-rendered parent or a tab switch must never tear down a live shell.
  const onStatusRef = useRef(onStatus);
  onStatusRef.current = onStatus;
  const onSelectionRef = useRef(onSelection);
  onSelectionRef.current = onSelection;
  const hiddenRef = useRef(hidden);
  hiddenRef.current = hidden;
  const termRef = useRef<Terminal | null>(null);
  const webglRef = useRef<WebglAddon | null>(null);

  useImperativeHandle(ref, () => ({
    getSelection: () => termRef.current?.getSelection() ?? '',
  }), []);

  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return;
    const setStatus = (s: TermStatus) => onStatusRef.current?.(s);
    setStatus('connecting');

    const term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      scrollback: 5000,
      cursorBlink: true,
      // Unicode11Addon drives the proposed `term.unicode` API; without this
      // flag loadAddon throws at mount.
      allowProposedApi: true,
      theme: xtermTheme(),
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    // Clickable URLs and correct emoji/wide-glyph widths.
    term.loadAddon(new WebLinksAddon());
    term.loadAddon(new Unicode11Addon());
    term.unicode.activeVersion = '11';
    term.open(mount);
    termRef.current = term;
    fit.fit();

    // Track Primer color-mode flips (ThemeProvider stamps <html data-color-mode>).
    const themeObserver = new MutationObserver(() => {
      term.options.theme = xtermTheme();
    });
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-color-mode'] });

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/ws/terminal`);
    ws.binaryType = 'arraybuffer';
    let ready = false;
    let exited = false;
    let disposed = false;

    // Sync the PTY to xterm's grid, tracking the last size actually sent —
    // comparing against that (not fit's before/after) closes the gap where
    // the grid was corrected while the socket wasn't ready yet.
    let sentCols = 0;
    let sentRows = 0;
    const syncSize = () => {
      fit.fit();
      if (!ready || ws.readyState !== WebSocket.OPEN) return;
      if (term.cols === sentCols && term.rows === sentRows) return;
      sentCols = term.cols;
      sentRows = term.rows;
      ws.send(JSON.stringify({ type: EV.terminalResize, payload: { cols: term.cols, rows: term.rows } }));
    };

    ws.onopen = () => {
      ws.send(JSON.stringify({ type: EV.auth, token: getToken() }));
    };
    ws.onmessage = (e: MessageEvent) => {
      if (typeof e.data !== 'string') {
        term.write(new Uint8Array(e.data as ArrayBuffer));
        return;
      }
      let env: { type: string; payload?: unknown };
      try {
        env = JSON.parse(e.data);
      } catch {
        return;
      }
      switch (env.type) {
        case EV.authOk:
          ws.send(JSON.stringify({
            type: EV.terminalOpen,
            payload: { sandbox_id: sandboxId, project_id: projectId, cols: term.cols, rows: term.rows },
          }));
          break;
        case EV.terminalReady:
          ready = true;
          setStatus('connected');
          // The open request carried whatever the pre-connect fit produced;
          // xterm's renderer may not have been measurable yet (a freshly
          // mounted tab), so recalibrate the PTY now that both ends are live.
          syncSize();
          if (!hiddenRef.current) term.focus();
          break;
        case EV.terminalError: {
          const msg = (env.payload as { message?: string })?.message || 'terminal error';
          term.writeln(`\x1b[31m${msg}\x1b[0m`);
          exited = true;
          setStatus('error');
          break;
        }
        case EV.terminalExit: {
          const code = (env.payload as { code?: number })?.code;
          term.writeln(`\r\n\x1b[2m[process exited${typeof code === 'number' && code >= 0 ? ` with code ${code}` : ''}]\x1b[0m`);
          exited = true;
          setStatus('exited');
          break;
        }
      }
    };
    ws.onclose = () => {
      if (disposed || exited) return;
      // A drop before/during the session is an error; after exit the server
      // closing the socket is the expected end of the stream.
      term.writeln('\r\n\x1b[2m[disconnected]\x1b[0m');
      setStatus('error');
    };

    const encoder = new TextEncoder();
    const dataSub = term.onData(d => {
      if (ready && ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(d));
    });
    const selectionSub = term.onSelectionChange(() => {
      onSelectionRef.current?.(term.hasSelection());
    });

    // Refit on any layout resize; propagate the new grid to the PTY. While
    // hidden (display:none) proposeDimensions is undefined and fit is a no-op;
    // becoming visible fires the observer again and refits.
    let resizeTimer: ReturnType<typeof setTimeout> | null = null;
    const resizeObserver = new ResizeObserver(() => {
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        resizeTimer = null;
        syncSize();
      }, 150);
    });
    resizeObserver.observe(mount);

    return () => {
      disposed = true;
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeObserver.disconnect();
      themeObserver.disconnect();
      dataSub.dispose();
      selectionSub.dispose();
      ws.close();
      termRef.current = null;
      webglRef.current = null;
      term.dispose(); // disposes loaded addons (fit, webgl) with it
    };
  }, [sandboxId, projectId]);

  // WebGL renderer as progressive enhancement, attached ONLY to the visible
  // tab: hugely faster on output floods, but it must not run on hidden
  // instances — a second live webgl addon corrupts the shared glyph-atlas
  // cell measurement (observed as a 2-column terminal), and browsers cap
  // concurrent WebGL contexts anyway. Hidden tabs fall back to the DOM
  // renderer, which is free while display:none. Also degrade gracefully:
  // no GPU / context loss just means DOM rendering, never a failure.
  useEffect(() => {
    const term = termRef.current;
    if (!term) return;
    if (hidden) {
      webglRef.current?.dispose();
      webglRef.current = null;
      return;
    }
    if (webglRef.current) return;
    try {
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => {
        webgl.dispose();
        if (webglRef.current === webgl) webglRef.current = null;
      });
      term.loadAddon(webgl);
      webglRef.current = webgl;
    } catch {
      webglRef.current = null;
    }
  }, [hidden, sandboxId]);

  return <div ref={mountRef} className="terminal-view" style={hidden ? { display: 'none' } : undefined} />;
});
