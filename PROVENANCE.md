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
real CPA/sub2api credential import. **Deferred by operator decision
(2026-11-10):** the routes/RPC are implemented, offline-tested, and proven
registered + fail-closed live; the operator chose not to run a real recovery
or import, so these stay unexercised end-to-end by design (not a blocker).

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

## v0.2.2 panel-cleanup live verification (2026-11-10)

The v0.2.2 `.so` (workbench removed + HTML-safe settings descriptions) was
deployed together with the SCPM Caddyfile change that dropped the now-unused
`/v0/resource/plugins/*` proxy block (SCPM `d8bc576`). Startup log:
`plugin registered ... version=0.2.2`. Verified through the public gateway
port:

- **Workbench gone:** the resource page and all three workbench management
  routes answer 404 (the host's own 404 — the routes are no longer
  registered).
- **Settings clean:** `GET /v0/management/plugins` → the plugin entry
  carries the 15 `config_fields`, zero menus, and no HTML-escaped entities
  in the descriptions (the `&#34;` garble from v0.2.1 is gone).
- **Kept surfaces intact:** the recovery route and an import route still
  answer their typed 400 validation errors; `POST /v1/images/generations`
  (`chatgpt-proxy/gpt-image-2-proxy`, n=1, low quality) → **HTTP 200**, one
  inline `b64_json` (~1.5 MB) — the image executor regression check.

## v0.2.3 payload-too-large live verification (2026-11-11)

Live incident: a long agent conversation exceeded the ChatGPT backend's
accepted payload size; the upstream edge answered HTTP 413 before the model
saw the request, and the plugin relayed the generic
`upstream_status: the upstream returned HTTP 413` (no cause, no fix). Root
cause confirmed from the EasyCLI error-capture logs (`Content-Length` ~680 KB
and ~310 KB conversation POSTs, both refused by the upstream edge — retrying
the same body cannot succeed, so the error is correctly non-retryable).

v0.2.3 types the 413 as `payload_too_large` with an actionable message and
was deployed to the embedded EasyCLI host. Startup log:
`plugin registered ... version=0.2.3`. Verified against the running host
through EasyCLI's loopback API (`127.0.0.1:8317` inside the gateway
container):

- **Typed 413:** a synthetic oversized `chatgpt-proxy/openai/gpt-6`
  conversation POST (~520 KB filler body) → **HTTP 413**,
  `{"error":{"message":"payload_too_large: the ChatGPT backend rejected the
  request body as too large (HTTP 413) — the conversation exceeds this
  model's accepted payload size; trim or compact the context, or use a model
  with a larger limit","type":"invalid_request_error"}}`. The new code and
  message ride through the RPC envelope instead of folding into
  `upstream_error`.
- **Gateway intact:** after the in-place `.so` swap + host restart the
  gateway container stayed `healthy`; the other three provider plugins
  re-registered unchanged (commandcode 0.5.0, mistral 0.1.0, opencode 2.5.0).
- **Offline:** the upstream suite pins the stream-start 413 mapping
  (`TestStreamStart413IsPayloadTooLarge`) and `matrix_test.go` pins the new
  README errors row; `go vet` + `go test ./...` green, c-shared `.so` rebuilt
  and exercised by `tests/loadsmoke`.

## v0.2.4 tool-emulation live verification (2026-11-11)

Commit `e941219` (deployed in-place: the prior build kept as
`easycliproxyapi-chatgpt-provider.so.bak-variantD-20261111`, the v0.2.3
original as `.bak-v0.2.3-pretoolscramble-20261111`; startup log
`plugin registered ... version=0.2.4`). Symptom that triggered the work:
an OpenCode session on `openai-plus-chatgpt` refused a trivial file write
("no access to file-editing tools") and then hallucinated completions —
the emulated tools never round-tripped.

Diagnosis, all against the live gateway:

- **Echo probe:** the model reproduced a pasted XML block character for
  character (CDATA included) EXCEPT the reference's canonical single
  tool-call token, which came back as a blank line — the ChatGPT web
  backend strips it from the assistant stream. `<invoke>` /
  `<function_call>` and the scrambled anagram `loot_llac` all pass the
  backend intact.
- **System-merged short contract:** the reference's "Tool use rules"
  block (scrambled or not) merged into the system message produced 0/11
  XML emission across models (`gpt-6`, `gpt-6-mini`, `gpt-5-6`,
  `gpt-5-6-thinking`, `gpt-6-thinking`, `gpt-6-instant`), always with a
  hallucinated completion instead.
