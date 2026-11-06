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

This repository currently contains the ABI skeleton (registration,
identifiers, config, dispatch, and the offline test harness). The protocol
implementations land incrementally in later tasks; see `docs/tasks/` for the
workstream state.

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
  (default `auto`), `base-url` (default empty — image `url` responses then
  fall back to `b64_json`), `message-as-error` (default `true`),
  `image-poll-timeout-secs` (default `120`), `image-poll-interval-secs`
  (default `10`), `request-timeout-secs` (default `600`), and
  `retry-401-refresh` (default `true`). Config parsing is fail-closed and
  rejects unknown keys.
- Client surface: models publish as `chatgpt-proxy/<model>`; image models as
  `chatgpt-proxy/gpt-image-2-proxy` and `chatgpt-proxy/codex-gpt-image-2`.
- Secrets (tokens, passwords) never appear in RPC envelopes, error
  messages, logs, or quota responses; auth-file names are stable hashes.

## Deploy

1. Build the artifact (glibc `linux/amd64` is the only host-loaded target):

   ```sh
   CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared \
     -o plugins/linux/amd64/easycliproxyapi-chatgpt-provider.so .
   ```

2. Copy the artifact into the host's plugin directory
   (`<plugins>/linux/amd64/easycliproxyapi-chatgpt-provider.so`).
3. Ensure the `plugins.configs.easycliproxyapi-chatgpt-provider` block is in
   the live host `config.yaml` (see Contracts).
4. Restart the host; the log must show
   `plugin registered plugin_id=easycliproxyapi-chatgpt-provider`.

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

## Related

- Sibling plugins: `easycliproxyapi-commandcode-plugin`,
  `easycliproxyapi-opencode-plugin`, `easycliproxyapi-mistral-plugin`
- The org folder-root `AGENTS.md` (brand rules, repo graph, machine rules)
