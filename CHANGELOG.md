# Changelog

This file records user-facing release summaries.

## v0.1.0 — 2026-11-06 (initial release)

First tagged release of the chatgpt-proxy provider: a Go `c-shared` plugin
compiled into the EasyCLI/CLIProxyAPI v8 host that serves ChatGPT
consumer-account credentials as provider `chatgpt-proxy`.

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
