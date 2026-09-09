import { describe, expect, it } from 'vitest';
import { itemImages, itemText, payloadEntry, toolOutputEntry } from '@/features/chat/TracePayload';

const atts = [{ id: 'att1', url: 'https://cdn.example/a.png' }];
const withImage = { type: 'message', role: 'user', content: [
  { type: 'input_text', text: 'what is this' },
  { type: 'input_image', image_url: 'agents-attachment:att1', detail: 'auto' },
] };
const imageOnly = { type: 'message', role: 'user', content: [
  { type: 'input_image', image_url: 'agents-attachment:att1' },
  { type: 'input_image', image_url: 'agents-attachment:gone' },
] };
const toolResult = JSON.stringify([{ type: 'input_text', text: 'found it' }, { type: 'input_image', image_url: 'data:image/png;base64,AAAA' }]);

describe('itemText', () => {
  it('counts the pictures an item carries instead of dropping them', () => {
    expect(itemText(withImage)).toBe('what is this\n[image]');
    expect(itemText(imageOnly)).toBe('[2 images]');
    expect(itemText({ type: 'function_call_output', call_id: 'c1', output: toolResult })).toBe('found it\n[image]');
    expect(itemText({ type: 'function_call_output', call_id: 'c1', output: 'plain' })).toBe('plain');
  });
});

describe('itemImages', () => {
  it('resolves a stored attachment through the span\'s list and passes other URLs through', () => {
    expect(itemImages(withImage, atts)).toEqual([{ url: 'https://cdn.example/a.png' }]);
    // A row that is gone, or a span without its list: the picture is named, not shown.
    expect(itemImages(imageOnly, atts)).toEqual([{ url: 'https://cdn.example/a.png' }, { url: undefined }]);
    expect(itemImages(withImage)).toEqual([{ url: undefined }]);
    expect(itemImages({ type: 'function_call_output', call_id: 'c1', output: toolResult })).toEqual([{ url: 'data:image/png;base64,AAAA' }]);
    expect(itemImages({ role: 'user', content: 'plain' })).toEqual([]);
  });
});

describe('payloadEntry', () => {
  it('leaves nothing to expand behind pictures that are all there is', () => {
    expect(payloadEntry(imageOnly, atts).full).toBe('');
    expect(payloadEntry(withImage, atts)).toMatchObject({ tag: 'user', text: 'what is this\n[image]', full: 'what is this\n[image]' });
    const bare = { type: 'reasoning', summary: [] };
    expect(payloadEntry(bare).full).toBe(JSON.stringify(bare, null, 2));
  });
});

describe('toolOutputEntry', () => {
  it('reads a multimodal result as text and pictures, anything else as text', () => {
    expect(toolOutputEntry(toolResult)).toEqual({ text: 'found it\n[image]', images: [{ url: 'data:image/png;base64,AAAA' }] });
    expect(toolOutputEntry('{"ok":true}')).toEqual({ text: '{"ok":true}', images: [] });
  });
});
