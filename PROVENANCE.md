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
