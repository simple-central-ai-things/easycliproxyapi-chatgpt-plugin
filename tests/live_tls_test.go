//go:build live

// TestLiveTLS is the operator-gated TLS fingerprint spike that decides the
// plugin's default upstream TLS mode: does chatgpt.com accept a plain stdlib
// Go TLS ClientHello with a valid token, or does it challenge it and demand a
// browser-grade fingerprint? It runs only when an operator explicitly builds
// with -tags live AND supplies LIVE_ACCESS_TOKEN; default CI never compiles
// or runs it, so the offline suite never touches the network.
//
// The test is deliberately a diagnostic, not a gate: it records what each
// transport leg observes and passes either way — except a leg that answers
// HTTP 401 (token invalid/expired) and a run in which every leg dies before
// observing any HTTP status (zero evidence). The recorded behavior is the
// evidence for the verdict ledger in PROVENANCE.md. The spike code lives
// entirely in _test.go files so the default build and the plugin .so link
// none of it.
package tests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	// upstreamModelsURL is the pinned upstream endpoint under test: the same
	// GET the reference web client uses for model discovery.
	upstreamModelsURL = "https://chatgpt.com/backend-api/models?history_and_training_disabled=false"

	// Pinned wire-fingerprint fields, inlined here because Task 5's header
	// builder does not exist yet and this test must not depend on it.
	pinnedOAILanguage      = "zh-CN"
	pinnedAcceptLanguage   = "zh-CN,zh;q=0.9"
	pinnedOrigin           = "https://chatgpt.com"
	pinnedReferer          = "https://chatgpt.com/"
	pinnedOAIClientVersion = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	pinnedOAIBuildNumber   = "6708908"
	// pinnedUserAgent is a browser-like User-Agent matching the Chrome 110
	// line the utls leg presents.
	pinnedUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36"

	// The reference sends both target headers as the bare path template
	// (no query string) even though the fetched URL carries the query —
	// services/openai_backend_api.py list_models passes `route` to
	// _headers, which fills both.
	pinnedTargetPath  = "/backend-api/models"
	pinnedTargetRoute = "/backend-api/models"

	// Leg labels recorded with every outcome so the verdict log names its
	// evidence.
	legAName = "(a) stdlib http.Client, default crypto/tls hello"
	legBName = "(b) utls HelloChrome_106_Shuffle (closest first-handshake id to Chrome 110 in utls v1.8.2), x/net/http2"

	// liveRequestTimeout bounds one leg so a hung handshake cannot wedge the
	// spike indefinitely.
	liveRequestTimeout = 60 * time.Second

	// snippetLen is how much of each body is echoed for eyeballing.
	snippetLen = 200
)

// legOutcome captures what one transport leg observed. It never carries the
// token or any Authorization value.
type legOutcome struct {
	name                string
	status              int
	bodyLen             int
	looksLikeModelsJSON bool
	snippet             string
	err                 error
}

// pinnedFingerprintHeaders builds the exact header set the spike sends with
// every leg. The token goes in Authorization and nowhere else.
func pinnedFingerprintHeaders(token string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", pinnedUserAgent)
	h.Set("OAI-Language", pinnedOAILanguage)
	h.Set("Accept-Language", pinnedAcceptLanguage)
	h.Set("Origin", pinnedOrigin)
	h.Set("Referer", pinnedReferer)
	h.Set("OAI-Client-Version", pinnedOAIClientVersion)
	h.Set("OAI-Client-Build-Number", pinnedOAIBuildNumber)
	h.Set("X-OpenAI-Target-Path", pinnedTargetPath)
	h.Set("X-OpenAI-Target-Route", pinnedTargetRoute)
	return h
}

// newStdlibClient returns leg (a): a plain stdlib http.Client with the
// default crypto/tls ClientHello — the question under test.
func newStdlibClient() *http.Client {
	return &http.Client{Timeout: liveRequestTimeout}
}