- **Placement/strength matrix (direct EasyCLI loopback, gpt-6):** short
  contract in system 0/6; long explanatory master in system 14/16
  cumulative; the live-tuned variant-D master (v1 master + AFTER RESULTS
  answer rules) 10/10 emission and 5/5 clean round-trip completions in
  the same-run head-to-head.
- **Captured OpenCode request:** a temporary debug build dumped one real
  executor request — 100KB client system prompt + 12 tools + a 4KB user
  message. Replayed through the gateway: master in the system tail alone
  0/3, master duplicated into the user tail 2/5, in both 3/5, master in
  system + the compact PROTOCOL REMINDER bridge in the last user message
  **5/5** (one turn even checked file existence via `shell` first).

Post-deploy verification:

- **Original failing scenario:** 3/3 parsed `tool_calls`
  (`finish_reason:"tool_calls"`, `name=write`,
  `arguments={"content":"helloworld","path":"a.txt"}`) on
  `openai-plus-chatgpt/openai/gpt-6:free`, non-stream; the streaming path
  emits the tool_calls delta + `[DONE]`.
- **Full OpenCode agent runs (`opencode run --auto --archived`):** 3/3
  created `a.txt` with the requested content through the real
  `patch`/`shell` tools.
- Offline: `scrambled_contract_test.go` (scrambled canonical parse /
  strip / fold + master-section pins), `user_bridge_test.go` (bridge
  placement), retargeted golden prompts; `go vet` + `go test ./...`
  green, c-shared `.so` rebuilt and exercised by `tests/loadsmoke`.

## v0.2.5 escape-artifact live verification (2026-11-12)

Commit `6620708`, deployed in-place (the v0.2.4 build kept as
`easycliproxyapi-chatgpt-provider.so.bak-variantD-20261111`; startup log
`plugin registered ... version=0.2.5`). Trigger: the interactive OpenCode
session `ses_e336ca8ffffeyecfCYlamLsNR7` ("Write helloworld to a.txt",
gpt-6 via litellm) — the v0.2.4 loop itself worked (5/5 requests emitted
tool calls, files b/c/d created), but the export showed two defects:

- On two of five turns the model wrapped the entire parameter object in
  the platform's `<escape>` markup instead of schema-named tags; the
  parser produced `{"escape": {"patchText": "*** Begin Patch..."}}` and
  OpenCode's patch tool answered "Invalid arguments" (the d.txt turn
  self-healed via the model's retry with correct tags).
- `<escape></escape>` blocks reached the client display verbatim, once as
  the ENTIRE completion answer (the b.txt turn).

Post-deploy verification:

- **Offline:** `escape_artifact_test.go` pins the sole-escape unwrap
  (JSON + CDATA + mixed-parameter negative), the prose strip (paired,
  self-closing, mid-sentence), the full-block parse, and the prompt rule;
  `go vet` + `go test ./...` green, c-shared rebuilt, loadsmoke pass.
- **Live smoke:** the original scenario answers `finish_reason:"tool_calls"`
  with `name=write`, `arguments={"content":"helloworld","path":"a.txt"}`.
- **Full OpenCode run:** one `opencode run --auto --archived` turn created
  `a.txt` with the requested content in a fresh scratch directory.

## v0.2.6 nudge-retry + all-tools coverage (2026-11-12)

Commit `c15a1a1`, deployed in-place (the v0.2.5 build kept as
`easycliproxyapi-chatgpt-provider.so.bak-v0.2.5-20261112`; startup log
`plugin registered ... version=0.2.6`). Trigger: in the interactive
OpenCode session `ses_e336ca8ffffeyecfCYlamLsNR7`, one of five tool turns
arrived with EMPTY parameters (`Arguments provided: {}`) and the client's
patch tool refused it; the model recovered once by retrying, but a later
shell call failed the same way and the turn ended without acting.

- **Offline:** `nudge_retry_test.go` scripts a two-turn upstream (first
  reply: an empty-`<parameters>` call; second: the repaired one) and pins
  the internal retry — two conversation stream starts, the nudged payload
  carrying the assistant replay + instruction, and the repaired
  `{"city":"Paris"}` arguments in the answer. Full suite green.
- **Live smoke (pre-cooldown):** the standard scenario answered
  `finish_reason:"tool_calls"` with correct write arguments.
