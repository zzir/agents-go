/* The headers JSON field's codec, shared by the sandbox and MCP forms: a stored
   map renders as JSON text, and the text packs back into a map — or throws the
   message the save toast shows. */
export function headersToText(h?: Record<string, string> | null): string {
  return h && Object.keys(h).length > 0 ? JSON.stringify(h) : '';
}

export function parseHeadersText(text: string): Record<string, string> | undefined {
  const raw = text.trim();
  if (!raw) return undefined;
  let parsed: unknown;
  try { parsed = JSON.parse(raw); }
  catch { throw new Error('Headers is not valid JSON — fix or clear it before saving'); }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Headers must be a JSON object, e.g. {"Authorization": "Bearer <token>"}');
  const entries = Object.entries(parsed as Record<string, unknown>);
  if (entries.some(([k, v]) => !k.trim() || typeof v !== 'string' || v === '')) throw new Error('Every header needs a name and a string value');
  return entries.length > 0 ? (parsed as Record<string, string>) : undefined;
}