// getOutcome performs the pinned GET with client and returns the observed
// outcome tagged with name. Transport errors (DNS, refused, handshake) are
// recorded, not fatal: they are data for the verdict.
func getOutcome(name string, client *http.Client, headers http.Header) legOutcome {
	req, err := http.NewRequest(http.MethodGet, upstreamModelsURL, nil)
	if err != nil {
		return legOutcome{name: name, err: err}
	}
	req.Header = headers

	resp, err := client.Do(req)
	if err != nil {
		return legOutcome{name: name, err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return legOutcome{name: name, status: resp.StatusCode, err: err}
	}
	return legOutcome{
		name:                name,
		status:              resp.StatusCode,
		bodyLen:             len(body),
		looksLikeModelsJSON: looksLikeModelsJSON(body),
		snippet:             snippet(body),
	}
}

// looksLikeModelsJSON is a cheap shape check: the models endpoint answers a
// JSON object with a "data" array.
func looksLikeModelsJSON(body []byte) bool {
	return len(body) > 0 && body[0] == '{' && bytes.Contains(body, []byte(`"data"`))
}

// snippet returns the first snippetLen bytes as a quoted, print-safe string.
// Quoting keeps control bytes from corrupting the test log.
func snippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) > snippetLen {
		body = body[:snippetLen]
	}
	return fmt.Sprintf("%q", body)
}

// recordLeg prints the full leg result. Nothing here may print the token.
func recordLeg(t *testing.T, out legOutcome) {
	t.Logf("leg %s", out.name)
	if out.err != nil {
		t.Logf("  transport error: %v", out.err)
		return
	}
	t.Logf("  http status:  %d", out.status)
	t.Logf("  models JSON:  %v", out.looksLikeModelsJSON)
	t.Logf("  body bytes:   %d", out.bodyLen)
	t.Logf("  snippet:      %s", out.snippet)
}

// TestLiveTLS runs the pinned GET once per available transport leg and
// records behavior. See the file doc for the pass/fail contract.
func TestLiveTLS(t *testing.T) {
	token := liveAccessToken()
	if token == "" {
		t.Fatal("operator-gated: set LIVE_ACCESS_TOKEN to run the TLS fingerprint spike")
	}

	headers := pinnedFingerprintHeaders(token)

	outA := getOutcome(legAName, newStdlibClient(), headers)
	recordLeg(t, outA)

	runnable := []legOutcome{outA}

	if utlsAvailable() {
		outB := getOutcome(legBName, newUtlsClient(), headers)
		recordLeg(t, outB)
		runnable = append(runnable, outB)
	} else {
		t.Log("leg (b) utls Chrome-110-era hello: utls leg unavailable in this build (rerun with -tags 'live utls' to link it)")
	}

	// The single deliberate gate: a 401 means the token is bad, not the
	// transport, so the run cannot be interpreted.
	for _, out := range runnable {
		if out.status == http.StatusUnauthorized {
			t.Errorf("leg %s answered 401 Unauthorized: LIVE_ACCESS_TOKEN is invalid or expired; supply a fresh token from a disposable account and rerun", out.name)
		}
	}

	// Evidence guard: if every leg died before an HTTP status was observed,
	// the run recorded nothing (e.g. an un-routable network path) and a
	// silent pass would masquerade as verdict evidence.
	if !evidenceObserved(runnable) {
		t.Fatal("all legs ended in transport errors with no HTTP status observed: no evidence recorded — check the operator network path and rerun")
	}

	t.Log("spike complete: record both legs' outcomes in PROVENANCE.md; this test is a diagnostic and passes by design")
}

// liveAccessToken reads and trims the operator-supplied token. A stray
// newline would otherwise make Go reject the Authorization header value and
// fail both legs with zero evidence.
func liveAccessToken() string {
	return strings.TrimSpace(os.Getenv("LIVE_ACCESS_TOKEN"))
}

// evidenceObserved reports whether at least one leg observed an HTTP status.
func evidenceObserved(legs []legOutcome) bool {
	for _, out := range legs {
		if out.status != 0 {
			return true
		}
	}
	return false
}

// errRoundTripper fails every request without touching the network, so the
// outcome plumbing is unit-testable offline.
type errRoundTripper struct{ err error }

