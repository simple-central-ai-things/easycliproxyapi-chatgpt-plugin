# AGENTS.md — easycliproxyapi-chatgpt-plugin

A Go `c-shared` plugin (a `.so`) that is compiled INTO the
EasyCLI/CLIProxyAPI v8 host — it exposes ChatGPT consumer-account
capabilities as provider `chatgpt-proxy` on the EasyCLI instance embedded in
SCPM's single `gateway` container (`easycli/`). EasyCLI has no published port
of its own: LiteLLM reaches its `/v1` API on loopback and Caddy serves its
panel/management paths same-origin on the one gateway port.

## What / why

- ChatGPT consumer accounts (OAuth + refresh-token lifecycle) are served as a
  first-class EasyCLI provider: browser OAuth login, per-credential upstream
  model discovery, streaming text conversation, image generation/editing, and
  native subscription quota cards.
- Each account is a host auth file (`type: "chatgpt-proxy"`, stable hash
  name, never an email). The host AuthManager owns the account pool —
  rotation, retry, enable/disable via the EasyCLI panel. There is no
  plugin-owned page.
- Upstream traffic goes through the host callbacks (`host.http.do` /
  `host.http.do_stream`) so host proxy settings apply; a direct-client
  fallback exists only for the `utls` TLS mode, taking the proxy from the
  host config summary.

## Naming exception

This repo intentionally keeps the functional
`easycliproxyapi-<provider>-plugin` name — no `simple-central-` prefix, no SC
short name. The plugin id (`easycliproxyapi-chatgpt-provider`) and the
registration `GitHubRepository` are bound to the binary and to the repo the
host plugin store polls; renaming would break updates. See the org
`AGENTS.md` functional-name exception.

## Layout

```text
main.go                       CGO glue only: exports cliproxy_plugin_init /
                              cliproxyPluginCall / cliproxyPluginFree /
                              cliproxyPluginShutdown; forwards every RPC to
                              internal/plugin.Manager.Handle (with the 2^31
                              size_t guard, maxRequestLen)
internal/plugin/
  plugin.go                   Manager + RPC dispatch (the single seam: all
                              host I/O flows through the injected caller),
                              identity constants (pluginID, providerKey,
                              pluginVersion, githubRepo), registration
                              envelope with the pinned capability surface
  config.go                   fail-closed YAML config: ParseConfig applies
                              defaults, rejects unknown keys, validates
                              tls-mode / model-prefix / timeouts
  envelope.go                 ok / errEnvelope helpers with typed
                              Error{Code,Message,HTTPStatus,Retryable}
  *_test.go                   offline unit tests against a fake caller
tests/loadsmoke/              dlopen() load smoke: the built .so through the
                              real C ABI against a fake host (offline; skips
                              when the artifact is absent; CI builds first)
registry.json                 host plugin-store registry entry
plugins/<os>/<arch>/          gitignored local build output (*.so/*.h)
.github/workflows/tests.yml   CI: go vet + go test + glibc c-shared build
                              (blocking gate) + loadsmoke
```

## Contracts

- Plugin identity (pinned by `internal/plugin/plugin_test.go`):
  `name=easycliproxyapi-chatgpt-provider`, `version=0.1.0`, provider key
  `chatgpt-proxy`,
  `GitHubRepository=https://github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin`
  (the host `validPlugin` gate rejects an empty value).
- Registration capabilities (exact surface): `auth_provider`,
  `model_provider`, `executor` (model scope `oauth`, formats `openai` +
  `openai-image` in and out), `quota_provider`.
- Provider identifiers: `auth.identifier`, `executor.identifier` and
  `quota.identifier` all return `{identifier:"chatgpt-proxy"}`.
- Host config block (EasyCLI `config.yaml`):
  `plugins.configs.easycliproxyapi-chatgpt-provider` — keys and defaults per
  README Contracts. Parsing is fail-closed; unknown keys are rejected except
  the host-owned `enabled`/`priority`, which CLIProxyAPI v8 injects into
  every plugin's config before registration (accepted, ignored).
- Model namespace: `chatgpt-proxy/<model>`; published image models
  `chatgpt-proxy/gpt-image-2-proxy` (web-drawing route) and
  `chatgpt-proxy/codex-gpt-image-2` (codex route). The `-proxy` suffix is
  deliberate: the host's built-in executor claims base name `gpt-image-2`
  as a hardcoded codex model, so the unique base name keeps image calls on
  the registry path.
- Secrets never cross the ABI except into the host's own credential store:
  no token bytes in RPC envelopes, error messages, logs, or quota responses.

## Build / test

Requires Go ≥ 1.26 (see `go.mod` / CI — CI pins `1.26.7`, matching the
sibling commandcode workflow), CGO, and a glibc (Debian/Ubuntu) host — the
EasyCLI runtime image is Debian-based, so a musl `.so` fails to `dlopen`.

```sh
go vet ./...
# Build first: tests/loadsmoke dlopen()s the artifact during go test.
CGO_ENABLED=1 go build -buildmode=c-shared \
  -o plugins/linux/amd64/easycliproxyapi-chatgpt-provider.so .
go test ./...
```

`linux/amd64` is the primary/release-gate target — the only artifact the
embedded EasyCLI host loads. The `.so`/`.h` artifacts are gitignored; CI
builds the c-shared artifact on `ubuntu-latest` and then runs vet + tests, so
`tests/loadsmoke` exercises the real `.so` in CI. Tests are pure — no
network, no EasyCLI binary, no secrets.

## Deploy

EasyCLI is embedded in SCPM's single `gateway` container (`easycli/`), with
no published port of its own; Caddy fronts its panel/management paths on the
one gateway port.

1. Copy the artifact into the host's plugin directory
   (`<plugins>/linux/amd64/easycliproxyapi-chatgpt-provider.so`).
2. Ensure the `plugins.configs.easycliproxyapi-chatgpt-provider` block is in
   the live `config.yaml` (see README).
3. Restart the host; the log must show
   `plugin registered plugin_id=easycliproxyapi-chatgpt-provider`.

## Conventions

- English only; no Cyrillic (scan: `grep -rnP '[\x{0400}-\x{04FF}]'`)
- Wire payloads to the upstream keep the reference's byte-identical
  fingerprint fields; golden-file tests pin them
- No host-specific details in this public repo — placeholders and env var
  names only; real values live in the private ops doc
- Commit identity `hello@babico.name.tr`; commit + push after every
  meaningful change
- no-rm (move to trash, never hard-delete); no `/tmp` (use
  `~/.cache/opencode/tmp`, `TMPDIR=` prefixes for build tools)
- Org brand rules + repo graph: the folder-root `AGENTS.md`

## Related

- README.md (this repo) · sibling plugins: commandcode, opencode, mistral
- docs page: `docs/repos/easycliproxyapi-chatgpt-plugin.md` (added by the
  docs workstream)
- siblings: SCPM (proxy-manager), the org folder-root `AGENTS.md`
