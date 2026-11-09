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
  model discovery, streaming text conversation, web search (cited answers),
  image generation/editing, and native subscription quota cards.
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
internal/model/               leaf package: Credential + auth-file storage
                              shape, Fingerprint, Config + ParseConfig — the
                              canonical type home (upstream consumes it too;
                              dependency direction plugin -> upstream ->
                              model; internal/plugin aliases the types)
internal/plugin/
  plugin.go                   Manager + RPC dispatch (the single seam: all
                              host I/O flows through the injected caller),
                              identity constants (pluginID, providerKey,
                              pluginVersion, githubRepo), registration
                              envelope with the pinned capability surface
  config.go / cred.go         aliases + wrappers into internal/model
  envelope.go                 ok / errEnvelope helpers with typed
                              Error{Code,Message,HTTPStatus,Retryable}
  auth.go                     auth.parse / auth.refresh: OAuth refresh_token
                              lifecycle + plan probe, fail-closed record
                              checks
  login.go                    auth.login.start / auth.login.poll: browser
                              OAuth PKCE over the host callback-file
                              mechanism (in-memory attempt store)
  password_login.go           auth.login.password: synchronous password
                              re-login (account recovery) — authorize
                              bootstrap + verify + exchange, stored like
                              the browser login (source_type "password")
  management.go               host Management API plumbing:
                              management.register (route + resource list,
                              no handler crosses the ABI) + management.handle
                              (exact-path router, 404/405 typed bodies)
                              for the import + workbench surfaces (the GET
                              resource path arrives full-path and un-
                              normalized)
  import.go                   F5 account imports: CPA codex export (file +
                              remote list/download) and sub2api admin
                              (login + paged accounts + accounts/data),
                              full-token-set capture, access_token dedupe
                              via host.auth.list/get, save through
                              saveCredential (host.auth.save), inline
                              {added, skipped, errors} summary
  workbench.go                F6 drawing workbench: the one browser
                              resource (the embedded data-free shell) +
                              the generate/edit/models management routes
                              driving the existing image pipeline
                              (credential acquired via host.auth.list/get,
                              the import's knownAccessTokens pattern;
                              n 1..4 reuses validateImageCount; synchronous
                              turns, client-side history — see README)
  workbench_shell.html        the single self-contained workbench page
                              (inline CSS/JS, DATA-FREE — pinned by test);
                              go:embed, served byte-exact, no build step
  password_sentinel.go        the auth-flow sentinel: FNV-1a-32 PoW solve +
                              sentinel/req acquisition (distinct from the
                              conversation-flow sentinel in upstream/;
                              differential-pinned to the reference
                              utils/sentinel.py)
  models.go                   per-credential model catalog (upstream
                              /backend-api/models union + image models),
                              stale-while-unavailable snapshots
  quota.go                    quota.describe / quota.fetch (account-status
                              triple probe -> native cards) / quota.reset
                              (typed refusal)
  executor.go                 executor execute/stream dispatch, 401
                              refresh-retry loop, upstream error -> envelope
                              mapping
  executor_text.go            the openai-format conversation turn
                              (resolution, function-tools emulation via
                              tools.go with the nameless-tools refusal,
                              effort mapping, usage estimate,
                              chat.completion frames)
  tools.go                    the OpenAI function-tools emulation: XML tool
                              prompt + reply parsing (ported from the MIT
                              reference's proven pattern), the agent-loop
                              folds and the buffered tool stream
  executor_image.go           the openai-image turn (route by base name,
                              generations/edits payload, inline b64_json)
  executor_responses.go       the openai-response (Responses API) turn:
                              input/instructions decode, response.completed
                              envelope + SSE event sequence, the
                              image_generation tool route onto the image
                              pipeline, function tools via the XML emulation
  executor_claude.go          the claude (Anthropic Messages) turn:
                              system/messages block-flatten decode
                              (tool_use/tool_result → text, Claude Code XML
                              rule swap), the message envelope + Anthropic
                              SSE event sequence (event:/data: framing, no
                              [DONE]), tools via the same XML emulation,
                              host-rendered Anthropic error envelopes
  executor_search.go          the web-search turn: detection (web_search
                              tools / tool_choice / web_search_options /
                              search model prefix → the search flow, else
                              the plain pipeline), the 401 refresh-retry
                              run loop, the cited-answer rendering
                              (Sources block + url_citation annotations),
                              the typed search error mapping
  matrix_test.go              pins the README contract matrices to the
                              dispatcher / registration / ParseConfig
  *_test.go                   offline unit tests against a fake caller
internal/upstream/            the upstream reverse protocol: transport seam
                              over the host callbacks, session headers,
                              sentinel/PoW/turnstile VM, conversation SSE +
                              patch protocol, poll, profile/quota probe,
                              web-drawing + codex image routes, the
                              web-search route (prepare + f/conversation
                              force_use_search + conversation poll + source
                              extraction), files trio; golden-file tests
                              (testdata/) pin the wire payloads
tests/loadsmoke/              dlopen() load smoke: the built .so through the
                              real C ABI against a fake host (offline; skips
                              when the artifact is absent; CI builds first)
tests/live_tls_test.go        operator-gated TLS fingerprint spike (-tags
                              live; verdict ledger PROVENANCE.md)
tests/live_executor_test.go   operator-gated live executor smoke (-tags live)
tests/utls_client*_test.go    utls leg of the spike (-tags 'live utls';
                              never linked into the default build)
registry.json                 host plugin-store registry entry
PROVENANCE.md                 TLS spike verdict ledger
CHANGELOG.md                  user-facing release summaries
RELEASE_NOTES.md              the v0.1.0 release notes
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
  `openai-image` + `openai-response` + `claude` in and out),
  `quota_provider`, `management_api` (turns on the host Management API for
  the import routes + the drawing workbench surface).
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

The default suite is offline; the live verification in `tests/` is
operator-gated behind `-tags live` (the TLS spike records its verdict in
`PROVENANCE.md`, the live executor smoke needs a deployed EasyCLI instance +
a live credential) and never compiles in CI. `internal/plugin/matrix_test.go`
pins the README contract matrices (capabilities, model ids, config defaults,
error classes) to the dispatcher, the registration envelope and
`ParseConfig` — when the README and the code disagree, CI is red; update
both in the same change.

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

Deploy handoff: deployment into the gateway is via the model gateway repo's
`easycli/` surface (see README Deploy).

## Conventions

- English only; no Cyrillic (scan: `grep -rnP '[\x{0400}-\x{04FF}]'`)
- Wire payloads to the upstream keep the reference's byte-identical
  fingerprint fields; golden-file tests pin them
- Provenance: the upstream wire protocol is ported from the MIT reference
  `basketikun/chatgpt2api`; a local read-only oracle clone is used during
  development (see PROVENANCE.md)
- No host-specific details in this public repo — placeholders and env var
  names only; real values live in the private ops doc
- Commit identity `hello@babico.name.tr`; commit + push after every
  meaningful change
- no-rm (move to trash, never hard-delete); no `/tmp` (use
  `~/.cache/opencode/tmp`, `TMPDIR=` prefixes for build tools)
- Org brand rules + repo graph: the folder-root `AGENTS.md`

## Related

- README.md (this repo) · CHANGELOG.md · RELEASE_NOTES.md · PROVENANCE.md
- sibling plugins: commandcode, opencode, mistral
- docs page: `docs/repos/easycliproxyapi-chatgpt-plugin.md`
- siblings: SCPM (proxy-manager), the org folder-root `AGENTS.md`
