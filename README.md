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

The protocol implementation lands incrementally: the credential lifecycle
(auth parse/refresh, browser OAuth login), the per-credential model catalog,
the quota cards, the OpenAI-format text conversation executor (streaming and
non-streaming) and the `openai-image` executor surface (image generation and
editing routed by model base name to the web-drawing and codex pipelines) are
in place. See `docs/tasks/` for the workstream state.

## Layout

```text
main.go                       CGO glue only: exports cliproxy_plugin_init /
                              cliproxyPluginCall / cliproxyPluginFree /
                              cliproxyPluginShutdown; forwards every RPC to
                              internal/plugin.Manager.Handle (with the 2^31
                              size_t guard, maxRequestLen)
internal/plugin/
  plugin.go                   Manager + RPC dispatch, identity constants,
                              registration envelope
  config.go                   fail-closed YAML config (ParseConfig)
  envelope.go                 ok / errEnvelope helpers (typed Error with
                              code, message, http_status, retryable)
  *_test.go                   offline unit tests against a fake host caller
tests/loadsmoke/              dlopen() load smoke: the built .so through the
                              real C ABI against a fake host (skips when the
                              artifact is absent; CI builds it first)
registry.json                 host plugin-store registry entry
plugins/<os>/<arch>/          gitignored local build output (*.so/*.h)
.github/workflows/tests.yml   CI: go vet + go test + glibc c-shared build
                              (blocking) + loadsmoke
```

## Contracts

- Plugin identity (pinned by `internal/plugin/plugin_test.go`):
  `name=easycliproxyapi-chatgpt-provider`, `version=0.1.0`, provider key
  `chatgpt-proxy`, `GitHubRepository` = this repo (the host `validPlugin`
  gate rejects an empty value).
- Registration capabilities: `auth_provider`, `model_provider`, `executor`
  (model scope `oauth`, formats `openai` + `openai-image`), `quota_provider`.
- Host config block (EasyCLI `config.yaml`):
  `plugins.configs.easycliproxyapi-chatgpt-provider` with keys
  `tls-mode` (`stdlib`|`utls`, default `stdlib`), `model-prefix` (default
  `chatgpt-proxy`), `image-model-name` (default `gpt-image-2-proxy`),
  `codex-image-model-name` (default `codex-gpt-image-2`),
  `drawing-upstream-model` (default `gpt-5-5`), `thinking-effort`
  (default `auto`), `base-url` (default empty — image responses are always
  served as inline `b64_json` — see the image surface note below),
  `message-as-error` (default `true`; see the image surface note below),
  `image-poll-timeout-secs` (default `120`), `image-poll-interval-secs`
  (default `10`), `request-timeout-secs` (default `600`), and
  `retry-401-refresh` (default `true`). Config parsing is fail-closed and
  rejects unknown keys — except the host-owned `enabled` and `priority`,
   which CLIProxyAPI v8 injects into every plugin's config before
   registration (see Deploy); those are accepted and ignored.
- Client surface: models publish as `chatgpt-proxy/<model>`; image models as
  `chatgpt-proxy/gpt-image-2-proxy` and `chatgpt-proxy/codex-gpt-image-2`.
- Executor surface: text conversation turns arrive as `executor.execute` /
  `executor.execute_stream` in the `openai` format. Model resolution strips
  the `chatgpt-proxy/openai/` prefix to the upstream slug; anything else
  (the image models, a foreign id) is refused with typed `400
  model_unknown`. Any non-empty `tools` array is refused with typed `400
  tools_unsupported` before any upstream call. `thinking_effort` /
  `reasoning_effort` map per the upstream table (none/auto omitted,
  low/medium/high as-is, xhigh/extended → `extended`). A `401` on the turn
  start forces the internal token refresh; the rotated record is persisted
  back to the auth file via `host.auth.save` BEFORE the single retry (an
  OpenAI refresh rotates the refresh token, so an unpersisted refresh would
  break the host's next scheduled `auth.refresh`); a failed refresh, a failed
  save, or a second `401` ends in typed `401 invalid_token`. Error classes:
  `invalid_token` 401, `upstream_stream_timeout` 504 (the turn may continue
  upstream), `content_policy_violation` 400, `rate_limited` 429 retryable
  (carrying `Retry-After`), `no_available_model` 404, `tools_unsupported`
  400, `model_unknown` 400, `no_assistant_content` 502 — a completed turn
  with no visible assistant message is always typed 502, never an empty 200.
- Image executor surface (`openai-image`, the image surface note): the host's
  `/v1/images/generations` and `/v1/images/edits` arrive at
  `executor.execute` / `executor.execute_stream` untranslated and are routed by
  model base name — `chatgpt-proxy/gpt-image-2-proxy` to the web-drawing
  pipeline (sentinel acquired once per request), `chatgpt-proxy/codex-gpt-image-2`
  to the codex pipeline (no sentinel, the stored plan). Two host-integration
  constraints apply behind the registry host: (1) image responses are ALWAYS
  served as inline `b64_json` — never a synthesized `url` — because the image
  pipelines return only raw bytes and the host re-render passes any non-empty
  `url` through verbatim, suppressing the host's own functional
  `data:image/png;base64,...` fallback, so a synthesized url would hand the
  client a dead link (`response_format:"url"` is therefore served as inline
  base64; `base-url` does not change image output); and (2) `message-as-error:
  false` makes the executor emit the prose as a byte-less `data[]` entry
  (`revised_prompt` + warning `message`) per the executor contract, but the host
  skips a byte-less entry and fails with `502` ("upstream did not return image
  output"), so behind the registry host that setting degrades to a 502 rather
  than surfacing the prose — use the default `true` for a clean typed 400.
- Usage counts are a documented ESTIMATE (`bytes / 4` per message): the
  upstream never returns token counts for these turns, so `prompt_tokens` /
  `completion_tokens` / `total_tokens` are heuristic and must never be
  treated as exact.
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
   from `easycli/config.yaml.template` (`bash easycli/deploy.sh` re-renders
   and restarts the gateway only when the config changed):

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
   login. The credential lands in EasyCLI's auth files
   (`type: "chatgpt-proxy"`, stable hashed name) and the host pool owns it
   from there (rotation, retry, enable/disable on the panel).

The models then publish through SCPM's catalog: `routing.yaml` maps the
`chatgpt-proxy/` namespace to the `chatgpt-proxy` plan tag
(`easycli_plans.prefixes`), so after a LiteLLM restart the chat models are
addressable as `chatgpt-proxy/<vendor>/<model>:<ctx>:free`. The published
image names have no chat endpoint on the plan bridge and are dropped from
the chat catalog like every other plan-bridge image name.

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
glibc `linux/amd64` artifact (the release gate) and then runs the same suite.

`tests/live_tls_test.go` is the operator-gated TLS fingerprint spike behind
`-tags live` (runbook in `PROVENANCE.md`). Its optional utls leg
(`-tags 'live utls'`) pins `github.com/refraction-networking/utls v1.8.2` +
`golang.org/x/net` in `go.mod`; both stay out of the default build and the
plugin `.so` until the spike's verdict says otherwise.

## Related

- Sibling plugins: `easycliproxyapi-commandcode-plugin`,
  `easycliproxyapi-opencode-plugin`, `easycliproxyapi-mistral-plugin`
- The org folder-root `AGENTS.md` (brand rules, repo graph, machine rules)
