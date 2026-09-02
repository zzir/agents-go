import { useState } from 'react';

// The request/response items a generation span carries, as the trace panel
// and the replay dialog both list them: a tag, a one-line preview, and the
// full text one click away.

export type PayloadRecord = Record<string, unknown>;

export function itemTag(item: PayloadRecord): string {
  if (typeof item.role === 'string' && item.role) return item.role;
  if (typeof item.type === 'string' && item.type) return item.type;
  return 'item';
}

// One-line summary text for a request/response item; falls back to JSON.
export function itemText(item: PayloadRecord): string {
  const content = item.content;
  if (typeof content === 'string' && content) return content;
  if (Array.isArray(content)) {
    // Refusal parts carry their text in `refusal`, not `text` — without
    // this, an Anthropic refusal in the trace renders as raw JSON.
    const texts = content
      .map(p => (p && typeof p === 'object' ? ((p as PayloadRecord).text ?? (p as PayloadRecord).refusal) : null))
      .filter((t): t is string => typeof t === 'string' && t !== '');
    if (texts.length > 0) return texts.join('\n');
  }
  if (item.type === 'function_call') {
    return String(item.name || '') + '(' + String(item.arguments || '') + ')';
  }
  if (item.type === 'function_call_output') {
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

// tagClass maps a payload tag to its Primer Label color variant so roles are
// distinguishable at a glance.
function tagClass(tag: string): string {
  const t = tag.split(' ')[0];
  if (t === 'user') return ' trace-ev-tag-user';
  if (t === 'assistant') return ' trace-ev-tag-assistant';
  if (t === 'system') return ' trace-ev-tag-system';
  if (t === 'function_call' || t === 'function_call_output' || t === 'tools' || t === 'input' || t === 'output') return ' trace-ev-tag-fn';
  return '';
}

// A single request/response entry: tag + one-line preview, expandable to the
// full text (or full item JSON when there is no plain text).
export function PayloadItem({ tag, text, full }: { tag: string; text: string; full: string }) {
  const [open, setOpen] = useState(false);
  const toggle = () => setOpen(o => !o);
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
        <span className="trace-payload-preview">{text.length > 120 ? text.slice(0, 120) + '…' : text}</span>
      </div>
      {open && <pre className="trace-span-data trace-payload-full">{full}</pre>}
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
export function ResponseItems({ items, prefix }: { items: PayloadRecord[]; prefix: string }) {
  return (
    <>
      {items.map((item, i) => (
        <PayloadItem key={prefix + i} tag={itemTag(item)} text={itemText(item)} full={itemText(item) === JSON.stringify(item) ? JSON.stringify(item, null, 2) : itemText(item)} />
      ))}
    </>
  );
}
