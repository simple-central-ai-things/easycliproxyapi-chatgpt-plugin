# easycliproxyapi-chatgpt-plugin

A Go `c-shared` plugin (a `.so`) compiled INTO the EasyCLI/CLIProxyAPI v8
host that exposes ChatGPT consumer-account capabilities — streaming text
conversation, web search, and image generation/editing — as the
OpenAI-compatible provider **`chatgpt-proxy`**.

## What

- Provider key, model prefix and auth-file type: `chatgpt-proxy`.
- Plugin identity (the `.so` basename and registration `Name`):
  `easycliproxyapi-chatgpt-provider`.
- Advertised capabilities: `auth_provider` (browser OAuth + refresh-token
  lifecycle), `model_provider` (per-credential upstream catalog),
  `executor` (formats `openai`, `openai-image`, `openai-response`, `claude`),
  `quota_provider` (native host panel cards), and `management_api`
  (the CPA / sub2api account imports as host Management API routes).

## Why

ChatGPT subscription models are normally served by the browser-based
`chatgpt.com` surface, which needs a full browser session. This plugin turns
each ChatGPT consumer account into a credential the EasyCLI host can manage
— rotation, retry, enable/disable on the host's own panel — and serves it
through the gateway's existing model routing. No extra container, no extra
published port: the plugin loads inside the host process.

The v0.2.0 surface: the credential lifecycle (auth parse/refresh, browser
OAuth login, password re-login / account recovery, with the plan persisted at
login and refresh), the per-credential model catalog, the quota cards, the
text conversation executor in the `openai`, `openai-response` (Responses API)
and `claude` (Anthropic Messages) formats (streaming and non-streaming), web
search with cited answers, the `openai-image` executor surface (image
generation and editing routed by model base name to the web-drawing and
codex pipelines), CPA / sub2api account imports as host Management API
routes, a `POST chatgpt-proxy/login/password` account-recovery management
route (the same core as the `auth.login.password` RPC), the drawing workbench
(a browser resource page driving the image pipeline), and the registration
`ConfigFields` (every tunable config key advertised so the host's
plugin-settings UI renders them). Remaining gaps are in `RELEASE_NOTES.md`.

## Layout

```text
main.go                       CGO glue only: exports cliproxy_plugin_init /
                              cliproxyPluginCall / cliproxyPluginFree /
                              cliproxyPluginShutdown; forwards every RPC to
                              internal/plugin.Manager.Handle (2^31 size_t
                              guard, maxRequestLen)
internal/model/               leaf package: the Credential + auth-file
                              storage shape, the Fingerprint block, Config +
                              ParseConfig — the canonical type home (the
                              upstream package consumes it too; dependency
                              direction plugin -> upstream -> model)
internal/plugin/              Manager + RPC dispatch (the single seam: all
                              host I/O flows through the injected caller),
                              identity constants, registration envelope,
                              auth parse/refresh, browser OAuth login,
                              password re-login (account recovery) with the
                              auth-flow sentinel PoW, Management API
                              plumbing (management.register/handle routing
                              the import + workbench surfaces and the
                              workbench resource page), CPA / sub2api
                              account imports, the drawing workbench
                              (workbench.go + the embedded data-free HTML
                              shell workbench_shell.html), per-credential
                              model catalog, quota cards, text/image/search
                              executors, envelope helpers, config/
                              credential aliases
internal/upstream/            the upstream reverse protocol: transport seam
                              over the host callbacks, session headers,
                              sentinel/PoW/turnstile VM, conversation SSE +
                              patch protocol, poll, profile/quota probe,
                              web-drawing + codex image routes, web-search
                              route, files trio; golden-file tests pin the
                              wire payloads
tests/loadsmoke/              dlopen() load smoke: the built .so through the
                              real C ABI against a fake host (skips when the
                              artifact is absent; CI builds it first)
tests/live_tls_test.go        operator-gated TLS fingerprint spike (-tags
                              live; verdict ledger PROVENANCE.md)
tests/live_executor_test.go   operator-gated live executor smoke (-tags live)
tests/utls_client*_test.go    the utls leg of the TLS spike (-tags 'live
                              utls'; never linked into the default build)
registry.json                 host plugin-store registry entry
PROVENANCE.md                 TLS spike verdict ledger
CHANGELOG.md                  user-facing release summaries
RELEASE_NOTES.md              the v0.2.0 release notes
plugins/<os>/<arch>/          gitignored local build output (*.so/*.h)
.github/workflows/tests.yml   CI: go vet + go test + glibc c-shared build
                              (blocking gate) + loadsmoke
```

## Contracts

The contract matrices below are pinned by `internal/plugin/matrix_test.go`:
CI fails when a documented matrix and the actual dispatcher, identity or
config surface disagree in either direction — the error classes matrix is
the exception: it is pinned one-directionally, each documented row against
the code.

- Plugin identity (pinned by `internal/plugin/plugin_test.go`):
  `name=easycliproxyapi-chatgpt-provider`, `version=0.2.1`, provider key
  `chatgpt-proxy`, `GitHubRepository` = this repo (the host `validPlugin`
  gate rejects an empty value). The `registry.json` store entry carries the
  same id, version and repository URL.
- Registration capabilities:

<!-- matrix:capabilities -->
| Capability | Advertised | Executor scope | Formats in/out |
| --- | --- | --- | --- |
| `auth_provider` | yes | — | — |
| `model_provider` | yes | — | — |
| `executor` | yes | `oauth` | `openai`, `openai-image`, `openai-response`, `claude` |
| `quota_provider` | yes | — | — |
| `management_api` | yes | — | — |
<!-- /matrix:capabilities -->

  `management_api` is what turns on the host Management API: the host
  invokes `management.register` only when it sees the flag, and the reply
  lists the plugin-owned import + workbench routes and the one workbench
  browser resource (see Account imports and Drawing workbench below).

- Published model ids (routed by the executor on these exact names):

<!-- matrix:models -->
| Route | Published id |
| --- | --- |
| `text` | `chatgpt-proxy/openai/<slug>` |
| `web-drawing` | `chatgpt-proxy/gpt-image-2-proxy` |
| `codex` | `chatgpt-proxy/codex-gpt-image-2` |
<!-- /matrix:models -->

  Text turns publish as `chatgpt-proxy/openai/<slug>` — the `openai`
  vendor segment is a constant because the upstream catalog carries no
  vendor field, so nothing is invented. The `codex` image route publishes
  only when the credential's plan gates it (plus/pro/team); the
  web-drawing route always publishes. The `-proxy` image base name is
  deliberate: the host's built-in executor claims `gpt-image-2` as a
  hardcoded codex model, so the unique base name keeps image calls on the
  registry path.
- Host config block (EasyCLI `config.yaml`):
  `plugins.configs.easycliproxyapi-chatgpt-provider`. Every plugin-owned
  key with its default:

<!-- matrix:config -->
| Key | Default |
| --- | --- |
| `model-prefix` | `"chatgpt-proxy"` |
| `image-model-name` | `"gpt-image-2-proxy"` |
| `codex-image-model-name` | `"codex-gpt-image-2"` |
| `drawing-upstream-model` | `"gpt-5-5"` |
| `thinking-effort` | `"auto"` |
| `base-url` | `""` |
| `message-as-error` | `true` |
| `image-poll-timeout-secs` | `120` |
| `image-poll-interval-secs` | `10` |
| `request-timeout-secs` | `600` |
| `tls-mode` | `"stdlib"` |
| `retry-401-refresh` | `true` |
| `search-upstream-model` | `"gpt-5-5"` |
| `search-timeout-secs` | `300` |
| `search-poll-interval-secs` | `3` |
<!-- /matrix:config -->

  Config parsing is fail-closed and rejects unknown keys — except the
  host-owned `enabled` and `priority`, which CLIProxyAPI v8 injects into
  every plugin's config before registration (accepted, ignored). The
  registration advertises exactly these keys as `Metadata.ConfigFields`,
  so the host panel's plugin-settings UI renders them instead of the
  operator hand-editing the block (`enabled`/`priority` stay out — the
  host owns them).