- **All-tools runner** (`~/.cache/opencode/tmp/run-tools-test.sh`, 10 of
  the 12 captured OpenCode tools): PASS read, patch, webfetch, execute,
  skill-prompt routing; SKIP image (harness image credits); the remaining
  failures (grep/shell/subagent/skill file-move) were UPSTREAM quota
  exhaustion, not emulation faults — the EasyCLI error logs show
  `no_assistant_content: the turn completed with no visible assistant
  content (http 502)` escalating to the pool's `auth_unavailable`
  cooldown after the day's ~60 verification turns. The typed 502 +
  cooldown behavior is the plugin/pool working as designed.

Re-run once the account window resets: `zsh ~/.cache/opencode/tmp/run-tools-test.sh`
(prompt catalog: `~/.cache/opencode/tmp/chatgpt-tools-prompts.md`).

## All-tools e2e final matrix + v0.2.8 empty-reply nudge (2026-11-12)

Commit `c2d7f5a` (version 0.2.8, in-place deploy, prior build kept as
`.bak-v0.2.7-20261112`). The runner covers 10 of the 12 captured OpenCode
tools (`question` is TUI-only, `websearch` evidence is integration-dependent):
`zsh ~/.cache/opencode/tmp/run-tools-test.sh` (prompt catalog:
`~/.cache/opencode/tmp/chatgpt-tools-prompts.md`).

Three runner passes on the live gateway:

| pass | build | result |
|---|---|---|
| 1 | v0.2.5 | 5 PASS, 4 FAIL (upstream quota cooldown — typed 502 `no_assistant_content` → pool `auth_unavailable`), image SKIP |
| 2 | v0.2.6 | 6 PASS; grep/shell FAIL = fabricated-attempt prose with NO XML (third kind) |
| 3 | v0.2.7 | 7 PASS; subagent/execute FAIL = markup-only replies (`<escape></escape>`, 6-7 output tokens) stripping to an empty delivery |
| 4 | v0.2.8 | **8 PASS, 1 FAIL, 1 SKIP** |

v0.2.8 adds the third nudge kind (markup-only empty reply re-runs once);
the targeted re-test passed for both failing scenarios (subagent reported
its phrase, execute returned EXEC-OK). The one remaining failure of pass 4
(subagent) is the same markup-only shape with a 7-token zero-part reply —
both attempts of the turn came back markup-only, consistent with account
degradation after the day's ~100 verification turns rather than an
unhandled emulation path (the nudge's one-retry budget is spent per turn).
The image SKIP is harness-side: the image-generation balance is insufficient
(OpenCode credits), unrelated to the plugin.

Known residual: ~1-in-10 stochastic emission misses; the three nudge kinds
convert most of them internally. Re-run the matrix once the account window
resets to confirm the subagent cell.

## v0.2.9 escalated nudge + image cell live verification (2026-11-12)

Commit `84d4bb7` (version 0.2.9, deployed in place over v0.2.8; prior build
kept as `.bak-v0.2.8-20261112`). Gateway log confirms
`plugin registered ... version=0.2.9`.

### Subagent cell — the v0.2.8 residual FAIL, now PASS

Live evidence (2026-11-12, subagent-cell retest): the session
`ses_e312d32bcffeQTHPdp3p31DUUJ` shows `subagent -> completed` with real
directory-exploration output ("Found 2 files ... `.opencode`"). v0.2.8's
one-nudge budget was the residual failure: both attempts of the turn came
back markup-only (a 7-token, zero-part reply), so a single polite retry
was spent and the turn ended with no delivery. v0.2.9 spends a two-unit
budget (`textTurn.nudged bool` → `nudgeSpent int`, `nudgeMaxRetries = 2`);
the second nudge appends the escalation demand (`nudgeEscalated`), and the
retry wiring needed no change because `nudgeRetry` already re-enters
`drainTextCompletion`. Pinned offline by `nudge_retry_test.go` (budget
thresholds, escalation only on the second nudge, and the three-turn
scripted pipeline answering the repaired call with 3 conversation starts).
Full suite green (vet + tests + loadsmoke).

### Image cell — now PASS through the OpenCode tool (cross-repo fix in SCPM)

The `experimental_image_generate` tool now produces a real file through the
plugin's image route (`chatgpt-proxy/gpt-image-2-proxy`, published by SCPM
as `openai-plus-chatgpt/openai/gpt-image-2-proxy`). Live evidence
(2026-11-12, session dir `tools-e2e/img-sani`): the tool part is `completed`
with model `litellm/openai-plus-chatgpt/openai/gpt-image-2-proxy` and the
written artifact is a real PNG (`circle.png`, 1536x1024, ~1.1 MB) —
IMAGE-TOOL-PASS.

