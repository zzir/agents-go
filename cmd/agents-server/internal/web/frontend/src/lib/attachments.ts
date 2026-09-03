// Image attachments: the composer-side half. Uploads go through POST
// /attachments (multipart), after a canvas downscale to the server-announced
// longest-side target — the original file is discarded, the workbench stores
// what the model will see.
import { api } from '@/lib/api';

// AttachmentMeta is an uploaded image as every surface passes it around: the
// run request (ids), the optimistic bubble, run.started and stored entries.
export interface AttachmentMeta {
  id: string;
  url: string;
}

export interface AttachmentConfig {
  enabled: boolean;
  max_bytes: number;
  max_count: number;
  downscale_px: number;
}

// The config is a deployment fact; fetch once per page load and share. A
// failed fetch reads as disabled — the affordances hide rather than break.
let configPromise: Promise<AttachmentConfig> | null = null;
export function fetchAttachmentConfig(): Promise<AttachmentConfig> {
  configPromise ??= (api.attachments.config() as Promise<AttachmentConfig>).catch(() => {
    configPromise = null;
    return { enabled: false, max_bytes: 0, max_count: 0, downscale_px: 0 };
  });
  return configPromise;
}

// imageAffordance is the composer's "Image…" menu item: taken when the server
// stores attachments AND the picked agent has Vision on, otherwise disabled
// with the missing switch as its hint. A config still loading disables it
// without a hint.
export function imageAffordance(cfg: AttachmentConfig | null, vision: boolean): { enabled: boolean; hint: string } {
  if (!cfg) return { enabled: false, hint: '' };
  if (!cfg.enabled) return { enabled: false, hint: 'not configured on this server' };
  if (!vision) return { enabled: false, hint: 'enable Vision on the agent' };
  return { enabled: true, hint: '' };
}

// attachmentIdsEqual compares two bubbles' attachment sets — the second half
// of the user-message dedup key (content alone collapses two image-only
// messages).
export function attachmentIdsEqual(a?: AttachmentMeta[], b?: AttachmentMeta[]): boolean {
  const ai = (a ?? []).map(x => x.id);
  const bi = (b ?? []).map(x => x.id);
  return ai.length === bi.length && ai.every((id, i) => id === bi[i]);
}

// downscaleImage re-encodes file with its longest side capped at maxPx.
// PNG sources stay PNG (alpha survives), everything else becomes JPEG. A file
// already within the cap is uploaded as-is — no pointless re-encode.
export async function downscaleImage(file: File, maxPx: number): Promise<Blob> {
  const bmp = await createImageBitmap(file);
  try {
    const longest = Math.max(bmp.width, bmp.height);
    if (longest <= maxPx) return file;
    const scale = maxPx / longest;
    const w = Math.round(bmp.width * scale);
    const h = Math.round(bmp.height * scale);
    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    if (!ctx) return file;
    ctx.drawImage(bmp, 0, 0, w, h);
    const type = file.type === 'image/png' ? 'image/png' : 'image/jpeg';
    const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, type, 0.85));
    return blob ?? file;
  } finally {
    bmp.close();
  }
}

// isImageFile admits what the server admits. GIFs are refused on purpose:
// providers read a single frame at best, which surprises worse than a clear
// "convert to a still image".
export function isImageFile(f: File): boolean {
  return f.type === 'image/png' || f.type === 'image/jpeg';
}

// uploadAttachment downscales and uploads one image, returning the stored
// attachment.
export async function uploadAttachment(file: File, cfg: AttachmentConfig): Promise<AttachmentMeta> {
  const blob = await downscaleImage(file, cfg.downscale_px || 1568);
  if (cfg.max_bytes && blob.size > cfg.max_bytes) {
    throw new Error('image is too large even after downscaling');
  }
  return api.attachments.upload(blob, file.name) as Promise<AttachmentMeta>;
}
