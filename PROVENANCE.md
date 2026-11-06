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
  `OAI-Client-Build-Number: 6708908`, a Chrome 110-line browser
  `User-Agent`, and `Authorization: Bearer <token>`.

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
| (b)   | utls `HelloChrome_106_Shuffle` (closest pinned Chrome 110 id) + x/net/http2 | PENDING RUN | PENDING RUN | PENDING RUN | see dependency note below |

## Verdict

Chosen default TLSMode: `stdlib`

> The operator updates this line after running the spike and records the
> rationale in the table above:
>
> - **(a) returns 200 with models JSON** → stdlib TLS is accepted; keep
>   `tls-mode: stdlib` and prune the utls/x-net experiment from `go.mod`
>   (keep `tests/utls_client*.go`, still tag-gated — documented here as
>   unused-but-retained in case the verdict is ever revisited).
> - **(a) is challenged (403 / Cloudflare interstitial) while (b) returns
>   200** → set the line above to `utls` and keep the pinned dependencies
>   below. The plugin's direct-client utls path reads the host proxy from
>   `HostConfigSummary.ProxyURL` (per the plan's global constraints).
> - **both legs fail** → the token or the operator's network path is wrong;
>   rerun before changing anything.

## Dependencies pinned for this spike

- `github.com/refraction-networking/utls v1.8.2` — pure Go; imported only by
  the utls-tagged leg (`tests/utls_client.go`) and never linked into the
  default build or the plugin `.so`. v1.8.2 has no `HelloChrome_110`
  constant and its 112/114/115 Chrome ids are PSK resumption variants
  unsuitable for a first handshake, so leg (b) pins
  `utls.HelloChrome_106_Shuffle` — the closest pinned Chrome 110 id
  (Chrome 106 already sends the shuffled-extension layout the 110 line
  kept).
- `golang.org/x/net v0.57.0` — `http2.Transport` with a custom `DialTLS` for
  the same leg (the stdlib's bundled http2 transport is not importable, and
  `net/http.Transport` cannot speak h2 over a non-`*tls.Conn`; the version is
  the graph maximum, already required by the SDK).
