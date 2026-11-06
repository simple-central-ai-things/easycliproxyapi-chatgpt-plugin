package upstream

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// The pinned upstream wire fingerprint. These constants are byte-identical to
// the reference web client (basketikun/chatgpt2api, MIT) and are pinned by
// TestSessionHeadersGolden: changing any value here changes the wire format
// and the golden test fails loudly. The Sec-Ch-Ua family carries the
// reference's Edge-143 desktop defaults; per-credential OAI-Device-Id /
// OAI-Session-Id come from the credential fingerprint instead.
const (
	chatGPTBaseURL = "https://chatgpt.com"

	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"

	defaultSecChUa            = `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`
	defaultSecChUaFullVersion = `"143.0.3650.96"`
	defaultSecChUaFullList    = `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`
	defaultSecChUaMobile      = `?0`
	defaultSecChUaPlatform    = `"Windows"`
	defaultSecChUaPlatformV   = `"19.0.0"`

	pinnedAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"
	pinnedOAILanguage    = "zh-CN"
	pinnedClientVersion  = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	pinnedClientBuild    = "6708908"
)

// SessionHeaders builds the full pinned fingerprint header set for one
// upstream call. It returns the exact reference session header block plus
// X-OpenAI-Target-Path and X-OpenAI-Target-Route, both set to pathTemplate
// (the reference _headers convention: for routes with placeholders the
// caller overrides one of the two afterwards — the wire end state is always
// Path=concrete path, Route=template).
//
// cred is a pointer because an empty fingerprint is generated once and
// persisted BACK into the credential (Fp.OAIDeviceID / Fp.OAISessionID as
// random UUIDv4s) so the ids stay stable across restarts; persist the
// credential after the first call (StorageJSON → host auth save). A nil cred
// yields the same pinned set with ephemeral ids and no stable fingerprint.
func SessionHeaders(cred *model.Credential, pathTemplate string) http.Header {
	if cred != nil {
		ensureFingerprint(cred)
	}
	h := make(http.Header, 26)
	ua := defaultUserAgent
	if cred != nil && strings.TrimSpace(cred.Fp.UserAgent) != "" {
		ua = cred.Fp.UserAgent
	}
	h.Set("User-Agent", ua)
	h.Set("Origin", chatGPTBaseURL)
	h.Set("Referer", chatGPTBaseURL+"/")
	h.Set("Accept-Language", pinnedAcceptLanguage)
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
	h.Set("Sec-Ch-Ua", defaultSecChUa)
	h.Set("Sec-Ch-Ua-Arch", `"x86"`)
	h.Set("Sec-Ch-Ua-Bitness", `"64"`)
	h.Set("Sec-Ch-Ua-Full-Version", defaultSecChUaFullVersion)
	h.Set("Sec-Ch-Ua-Full-Version-List", defaultSecChUaFullList)
	h.Set("Sec-Ch-Ua-Mobile", defaultSecChUaMobile)
	h.Set("Sec-Ch-Ua-Model", `""`)
	h.Set("Sec-Ch-Ua-Platform", defaultSecChUaPlatform)
	h.Set("Sec-Ch-Ua-Platform-Version", defaultSecChUaPlatformV)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	if cred != nil {
		h.Set("OAI-Device-Id", cred.Fp.OAIDeviceID)
		h.Set("OAI-Session-Id", cred.Fp.OAISessionID)
	} else {
		h.Set("OAI-Device-Id", newUUID())
		h.Set("OAI-Session-Id", newUUID())
	}
	h.Set("OAI-Language", pinnedOAILanguage)
	h.Set("OAI-Client-Version", pinnedClientVersion)
	h.Set("OAI-Client-Build-Number", pinnedClientBuild)
	h.Set("X-OpenAI-Target-Path", pathTemplate)
	h.Set("X-OpenAI-Target-Route", pathTemplate)
	return h
}

// ensureFingerprint fills empty per-credential ids with fresh random UUIDv4s,
// persisting them into cred.Fp so the fingerprint is stable for the life of
// the account (and across restarts once the credential is saved).
func ensureFingerprint(cred *model.Credential) {
	if strings.TrimSpace(cred.Fp.OAIDeviceID) == "" {
		cred.Fp.OAIDeviceID = newUUID()
	}
	if strings.TrimSpace(cred.Fp.OAISessionID) == "" {
		cred.Fp.OAISessionID = newUUID()
	}
}

// Authed adds `Authorization: Bearer <access_token>` to the header set. The
// bearer token only crosses the ABI on the way out to the upstream; it never
// appears in envelopes, logs, or error text. A credential without an access
// token leaves the set unchanged (the anonymous variants use AnonPath).
func Authed(cred *model.Credential, h http.Header) http.Header {
	if cred != nil && strings.TrimSpace(cred.AccessToken) != "" {
		h.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	return h
}

// AnonPath rewrites a backend-api route to its anonymous variant:
// /backend-api/... → /backend-anon/.... The rewrite is idempotent — an
// already-anonymous path comes back unchanged.
func AnonPath(path string) string {
	return strings.Replace(path, "/backend-api/", "/backend-anon/", 1)
}

// newUUID returns an RFC 4122 version-4 UUID literal built from crypto/rand
// (the reference's new_uuid() equivalent).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand Read only fails on a broken system RNG; degrade to a
		// time-seeded panic rather than shipping a predictable fingerprint.
		panic(fmt.Sprintf("uuid generation: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
