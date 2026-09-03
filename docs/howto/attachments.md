# Image input (attachments)

Paste images into the chat composer, or pick them from its **+** menu, and the
model sees them as native vision input. This is a **workbench** feature: a user
message with `input_image` parts was always valid SDK input; what the server
adds is storage, upload, and the resolution of stored references at the model
boundary ([workbench invariants 56–58](../explanation/workbench-invariants.md)).

Two switches gate it, both off by default:

1. **Attachment storage** (Settings → General → Attachment storage): an
   S3-compatible bucket the image bytes live in. Without it the upload
   endpoint answers 503 and the composer shows no image affordances.
2. **Vision** per agent (agent editor → Behavior): the claim that this
   agent's model accepts image input. A run carrying images on an agent
   without it fails its pre-flight with a config error.

## Configuring the bucket

The bucket must allow **anonymous public reads** — the URLs are stable and
unsigned by design, so anyone holding a link can read the image
([decisions §5.42](../explanation/decisions.md#542-image-attachments-live-in-an-s3-bucket-as-stable-public-urls)).
The section is one form — Save, Test, Clear — and every non-empty save probes
the bucket end to end before storing, naming the stage that failed
([invariant 58](../explanation/workbench-invariants.md)).

| Setting | Cloudflare R2 | AWS S3 | MinIO |
|---|---|---|---|
| S3 endpoint | `https://<account>.r2.cloudflarestorage.com` | `https://s3.<region>.amazonaws.com` | `http(s)://<host>:9000` |
| S3 region | `auto` | the bucket's region | `auto` |
| Path-style addressing | off | off | **on** |
| Public base URL | the bucket's `r2.dev` URL or a custom domain | `https://<bucket>.s3.<region>.amazonaws.com` | `<endpoint>/<bucket>` |

- **R2**: enable public access on the bucket (or front it with a CDN domain).
- **AWS**: turn off "Block public access" and add a public-read bucket policy
  for `arn:aws:s3:::<bucket>/attachments/*`.
- **MinIO**: `mc anonymous set download <alias>/<bucket>` — and note the
  public base only works if the model provider can reach the host, so an
  internal-network MinIO cannot serve a cloud model.

Uploads never sent with a message are collected after a 24-hour grace, object
before row ([invariant 57](../explanation/workbench-invariants.md)). A bucket
lifecycle rule on the `attachments/` prefix is a reasonable backstop, but the
server's rows, not the bucket, are the source of truth for what is alive.

## Using it

With both switches on, png or jpeg images pasted into the composer or picked
through **+ → Image…** queue on a thumbnail strip (up to 8 per message,
10 MiB each; GIFs are refused — providers read one frame at best). With a
switch off the menu item is disabled and names the missing switch. There is
no drag-and-drop.
Images are downscaled in the browser to 1568px on the longest side before
uploading; the original file is discarded — the workbench stores what the
model sees.

Over the API — upload, then name the id on a run:

```bash
curl -H "Authorization: Bearer $TOKEN" -F file=@shot.png http://localhost:9527/api/v1/attachments
# → {"id":"…","url":"https://…","mime":"image/png","size":…}

curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"input":"what is in this screenshot?","attachment_ids":["<id>"],"agent_config_id":"…"}' \
  http://localhost:9527/api/v1/sessions/<sid>/runs
```

`GET /api/v1/attachments/config` reports whether storage is configured and the
limits a client should apply before uploading; `DELETE /api/v1/attachments/<id>`
drops an upload you changed your mind about. The refusals, the storage-section
write (`PUT /api/v1/attachments/storage` — one value, admin-only, a masked
`secret_access_key` keeping the stored one) and the key list are in
[the wire surface](../reference/protocol.md#attachments--apiv1attachments) and
the [configuration reference](../reference/configuration.md#runtime-settings).

## What to know

- Entries store a reference that only the model boundary expands
  ([invariant 56](../explanation/workbench-invariants.md)), so **changing the
  bucket or public base URL without moving the objects breaks images already
  in history** — they degrade to `[image unavailable]`, the conversation
  itself staying readable.
- Attachments ride chat messages only; task spawns, workflow steps and
  mid-run injections are text-only.
- Anthropic-backed agents work (the adapter translates `input_image` URLs);
  the `detail` hint is OpenAI-only and fixed at `auto`. The ChatGPT-login
  (Codex) backend accepts image input and downloads the URL server-side.
- If the bucket is down or unreachable from the provider's side, the run
  fails with the provider's download error — check the bucket before the
  model.
