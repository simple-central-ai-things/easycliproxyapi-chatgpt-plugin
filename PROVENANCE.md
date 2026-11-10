# PROVENANCE — TLS fingerprint spike

This file is the verdict ledger for the operator-gated live spike in
`tests/live_tls_test.go` (`-tags live`). It records the evidence that decides
the plugin's default upstream `tls-mode`.

## Spike

- **Date:** 2026-11-06 (scaffold; operator run pending)
- **Token provenance:** a disposable account — a throwaway ChatGPT consumer
  account created solely for this spike. Its access token is supplied by the
  operator as the `LIVE_ACCESS_TOKEN` environment variable and is never
  committed, logged, or printed.
- **Endpoint under test:**
  `GET https://chatgpt.com/backend-api/models?history_and_training_disabled=false`
- **Wire fingerprint (pinned, inlined in the test):** `OAI-Language: zh-CN`,
  `Accept-Language: zh-CN,zh;q=0.9`, `Origin: https://chatgpt.com`,
  `Referer: https://chatgpt.com/`,
  `OAI-Client-Version: prod-a194cd50d4416d3c0b47c740f206b12ce60f5887`,
  `OAI-Client-Build-Number: 6708908`, `X-OpenAI-Target-Path: /backend-api/models`
  and `X-OpenAI-Target-Route: /backend-api/models` (both carry the bare path
  template without the query string, per the reference's `list_models` →
  `_headers(route)` convention), a Chrome 110-line browser `User-Agent`, and
  `Authorization: Bearer <token>`.
- **Out of scope for the spike:** wire-level header casing and ordering
  canonicalization — Go's `http.Header.Set` canonicalizes casing and the
  transport emits its own ordering; the production transport (Task 5) owns
  byte-exact header order if it ever matters to the upstream.

## How to run (operator)

```sh
LIVE_ACCESS_TOKEN=<token> \
TMPDIR=$HOME/.cache/opencode/tmp \
go test -tags 'live utls' -run TestLiveTLS -v -count=1 ./tests/
```

Leg (a) runs with `-tags live` alone; leg (b) additionally needs the `utls`
tag, which links `github.com/refraction-networking/utls`. A 401 from any leg
fails the run with an "invalid or expired token" message — supply a fresh
token and rerun.

## Outcomes

| Leg   | Transport                              | HTTP status | Models JSON | Body bytes | Snippet / notes |
|-------|----------------------------------------|-------------|-------------|------------|-----------------|
| (a)   | stdlib `http.Client`, default `crypto/tls` hello | PENDING RUN | PENDING RUN | PENDING RUN |                 |
| (b)   | utls `HelloChrome_106_Shuffle` (closest first-handshake id to Chrome 110 in v1.8.2) + x/net/http2 | PENDING RUN | PENDING RUN | PENDING RUN | UA/hello mismatch noted below |

## Verdict

Chosen default TLSMode: `stdlib`

> The operator updates this line after running the spike and records the
> rationale in the table above:
>
> - **(a) returns 200 with models JSON** → stdlib TLS is accepted; keep
>   `tls-mode: stdlib` and prune the utls/x-net experiment from `go.mod`
>   (keep `tests/utls_client*_test.go`, still tag-gated — documented here as
>   unused-but-retained in case the verdict is ever revisited).
> - **(a) is challenged (403 / Cloudflare interstitial) while (b) returns
>   200** → set the line above to `utls` and keep the pinned dependencies
>   below. The plugin's direct-client utls path reads the host proxy from
>   `HostConfigSummary.ProxyURL` (per the plan's global constraints). Note
>   that this verdict also promotes a SECOND production dependency —
>   `golang.org/x/net` for the h2 transport (see below) — which needs
>   controller sign-off before Task 5 wires the production path.
> - **both legs fail** → the token or the operator's network path is wrong;
>   rerun before changing anything.

## Dependencies pinned for this spike

- `github.com/refraction-networking/utls v1.8.2` — pure Go; imported only by
  the utls-tagged leg (`tests/utls_client_test.go`) and never linked into
  the default build or the plugin `.so`. v1.8.2 has no `HelloChrome_110`
  constant and its 112/114/115 Chrome ids are PSK session-resumption
  variants unsuitable for opening a first handshake, so leg (b) pins
  `utls.HelloChrome_106_Shuffle` — the closest first-handshake Chrome id
  available in v1.8.2.
  **Known spike limitation:** the `User-Agent` still claims Chrome/110
  while the TLS fingerprint is Chrome 106's; if leg (b)'s verdict proves
  load-bearing, Task 5's production transport may pick a different (or
  custom) hello to remove that mismatch.
