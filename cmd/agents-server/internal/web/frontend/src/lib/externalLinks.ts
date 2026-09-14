// installExternalLinkOpener makes a plain click on a link to another origin
// open a new tab (noopener) instead of navigating the app away: rendered
// markdown carries no target (the sanitizer drops one), and the page's state
// would not survive the navigation. Modifier clicks and links naming their
// own target keep the browser's behavior. Returns the uninstall.
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
