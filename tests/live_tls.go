//go:build live

// Shared plumbing for the operator-gated TLS fingerprint spike. It is a
// non-test file (but tagged) so the utls leg implementation, which is also
// non-test, can reuse the constants and the outcome recorder. Everything
// here only compiles under -tags live, so the default build and the plugin
// .so never link any of it.
package tests

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
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

// newStdlibClient returns leg (a): a plain stdlib http.Client with the
// default crypto/tls ClientHello — the question under test.
func newStdlibClient() *http.Client {
	return &http.Client{Timeout: liveRequestTimeout}
}

// getOutcome performs the pinned GET with client and returns the observed
// outcome. Transport errors (DNS, refused, handshake) are recorded, not
// fatal: they are data for the verdict.
func getOutcome(client *http.Client, headers http.Header) legOutcome {
	req, err := http.NewRequest(http.MethodGet, upstreamModelsURL, nil)
	if err != nil {
		return legOutcome{err: err}
	}
	req.Header = headers

	resp, err := client.Do(req)
	if err != nil {
		return legOutcome{err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return legOutcome{status: resp.StatusCode, err: err}
	}
	return legOutcome{
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