- `golang.org/x/net v0.57.0` — `http2.Transport` with a custom `DialTLS` for
  the same leg (the stdlib's bundled http2 transport is not importable, and
  `net/http.Transport` cannot speak h2 over a non-`*tls.Conn`; the version is
  the graph maximum, already required by the SDK). Test-only today; a `utls`
  verdict promotes it to a SECOND production dependency needing controller
  sign-off (see Verdict above).

## OAuth login — registered-redirect fix (2026-11-06)

- **Live attempt 2026-11-06 (operator verification):** `auth.login.start`
  built the authorize URL with the host-computed loopback callback as
  `redirect_uri` (`http://127.0.0.1:<port>/v8/management/oauth/callback`).
  OpenAI's auth0 endpoint rejected it live with
  `{"error":{"code":"invalid_authorize_request","message":"Invalid authorize request"}}`
  — the platform client `app_2SKx67EdpoN0G6j64rFvigXD` only allows its
  REGISTERED redirect URIs, and the host's loopback is not one of them.
- **Fix:** pin `redirect_uri` to the registered
  `https://platform.openai.com/auth/callback` and ignore the host `BaseURL`
  for the redirect (the token exchange sends the same pinned URI — the
  OAuth2 redirect_uri binding). This is the proven model of the MIT
  reference `basketikun/chatgpt2api` (`services/oauth_login_service.py`):
  after approving, the user's browser lands on the registered URL with
  `?code=...&state=...` and the user copies the full address-bar URL into
  the EasyCLI management panel's OAuth paste field.
- **Paste-back verified against the host:** CLIProxyAPI v8.0.0 SDK
  `internal/api/handlers/management/oauth_callback.go` (`PostOAuthCallback`
  parses `code`/`state`/`error` from the pasted `redirect_url`'s query
  string, lines 55–74) and `oauth_sessions.go` (`writeOAuthCallbackFile`
  publishes `<authDir>/.oauth-<provider>-<state>.oauth` with
  `{"code","state","error"}` — exactly the file the plugin's
  `auth.login.poll` reads; the plugin session is registered by
  `ServePluginAuthURL` in `auth_files_oauth_callback.go:213`). The
  management panel's paste input is in the host's management-center asset
  (`management.html`, v1.27.3): the provider auth modal's callback field
  POSTs `{provider, redirect_url}` to `/oauth/callback`.

## Live serving verification (2026-11-06)

The dedicated spike above was superseded in substance by real serving against
a live ChatGPT Plus account (a credential logged in through the panel's OAuth
bridge, plan persisted as `plus`):

- **TLS verdict — resolved by serving:** real `POST /v1/chat/completions` and
  `POST /v1/images/generations` calls reached `chatgpt.com` over the stdlib
  default TLS hello and returned **HTTP 200** — no 403 / Cloudflare
  interstitial. So leg (a) of the spike table is effectively answered:
  **keep `tls-mode: stdlib`**; the utls/x-net leg (b) is unused-but-retained.
  (The table cells are left `PENDING RUN` only because the standalone
  `live_tls_test.go` was not re-run; serving is stronger evidence.)
- **Turnstile multi-stage fix (`a118860`):** the first real text turn failed
  with `turnstile_unsolved`. Root cause: the live challenge is a
  self-modifying multi-stage program — stage 1 parses a NEW program into the
  data slot the VM loop executes, and a later stage serializes the
  fingerprint OrderedMap. The reference oracle's `for token in token_list`
  never executes the replacement and its `OrderedMap` makes `json.dumps`
  raise, so the faithful Go port (correctly) matched the oracle — neither
  solved it. The fix re-reads program slot 9 after every token and restarts
  from index 0 on a genuine replacement (backing-pointer identity, capped at
  64 passes), and op 15 serializes OrderedMap contents (JSON.stringify
  semantics, depth-guarded). Regression fixture `r01-live-two-stage` (a real
  dx+p, secret-scanned) pins the `[89 505 5]` pass chain; all 28 synthetic
  differential vectors stay byte-exact green.
- **Text:** `POST /v1/chat/completions` (`chatgpt-proxy/openai/gpt-5-6-mini`,
  "Reply with exactly: PONG") → **HTTP 200** with assistant content, 3×
  consecutive (including with the debug hook disarmed).
- **Image:** `POST /v1/images/generations` (`chatgpt-proxy/gpt-image-2-proxy`)
  → **HTTP 200**, 1 image, ~1.1 MB, valid PNG magic (`89 50 4e 47 0d 0a 1a
  0a`).

These confirm the register → login → models → text turn → image turn chain
end-to-end on the live gateway. The remaining operator-gated items are the
formal standalone spike runs, not blockers.

