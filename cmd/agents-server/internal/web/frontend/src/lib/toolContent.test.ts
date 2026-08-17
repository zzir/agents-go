import { describe, it, expect } from 'vitest';
import { parseToolContent } from '@/lib/toolContent';

describe('parseToolContent', () => {
  it('reads the Responses content list a multimodal result displays as', () => {
    const out = '[{"text":"shot","type":"input_text"},{"image_url":"data:image/png;base64,AAAA","type":"input_image"},{"file_data":"QUJD","filename":"a.pdf","type":"input_file"}]';
    expect(parseToolContent(out)).toEqual([
      { text: 'shot', type: 'input_text' },
      { image_url: 'data:image/png;base64,AAAA', type: 'input_image' },
      { file_data: 'QUJD', filename: 'a.pdf', type: 'input_file' },
    ]);
  });

  it('leaves everything else as text', () => {
    expect(parseToolContent('plain result')).toBeNull();
    expect(parseToolContent('{"ok":true}')).toBeNull();
    expect(parseToolContent('[1,2,3]')).toBeNull(); // a JSON list that is not content
    expect(parseToolContent('[{"type":"output_text","text":"x"}]')).toBeNull(); // not an input part
    expect(parseToolContent('[]')).toBeNull();
    expect(parseToolContent('[{"text":"x","type":"input_text"}, "stray"]')).toBeNull(); // one bad part spoils the list
  });
});
