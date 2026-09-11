// MAX_FRAME_BYTES is the server's inbound WebSocket frame limit (1 MiB): a
// larger frame closes the socket with 1009 instead of answering.
export const MAX_FRAME_BYTES = 1024 * 1024;

// frameTooLarge reports whether the envelope {type, payload}, as the socket
// sends it, would overflow the frame — JSON escaping (a quote, a newline)
// counts, not only the text.
export function frameTooLarge(type: string, payload: unknown): boolean {
  return new TextEncoder().encode(JSON.stringify({ type, payload })).length > MAX_FRAME_BYTES;
}