## v0.2.0 Phase-2 live serving verification (2026-11-09)

The Phase-2 `.so` was deployed to the live gateway (plugin registered at
`version=0.2.0` in the startup log) and verified end-to-end against the live
ChatGPT Plus credential, through the public gateway port. The deployed build
includes the four Phase-2 review minors (`3aa37d2`: Responses image tool
honors a bare `tool_choice`, sub2api single envelope unwrap, import token
merge of flat + nested sets, search map-ordering doc note) — re-deployed and
re-verified before the v0.2.0 tag moved to it:

- **Responses API (`openai-response`):** `POST /v1/responses` → **HTTP 200**,
  the `response.completed` envelope (`object:"response"`, `id:"resp_<hex>"`,
  `output[].message` with `output_text`), model echo of the requested name.
- **Anthropic Messages (`claude`):** `POST /v1/messages` → **HTTP 200**, the
  `message` envelope (`id:"msg_<uuid>"`, `content[].text`,
  `stop_reason:"end_turn"`, `stop_sequence:null`, usage).
- **Web search:** `POST /v1/chat/completions` carrying a `web_search_preview`
  tool → **HTTP 200**, the cited answer with `url_citation` annotations
  (real fetched sources) — the search flow (prepare + `force_use_search` +
  poll) ran against the live backend.
- **Import routes (registration + dispatch only, no credential written):**
  `POST chatgpt-proxy/import/cpa-file` with an empty body → typed
  `import_payload_invalid` 400; `GET chatgpt-proxy/import/cpa-remote/files`
  and `GET chatgpt-proxy/import/sub2api/accounts` against an unreachable
  server → typed retryable `import_remote_failed` 502. A real import (writing
  a `host.auth.save` record) is operator-gated: it needs a live CPA/sub2api
  server or a codex export.
- **Drawing workbench:** the resource page is served unauthenticated at
  `/v0/resource/plugins/<plugin-id>/chatgpt-proxy/workbench` (**HTTP 200**,
  ~20 KB self-contained HTML, inline script only, client-side history keys);
  `GET .../workbench/models` → **HTTP 200** with both published image ids;
  `POST .../workbench/generate` (n=1, low quality) → **HTTP 200** in ~36 s,
  one inline `b64_json` entry with valid PNG magic; `POST .../workbench/edit`
  with a data-URL reference → **HTTP 200**, one valid PNG.
- **Streaming formats:** `POST /v1/responses` with `stream:true` → **HTTP
  200**, the full Responses SSE sequence — `response.created` →
  `response.output_item.added` → `response.output_text.delta` →
  `response.output_text.done` → `response.output_item.done` →
  `response.completed` (with usage). The Responses stream is framed as bare
  `data:` lines carrying the event `type` in the payload (the offline-pinned
  contract; no `event:` lines and no `[DONE]`). `POST /v1/messages` with
  `stream:true` → **HTTP 200**, the Anthropic sequence — `message_start` →
  `content_block_start` → `content_block_delta` → `content_block_stop` →
  `message_delta` → `message_stop`, framed `event:/data:`, no `[DONE]`.
- **Responses `image_generation` tool:** `POST /v1/responses` carrying
  `tools:[{"type":"image_generation"}]` → **HTTP 200**, an
  `image_generation_call` output item with a ~1.5 MB inline base64 `result`
  — the tool routes through the live image pipeline.
- **openai regression:** `POST /v1/chat/completions` still → **HTTP 200**
  chat.completion envelope.

Operator-gated (needs the account's email+password or a real import source,
which are not recorded here): a live `auth.login.password` recovery and a
real CPA/sub2api credential import.

## v0.2.1 panel-surface live verification (2026-11-10)

The v0.2.1 `.so` (registration `ConfigFields` + the
`POST chatgpt-proxy/login/password` management route) was deployed to the
live gateway (startup log: `plugin registered ... version=0.2.1`) and
verified through the public gateway port with the management key:

- **ConfigFields:** `GET /v0/management/plugins` → the plugin entry
  advertises all 15 config fields (`base-url`, `tls-mode`,
  `search-timeout-secs`, …), with `tls-mode` as an enum
  (`["stdlib","utls"]`) — the panel's plugin-settings UI now renders them.
- **Recovery route:** `POST .../chatgpt-proxy/login/password` with an empty
  body → typed `invalid_request` 400 ("email and password are required")
  before any upstream call — the route is registered, dispatched, and
  fail-closed. A real recovery run is operator-gated (it would attempt a
  login against the account; no failed attempt was made during
  verification). A `GET` on the POST-only route answers the host's own 404
  (the host registers exact method+path pairs) — expected, not a defect.
