import type { DiffLine } from '@/lib/diff';
import './diff-lines.css';

// DiffLines renders a line diff: one row per line, its sign in a gutter.
export function DiffLines({ lines }: { lines: DiffLine[] }) {
  return lines.map((l, i) => (
    <div key={i} className={'diff-line diff-' + l.type}>
      <span className="diff-sign">{l.type === 'add' ? '+' : l.type === 'del' ? '-' : ' '}</span>
      {l.text}
    </div>
  ));
}
