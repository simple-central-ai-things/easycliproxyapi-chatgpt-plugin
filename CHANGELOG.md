# Changelog

This file records user-facing release summaries.

## v0.2.4 — 2026-11-11 (function tools actually round-trip on GPT-6-era models)

Live diagnosis (2026-11-11): an OpenCode session on
`openai-plus-chatgpt` answered "I have no access to file-editing tools" and
simply refused — the tool emulation was not round-tripping. Two independent
breaks, both proven live against the deployed gateway (evidence in
`PROVENANCE.md`):

- **The platform strips the standard tool-call tag.** An echo probe showed
  the ChatGPT web backend removes the reference's canonical single
  tool-call token from the assistant stream — whatever the model emitted
  never reached the parser. The canonical spelling is now a SCRAMBLED
  anagram (`loot_llac`, assembled from parts in source like before); the
  parser keeps accepting the standard, `function_call` and `invoke`
  spellings defensively.
- **The reference's short "Tool use rules" block is ignored.** On
  GPT-6-era models the short contract merged into the system message
  produced hallucinated completions ("Created `a.txt`...") with no XML —
  0/11 cumulative across placements. The prompt is now the live-tuned
  variant-D master: the mechanics of caller-side execution (prose alone
  changes nothing), the scrambled protocol with its rationale, the
  per-tool schema blocks, the AFTER RESULTS answer-quality rules (clean
  prose completion, no internal markup), and the must-call-a-tool rules.
- **Recency bridge.** A master merged into the tail of a large client
  system prompt (a captured OpenCode request carries a 100KB system + 12
  tools) still diluted to 0/3; tool mode now appends a compact PROTOCOL
  REMINDER bridge to the LAST user message, which lifted the same captured
  request to 5/5.

Live verification on the deployed gateway: 3/3 parsed `tool_calls` on the
original failing scenario (non-stream and stream), 5/5 emission on the
captured OpenCode request, 5/5 clean round-trip completions, and 3/3 full
OpenCode agent runs that created the file through the real `patch`/`shell`
tools. Offline: `scrambled_contract_test.go`, `user_bridge_test.go` and the
retargeted golden pins keep the suite green; `go test ./...` + loadsmoke
pass.

## v0.2.3 — 2026-11-11 (typed 413: `payload_too_large` with the actionable message)

Live incident (2026-11-11): a long agent conversation exceeded the ChatGPT
backend's accepted payload size, and the upstream edge answered HTTP 413
before the model ever saw the request. The plugin relayed it as the generic
`upstream_status: the upstream returned HTTP 413`, which names neither the
cause nor the fix — and every retry, including context compaction, fails the
same way, because the whole OpenAI message array rides on one stateless
request.

- **`payload_too_large` (413, never retryable).** The conversation status
 mapping now types the 413 with a static message that says what happened and
 what to do: "trim or compact the context, or use a model with a larger
 limit". The typed code rides through to the RPC envelope (it no longer folds
 into `upstream_error`), the README errors matrix pins the new row, and the
 matrix + upstream suites cover it offline. A 413 means the SAME body fails
 again, so it is not retryable by construction.

## v0.2.2 — 2026-11-10 (removed the drawing workbench; HTML-safe settings text)

Operator decision: the host is a model-serving proxy, so the plugin no longer
ships a browser web console. Two changes:

- **Drawing workbench removed.** The v0.2.0 workbench resource page
 (`/v0/resource/plugins/<plugin-id>/chatgpt-proxy/workbench`) and its
 `generate` / `edit` / `models` management routes are gone. The image
 EXECUTOR stays — `/v1/images/generations` and `/v1/images/edits` (the
 `openai-image` format, both published image models) still route by model base
 name to the web-drawing and codex pipelines and return inline `b64_json`.
 That is the model-serving surface; the workbench was an out-of-place UI on
 top of it. The management route-set pin now asserts exactly the 5 import
 routes + the account-recovery route and zero browser resources.
- **HTML-safe panel settings descriptions.** The 15 registration `ConfigFields`
 descriptions no longer contain quote characters, which the host's JSON
 encoder HTML-escaped to `&#34;` and the panel rendered verbatim (garbled).
 They are now plain text (defaults and units retained), so the panel's
 plugin-settings UI renders them cleanly. (The field LABELS are the config
 key names — the host SDK's `ConfigField` has no separate title field — so
 names stay kebab-case by contract.)

## v0.2.1 — 2026-11-10 (panel surfaces: config fields + account-recovery route)

Two operator-facing additions on top of v0.2.0, so the host management panel
can drive surfaces that previously required hand-editing config or had no UI
path:

- **Registration `ConfigFields`.** The registration now advertises one
  `ConfigField` for every tunable config key `ParseConfig` accepts (15
  fields; the host-owned `enabled`/`priority` are excluded). The host's
  plugin-settings UI renders them, so operators set `tls-mode`, the search
  timeouts, `message-as-error`, etc. in the panel instead of editing the
  `plugins.configs.easycliproxyapi-chatgpt-provider` YAML block. `tls-mode`
  is the only enum field (`["stdlib","utls"]`); the rest are typed
  string/integer/boolean to match the validator. A new docs-pin test holds
  the advertised set equal to the README config matrix (both directions).
- **Account-recovery management route.** `POST chatgpt-proxy/login/password`
  (under `/v0/management/`) triggers the same password-recovery core as the
  `auth.login.password` RPC — sentinel PoW + PKCE + `password/verify` +
  `oauth/token` — so the panel can recover an account whose refresh token
  died. Body is `{email, password}` (lowercase or PascalCase); success is the
  poll-shaped reply with the recovered record persisted via `host.auth.save`
  (same stable-hash file name the RPC path converges on), failure is the typed
  error with the password never echoed. The password crosses the ABI only as
  the request input.

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