The last mile was NOT a plugin fault: the plugin's registry route returns a
clean `{created, data:[{b64_json, revised_prompt}], usage}` envelope
(verified by a loopback POST inside the gateway container). LiteLLM, sitting
between the plugin and OpenCode, re-serializes that through its own
`OpenAIImageResponse` model and emits explicit `null` for the gpt-image-1
envelope fields (`background`/`quality`/`size`/`output_format`) plus
`usage.output_tokens_details`. OpenCode 2.2.7's image schema models those
top-level fields as `Schema.optional` (tolerates absence, rejects an explicit
`null`) and every image package shares that schema, so the valid image was
discarded as "returned an invalid response" with no file written. The fix
lives in SCPM (`image_sanitizer`, a `litellm_settings.callbacks` post-call
hook that returns image responses with every explicit-null key removed —
`simple-central-proxy-manager@12e95cf`), verified live: an end-to-end POST
through the gateway now carries zero explicit nulls.

### All-tools matrix — pass 5 (v0.2.9)

`zsh ~/.cache/opencode/tmp/run-tools-test.sh` on the live gateway
(10 of the 12 captured OpenCode tools; `question` is TUI-only, `websearch`
evidence is integration-dependent):

| pass | build | result |
|---|---|---|
| 1-4 | v0.2.5-0.2.8 | 5→6→7→8 PASS; image SKIP; residual subagent FAIL (markup-only, 1-nudge budget) |
| 5 | v0.2.9 | **8 PASS, 2 FAIL, 0 SKIP** |

Pass 5: PASS read, glob, grep, shell, webfetch, skill, execute, **image**.
The image cell flipped from SKIP to PASS (SCPM sanitizer above). The two FAILs
(patch, subagent) returned the known degradation shapes — an empty-`<parameters>`
call and a 7-token markup-only reply — and re-run IN ISOLATION both PASS:
patch `ses_e30c9e8caffe81s9nz23W2Rb7Z` (`patch -> completed`, notes.txt carries
`UNIQUE-PATCH-1`) and subagent `ses_e30c936ecffe37mUOsSRd9BnW9` (child replies
`SUBAGENT-OK`). No quota error (typed 502 / `auth_unavailable` cooldown) appears
in the EasyCLI error logs for those turns — the failures are account
content-degradation after the day's ~100 verification turns, exhausted even the
two-unit nudge budget, not an emulation fault. Re-run the matrix once the
account window resets to confirm both cells green in sequence.


## Cross-harness verification — Claude Code + DSH (2026-11-12)

The tool-calling and subagent capabilities were verified through two more
harnesses against the same chatgpt-proxy model
(`openai-plus-chatgpt/openai/gpt-6:free`), both reaching the plugin through
SCPM's LiteLLM gateway on `:31003`. Codex was excluded by operator choice;
Antigravity is not wired to SCPM's OpenAI chat models (Gemini-based); image is
OpenCode-specific.

| harness | protocol path | tool-calling | subagent |
|---|---|---|---|
| OpenCode | openai-completions (direct) | PASS | PASS |
| Claude Code | Anthropic Messages → LiteLLM translate → openai | PASS | PASS |
| DSH | openai-completions (direct) | PASS | PASS |

Evidence (each run forced a real tool call and a delegation, checking the
executed side-effect / child reply, not model prose):

- **Claude Code** (`claude --model .../gpt-6:free -p`): a Bash tool call ran
  `echo CC-TOOL-EVIDENCE-42` and Claude Code rendered its tool result; the
  Task subagent replied `CC-SUBAGENT-OK` verbatim. This is the distinct path —
  Anthropic `tool_use` is translated by LiteLLM to an OpenAI `tools` array,
  emulated by the plugin's XML protocol, and translated back, so it proves the
  emulation survives protocol translation, not just the direct openai path.
- **DSH** (`dsh --profile headless --patch <overlay>`): a shell tool call ran
  `echo DSH-TOOL-EVIDENCE-42`; the subagent tool delegated and the child
  replied `DSH-SUBAGENT-OK` verbatim.

DSH finding (config, not plugin): the default headless profile auto-loads ~17
MCP servers whose tool schemas bloat the first request body past the chatgpt
model's accepted payload size, so the very first turn fails with a typed
`payload_too_large` (HTTP 413) before any tool call. Disabling the MCP servers
in a `--patch` overlay (`disabled: true`) makes the payload fit and both
smokes pass. The model route itself resolves correctly — the 413 is the
backend rejecting an oversized body, which is a harness-config issue, not an
emulation fault.

