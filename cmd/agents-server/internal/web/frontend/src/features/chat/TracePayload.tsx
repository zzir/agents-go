import { useState } from 'react';
import { ImageIcon } from '@primer/octicons-react';
import { ZoomOverlay } from '@/features/chat/ZoomOverlay';
import { parseToolContent } from '@/lib/toolContent';
import type { AttachmentMeta } from '@/lib/attachments';

// The request/response items a generation span carries, as the trace panel
// and the replay dialog both list them: a tag, a one-line preview, the images
// the item carries, and the full text one click away.

export type PayloadRecord = Record<string, unknown>;

// PayloadImage is one picture an item carries, resolved to a URL the browser
// can show; none when the attachment row is gone or storage is off.
export interface PayloadImage { url?: string }

// The stored form of an image attachment; the span's attachments say what it
// resolves to today (workbench invariant 70).
const ATTACHMENT_SCHEME = 'agents-attachment:';

export function itemTag(item: PayloadRecord): string {
  if (typeof item.role === 'string' && item.role) return item.role;
  if (typeof item.type === 'string' && item.type) return item.type;
  return 'item';
}

// outputParts is a function_call_output's multimodal result as content parts:
// the Responses content list, sent as a JSON string or as the list itself.
function outputParts(item: PayloadRecord): PayloadRecord[] | null {
  const o = item.output;
  if (typeof o === 'string') return parseToolContent(o);
  return Array.isArray(o) ? parseToolContent(JSON.stringify(o)) : null;
}

// itemParts is the content list an item carries: a message's parts, a tool
// result's, or none for a string content.
function itemParts(item: PayloadRecord): PayloadRecord[] {
  if (Array.isArray(item.content)) return payloadItems(item.content);
  if (item.type === 'function_call_output') return outputParts(item) || [];
  return [];
}

function imageMarker(n: number): string {
  return n === 1 ? '[image]' : '[' + n + ' images]';
}

// partsText is the text of a content list, its images counted at the end.
// Refusal parts carry their text in `refusal`, not `text` — without this, an
// Anthropic refusal in the trace renders as raw JSON.
function partsText(parts: PayloadRecord[]): string {
  const texts = parts
    .map(p => p.text ?? p.refusal)
    .filter((t): t is string => typeof t === 'string' && t !== '');
  const images = parts.filter(p => p.type === 'input_image').length;
  if (images > 0) texts.push(imageMarker(images));
  return texts.join('\n');
}

// One-line summary text for a request/response item; falls back to JSON.
export function itemText(item: PayloadRecord): string {
  const content = item.content;
  if (typeof content === 'string' && content) return content;
  if (Array.isArray(content)) {
    const s = partsText(payloadItems(content));
    if (s) return s;
  }
  if (item.type === 'function_call') {
    return String(item.name || '') + '(' + String(item.arguments || '') + ')';
  }
  if (item.type === 'function_call_output') {
    const parts = outputParts(item);
    if (parts) return partsText(parts);
    const o = item.output;
    return typeof o === 'string' ? o : JSON.stringify(o);
  }
  if (Array.isArray(item.summary)) {
    const s = item.summary
      .map(p => (p && typeof p === 'object' ? (p as PayloadRecord).text : null))
      .filter((t): t is string => typeof t === 'string' && t !== '')
      .join('\n');
    if (s) return s;
  }
  return JSON.stringify(item);
}

// resolveImage is the URL an image part shows: a stored attachment through
// the span's attachments, anything else (a data: or https: URL) as it is.
function resolveImage(url: string, attachments?: AttachmentMeta[]): PayloadImage {
  if (!url.startsWith(ATTACHMENT_SCHEME)) return { url: url || undefined };
  const id = url.slice(ATTACHMENT_SCHEME.length);
  return { url: attachments?.find(a => a.id === id)?.url || undefined };
}

function partImages(parts: PayloadRecord[], attachments?: AttachmentMeta[]): PayloadImage[] {
  return parts
    .filter(p => p.type === 'input_image')
    .map(p => resolveImage(typeof p.image_url === 'string' ? p.image_url : '', attachments));
}

// itemImages lists the pictures an item carries: a user message's, or a tool
// result's.
export function itemImages(item: PayloadRecord, attachments?: AttachmentMeta[]): PayloadImage[] {
  return partImages(itemParts(item), attachments);
}

