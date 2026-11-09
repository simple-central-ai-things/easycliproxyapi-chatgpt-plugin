# Changelog

This file records user-facing release summaries.

## v0.2.0 — 2026-11-09 (Phase 2: full reference feature parity)

Second release of the chatgpt-proxy provider: the six Phase 2 features ported
from the MIT reference, each independently reviewed. The registration now
advertises `openai-response` and `claude` executor formats and the
`management_api` capability.

- **Responses API format (`openai-response`).** `/v1/responses` in and out
  over the same conversation pipeline: `input` (string or item list) +
  `instructions` decode to messages; non-stream returns the
  `response.completed` envelope; streaming emits the Responses SSE sequence
  (`response.created` → `output_item.added` → `output_text.delta`* →
  `response.completed`). The `image_generation` tool routes through the image
  pipelines and emits `image_generation_call` items with a base64 `result`;
  function tools reuse the XML emulation.
- **Anthropic Messages format (`claude`).** `/v1/messages` in and out:
  `system`/message content blocks decode (tool_use/tool_result flatten to
  text, Claude Code rule-swap), tools reuse the XML emulation, the reply
  parses back into `tool_use` content blocks; non-stream returns the
  `message` envelope (`stop_reason`, `toolu_<uuid>` ids); streaming emits the
  Anthropic SSE sequence (`event:/data:` framing, no `[DONE]`). The host's
  Claude handler renders the typed plugin error as the Anthropic error
  envelope.
- **Web search (cited answers).** A request carrying a `web_search`
  (`_preview`) tool with no other tools, a `web_search` `tool_choice`, a
  `web_search_options` block, or a search model prefix routes through the
  upstream search flow (`system_hints:["search"]`, `force_use_search`,
  conversation poll with the finished-status / stable-text stop condition)
  instead of plain conversation, and returns the answer with a numbered
  `Sources:` block + `url_citation` annotations in every text format. New
  config keys `search-upstream-model` (default `gpt-5-5`),
  `search-timeout-secs` (300) and `search-poll-interval-secs` (3); new error
  classes `search_poll_timeout` and `search_response_malformed`.
- **Password re-login (account recovery).** New synchronous
  `auth.login.password` RPC recovers an account whose refresh token died:
  auth-flow sentinel (FNV-1a-32 proof-of-work, differentially pinned to the
  reference) + PKCE S256 → `POST /api/accounts/password/verify` (JSON
  `{"password":…}`, email as `login_hint`) → `continue_url` code extraction
  (an `email_otp_verification` target is refused typed) →
  `POST /api/accounts/oauth/token`. The recovered triple stores in the same
  shape a browser login produces (`source_type:"password"`); the password
  crosses the ABI only as the RPC input and never reaches logs, envelopes or
  stored records.
- **CPA / sub2api account imports.** The registration now advertises
  `management_api`; five exact-path host Management API routes import
  ChatGPT accounts and write them via `host.auth.save` as `type:"chatgpt-proxy"`
  records (`source_type:"codex"`, the full token set the source provides,
  stable-hash file names, dedupe by access_token): `POST
  chatgpt-proxy/import/cpa-file` (inline codex export), `GET/POST
  chatgpt-proxy/import/cpa-remote[/files]` (a remote CPA management server),
  and `GET/POST chatgpt-proxy/import/sub2api[/accounts]` (a sub2api admin).
  The import secret_key/password/JWT never leaves the outbound transport.
- **Drawing workbench.** A browser-navigable resource page under
  `/v0/resource/plugins/<plugin-id>/` (a data-free, dependency-free HTML/JS
  shell) plus `POST chatgpt-proxy/workbench/generate`, `POST
  chatgpt-proxy/workbench/edit` and `GET chatgpt-proxy/workbench/models`
  management routes that drive the existing image pipeline (credential
  selected from the host store, `n` 1..4, inline `b64_json` results, history
  kept client-side).