## Coding-task e2e — Claude Code + DSH vs OpenCode (2026-11-12, degradation-pending-retest)

A real agentic coding task (write `tri.py` + `test_tri.py`, run the test, fix
until it passes) was run against the same chatgpt model
(`openai-plus-chatgpt/openai/gpt-6:free`) through Claude Code, DSH and OpenCode,
each verified independently (a file-existence + correctness probe + a re-run of
the agent's own test — agent prose is never trusted as evidence).

| harness | result | evidence |
|---|---|---|
| OpenCode | PASS | `tri.py` + `test_tri.py` created, correctness probe OK, test exits 0 |
| Claude Code | NO-FILES | model emitted zero `tool_use` blocks; prose only |
| DSH | NO-FILES | model emitted zero `tool_use` blocks; prose only |

Diagnosis: **content degradation, not a protocol/transport fault.** The Claude
Code session transcripts contain only a single assistant text message and no
`tool_use` at all — on one run the model even referred to "the previous tool
call was not executed" with no prior call in the transcript (fabricated-attempt
shape). A protocol fault would surface as an emitted-but-rejected `tool_use` +
failed `tool_result`; that is absent. The earlier simple smokes (an explicit
`echo` Bash call and a subagent delegation) DID emit real `tool_use` in all
three harnesses, so tool emission works — only the complex multi-tool coding
task degraded into prose. DSH failing on the same openai-completions path
OpenCode passed rules out a path-specific cause.

