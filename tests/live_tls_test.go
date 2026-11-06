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
// HTTP 401, which means the supplied token is invalid or expired and the
// operator must supply a fresh one. The recorded behavior is the evidence
// for the verdict ledger in PROVENANCE.md. The shared plumbing lives in
// live_tls.go (same tag, non-test file so the utls leg can reuse it).
package tests

import (
	"net/http"
	"os"
	"testing"
)

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
	return h
}

// TestLiveTLS runs the pinned GET once per available transport leg and
// records behavior. See the file doc for the pass/fail contract.
func TestLiveTLS(t *testing.T) {
	token := os.Getenv("LIVE_ACCESS_TOKEN")
	if token == "" {
		t.Fatal("operator-gated: set LIVE_ACCESS_TOKEN to run the TLS fingerprint spike")
	}

	headers := pinnedFingerprintHeaders(token)

	outA := legOutcome{name: "(a) stdlib http.Client, default crypto/tls hello"}
	outA = getOutcome(newStdlibClient(), headers)
	recordLeg(t, outA)

	if utlsAvailable() {
		outB := legOutcome{name: "(b) utls Chrome-110-era hello, x/net/http2"}
		outB = getOutcome(newUtlsClient(), headers)
		recordLeg(t, outB)

		if outB.status == http.StatusUnauthorized {
			t.Errorf("leg %s answered 401 Unauthorized: LIVE_ACCESS_TOKEN is invalid or expired; supply a fresh token from a disposable account and rerun", outB.name)
		}
	} else {
		t.Log("leg (b) utls Chrome-110-era hello: utls leg unavailable in this build (rerun with -tags 'live utls' to link it)")
	}

	if outA.status == http.StatusUnauthorized {
		t.Errorf("leg %s answered 401 Unauthorized: LIVE_ACCESS_TOKEN is invalid or expired; supply a fresh token from a disposable account and rerun", outA.name)
	}

	t.Log("spike complete: record both legs' outcomes in PROVENANCE.md; this test is a diagnostic and passes by design")
}
