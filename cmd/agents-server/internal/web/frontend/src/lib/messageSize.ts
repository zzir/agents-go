// MAX_MESSAGE_BYTES is the server's inbound WebSocket frame limit (1 MiB): a
// larger frame closes the socket with 1009 instead of answering.
export const MAX_MESSAGE_BYTES = 1024 * 1024;

// isTooLarge reports whether a prompt's UTF-8 form would overflow the frame.
export function isTooLarge(text: string): boolean {
  return new TextEncoder().encode(text).length > MAX_MESSAGE_BYTES;
}
