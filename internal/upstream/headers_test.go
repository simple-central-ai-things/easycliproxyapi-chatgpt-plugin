package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// fixtureCredential is the golden-test credential: its fingerprint is
// pre-set (so the header map is deterministic) and its access token is a
// fixture, never a real secret.
func fixtureCredential() model.Credential {
	return model.Credential{
		Email:       "fixture@example.com",
		AccessToken: "fixture-access-token",
		Fp: model.Fingerprint{
			OAIDeviceID:  "11111111-1111-4111-8111-111111111111",
			OAISessionID: "22222222-2222-4222-8222-222222222222",
		},
	}
}

// TestSessionHeadersGolden pins the ENTIRE session header map as JSON: every
// fingerprint constant from the plan's Global Constraints (and the reference
// Edge-143 Sec-Ch-Ua defaults) must match byte-for-byte, and the exact header
// set — no more, no fewer — must be present. Any change to a pinned constant
// or to the set fails loudly here.
func TestSessionHeadersGolden(t *testing.T) {
	cred := fixtureCredential()
	got := SessionHeaders(&cred, "/backend-api/models")

	// Expected set transcribed from the reference session header block
	// (openai_backend_api.py, the session.headers.update) plus the two
	// X-OpenAI-Target-* headers from _headers. Fp.UserAgent is empty so the
	// pinned Edge-143 default UA applies. Header NAMES are spelled in Go's
	// canonical MIME form (Oai-*, X-Openai-*): http.Header canonicalizes
	// casing and the wire comparison is on the marshaled form — names are
	// case-insensitive on the wire (RFC 7230 §3.2), values are pinned
	// byte-for-byte here.
	want := http.Header{
		"Accept-Language":             {"zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"},
		"Cache-Control":               {"no-cache"},
		"Oai-Client-Build-Number":     {"6708908"},
		"Oai-Client-Version":          {"prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"},
		"Oai-Device-Id":               {"11111111-1111-4111-8111-111111111111"},
		"Oai-Language":                {"zh-CN"},
		"Oai-Session-Id":              {"22222222-2222-4222-8222-222222222222"},
		"Origin":                      {"https://chatgpt.com"},
		"Pragma":                      {"no-cache"},
		"Priority":                    {"u=1, i"},
		"Referer":                     {"https://chatgpt.com/"},
		"Sec-Ch-Ua":                   {`"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`},
		"Sec-Ch-Ua-Arch":              {`"x86"`},
		"Sec-Ch-Ua-Bitness":           {`"64"`},
		"Sec-Ch-Ua-Full-Version":      {`"143.0.3650.96"`},
		"Sec-Ch-Ua-Full-Version-List": {`"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`},
		"Sec-Ch-Ua-Mobile":            {"?0"},
		"Sec-Ch-Ua-Model":             {`""`},
		"Sec-Ch-Ua-Platform":          {`"Windows"`},
		"Sec-Ch-Ua-Platform-Version":  {`"19.0.0"`},
		"Sec-Fetch-Dest":              {"empty"},
		"Sec-Fetch-Mode":              {"cors"},
		"Sec-Fetch-Site":              {"same-origin"},
		"User-Agent":                  {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"},
		"X-Openai-Target-Path":        {"/backend-api/models"},
		"X-Openai-Target-Route":       {"/backend-api/models"},
	}
	if len(got) != len(want) {
		t.Fatalf("header set size = %d, want %d\n got: %v", len(got), len(want), got)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("golden header map mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestSessionHeadersFpOverrides verifies a credential's stored fingerprint
// User-Agent wins over the pinned default, and Authed adds the bearer token.
func TestSessionHeadersFpOverrides(t *testing.T) {
	cred := fixtureCredential()
	cred.Fp.UserAgent = "Mozilla/5.0 (custom)"

	got := SessionHeaders(&cred, "/backend-api/models")
	if ua := got.Get("User-Agent"); ua != "Mozilla/5.0 (custom)" {
		t.Fatalf("User-Agent = %q, want the credential's stored fingerprint UA", ua)
	}

	authed := Authed(&cred, got)
	if az := authed.Get("Authorization"); az != "Bearer fixture-access-token" {
		t.Fatalf("Authorization = %q, want %q", az, "Bearer fixture-access-token")
	}
	if len(authed) != 27 {
		t.Fatalf("authed header set size = %d, want 27 (26 pinned + Authorization)", len(authed))
	}
}

// TestFingerprintStability verifies that an empty fingerprint is generated
// once (random UUIDv4), persisted back into the credential, and survives a
// storage round-trip — so OAI-Device-Id / OAI-Session-Id are stable across
// restarts and re-parses.
func TestFingerprintStability(t *testing.T) {
	cred := model.Credential{Email: "fixture@example.com"}

	first := SessionHeaders(&cred, "/backend-api/models")
	deviceID := first.Get("OAI-Device-Id")
	sessionID := first.Get("OAI-Session-Id")
	for name, value := range map[string]string{
		"OAI-Device-Id":  deviceID,
		"OAI-Session-Id": sessionID,
	} {
		if !isUUIDv4(value) {
			t.Fatalf("%s = %q, want a generated UUIDv4", name, value)
		}
	}
	if cred.Fp.OAIDeviceID != deviceID || cred.Fp.OAISessionID != sessionID {
		t.Fatalf("generated ids not persisted into cred.Fp: %+v", cred.Fp)
	}

	// Re-parse the persisted credential and build headers again: same ids.
	parsed, err := model.FromStorageJSON(cred.StorageJSON())
	if err != nil {
		t.Fatalf("FromStorageJSON: %v", err)
	}
	second := SessionHeaders(&parsed, "/backend-api/models")
	if got := second.Get("OAI-Device-Id"); got != deviceID {
		t.Fatalf("re-parsed OAI-Device-Id = %q, want %q (stable)", got, deviceID)
	}
	if got := second.Get("OAI-Session-Id"); got != sessionID {
		t.Fatalf("re-parsed OAI-Session-Id = %q, want %q (stable)", got, sessionID)
	}
}

// isUUIDv4 reports whether s is an RFC 4122 version-4 UUID literal
// (8-4-4-4-12 hex, version nibble 4, variant nibble 8/9/a/b).
func isUUIDv4(s string) bool {
	if len(s) != 36 || strings.Count(s, "-") != 4 {
		return false
	}
	parts := strings.Split(s, "-")
	sizes := []int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != sizes[i] {
			return false
		}
	}
	if parts[2][0] != '4' {
		return false
	}
	switch parts[3][0] {
	case '8', '9', 'a', 'b':
	default:
		return false
	}
	return true
}

func TestAnonPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"api to anon", "/backend-api/models", "/backend-anon/models"},
		{"anon idempotent", "/backend-anon/models", "/backend-anon/models"},
		{"nested path", "/backend-api/sentinel/chat-requirements/prepare", "/backend-anon/sentinel/chat-requirements/prepare"},
		{"query preserved", "/backend-api/models?history_and_training_disabled=false", "/backend-anon/models?history_and_training_disabled=false"},
		{"foreign path untouched", "/backend-anon/x", "/backend-anon/x"},
		{"non-backend path untouched", "/oai/files", "/oai/files"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnonPath(tc.in); got != tc.want {
				t.Fatalf("AnonPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
