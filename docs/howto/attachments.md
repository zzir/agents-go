# Image input (attachments)

Paste images into the chat composer, or pick them from its **+** menu, and
the model sees them as native vision input. This is a **workbench** feature: the SDK itself is unchanged (a user message with
`input_image` parts was always valid input); what the server adds is storage,
upload, and the resolution of stored references at the model boundary
([workbench invariants 56–58](../explanation/workbench-invariants.md)).

Two switches gate it, both off by default:

1. **Attachment storage** (Settings → General → Attachment storage): an
   S3-compatible bucket the image bytes live in. Without it the upload
   endpoint answers 503 and the composer shows no image affordances.
2. **Vision** per agent (agent editor → Behavior): the claim that this
   agent's model accepts image input. A run carrying images on an agent
   without it fails its pre-flight with a config error.

## Configuring the bucket

The bucket must allow **anonymous public reads**: model providers fetch
attachment URLs without credentials, and so can anyone else holding a link —
the URLs are stable and unsigned by design ([decisions
§5.42](../explanation/decisions.md#542-image-attachments-live-in-an-s3-bucket-as-stable-public-urls)).
The section is one form — Save, Test, Clear. Save validates and stores the
whole section atomically; Save and Test both run an end-to-end probe (signed
upload, anonymous read through the public base URL, delete) and report the
failing stage; Clear removes the section and turns image input off.

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

Uploads never sent with a message are garbage-collected after 24 hours
(object first, then the row). Add a bucket lifecycle rule on the `attachments/`
prefix as a backstop — e.g. expire objects after 7 days on R2/S3 — the
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

Over the API:

```bash
curl -H "Authorization: Bearer $TOKEN" -F file=@shot.png http://localhost:9527/api/v1/attachments
# → {"id":"…","url":"https://…","mime":"image/png","size":…}

curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"input":"what is in this screenshot?","attachment_ids":["<id>"],"agent_config_id":"…"}' \
  http://localhost:9527/api/v1/sessions/<sid>/runs
```

`GET /api/v1/attachments/config` reports whether storage is configured and
the limits the client should apply (`{enabled, max_bytes, max_count,
downscale_px}`). An upload you change your mind about goes with
`DELETE /api/v1/attachments/<id>` — yours only, and only until a run has
accepted it (`409` after: it is part of session history).

The storage section is written the same way the form writes it — as one
value, admin-only:

```bash
# Test the values without storing them (204 = the bucket works; 400 names the failing stage)
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"endpoint":"https://<account>.r2.cloudflarestorage.com","region":"auto","bucket":"agents",
       "access_key_id":"…","secret_access_key":"…","public_base_url":"https://pub-….r2.dev","path_style":false}' \
  -X POST http://localhost:9527/api/v1/attachments/storage/test

# Save: the same probe first, then all seven keys in one transaction; answers the config
curl … -X PUT http://localhost:9527/api/v1/attachments/storage -d '{…the same body…}'

# Clear: an all-empty body removes the section and turns image input off
curl … -X PUT http://localhost:9527/api/v1/attachments/storage -d '{}'
```

A masked `secret_access_key` (`********`) keeps the stored secret, so the
other fields can change without retyping it; an empty `region` is `auto`.
The keys are `settings` rows, but `PUT /settings/:key` refuses them one at a
time — the section is only valid as a whole
([invariant 58](../explanation/workbench-invariants.md), and the key list in
the [configuration reference](../reference/configuration.md#runtime-settings)).

## What to know

- Entries store a reference (`agents-attachment:<id>`), resolved to the
  public URL only when a request leaves for the model. **Changing the bucket
  or public base URL without moving the objects breaks images already in
  history** — they degrade to an `[image unavailable]` placeholder, the
  conversation itself stays readable.
- Attachments ride chat messages only; task spawns, workflow steps and
  mid-run injections are text-only.
- Anthropic-backed agents work (the adapter translates `input_image` URLs);
  the `detail` hint is OpenAI-only and fixed at `auto`. The ChatGPT-login
  (Codex) backend accepts image input and downloads the URL server-side.
- If the bucket is down or unreachable from the provider's side, the run
  fails with the provider's download error — check the bucket before the
  model.