func (r errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.err
}

// TestPinnedFingerprintHeaders pins the header set to the reference's
// convention — including the two target headers that must ride every
// backend-api call (finding: they were once missing from the spike).
func TestPinnedFingerprintHeaders(t *testing.T) {
	h := pinnedFingerprintHeaders("token")

	if got := h.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer token")
	}
	if got := h.Get("User-Agent"); got != pinnedUserAgent {
		t.Errorf("User-Agent = %q, want pinned value", got)
	}
	if got := h.Get("OAI-Language"); got != pinnedOAILanguage {
		t.Errorf("OAI-Language = %q, want %q", got, pinnedOAILanguage)
	}
	if got := h.Get("Accept-Language"); got != pinnedAcceptLanguage {
		t.Errorf("Accept-Language = %q, want %q", got, pinnedAcceptLanguage)
	}
	if got := h.Get("Origin"); got != pinnedOrigin {
		t.Errorf("Origin = %q, want %q", got, pinnedOrigin)
	}
	if got := h.Get("Referer"); got != pinnedReferer {
		t.Errorf("Referer = %q, want %q", got, pinnedReferer)
	}
	if got := h.Get("OAI-Client-Version"); got != pinnedOAIClientVersion {
		t.Errorf("OAI-Client-Version = %q, want pinned value", got)
	}
	if got := h.Get("OAI-Client-Build-Number"); got != pinnedOAIBuildNumber {
		t.Errorf("OAI-Client-Build-Number = %q, want %q", got, pinnedOAIBuildNumber)
	}
	// Both target headers carry the bare path template, no query string —
	// the reference's list_models convention.
	if got := h.Get("X-OpenAI-Target-Path"); got != pinnedTargetPath {
		t.Errorf("X-OpenAI-Target-Path = %q, want %q", got, pinnedTargetPath)
	}
	if got := h.Get("X-OpenAI-Target-Route"); got != pinnedTargetRoute {
		t.Errorf("X-OpenAI-Target-Route = %q, want %q", got, pinnedTargetRoute)
	}
}

// TestGetOutcomeTagsLegName pins the leg-name fix: every getOutcome return
// path must carry the leg's name, even when the request dies before any
// HTTP status.
func TestGetOutcomeTagsLegName(t *testing.T) {
	client := &http.Client{Transport: errRoundTripper{err: errors.New("offline unit test: no network")}}
	out := getOutcome("leg X", client, pinnedFingerprintHeaders("token"))
	if out.name != "leg X" {
		t.Fatalf("outcome name = %q, want %q", out.name, "leg X")
	}
	if out.err == nil {
		t.Fatal("expected the injected transport error, got nil")
	}
	if out.status != 0 {
		t.Fatalf("status = %d, want 0 for a pre-HTTP failure", out.status)
	}
}

// TestEvidenceObserved pins the all-legs-failed guard used by TestLiveTLS.
func TestEvidenceObserved(t *testing.T) {
	if evidenceObserved(nil) {
		t.Fatal("no legs at all must not count as evidence")
	}
	if evidenceObserved([]legOutcome{{name: "a", err: errors.New("died")}}) {
		t.Fatal("legs that only saw transport errors must not count as evidence")
	}
	if !evidenceObserved([]legOutcome{
		{name: "a", err: errors.New("died")},
		{name: "b", status: 403},
	}) {
		t.Fatal("a leg with an observed status is evidence")
	}
}

// TestLiveAccessTokenTrims pins the TrimSpace fix: a token with surrounding
// whitespace must come back clean.
func TestLiveAccessTokenTrims(t *testing.T) {
	t.Setenv("LIVE_ACCESS_TOKEN", "  token-with-spaces \n")
	if got := liveAccessToken(); got != "token-with-spaces" {
		t.Fatalf("liveAccessToken() = %q, want %q", got, "token-with-spaces")
	}
	t.Setenv("LIVE_ACCESS_TOKEN", "")
	if got := liveAccessToken(); got != "" {
		t.Fatalf("liveAccessToken() = %q for empty env, want empty", got)
	}
}
