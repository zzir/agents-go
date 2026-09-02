import { useState, memo } from 'react';
import { ChevronRightIcon } from '@primer/octicons-react';
import { useAsyncMarkdown } from '@/lib/markdown';

// compactTokens renders an estimate the way an estimate should read: two
// significant figures and a k, never a precise-looking count. CharEstimator is
// a character ratio, not a tokenizer.
function compactTokens(n: number): string {
  return n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k' : String(n);
}

// CompactionCard is an inline marker where a pass happened: the history it
// folded renders in place ABOVE it (the transcript is decoupled from the
// model's context — see the Context panel for what the model still reads), so
// the card carries only the shrink figures and, one expand away, the summary
// that now stands in for that history in the model's view.
interface CompactionCardProps {
  content?: string;
  tokensBefore?: number;
  tokensAfter?: number;
}

export const CompactionCard = memo(function CompactionCard(
  { content, tokensBefore, tokensAfter }: CompactionCardProps,
) {
  const [expanded, setExpanded] = useState(false);
  const summaryText = (content || '').replace(/^\[Conversation Summary\]\s*/, '');
  const summaryHtml = useAsyncMarkdown(expanded ? summaryText : '');
  const shrank = tokensBefore && tokensAfter && tokensBefore > tokensAfter;

  return (
    <div className="compaction-card">
      <div
        className={'compaction-card-toggle' + (expanded ? ' expanded' : '')}
        onClick={() => setExpanded(!expanded)}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>Compaction</span>
        {shrank && (
          <span className="compaction-card-savings">
            ~{compactTokens(tokensBefore)} → ~{compactTokens(tokensAfter)} tokens
          </span>
        )}
      </div>
      {expanded && (
        <div className="compaction-card-body">
          <div className="compaction-card-note">
            The history above stays in full — the model now reads this summary in its place.
          </div>
          {summaryText && <div className="markdown-body" dangerouslySetInnerHTML={{ __html: summaryHtml }} />}
        </div>
      )}
    </div>
  );
});