- Credential records (host auth files, `type: "chatgpt-proxy"`):
  `access_token`, `refresh_token`, `id_token`, `email`, `user_id`,
  `account_id`, `plan`, `source_type`, the `fp` fingerprint block
  (`user_agent`, `impersonate`, `oai_device_id`, `oai_session_id`) and
  `last_refresh_at` / `last_refresh_error`. `plan` is
  `free|plus|pro|team|enterprise` (`""` = unknown and picked up at the
  first refresh — which is where imported records start); `source_type`
  is `web|codex|oauth_login|password` (`password` marks a record recovered
  by the password re-login below, `codex` the imports — both CPA and
  sub2api sources, the reference's tag). Auth-file names are stable hashes
  — never an email. Host-managed keys (`label`, `disabled`, `priority`, ...) travel
  in metadata and survive refresh rewrites untouched.
- OAuth login (browser sign-in from the management panel):
  `auth.login.start` builds the authorize URL against exactly
  `https://auth.openai.com/api/accounts/authorize` with the pinned
  registered `redirect_uri`
  `https://platform.openai.com/auth/callback` — the OpenAI platform
  client only allows its registered redirect URIs, so the host-computed
  loopback callback (sent as `BaseURL`) is ignored (auth0 rejects it).
  PKCE (S256) plus a 32-char alphanumeric state (inside the host's
  `ValidateOAuthState` charset) guard the flow; one sign-in window lasts
  10 minutes. After approving, the browser lands on
  `https://platform.openai.com/auth/callback?code=...&state=...`; the
  user copies that full address-bar URL and pastes it into the EasyCLI
  management panel's OAuth paste field. The host's `oauth/callback`
  handler extracts `code`/`state` from the pasted URL's query string and
  writes the host callback file; the plugin's `auth.login.poll` then
  exchanges the one-time code (with the same pinned `redirect_uri`) and
  verifies the account before returning the credential. No loopback port
  is reachable for the redirect, by design.
- Password re-login (account recovery): `auth.login.password` recovers an
  account whose refresh token died, given its email + password. It is a
  SINGLE synchronous RPC — the browser flow's start/poll pair and attempt
  store exist to bridge the async gap to the host's callback file, which
  this flow has no part of: every leg completes inside one call, so there
  is no attempt record to store, GC or replay-protect. Wire shape:
  `{"Provider","Email","Password","host_callback_id"}` (PascalCase, the
  SDK's request convention) answers the poll response shape (`status`
  success/error + the `auth` storage record on success); typed refusals
  travel as error envelopes. The flow: ① an authorize bootstrap GET
  against `https://auth.openai.com/api/accounts/authorize` (sixteen pinned
  params — the email travels as `login_hint`, the session is keyed by the
  `oai-did` device cookie; a redirect to an error page is decoded, with
  `rate_limit_exceeded` its own typed class) → ② the AUTH-FLOW sentinel —
  the FNV-1a-32 proof-of-work challenge over
  `sentinel.openai.com/backend-api/sentinel/req` (flow `password_verify`,
  500k-iteration cap, differential-pinned to the reference
  `utils/sentinel.py`; distinct from the conversation-flow sentinel in
  `internal/upstream`) — ③ `POST /api/accounts/password/verify` with a
  JSON `{"password": ...}` body (the reference's exact shape — the email
  never repeats), the `openai-sentinel-token` header and the `oai-did` +
  `oai-sc` cookies → ④ the `continue_url`'s `code` traded at the same
  token endpoint as the browser login with the flow's own 86-char PKCE
  verifier (`auth0-client` header, the reference's exact exchange headers)
  → ⑤ the SAME storage as the browser login: `/backend-api/me`
  verification, JWT normalization, best-effort plan probe, stable hashed
  file name — `source_type: "password"`, so `auth.parse`, `auth.refresh`,
  the executor refresh-retry and quota work on the record unchanged. A
  `continue_url` with no code whose `page.type` is
  `email_otp_verification` is typed `need_verification_code` (the account
  is unusable headless — 2FA pages land in the retryable `no_auth_code`);
  the code is never exchanged without one. Secret hygiene: the password
  crosses the ABI only as this RPC's input and appears only in the verify
  body; verify refusals answer FIXED typed messages and never echo the
  upstream error body (which could echo credential material).
- Account imports (host Management API routes): the plugin advertises the
  `management_api` capability, so the host invokes `management.register` at
  startup; the reply lists exactly these routes by method + path +
  description (the handler never crosses the ABI — the host calls back via
  `management.handle` for every hit). Paths are EXACT, no path params, and
  resolved by the host under `/v0/management/` (the host authenticates
  management traffic before dispatching it to the plugin):

  | Method | Path (under `/v0/management/`) | Request body | Response |
  | --- | --- | --- | --- |
  | `POST` | `chatgpt-proxy/import/cpa-file` | the CLIProxyAPI codex auth-file export JSON directly | `{added, skipped, errors[], file?}` |
  | `GET` | `chatgpt-proxy/import/cpa-remote/files` | `{base_url, secret_key}` | `{files:[{name, email}]}` |
  | `POST` | `chatgpt-proxy/import/cpa-remote` | `{base_url, secret_key, files:[names]}` | `{added, skipped, errors[]}` |
  | `GET` | `chatgpt-proxy/import/sub2api/accounts` | `{base_url, email, password}` | `{accounts:[{id, name, email, plan_type, status, expires_at, has_refresh_token}]}` |
  | `POST` | `chatgpt-proxy/import/sub2api` | `{base_url, email, password, accounts:[ids]}` | `{added, skipped, errors[]}` |

  Each handler takes the source config INLINE in the body — the
  reference's pool/server dict passed by value. Pool/server persistence
  (CRUD), the reference's async job record, the sub2api `api_key` auth
  alternative and its groups list are follow-ups; these handlers run to
  completion inside one `management.handle` call and answer the job-summary
  shape `{added, skipped, errors:[{name, message}]}` inline. An unknown
  exact path answers `404 management_not_found`, a known path with the
  wrong method `405 management_method_not_allowed` (defense in depth — the
  host's own lookup filters both before dispatching).
  - CPA legs: `GET {base_url}/v0/management/auth-files` and
    `GET {base_url}/v0/management/auth-files/download?name=<file>`, both
    with `Authorization: Bearer <secret_key>`. The download payload is a
    CLIProxyAPI codex auth file; the reference extracts ONLY the access
    token, but this plugin captures the FULL token set it provides
    (access token required; refresh/id tokens when present, flat or nested
    under `tokens`) — a deliberate superset, so the saved credential joins
    the plugin's OAuth `auth.refresh` lifecycle. The reference's
    post-import keepalive (`refresh_accounts`) is NOT reimplemented: the
    executor's 401 refresh-retry and the host-scheduled refresh do that on
    demand. Batch imports run sequentially (deterministic summary order,
    unlike the reference's thread pool); one bad file records an error and
    never aborts the rest.
  - sub2api legs: `POST {base_url}/api/v1/auth/login` with `{email,
    password}` → the admin JWT (envelope unwrapped), used as
    `Authorization: Bearer <jwt>` for `GET /api/v1/admin/accounts`
    (paged: `platform=openai`, `type=oauth`, `page_size=200`, until the
    page is short or `page*200 >= total`) and `GET /api/v1/admin/accounts/data?ids=<csv>&timezone=Asia/Shanghai`
    (the reference's GET shape is ported as-is — the ids travel as a
    comma-joined query param, never a body). The `{code, message, data}`
    envelope unwrap and the paged-item extraction (`items`/`data`/`list`
    keys, bare lists) are ported faithfully to handle the sub2api
    response-shape variations. Token keys fall back
    `access_token` → `accessToken` → `token`. The JWT is acquired per
    import call — never persisted, and never logged, replied or saved.
  - Credential save + dedupe: every import is saved through the same
    `host.auth.save` + stable-hash file name (`chatgpt-proxy-<sha256-16
    hex>.json`, never an email) the logins use, with `source_type:
    "codex"` (the reference tags BOTH import sources "codex" — the token
    shape both carry) and the same record shape, so `auth.parse`,
    `auth.refresh`, the executor refresh-retry and quota work on it
    unchanged. JWT claims backfill any missing identity (the auth.parse
    precedent); the plan is left `""` and picked up at the first refresh.
    Dedupe by access_token runs against BOTH the existing store
    (`host.auth.list`/`host.auth.get`) and the batch itself: an import
    whose access token is already known answers `skipped` — not added, not
    an error, and never a save. Saved credentials contain ONLY the token
    set + identity + source_type — never the import source's
    secret_key/password.
  - Secret hygiene (first-class): the CPA `secret_key`, the sub2api
    password and the sub2api JWT cross the ABI only as the
    `management.handle` request input and appear only in the outbound
    `Authorization` headers / the sub2api login body. Every reply, typed
    refusal, summary entry and saved credential carries FIXED text —
    upstream bodies are read only to pick the error branch, never echoed
    (the F4 `classifyPasswordVerifyError` pattern). The error classes:
    `import_invalid_request` (400, malformed body / missing config),
    `import_unauthorized` (401, the remote rejected the credentials),
    `import_remote_failed` (502 retryable, transport or other refusal),
    `import_payload_invalid` (400 inline body / 502 remote payload shape),
    `import_save_failed` (500 retryable, the host credential store
    refused).
- Account recovery (host Management API route): `POST chatgpt-proxy/login/password`
  (under `/v0/management/`) exposes the SAME recovery core the
  `auth.login.password` RPC runs — `handlePasswordLogin`, no fork of the
  flow — because the SDK's auth contract knows only
  `auth.login.start`/`auth.login.poll`, so the panel had no built-in UI
  path to recovery. The RPC itself stays unchanged; both surfaces share
  the core.
  - Request body: `{email, password}` (the RPC's PascalCase spelling is
    accepted too — the route merges both into the core's own request
    type). Missing/blank email or password answers the core's fail-closed
    typed `invalid_request` 400 BEFORE any network call.
  - Response: `200` with the poll response shape — `status` success/error
    + the `auth` storage record on success (an account-verification
    outcome travels IN the body, exactly like the RPC) — or a typed
    `{error:{code,message,retryable}}` body with the status from the typed
    error. The error classes are the password re-login ones already in the
    error matrix (`invalid_password` 401, `unsupported_region` 403,
    `login_state_invalid` 400 retryable, `password_verify_failed` 502
    retryable, `need_verification_code` 403, `no_auth_code` 502 retryable,
    `sentinel_pow_failed` 502 retryable, `rate_limit_exceeded` 429
    retryable, plus `invalid_request` 400 and the core's generic
    `login_failed` 500/502) — no new classes.
  - Persistence: on success the route saves the recovered credential
    itself through `host.auth.save` (a `management.handle` reply is served
    as-is by the host — there is no `auth.login.poll` persistence
    contract), with the stable-hash file name and `source_type:
    "password"`, so `auth.parse`, `auth.refresh`, the executor
    refresh-retry and quota work on it unchanged; a store refusal answers
    a typed retryable `login_failed` 500 (the recovery succeeded, only
    persistence was refused — retry the same route).
  - Secret hygiene: the password crosses the ABI only as the route body
    input and appears ONLY in the outbound verify body — never in the
    response envelope, an error message, the saved credential, or any
    other outbound call; verify refusals answer the FIXED typed messages
    and never echo the upstream body (canary-pinned by
    `login_route_test.go`, extending the F4 core hygiene tests).
- Drawing workbench (plugin-owned image console, F6): the registration
  reply also carries ONE browser resource — the workbench console page —
  plus three management routes that drive the existing openai-image
  pipeline (see the Image executor surface) synchronously:

  | Method | Path | Request body | Response |
  | --- | --- | --- | --- |
  | `GET` | `/v0/resource/plugins/easycliproxyapi-chatgpt-provider/chatgpt-proxy/workbench` | — | the HTML shell (`text/html`, the pinned bytes) |
  | `POST` | `chatgpt-proxy/workbench/generate` | `{prompt, model?, size?, quality?, n?, credential?}` | `{created, data:[{revised_prompt, b64_json}], usage}` |
  | `POST` | `chatgpt-proxy/workbench/edit` | the generate body + `reference_images:[<data-url or b64>]` | the same images envelope |
  | `GET` | `chatgpt-proxy/workbench/models` | — | `{models:[<two published image ids>]}` |

  (The management paths are under `/v0/management/`; the resource path is
  the browser-navigable full path the host serves.)
  - The shell is a SINGLE self-contained HTML page (inline CSS/JS, no
    external assets, no build step), embedded into the `.so` and served
    byte-exact. It is **DATA-FREE** (security rule, pinned by
    `TestWorkbenchShellIsDataFree`): its bytes carry no credential, token,
    email or config value — all dynamic data arrives from the management
    routes above via same-origin `fetch(..., {credentials:'include'})`.
    The host's resource path (`ServeResourceHTTP`) writes the body verbatim
    — no CSP injection, no sanitizing — which is what makes inline JS in
    the raw page safe and servable; the data routes themselves are
    management-authenticated by the host before dispatch.
  - Credential selection (the management context has no host-injected
    record): the optional `credential` field is an auth-file name and
    selects that exact own-provider entry via host.auth.list/get (the
    import's `knownAccessTokens` pattern); an absent name defaults to the
    FIRST own-provider entry (store order). An empty store answers
    `404 workbench_credential_missing` pointing at the import routes, a
    foreign name `404 workbench_credential_not_found`, and a refused or
    unreadable store leg `500 workbench_store_unavailable` (retryable).
    The acquired file name becomes the turn's auth file, so the pipeline's
    401 refresh-retry persists to the right record.
  - The `model` field is a published image id (`chatgpt-proxy/gpt-image-2-proxy`
    → drawing route, `chatgpt-proxy/codex-gpt-image-2` → codex route; the
    models route answers the true ids of the registered config); absent
    `model` defaults to the drawing route, anything else is the typed
    `model_unknown`. `n` is validated 1..4 by the executor's own
    `validateImageCount` (the workbench count IS the executor's n —
    sequential in one attempt; NOT the reference web's 1..100-per-task).
    Edit references decode through the existing `decodeReferenceImages`
    path (data-URL / bare base64 / binary, the 10-image cap enforced).
  - Deliberate simplifications vs the reference's web image app (the
    interaction oracle): SYNCHRONOUS — the reference is a separate HTTP
    server with background workers, so it answers a task id and polls;
    this plugin runs the pipeline inline in the host process, so each
    generate/edit POST runs the whole turn and returns the result grid
    directly (no task/poll loop). CLIENT-SIDE HISTORY — the shell keeps
    its history in the BROWSER's localStorage (prompt/config per turn +
    shrunken previews), exactly like the reference's image-conversations
    store; there is NO server-side history store. Pipeline failures answer
    the executor's typed codes unchanged (`invalid_request`, `model_unknown`,
    `content_policy_violation`, `invalid_token`, `upstream_error`, ...) —
    one vocabulary for the image surface.
- Executor surface (text, `openai` format): conversation turns arrive as
  `executor.execute` / `executor.execute_stream` in the `openai` format.
  Model resolution strips the `chatgpt-proxy/openai/` prefix to the
  upstream slug; anything else (the image models, a foreign id) is refused
  with typed `400 model_unknown`. A `tools` array of OpenAI function tools
  is EMULATED (the upstream conversation API has no tool surface): the
  per-tool schemas plus the fixed "Tool use rules:" block are merged into
  the conversation's system message(s), the reply's tool-call XML is parsed
  back into OpenAI `tool_calls` (fresh `call_<uuid>` ids, JSON-string
  arguments, `finish_reason:"tool_calls"`, markup-stripped `content`), and
  `role:"tool"` results plus prior assistant `tool_calls` round-trip into
  the conversation. Streaming WITH tools is buffered, not token-streamed:
  the whole upstream turn is drained, then emitted as a role frame, a
  finish frame carrying any `tool_calls`, and `[DONE]`. A tools array where
  no tool carries a usable function name is refused with typed
  `400 tools_unsupported` before any upstream call — except a
  web-search tools array, which routes through web search (below).
  `thinking_effort` / `reasoning_effort` map per the upstream table (none
  / auto omitted, low/medium/high as-is, xhigh/extended to
  `extended`). A `401` on the turn start forces the internal token
  refresh; the rotated record is persisted back to the auth file via
  `host.auth.save` BEFORE the single retry (an OpenAI refresh rotates the
  refresh token, so an unpersisted refresh would break the host's next
  scheduled `auth.refresh`); a failed refresh, a failed save, or a second
  `401` ends in typed `401 invalid_token`.
  Web search: a request is a SEARCH request (the reference's
  `is_web_search_chat_request`) when it carries a `web_search` /
  `web_search_preview` / `web_search_preview_2025_03_11` tool with NO other
  tools, a `web_search` `tool_choice`, a `web_search_options` block, or a
  search model prefix on the resolved slug (`gpt-4o-search-preview`,
  `gpt-4o-mini-search-preview`, `gpt-5-search-api` — exact or with a `-…`
  version suffix). Search requests run the upstream search flow instead of
  the plain conversation pipeline: the sentinel handshake, a conduit prepare
  with `system_hints:["search"]`, the `f/conversation` SSE with
  `force_use_search` (capturing the conversation id), then a poll of the
  conversation document until the assistant answer carries a finished status
  (`finished_successfully` / `finished_partial_completion`) or is stable for
  two consecutive polls (budget `search-timeout-secs`, interval
  `search-poll-interval-secs`; the upstream model is
  `search-upstream-model`). The answer returns in the normal chat envelope:
  the assistant content is the cited answer — the cleaned text plus a
  numbered `Sources:` block — with `url_citation` annotations attached to
  the message (the reference's `text_with_url_citations` /
  `chat_completion_annotations` shape). Streaming search turns are buffered
  and emit one role+content frame, the terminal stop frame (usage estimate)
  and `[DONE]` (annotations are dropped in stream form, as in the
  reference). A search turn settling with no assistant answer is the typed
  502 `no_assistant_content`; a poll-budget exhaustion with no extraction is
  typed 504 `search_poll_timeout`; a malformed search response (missing
  conduit token, no conversation id in the stream) is typed 502
  `search_response_malformed`. A search tool mixed with a function tool is
  NOT a search request: it falls back to the ordinary emulated-tools
  pipeline (the search flow posts no tools upstream, so the search tool is
  ignored there). The `openai-response` and `claude` formats detect the same
  signals (tools + model prefix; the claude format carries no web_search
  tools, so only the model prefix fires) and answer with the cited text in
  their normal envelopes — one `output_text` part / one text block; the
  `url_citation` annotations are an openai-format surface.
- Executor surface (Responses, `openai-response` format): the host's
  `/v1/responses` requests arrive untranslated and reuse the SAME upstream
  conversation pipeline as the `openai` format — only the inbound decode
  (`instructions` → leading system message, `input` string/item-list →
  conversation messages: `function_call` / `function_call_output` items
  fold like the agent loop, `reasoning` items are skipped, image content
  parts are refused typed on the text path) and the outbound shaping
  differ. Non-stream answers carry the `response.completed` envelope
  object (`resp_<uuid hex>` id, `output` message items with
  `output_text` parts, usage in the reference's `input_tokens` /
  `output_tokens` / `*_tokens_details` shape). Stream emits the Responses
  SSE sequence — `response.created` → `response.output_item.added` →
  `response.output_text.delta`* → `response.output_text.done` →
  `response.output_item.done` → `response.completed` (with usage) — and
  NO `data: [DONE]` sentinel; `response.completed` is the terminator.
  An `image_generation` tool (or a bare `tool_choice` of type
  `image_generation`, as the reference's routing predicate honors) routes
  the request to the image pipeline
  (prompt from `input_text` parts and user messages, the LAST input image
  as reference, the tool's `size` / `quality` riding through, a published
  text model on the drawing route) and answers with `ig_<n>`
  `image_generation_call` output items carrying the base64 `result`; a
  missing prompt is typed `400 invalid_request` ("input text is required")
  before any upstream call. Function tools reuse the `openai` format's XML
  emulation and parse into `function_call` output items; a web-search tools
  array (with no function tool mixed in) or a search model prefix routes
  through the search flow and answers the cited text as one `output_text`
  item (see the web-search note in the `openai` surface); any other
  non-function tool type keeps the typed `400 tools_unsupported` refusal.
  Tool, image and search requests buffer the whole turn before emitting the
  item sequence (documented tradeoff, as with `openai` streaming).
- Executor surface (Anthropic Messages, `claude` format): the host's
  `/v1/messages` requests arrive untranslated and reuse the SAME upstream
  conversation pipeline — only the inbound decode (`system` string or text
  block list → leading system message; each message's content flattened:
  text parts in order, `tool_use` parts rewritten to the reference's exact
  single-`<tool_calls>` XML text block, `tool_result` parts to
  `Tool result {tool_use_id}: {content}`, image and other parts
  contributing nothing) and the outbound shaping differ. Tools reuse the
  `openai` format's XML emulation (`input_schema` accepted); when the
  system prompt carries "You are Claude Code", the per-tool schema prompt
  is replaced by the reference's single-line XML rule (appended even
  without a tools array). Non-stream answers carry the `message` envelope
  (`msg_<uuid>` id with dashes — unlike the Responses hex form —
  `content[]` of `text` and/or `tool_use` blocks with fresh `toolu_<uuid>`
  ids, `stop_reason` `end_turn` / `tool_use`, `stop_sequence` null, usage
  `input_tokens` / `output_tokens` as the documented bytes/4 estimate).
  Stream emits the Anthropic SSE sequence — `message_start` →
  `content_block_start` → `content_block_delta`* → `content_block_stop`
  (per block) → `message_delta` (stop_reason + output usage) →
  `message_stop` — each event framed `event: <type>` / `data: <json>`;
  EVERY claude turn buffers the whole upstream conversation first and
  emits the sequence at once (documented tradeoff: the reference's
  sequence at completion; tool blocks announce empty input then one
  `input_json_delta`), and there is NO `data: [DONE]` sentinel —
  `message_stop` is the terminator. The claude request carries no
  thinking-effort spelling, so the config default applies. A search model
  prefix on the resolved slug routes the turn through the search flow and
  answers the cited text as one `text` block (Anthropic tools carry no
  `type`, so the claude format has no web-search tool signal; see the
  web-search note in the `openai` surface).
- Error rendering for `claude`: every failure answers the SAME typed
  plugin error envelope as the other formats; the host's Claude handler
  (the `/v1/messages` route) renders it as the Anthropic
  `{"type":"error","error":{"type","message"}}` body — the error type is
  derived from the HTTP status (401 → `authentication_error`, 429 →
  `rate_limit_error`, 5xx → `api_error`, ...), mid-stream failures as the
  `event: error` frame — so the plugin carries no per-format error bytes.
- Error classes (pinned one-directionally by `matrix_test.go` — each
  documented row against the dispatcher; "dispatch" pins drive the
  registered Manager offline against a fake host, "error-map" pins drive
  the upstream error mapping with the upstream's own typed errors):

<!-- matrix:errors -->
| Code | HTTP status | Retryable | Pin |
| --- | --- | --- | --- |
| `invalid_request` | 400 | no | dispatch |
| `unknown_method` | 404 | no | dispatch |
| `model_unknown` | 400 | no | dispatch |
| `tools_unsupported` | 400 | no | dispatch |
| `unsupported_protocol` | 400 | no | dispatch |
| `invalid_credential` | 400, 401 | no | dispatch |
| `invalid_stream` | 400 | no | dispatch |
| `unsupported` | 501 | no | dispatch |
| `quota_failure` | 400, 500 | varies | dispatch |
| `invalid_token` | 401 | no | error-map |
| `rate_limited` | 429 | yes | error-map |
| `upstream_stream_timeout` | 504 | yes | error-map |
| `no_available_model` | 404 | no | error-map |
| `upstream_error` | 502 | yes | error-map |
| `content_policy_violation` | 400 | no | error-map |
| `no_assistant_content` | 502 | no | error-map |
| `search_poll_timeout` | 504 | yes | error-map |
| `search_response_malformed` | 502 | no | error-map |
| `invalid_password` | 401 | no | dispatch |
| `unsupported_region` | 403 | no | dispatch |
| `login_state_invalid` | 400 | yes | dispatch |
| `password_verify_failed` | 502 | yes | dispatch |
| `need_verification_code` | 403 | no | dispatch |
| `no_auth_code` | 502 | yes | dispatch |
| `sentinel_pow_failed` | 502 | yes | dispatch |
| `rate_limit_exceeded` | 429 | yes | dispatch |
| `import_invalid_request` | 400 | no | import |
| `import_unauthorized` | 401 | no | import |
| `import_remote_failed` | 502 | yes | import |
| `import_payload_invalid` | 400, 502 | no | import |
| `import_save_failed` | 500 | yes | import |
| `workbench_credential_missing` | 404 | no | workbench |
| `workbench_credential_not_found` | 404 | no | workbench |
| `workbench_store_unavailable` | 500 | yes | workbench |
<!-- /matrix:errors -->

  `rate_limited` carries `Retry-After` when upstream sends it;
  `upstream_stream_timeout` (504) fires on the SSE hard cap — the turn
  may still be running upstream; `upstream_error` is the transport
  fallback shape (a typed upstream status rides through otherwise);
  `no_assistant_content` — a completed turn with no visible assistant
  message is always typed 502, never an empty 200; `unsupported` is the
  `quota.reset` refusal (the upstream windows are rate limits that reset
  on their own schedule). The password re-login codes
  (`invalid_password` … `rate_limit_exceeded`) come from
  `auth.login.password`: the verify refusals carry fixed messages and
  never echo the upstream body, `need_verification_code` marks an
  email-OTP account that headless login cannot finish, and
  `sentinel_pow_failed` is the PoW iteration cap (retryable — a fresh
  challenge usually solves). The `POST chatgpt-proxy/login/password`
recovery route answers these SAME classes as typed management bodies
(`{error:{code,message,retryable}}` with the status from the typed error —
see Account recovery above): it reuses the core's validation
(`invalid_request` 400) and the core's generic `login_failed` (500 retryable
store refusal / 502 transport-family) rather than defining new ones. The
import codes (`import_invalid_request` …
  `import_save_failed`) come from the Management API import routes:
  `import_payload_invalid` answers 400 for an inline export that carries
  no access token and 502 for a remote payload that misses the expected
  shape; `import_save_failed` is a host credential-store refusal (the
  import aborts typed rather than saving blind); the rest, and every
  per-file error entry, carry fixed text only — the "import" pins in
  `matrix_test.go` drive the routes offline against a scripted host
  transport. The workbench credential classes
  (`workbench_credential_missing` … `workbench_store_unavailable`) come
  from the drawing workbench's store acquisition (see Drawing workbench
  below): `workbench_credential_missing` answers when no `chatgpt-proxy`
  credential exists yet and points at the import routes,
  `workbench_credential_not_found` when an explicit `credential` name is
  not an own-provider entry, and `workbench_store_unavailable` (retryable)
  for a refused/unreadable host.auth.list/get leg; pipeline failures on
  the workbench reuse the executor's codes unchanged — the "workbench"
  pins in `matrix_test.go` drive the routes offline.
- Image executor surface (`openai-image`, the image surface note): the
  host's `/v1/images/generations` and `/v1/images/edits` arrive at
  `executor.execute` / `executor.execute_stream` untranslated and are
  routed by model base name — `chatgpt-proxy/gpt-image-2-proxy` to the
  web-drawing pipeline (sentinel acquired once per request),
  `chatgpt-proxy/codex-gpt-image-2` to the codex pipeline (no sentinel,
  the stored plan). Two host-integration constraints apply behind the
  registry host: (1) image responses are ALWAYS served as inline
  `b64_json` — never a synthesized `url` — because the image pipelines
  return only raw bytes and the host re-render passes any non-empty `url`
  through verbatim, suppressing the host's own functional
  `data:image/png;base64,...` fallback, so a synthesized url would hand
  the client a dead link (`response_format:"url"` is therefore served as
  inline base64; `base-url` does not change image output); and (2)
  `message-as-error: false` makes the executor emit the prose as a
  byte-less `data[]` entry (`revised_prompt` + warning `message`) per the
  executor contract, but the host skips a byte-less entry and fails with
  `502` ("upstream did not return image output"), so behind the registry
  host that setting degrades to a 502 rather than surfacing the prose —
  use the default `true` for a clean typed 400.
- Usage counts are a documented ESTIMATE (`bytes / 4` per message): the
  upstream never returns token counts for these turns, so `prompt_tokens`
  / `completion_tokens` / `total_tokens` are heuristic and must never be
  treated as exact.
- Wire-format fidelity: upstream payloads and session headers keep the
  reference's (`basketikun/chatgpt2api`, MIT) byte-identical fingerprint
  fields — requests look like a stock web client on purpose, and the
  golden-file tests in `internal/upstream/testdata/` pin them.
- Secrets (tokens, passwords) never appear in RPC envelopes, error
  messages, logs, or quota responses; auth-file names are stable hashes.

## Deploy

The host is the EasyCLI (CLIProxyAPI v8) process embedded in SCPM's
(simple-central-proxy-manager) single `gateway` container — EasyCLI has no
port of its own, so the integration is entirely SCPM-side config + this
artifact.

1. Build the artifact (glibc `linux/amd64` is the only host-loaded target):

   ```sh
   CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared \
     -o plugins/linux/amd64/easycliproxyapi-chatgpt-provider.so .
   ```

2. Copy the artifact into EasyCLI's plugin directory on the deploy host —
   `<scpm-deploy-dir>/easycli/plugins/linux/amd64/`. That directory is
   bind-mounted into the gateway container, so a restart picks it up with no
   image rebuild.
3. Ensure the live `config.yaml` carries the plugin block; SCPM renders it
   from `easycli/config.yaml.template`. An existing `config.yaml` stays
   panel-owned, so `bash easycli/deploy.sh` installs the rendered config only
   when the file is missing (first deploy) or with `--force-config` (keeps a
   rollback copy and clobbers panel edits):

   ```yaml
   plugins:
     configs:
       easycliproxyapi-chatgpt-provider:
         enabled: true
         model-prefix: chatgpt-proxy
         image-model-name: gpt-image-2-proxy
         codex-image-model-name: codex-gpt-image-2
         drawing-upstream-model: gpt-5-5
         thinking-effort: auto
         base-url: ""
         message-as-error: true
         image-poll-timeout-secs: 120
         image-poll-interval-secs: 10
         request-timeout-secs: 600
         tls-mode: stdlib
         retry-401-refresh: true
         search-upstream-model: gpt-5-5
         search-timeout-secs: 300
         search-poll-interval-secs: 3
   ```

   The block is secrets-free on purpose: OAuth credentials come from the
   browser login in the management panel (host auth files), so there is no
   `api-keys` entry and no SCSM key to feed the plugin. Every plugin field
   is pinned at its default (see Contracts). `enabled`/`priority` are
   host-owned: CLIProxyAPI v8 injects them into the plugin's config before
   registration, so the parser tolerates and ignores them.
4. Restart the gateway; the startup log must show
   `plugin registered plugin_id=easycliproxyapi-chatgpt-provider`.
5. Sign in once with a ChatGPT consumer account through the panel's OAuth
   login: open the panel's authorize URL, approve, and when the browser
   lands on `https://platform.openai.com/auth/callback?code=...&state=...`
   copy that full address-bar URL and paste it into the panel's OAuth
   paste field — the plugin then exchanges the code. The credential
   lands in EasyCLI's auth files (`type: "chatgpt-proxy"`, stable hashed
   name) and the host pool owns it from there (rotation, retry,
   enable/disable on the panel).

The models then publish through SCPM's catalog: `routing.yaml` maps the
`chatgpt-proxy/` namespace to the `chatgpt-proxy` plan tag
(`easycli_plans.prefixes`), so after a LiteLLM restart the chat models are
addressable as `chatgpt-proxy/<vendor>/<model>:<ctx>:free` (the `openai`
vendor segment, per the text publish pattern above). The published image
names have no chat endpoint on the plan bridge and are dropped from the
chat catalog like every other plan-bridge image name.

The `.so`/`.h` artifacts are build output and are gitignored; they are never
committed.

## Build / test

```sh
go vet ./...
# Build first: tests/loadsmoke dlopen()s the artifact during go test.
CGO_ENABLED=1 go build -buildmode=c-shared \
  -o plugins/linux/amd64/easycliproxyapi-chatgpt-provider.so .
go test ./...
```

Tests are offline: no network, no credentials, no host binary. CI builds the
glibc `linux/amd64` artifact (the release gate) and then runs the same
suite. `internal/plugin/matrix_test.go` additionally pins the README
contract matrices to the dispatcher, the registration envelope and
`ParseConfig` — README drift fails CI.

`tests/live_tls_test.go` is the operator-gated TLS fingerprint spike behind
`-tags live` (runbook in `PROVENANCE.md`); its verdict decides the default
`tls-mode`. `tests/live_executor_test.go` is the operator-gated live
executor smoke, same tag. Their optional utls leg
(`-tags 'live utls'`) pins `github.com/refraction-networking/utls v1.8.2` +
`golang.org/x/net` in `go.mod`; both stay out of the default build and the
plugin `.so` until the spike's verdict says otherwise.

## Related

- Sibling plugins: `easycliproxyapi-commandcode-plugin`,
  `easycliproxyapi-opencode-plugin`, `easycliproxyapi-mistral-plugin`
- Hub page: `docs/repos/easycliproxyapi-chatgpt-plugin.md` in the public
  `docs` repo (simple-central-ai-things)
- The org folder-root `AGENTS.md` (brand rules, repo graph, machine rules)
