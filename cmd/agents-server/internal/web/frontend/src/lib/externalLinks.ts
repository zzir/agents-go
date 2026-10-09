// installExternalLinkOpener opens a plain click on a cross-origin link in a new
// tab (noopener) instead of navigating the app away (sanitized markdown carries no
// target); modifier clicks and links naming a target keep the browser's behavior.
export function installExternalLinkOpener(root: Document = document): () => void {
  const onClick = (e: MouseEvent) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = (e.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null;
    if (!a || a.target) return;
    let url: URL;
    try { url = new URL(a.getAttribute('href') || '', root.baseURI); } catch { return; }
    if ((url.protocol !== 'http:' && url.protocol !== 'https:') || url.origin === new URL(root.baseURI).origin) return;
    e.preventDefault();
    window.open(url.href, '_blank', 'noopener,noreferrer');
  };
  root.addEventListener('click', onClick);
  return () => root.removeEventListener('click', onClick);
}
