import { useMemo, useState } from 'react';
import { Button } from '@primer/react';
import { FileIcon } from '@primer/octicons-react';
import { parseToolContent, type ToolContentPart } from '@/lib/toolContent';
import { ZoomOverlay } from '@/features/chat/ZoomOverlay';

// ToolOutputBody renders a tool's output: a multimodal result part by part —
// text as text, an image as the image, a file as something to open or save —
// and anything else as the text it is.
export function ToolOutputBody({ output }: { output: string }) {
  const parts = useMemo(() => parseToolContent(output), [output]);
  if (!parts) return <pre>{output}</pre>;
  return (
    <div className="ToolCallCard-content">
      {parts.map((p, i) => <ContentPart key={i} part={p} />)}
    </div>
  );
}

function ContentPart({ part }: { part: ToolContentPart }) {
  switch (part.type) {
    case 'input_text':
      return part.text ? <pre>{part.text}</pre> : null;
    case 'input_image':
      return part.image_url
        ? <ToolImage src={part.image_url} />
        : <span className="ToolCallCard-file">image {part.file_id || ''}</span>;
    case 'input_file': {
      const name = part.filename || part.file_id || 'file';
      if (part.file_url) {
        return <Button as="a" href={part.file_url} target="_blank" rel="noopener noreferrer" size="small" leadingVisual={FileIcon}>{name}</Button>;
      }
      if (part.file_data) {
        return <Button as="a" href={'data:application/octet-stream;base64,' + part.file_data} download={name} size="small" leadingVisual={FileIcon}>{name}</Button>;
      }
      return <span className="ToolCallCard-file"><FileIcon size={14} /> {name}</span>;
    }
  }
}

// ToolImage is the picture at a size that fits the card, opening at its
// natural size on click.
function ToolImage({ src }: { src: string }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <img className="ToolCallCard-image" src={src} alt="tool result" onClick={() => setOpen(true)} />
      {open && (
        <ZoomOverlay onClose={() => setOpen(false)}>
          <img src={src} alt="tool result" />
        </ZoomOverlay>
      )}
    </>
  );
}
