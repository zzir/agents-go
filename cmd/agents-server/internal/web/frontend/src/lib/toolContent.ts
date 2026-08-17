// A multimodal tool result displays as the Responses content list the model
// received (SDK spec §2.7b): input_text / input_image / input_file parts. This
// reads one back; anything else — a plain string, JSON of another shape — is
// text and renders as it always did.

export type ToolContentPart =
  | { type: 'input_text'; text: string }
  | { type: 'input_image'; image_url?: string; file_id?: string; detail?: string }
  | { type: 'input_file'; file_data?: string; file_url?: string; file_id?: string; filename?: string };

const PART_TYPES = new Set(['input_text', 'input_image', 'input_file']);

export function parseToolContent(output: string): ToolContentPart[] | null {
  const s = output.trimStart();
  if (!s.startsWith('[')) return null;
  let parsed: unknown;
  try { parsed = JSON.parse(s); } catch { return null; }
  if (!Array.isArray(parsed) || parsed.length === 0) return null;
  const parts: ToolContentPart[] = [];
  for (const p of parsed) {
    if (!p || typeof p !== 'object' || !PART_TYPES.has((p as { type?: unknown }).type as string)) return null;
    parts.push(p as ToolContentPart);
  }
  return parts;
}
