# v0.1.0 — release notes (2026-11-06)

Status: **tagged `v0.1.0`**. First release of the chatgpt-proxy provider
for the EasyCLI/CLIProxyAPI v8 host.

## Compatibility

- Initial release — there is no earlier artifact. Deploy is a copy of the
  glibc `linux/amd64` `.so` into the host's plugin directory plus the
  `plugins.configs.easycliproxyapi-chatgpt-provider` block in the live
  `config.yaml` (see README Deploy); a restart picks it up.
- Identity and contracts are pinned by tests: plugin id
  `easycliproxyapi-chatgpt-provider`, version `0.1.0`, provider key /
  model prefix / auth-file type `chatgpt-proxy`, the registry.json store
  entry, and the README contract matrices.

## Offline-verified surface

Everything below is exercised by the default `go test ./...` suite — no
network, no credentials, no host binary — and by CI, which builds the
glibc `linux/amd64` artifact and then runs the same suite, so the load
smoke `dlopen()`s the real `.so`:

- **Registration and lifecycle.** The pinned identity, the exact
  capability surface (`auth_provider`, `model_provider`, `executor`
  scope `oauth` formats `openai` + `openai-image` in and out,
  `quota_provider`), the three capability identifiers, fail-closed
  config parsing, and typed error envelopes for every refused or
  unknown method.
- **Credential lifecycle.** `auth.parse` / `auth.refresh` for
  `type: "chatgpt-proxy"` records (refresh_token rotation, plan probe
  persisted, host-managed metadata keys never clobbered) and
  `auth.login.start` / `auth.login.poll` — the browser OAuth PKCE flow
  over the host callback-file mechanism.
- **Model catalog.** Per-credential upstream `/backend-api/models`
  discovery published as `chatgpt-proxy/openai/<slug>`, the two image
  models (`chatgpt-proxy/gpt-image-2-proxy` web-drawing, always;
  `chatgpt-proxy/codex-gpt-image-2` codex, plan-gated plus/pro/team),
  `model.static` serving the last-known-good union on first boot, and
  stale-while-unavailable snapshots.
- **Quota.** `quota.describe` (static metadata, no reset),
  `quota.fetch` mapping the account-status probe to normalized native
  cards (never inventing data: no plan means no subscription, an absent
  window means no bucket), and the typed `quota.reset` refusal.
- **Text executor.** Streaming and non-streaming OpenAI-format turns
  over the sentinel/PoW/turnstile handshake and the conversation SSE
  patch protocol, the typed 401 refresh-retry loop (rotated credential
  persisted via `host.auth.save` before the single retry), tools and
  foreign-model refusals before any upstream call, the thinking-effort
  mapping, and the full error-class map (pinned in the README matrix).
- **Image executor.** `/v1/images/generations` and `/v1/images/edits`
  routed by model base name to the web-drawing and codex pipelines,
  always served as inline `b64_json`, with the `message-as-error`
  degradation documented for the registry host.
- **Wire fidelity.** Golden-file tests pin the upstream payloads and
  session-header fingerprint fields byte-identical to the reference
  (`internal/upstream/testdata/`).
- **Docs pins.** `internal/plugin/matrix_test.go` checks the README
  contract matrices (capabilities, model ids, config defaults, error
  classes) against the dispatcher, the registration envelope and
  `ParseConfig`; registry.json against the identity constants.

## Live verification — operator-gated, pending

The offline suite proves the wire contract; the live evidence is
operator-gated and **pending** at this tag:

- **TLS fingerprint spike** (`tests/live_tls_test.go`, `-tags live`) —
  decides the default `tls-mode` (currently `stdlib`). The verdict ledger
  in `PROVENANCE.md` records both legs as PENDING RUN; the runbook is in
  that file.
- **Live executor smoke** (`tests/live_executor_test.go`, `-tags live`) —
  one non-stream and one streamed turn against a deployed EasyCLI
  instance with a live credential.
- **SCPM gateway integration** — the offline SCPM-side wiring (the
  `easycli/config.yaml.template` block and the `routing.yaml` prefix
  entry, pinned by SCPM tests) landed in
  simple-central-proxy-manager commit `de90640`; the live gateway
  restart + panel login + `/v1/models` listing verification is the
  operator-gated step of that workstream.

Record live evidence in `PROVENANCE.md` (spike verdict) and the private
ops doc as it lands; the README Deploy section describes the expected
end state.

## Known gaps (Phase 2 list)

Phase 2 extensions, each independently shippable after Phase 1 proves
itself live — see the plan's Phase 2 section:

- **P2-1 `openai-response` format** — Responses API in/out over the
  conversation events (for Responses-only clients).
- **P2-2 `claude` format** — Anthropic Messages with tool-call XML
  emulation.
- **P2-3 Web search** — `executor.http_request` for the host
  `/v1/alpha/search` surface, or a `model_router` interception.
- **P2-4 Password re-login** — headless PKCE + auth-flow sentinel (FNV-1a
  PoW) + `auth0Client` header; replaces `credential_terminated`
  dead-ends.
- **P2-5 Pool imports** — `management.handle` routes for remote CPA
  servers, the sub2api admin API and raw-token paste, writing converted
  `type: "chatgpt-proxy"` records via `host.auth.save`.
- **P2-6 Drawing workbench** — a GET-only resource page shell plus
  management POST routes for generate/edit/history.
- **P2-7 Executor extensions** — parallel `n>1` generation,
  per-credential proxy fields, a `chat_completion_cache` equivalent,
  image `base_url` / retention settings.

Also not covered by v0.1.0: tool calling (refused typed with
`tools_unsupported`), and exact token counts (usage is a documented
`bytes / 4` estimate — the upstream never returns counts for these
turns).