Deployment: as v0.1.0 — copy the rebuilt glibc `linux/amd64` `.so` into the
host's plugin directory and restart; the new routes/formats appear on
registration. The README contract matrices (capabilities, formats, config,
error classes, import + workbench routes) are pinned by
`internal/plugin/matrix_test.go` and grew with this release.

## v0.1.0 — 2026-11-09 (initial release + live fixes)

First tagged release of the chatgpt-proxy provider: a Go `c-shared` plugin
compiled into the EasyCLI/CLIProxyAPI v8 host that serves ChatGPT
consumer-account credentials as provider `chatgpt-proxy`.

- OpenAI function tools are now emulated on the text executor instead of
  refused. A well-formed `tools` array is rendered into the conversation as
  an XML tool prompt; the reply is parsed back into OpenAI `tool_calls`
  (`finish_reason:"tool_calls"`, fresh `call_<uuid>` ids, JSON-string
  arguments, markup-stripped content), `role:"tool"` results and prior
  assistant `tool_calls` round-trip into the conversation, and streaming
  with tools is buffered (role + finish frames + `[DONE]`, not token-
  streamed). Typed `400 tools_unsupported` is kept only for tools arrays
  where no tool carries a usable function name.
- Fixed: the first real text turn on accounts that receive a turnstile
  challenge failed with `turnstile_unsolved` (HTTP 502). The live challenges
  are self-modifying multi-stage programs — stage 1 parses a new program
  into the data slot the VM executes, and a later stage dumps the
  fingerprint map — while the reference solver only ever executes the
  initial program and cannot serialize its OrderedMap. The VM now executes
  each replacement program from its first token (bounded chain) and op 15
  serializes OrderedMap contents (JSON.stringify semantics). Pinned by the
  real-challenge fixture `r01-live-two-stage`; verified end-to-end against a
  live account (200 + assistant content).
- Added: an operator-only turnstile capture hook — set
  `CHATGPT_PROXY_DEBUG_TURNSTILE` to a writable directory and every solve
  appends one JSON capture (dx, XOR key, decoded program, execution trace,
  failure code; never the produced token). Unset = fully inert.
- OAuth login pins the registered redirect `https://platform.openai.com/auth/callback`
  (the platform client rejects arbitrary loopback redirect URIs); the user
  copies the address-bar callback URL into the EasyCLI panel's OAuth field.

- Plugin identity `easycliproxyapi-chatgpt-provider`, version `0.1.0`,
  provider key / model prefix / auth-file type `chatgpt-proxy`; the host
  plugin store polls this repo (`registry.json`).
- Registration surface: `auth_provider` (browser OAuth login +
  refresh-token lifecycle, plan persisted at login and refresh),
  `model_provider` (per-credential upstream catalog + image models,
  stale-while-unavailable), `executor` (formats `openai` and
  `openai-image`, model scope `oauth`), `quota_provider` (native panel
  cards from the account-status probe; `quota.reset` refused typed).
- Text conversation executor: streaming and non-streaming OpenAI-format
  turns over the sentinel handshake + conversation SSE patch protocol,
  with the typed 401 refresh-retry loop (rotated credential persisted via
  `host.auth.save` before the single retry) and the full error-class map.
- Image executor: `/v1/images/generations` and `/v1/images/edits` routed
  by model base name — `chatgpt-proxy/gpt-image-2-proxy` (web-drawing) and
  `chatgpt-proxy/codex-gpt-image-2` (codex, plus/pro/team-gated); image
  responses are always inline `b64_json`.
- README contract matrices (capabilities, published model ids, config
  defaults, error classes) pinned to the dispatcher, the registration
  envelope and `ParseConfig` by `internal/plugin/matrix_test.go` — README
  drift fails CI.
- Release artifacts: `RELEASE_NOTES.md`, this `CHANGELOG.md`, tag
  `v0.1.0`. See `PROVENANCE.md` for the operator-gated TLS spike ledger
  and the live-verification status.
