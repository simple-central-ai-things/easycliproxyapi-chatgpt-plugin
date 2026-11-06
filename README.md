# easycliproxyapi-chatgpt-plugin

A Go `c-shared` plugin (a `.so`) compiled INTO the EasyCLI/CLIProxyAPI v8
host that exposes ChatGPT consumer-account capabilities — streaming text
conversation and image generation/editing — as the OpenAI-compatible
provider **`chatgpt-proxy`**.

## What

- Provider key, model prefix and auth-file type: `chatgpt-proxy`.
- Plugin identity (the `.so` basename and registration `Name`):
  `easycliproxyapi-chatgpt-provider`.
- Advertised capabilities: `auth_provider` (browser OAuth + refresh-token
  lifecycle), `model_provider` (per-credential upstream catalog),
  `executor` (formats `openai` and `openai-image`), and `quota_provider`
  (native host panel cards).

## Why

ChatGPT subscription models are normally served by the browser-based
`chatgpt.com` surface, which needs a full browser session. This plugin turns
each ChatGPT consumer account into a credential the EasyCLI host can manage
— rotation, retry, enable/disable on the host's own panel — and serves it
through the gateway's existing model routing. No extra container, no extra
published port: the plugin loads inside the host process.

The v0.1.0 surface: the credential lifecycle (auth parse/refresh, browser
OAuth login with the plan persisted at login and refresh), the per-credential
model catalog, the quota cards, the OpenAI-format text conversation executor
(streaming and non-streaming) and the `openai-image` executor surface (image
generation and editing routed by model base name to the web-drawing and
codex pipelines). Known gaps are the Phase 2 list in `RELEASE_NOTES.md`.

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
                              per-credential model catalog, quota cards,
                              text + image executors, envelope helpers,
                              config/credential aliases
internal/upstream/            the upstream reverse protocol: transport seam
                              over the host callbacks, session headers,
                              sentinel/PoW/turnstile VM, conversation SSE +
                              patch protocol, poll, profile/quota probe,
                              web-drawing + codex image routes, files trio;
                              golden-file tests pin the wire payloads
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
RELEASE_NOTES.md              the v0.1.0 release notes
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
  `name=easycliproxyapi-chatgpt-provider`, `version=0.1.0`, provider key
  `chatgpt-proxy`, `GitHubRepository` = this repo (the host `validPlugin`
  gate rejects an empty value). The `registry.json` store entry carries the
  same id, version and repository URL.
- Registration capabilities:

<!-- matrix:capabilities -->
| Capability | Advertised | Executor scope | Formats in/out |
| --- | --- | --- | --- |
| `auth_provider` | yes | — | — |
| `model_provider` | yes | — | — |
| `executor` | yes | `oauth` | `openai`, `openai-image` |
| `quota_provider` | yes | — | — |
<!-- /matrix:capabilities -->

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
<!-- /matrix:config -->

  Config parsing is fail-closed and rejects unknown keys — except the
  host-owned `enabled` and `priority`, which CLIProxyAPI v8 injects into
  every plugin's config before registration (accepted, ignored).
- Credential records (host auth files, `type: "chatgpt-proxy"`):
  `access_token`, `refresh_token`, `id_token`, `email`, `user_id`,
  `account_id`, `plan`, `source_type`, the `fp` fingerprint block
  (`user_agent`, `impersonate`, `oai_device_id`, `oai_session_id`) and
  `last_refresh_at` / `last_refresh_error`. `plan` is
  `free|plus|pro|team|enterprise` (`""` = unknown) and is persisted at
  login and refreshed on every token refresh; `source_type` is
  `web|codex|oauth_login`. Auth-file names are stable hashes — never an
  email. Host-managed keys (`label`, `disabled`, `priority`, ...) travel
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
- Executor surface (text, `openai` format): conversation turns arrive as
  `executor.execute` / `executor.execute_stream` in the `openai` format.
  Model resolution strips the `chatgpt-proxy/openai/` prefix to the
  upstream slug; anything else (the image models, a foreign id) is refused
  with typed `400 model_unknown`. Any non-empty `tools` array is refused
  with typed `400 tools_unsupported` before any upstream call.
  `thinking_effort` / `reasoning_effort` map per the upstream table (none
  / auto omitted, low/medium/high as-is, xhigh/extended to
  `extended`). A `401` on the turn start forces the internal token
  refresh; the rotated record is persisted back to the auth file via
  `host.auth.save` BEFORE the single retry (an OpenAI refresh rotates the
  refresh token, so an unpersisted refresh would break the host's next
  scheduled `auth.refresh`); a failed refresh, a failed save, or a second
  `401` ends in typed `401 invalid_token`.
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
<!-- /matrix:errors -->

  `rate_limited` carries `Retry-After` when upstream sends it;
  `upstream_stream_timeout` (504) fires on the SSE hard cap — the turn
  may still be running upstream; `upstream_error` is the transport
  fallback shape (a typed upstream status rides through otherwise);
  `no_assistant_content` — a completed turn with no visible assistant
  message is always typed 502, never an empty 200; `unsupported` is the
  `quota.reset` refusal (the upstream windows are rate limits that reset
  on their own schedule).
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
