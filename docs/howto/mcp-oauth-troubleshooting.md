# MCP OAuth: a server stuck at "authorizing"

For the workbench: an MCP server's connect button opened the popup, and the
row never leaves `authorizing`. The interactive flow logs each step, so the
server log (`--log-level info`) tells the two failures apart by which line is
missing. What each status means is in
[the wire surface](../reference/protocol.md#mcp-servers--apiv1mcp-servers).

## Read the log

A completing login writes three lines in order:

1. `authorization URL issued` — carries the exact `redirect_uri` the
   authorization server must send the browser back to.
2. `callback: authorization code delivered`.
3. `interactive connect established`.

Find which one is missing.

## No callback line at all

Only the panel's `GET /mcp-servers` poll repeats: the browser never reached
the callback. Two causes:

- The authorization server rejected the `redirect_uri` — a pre-registered
  `oauth_client_id` whose allowed callback does not list this exact path.
  Register the path the first log line names.
- The browser cannot reach the origin the `redirect_uri` names. The server
  builds it from `--base-url` when set, otherwise from the direct request's
  scheme and host — forwarding headers (`Forwarded`, `X-Forwarded-*`) are
  never consulted — so behind a reverse proxy without `--base-url` the URI
  names the backend, not what the browser loaded. Set `--base-url`
  ([deploying](workbench-deploy.md#deployment)).

A callback that arrives but cannot be matched logs
`callback: could not deliver authorization code` with the reason.

## `code delivered`, then `ended without connecting`

With `authorization completed but was not accepted`: the browser round-trip
worked, but the authorization did not yield a working session, so the SDK
re-authorized mid-connect — and the interactive park is single-shot (one
popup, no second one to service), so the attempt fails fast rather than
hanging until the five-minute timeout. Read the row's `has_oauth_token`:

- **Still `false`** — the SDK rejected the authorization response before any
  token exchange. Typically the AS metadata is inconsistent with the
  authorize redirect (RFC 9207): `iss` arrives but the metadata does not
  advertise `authorization_response_iss_parameter_supported`, or the
  advertised `issuer` differs from the `iss` received — common when a gateway
  proxies a real IdP's endpoints under its own issuer. This is a server-side
  metadata bug: its metadata must present the issuer exactly as the IdP
  responds, or its protected-resource metadata should point
  `authorization_servers` at the IdP directly.
- **`true`** — a token was issued and persisted, but the resource server
  rejected it. Set the server's `oauth_scopes` to what it requires, or confirm
  the token's audience is this MCP endpoint.

## Start over

Calling connect again while `authorizing` is safe and intended: it supersedes
the stale attempt and returns a fresh authorize URL. To re-authorize with a
different account, drop the saved grant first — "Clear auth" in the server's
edit form (`DELETE /mcp-servers/:id/oauth-token`).