This matches the residual documented since v0.2.6: stochastic emission misses
that grow as the account degrades (the day's ~150 verification turns). The
nudge budget targets empty-args / no-call-XML / markup-only shapes, not a model
that cleanly declines in prose, so this shape is not intercepted.

Re-test once the account window resets to give a clean per-harness coding
verdict: `zsh ~/.cache/opencode/tmp/run-coding-test.sh`.

## Cross-harness coding-task fix — structural refusal nudge (v0.2.11 + v0.2.12, 2026-11-12)

The coding-task e2e (write `tri.py` + `test_tri.py`, run, fix until passing)
initially failed on Claude Code and DSH but passed on OpenCode, in the same
run on the same account, with DSH sharing OpenCode's openai-completions path —
so the refusal was harness-context-sensitive, not a protocol or pure account
issue. The transcripts showed the failure shape precisely: the model emitted
ZERO `tool_use` blocks and answered in prose declining the tool call, then
stopped. Each retry reworded the refusal, dodging the v0.2.10/0.2.11 phrase
list.

Two reinforcing fixes, TDD'd and deployed:

- **v0.2.11** — the variant-D footer RULES and the recency-edge PROTOCOL
  REMINDER now name prose refusal ("cannot / have not executed / did not call a
  tool") as a wasted turn that must be the protocol XML; the refusal phrase
  list widened. Live: this flipped the **Claude Code** coding task from refusal
  to PASS (2 real `tool_use` Bash blocks, correct files, `test_tri.py` exits 0,
  verified independently — not trusted from the model's prose).
- **v0.2.12** — replaced the phrase whack-a-mole with a **structural detector**:
  a no-XML turn warrants the nudge when the reply pairs a tooling noun
  (`tool`/`command`/`file`/`run`/`execute`/…) with a non-completion cue
  (`can't`/`unable`/`not yet`/`not been created`/…). The *combination* signals a
  refusal, so rewordings are caught; multi-word cues keep legitimate answers
  that merely contain "not" untriggered. Live: this flipped the **DSH** coding
  task from refusal to PASS (controlled delta — v0.2.11 refused, v0.2.12 passed,
  same account minutes apart, only the trigger changed), confirming DSH's
  failure was the reworded-refusal gap, not a deeper prompt conflict.

### Full coding matrix on v0.2.12 (current, heavily-degraded account)

`run-coding-test.sh` (Claude Code → DSH → OpenCode, sequential): Claude Code
PASS (CORRECTNESS-OK, test exit 0); DSH and OpenCode produced no files. Their
failure shapes were NEW, not the refusal:

- DSH: "I'll create both files… and run the requested test command" — a
  plan-description with no tool call and no refusal cue (it sounds confident, so
  the trigger correctly does not fire).
- OpenCode: "Approve this design so I can proceed?" — an approval-seeking
  posture that stops waiting for a reply that never arrives in headless mode.

This is the degradation long-tail: a ~200-turn-degraded account emits
tool-emission failures in novel, per-run shapes (refusal, plan-description,
approval-seeking) that no fixed trigger set can enumerate without over-fitting to
today's artifacts and risking false positives on a healthy account. The
structural refusal detector (v0.2.12) addresses the dominant documented shape and
is verified on Claude Code and DSH in isolation. **A clean per-harness coding
verdict requires the account window to reset** — re-run
`zsh ~/.cache/opencode/tmp/run-coding-test.sh` and
`zsh ~/.cache/opencode/tmp/run-tools-test.sh` then.


## Model-tier is the dominant reliability variable (2026-11-13)

The cross-harness coding/tool failures were initially attributed to account
degradation. That was wrong: a live quota fetch on 2026-11-13 showed the
account is **Plus** with every rate-limit bucket at `remainingFraction: 1`
(100% remaining) — no quota wall. On that fresh account, the *same* tasks were
still stochastic with `gpt-6:free`: Claude Code coding failed, grep/subagent/
skill failed with an "affirmative intent without action" shape ("I'll move
t1.txt…" then no tool call, t1.txt still present).

Comparing the failure shapes on **`gpt-6-thinking`** (reasoning variant), same
fresh account:

| task | gpt-6:free | gpt-6-thinking |
|---|---|---|
| coding (tri.py + test) | flaky (fails in sequence) | PASS (CORRECTNESS-OK, exit 0) |
| grep | FAIL (intent, no call) | PASS |
| subagent | FAIL | PASS |
| skill safe-delete | FAIL | FAIL (1/4 residual) |

**Conclusion:** tool-emission reliability is dominated by the MODEL TIER, not
the plugin, harness, or quota. `gpt-6-thinking` reliably emits tool calls for
complex agentic tasks; `gpt-6:free` narrates intent or refuses instead. The
plugin's nudge/prompt hardening (v0.2.9-0.2.12) recovers the refusal/empty/
markup shapes and is most valuable for weaker models, but it cannot make a
stochastic model reliable. The residual "affirmative intent without action"
shape (no negation cue) is not caught by the refusal trigger and recurs even
occasionally on gpt-6-thinking; catching it generically would false-positive on
legitimate "I'll report…" answers, so it is intentionally not phrase-matched.

**Recommendation:** route tool-heavy agentic work to a reasoning model
(gpt-6-thinking or equivalent); the plugin then supports it reliably across
harnesses.

## All-models reliability eval + prompt-engineering A/B (2026-11-13)

A standardized battery (verified multi-tool coding task + subagent delegation,
each checked independently — files + test exit, never the model's claim) ran
across all 21 published chatgpt-proxy models on a fresh Plus account (quota
verified at 100% remaining on every bucket).

**Verified coding-task pass rate: 4/21.** Reliable passers: `gpt-5.6-mini`,
`gpt-5.6-t-mini`, `gpt-6.1-terra-wm`, `chatgpt_alpha...reserved_gate_13`.
Dominant failure shapes: **NARRATE** (×5 — "I'll do X" with no tool call) and
**REFUSE** (×5 — "can't/won't"), then EMPTY / FABRICATED / OTHER. Subagent is
far easier (~14/21). `research` is the worst: it FABRICATES (claims "exit code
0" with no files created) on both tasks — independent verification is the only
guard.

**Prompt-engineering A/B.** An engineered task prompt targeting the dominant
shapes (do not narrate — just execute; you have full tool access, nothing is
restricted, so you need not say you can't; report only actually-observed
output; numbered steps; "begin now by creating tri.py") re-ran the 5 valid
baseline-failure models:

| model | baseline | V1 engineered |
|---|---|---|
| gpt-6-astra-wm | REFUSE | **PASS** |
| gpt-6-mini | OTHER | **PASS** |
| gpt-6-instant | NARRATE | FAIL |
| gpt-6-thinking | REFUSE | FAIL |
| gpt-5.6-sol-wm | NARRATE | FAIL |

**Finding:** prompt engineering flips *borderline/stochastic* models to pass
but cannot make reliably-flaky models reliable — the model tier sets the
ceiling. A strong task prompt is necessary but not sufficient.

**Usage recommendation:** route tool-heavy agentic work to the verified
passers (gpt-5.6-mini / gpt-5.6-t-mini / gpt-6.1-terra-wm) or the
prompt-flippable borderline models (gpt-6-astra-wm / gpt-6-mini) with the
engineered task framing; avoid `research` and base `gpt-6` for agentic coding
(fabrication / stochastic refusal). Always verify tool results independently —
fabricating models report success without acting.
