//go:build live && !utls

// Stub of the utls leg for builds with -tags live but without the utls tag:
// leg (b) cannot run, so the test reports it unavailable instead. The real
// implementation lives in utls_client_test.go (//go:build live && utls).
package tests

import (
	"net/http"
)

// utlsAvailable reports that the utls dependency is not linked in this build.
func utlsAvailable() bool { return false }

// newUtlsClient must never be called from a stub build: callers gate on
// utlsAvailable(). It exists so live_tls_test.go compiles with either tag.
func newUtlsClient() *http.Client {
	panic("utls leg not linked: rerun with -tags 'live utls'")
}
