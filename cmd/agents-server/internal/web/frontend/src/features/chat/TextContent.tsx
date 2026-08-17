import { useMemo } from 'react';
import { useAsyncMarkdown, splitMermaidBlocks } from '@/lib/markdown';
import { MermaidBlock } from '@/features/chat/MermaidBlock';

interface MermaidSegment {
  type: 'md' | 'mermaid';
  text: string;
}

function MdSegment({ text }: { text: string }) {
  const html = useAsyncMarkdown(text);
  return <div dangerouslySetInnerHTML={{ __html: html }} />;
}

// One settled assistant text: markdown through the worker pipeline, with each
// fenced ```mermaid``` block rendered as a diagram in place.
export function TextContent({ content }: { content: string }) {
  const segments: MermaidSegment[] = useMemo(() => splitMermaidBlocks(content), [content]);
  return (
    <div className="turn-text markdown-body">
      {segments.map((seg, i) =>
        seg.type === 'mermaid'
          ? <MermaidBlock key={`m${i}`} source={seg.text} />
          : <MdSegment key={`t${i}`} text={seg.text} />
      )}
    </div>
  );
}