// payloadEntry is what a PayloadItem shows for an item: tag, preview, the
// images it carries, and the text behind the click — the item's JSON when it
// has no text of its own, nothing when the images are all there is.
export function payloadEntry(item: PayloadRecord, attachments?: AttachmentMeta[]): { tag: string; text: string; full: string; images: PayloadImage[] } {
  const text = itemText(item);
  const images = itemImages(item, attachments);
  let full = text === JSON.stringify(item) ? JSON.stringify(item, null, 2) : text;
  if (images.length > 0 && text === imageMarker(images.length)) full = '';
  return { tag: itemTag(item), text, full, images };
}

// toolOutputEntry reads a function span's stringified result: a multimodal
// content list becomes its text and pictures, anything else stays text.
export function toolOutputEntry(output: string): { text: string; images: PayloadImage[] } {
  const parts = parseToolContent(output);
  if (!parts) return { text: output, images: [] };
  return { text: partsText(parts), images: partImages(parts) };
}

// tagClass maps a payload tag to its Primer Label color variant so roles are
// distinguishable at a glance.
function tagClass(tag: string): string {
  const t = tag.split(' ')[0];
  if (t === 'user') return ' trace-ev-tag-user';
  if (t === 'assistant' || t === 'partial') return ' trace-ev-tag-assistant';
  if (t === 'system' || t === 'prompt') return ' trace-ev-tag-system';
  if (t === 'function_call' || t === 'function_call_output' || t === 'tools' || t === 'input' || t === 'output') return ' trace-ev-tag-fn';
  return '';
}

// PayloadThumbs is the strip of an item's pictures on its preview line.
function PayloadThumbs({ images }: { images: PayloadImage[] }) {
  return (
    <span className="trace-payload-thumbs">
      {images.map((im, i) => im.url
        ? <img key={i} className="trace-payload-thumb" src={im.url} alt="" loading="lazy" />
        : <span key={i} className="trace-payload-thumb trace-payload-thumb-missing" title="image unavailable"><ImageIcon size={12} /></span>)}
    </span>
  );
}

// PayloadImages shows an open item's pictures at a size that fits the panel,
// each opening at its natural size on click.
function PayloadImages({ images }: { images: PayloadImage[] }) {
  const [zoomed, setZoomed] = useState<string | null>(null);
  return (
    <div className="trace-payload-images">
      {images.map((im, i) => {
        const url = im.url;
        return url
          ? <img key={i} className="trace-payload-image" src={url} alt="" loading="lazy" onClick={() => setZoomed(url)} />
          : <span key={i} className="trace-payload-image-missing"><ImageIcon size={14} /> image unavailable</span>;
      })}
      {zoomed && (
        <ZoomOverlay onClose={() => setZoomed(null)}>
          <img src={zoomed} alt="" style={{ maxWidth: '90vw', maxHeight: '90vh' }} />
        </ZoomOverlay>
      )}
    </div>
  );
}

// A single request/response entry: tag + one-line preview (thumbnails first
// when it carries pictures), expandable to the pictures and the full text (or
// the full item JSON when there is no plain text).
export function PayloadItem({ tag, text, full, images }: { tag: string; text: string; full: string; images?: PayloadImage[] }) {
  const [open, setOpen] = useState(false);
  const toggle = () => setOpen(o => !o);
  const pics = images && images.length > 0 ? images : null;
  return (
    <div className="trace-payload-item">
      <div
        className="trace-payload-line"
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } }}
      >
        <span className={'trace-ev-tag' + tagClass(tag)} title={tag}>{tag}</span>
        <span className="trace-payload-preview">
          {pics && <PayloadThumbs images={pics} />}
          {text.length > 120 ? text.slice(0, 120) + '…' : text}
        </span>
      </div>
      {open && pics && <PayloadImages images={pics} />}
      {open && full && <pre className="trace-span-data trace-payload-full">{full}</pre>}
    </div>
  );
}

export function payloadItems(value: unknown): PayloadRecord[] {
  return Array.isArray(value) ? value.filter((x): x is PayloadRecord => !!x && typeof x === 'object') : [];
}

// prettyMaybeJSON pretty-prints a value that may hold a JSON string.
export function prettyMaybeJSON(v: unknown): string {
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// ResponseItems lists response items with the shared tag + preview +
// expandable-full treatment.
export function ResponseItems({ items, prefix, attachments }: { items: PayloadRecord[]; prefix: string; attachments?: AttachmentMeta[] }) {
  return (
    <>
      {items.map((item, i) => <PayloadItem key={prefix + i} {...payloadEntry(item, attachments)} />)}
    </>
  );
}
