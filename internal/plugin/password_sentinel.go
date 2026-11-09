package plugin

// Auth-flow sentinel: the FNV-1a-32 proof-of-work challenge that guards the
// password/verify flow (reference utils/sentinel.py, SentinelTokenGenerator
// + build_sentinel_token). This is DISTINCT from the CONVERSATION-flow
// sentinel in internal/upstream (sentinel.go + turnstile VM): different
// endpoint, different token shape, different flow id. The port is
// differential-pinned against the reference's own output — see
// password_login_test.go for the vector provenance.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"time"
)

const (
	// sentinelReqURL is the PoW challenge endpoint (sentinel.py:123).
	sentinelReqURL = "https://sentinel.openai.com/backend-api/sentinel/req"
	// sentinelFrameReferer and sentinelOrigin are the pinned req headers
	// (sentinel.py:126-128).
	sentinelFrameReferer = "https://sentinel.openai.com/backend-api/sentinel/frame.html"
	sentinelOrigin       = "https://sentinel.openai.com"
	// sentinelFlowPasswordVerify is the flow id for password recovery
	// (account_service.py:696).
	sentinelFlowPasswordVerify = "password_verify"
	// sentinelPoWCap bounds the solve loop (sentinel.py:20).
	sentinelPoWCap = 500_000
	// sentinelSDKPin is the challenge-config SDK marker (sentinel.py:49).
	sentinelSDKPin = "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js"
)

// sentinelUserAgent is the fingerprint UA both reference flows share:
// account_service.py:_OAUTH_USER_AGENT == sentinel.py:DEFAULT_SENTINEL_USER_AGENT.
const sentinelUserAgent = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) " +
	"Chrome/145.0.0.0 Safari/537.36")

// sentinelSecCHUA pins the sec-ch-ua client-hint (sentinel.py:93).
const sentinelSecCHUA = `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`

// sentinelSecCHUAPlatform pins the sec-ch-ua-platform client-hint.
const sentinelSecCHUAPlatform = `"Windows"`

// errSentinelPoWCap marks a solve that exhausted the iteration budget. The
// reference answers a silent error-marker token here; F4 deliberately turns
// it into a typed error (surfaced as sentinel_pow_failed) instead of handing
// the provider a token it will reject.
var errSentinelPoWCap = errors.New("sentinel proof-of-work iteration cap breached")

// fnv1a32Sentinel ports SentinelTokenGenerator._fnv1a_32 (sentinel.py:28-39):
// 32-bit FNV-1a over UNICODE CODEPOINTS (Python iterates str by rune — not
// byte — so ord(ch) may exceed 255), followed by the sentinel's three extra
// finalizer mixes. The output is 8 lowercase hex chars.
func fnv1a32Sentinel(text string) string {
	h := uint32(2166136261)
	for _, r := range text {
		h ^= uint32(r)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

// pyRound mirrors Python's round(): banker's rounding (round-half-to-even),
// which math.Round would get wrong at exact .5 values.
func pyRound(f float64) float64 {
	return math.RoundToEven(f)
}

// pyB64 ports _b64 (sentinel.py:64-66): compact JSON (no separators, no HTML
// escaping) base64-standared. The HTML-escape off switch matters — the
// reference dumps raw bytes and the differential vectors pin them.
func pyB64(data []any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return "" // unreachable for []any of our config types
	}
	return base64.StdEncoding.EncodeToString(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// sentinelSeams are the injectable draws the challenge config reads.
// Production wires the real clock and math/rand/v2; the differential tests
// freeze them so the reference's output is reproducible byte for byte.
type sentinelSeams struct {
	float01   func() float64
	uniform   func(a, b float64) float64
	choiceStr func(opts []string) string
	choiceInt func(opts []int) int
	wall      func() time.Time
}

// sentinelGenerator ports SentinelTokenGenerator (sentinel.py:18-84) with the
// per-login identity fields plus the seam/config injections.
type sentinelGenerator struct {
	deviceID  string
	userAgent string
	// sidValue is the generator's own random session id (sentinel.py:26).
	sidValue string
	seams    sentinelSeams
	// config builds the 18-element challenge array (getRequirementConfig in
	// production; the frozen differential array in tests).
	config func() []any
	// maxAttempts bounds the solve loop (sentinelPoWCap in production; the
	// tests shrink it to prove the cap is an error, not a hang).
	maxAttempts int
}

// newSentinelGenerator returns the production generator for deviceID: real
// clock, math/rand/v2 draws, the full iteration budget.
func newSentinelGenerator(deviceID, userAgent, sidValue string) *sentinelGenerator {
	g := &sentinelGenerator{
		deviceID:  deviceID,
		userAgent: userAgent,
		sidValue:  sidValue,
		seams: sentinelSeams{
			float01:   mrand.Float64,
			uniform:   func(a, b float64) float64 { return a + mrand.Float64()*(b-a) },
			choiceStr: func(opts []string) string { return opts[mrand.IntN(len(opts))] },
			choiceInt: func(opts []int) int { return opts[mrand.IntN(len(opts))] },
			wall:      time.Now,
		},
		maxAttempts: sentinelPoWCap,
	}
	g.config = g.getRequirementConfig
	return g
}

// getRequirementConfig ports _get_config (sentinel.py:41-62): the 18-element
// challenge array. Indices 3 and 9 are overwritten by the caller per attempt
// (iteration counter, elapsed ms) — exactly as the reference mutates them.
func (g *sentinelGenerator) getRequirementConfig() []any {
	perfNow := g.seams.uniform(1000, 50000)
	now := g.seams.wall()
	return []any{
		"1920x1080",
		// Python strftime("%a %b %d %Y %H:%M:%S GMT+0000 (Coordinated
		// Universal Time)", gmtime()) — the Go layout is its byte twin.
		now.UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)",
		4294705152,
		g.seams.float01(),
		g.userAgent,
		sentinelSDKPin,
		nil,
		nil,
		"en-US",
		g.seams.float01(),
		g.seams.choiceStr([]string{"vendorSub-undefined", "plugins-undefined", "mimeTypes-undefined", "hardwareConcurrency-undefined"}),
		g.seams.choiceStr([]string{"location", "implementation", "URL", "documentURI", "compatMode"}),
		g.seams.choiceStr([]string{"Object", "Function", "Array", "Number", "parseFloat", "undefined"}),
		perfNow,
		g.sidValue,
		"",
		g.seams.choiceInt([]int{4, 8, 12, 16}),
		// Python: time.time() * 1000 - perf_now (float arithmetic).
		float64(now.UnixMilli()) - perfNow,
	}
}

// generateRequirementsToken ports generate_requirements_token
// (sentinel.py:68-72): the gAAAAAC-prefixed config blob.
func (g *sentinelGenerator) generateRequirementsToken() string {
	data := g.config()
	data[3] = 1
	data[9] = int(pyRound(g.seams.uniform(5, 50)))
	return "gAAAAAC" + pyB64(data)
}

// generateToken ports generate_token (sentinel.py:74-84): the FNV-1a-32 PoW
// solve. The config is built ONCE and only indices 3/9 mutate per attempt —
// copying the reference's mutation pattern so a solve reproduces its bytes.
// The acceptance rule is a lexicographic hex prefix compare
// (hash[:len(difficulty)] <= difficulty), which is numeric for same-length
// lowercase hex. A cap breach returns errSentinelPoWCap — never a token.
func (g *sentinelGenerator) generateToken(seed, difficulty string) (string, error) {
	if difficulty == "" {
		difficulty = "0"
	}
	base := g.config()
	start := g.seams.wall()
	for i := 0; i < g.maxAttempts; i++ {
		data := make([]any, len(base))
		copy(data, base)
		data[3] = i
		data[9] = int(pyRound(g.seams.wall().Sub(start).Seconds() * 1000))
		payload := pyB64(data)
		if strings.Compare(fnv1a32Sentinel(seed + payload)[:len(difficulty)], difficulty) <= 0 {
			return "gAAAAAB" + payload + "~S", nil
		}
	}
	return "", errSentinelPoWCap
}

// sentinelValue is the openai-sentinel-token envelope (sentinel.py:156,
// key order p/t/c/id/flow — struct declaration order pins it).
type sentinelValue struct {
	P    string `json:"p"`
	T    string `json:"t"`
	C    string `json:"c"`
	ID   string `json:"id"`
	Flow string `json:"flow"`
}

// sentinelAcquisition is the outcome of one auth-flow sentinel round.
// token == "" means "no openai-sentinel-token header at all": the reference
// swallows every acquisition failure except the PoW cap breach (err).
type sentinelAcquisition struct {
	token string
	oaiSC string
	err   error
}

// acquireAuthSentinel ports build_sentinel_token (sentinel.py:96-159) over
// the host transport seam. The req body keeps the reference's json.dumps
// default spacing (sentinel.py:124) byte for byte; the response envelope
// stays compact (sentinel.py:156).
func (m *Manager) acquireAuthSentinel(callbackID, deviceID string, g *sentinelGenerator) sentinelAcquisition {
	body := fmt.Sprintf(`{"p": %s, "id": %s, "flow": %s}`,
		jsonQuoted(g.generateRequirementsToken()),
		jsonQuoted(deviceID),
		jsonQuoted(sentinelFlowPasswordVerify))
	resp, err := m.doHostHTTP(callbackID, http.MethodPost, sentinelReqURL, map[string][]string{
		"Content-Type":       {"text/plain;charset=UTF-8"},
		"Referer":            {sentinelFrameReferer},
		"Origin":             {sentinelOrigin},
		"User-Agent":         {g.userAgent},
		"sec-ch-ua":          {sentinelSecCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sentinelSecCHUAPlatform},
	}, []byte(body))
	if err != nil {
		// Host seam down: account_service.py:700-701 swallows the failure
		// and verifies without the header.
		return sentinelAcquisition{}
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		// Non-JSON response: sentinel.py:140-145 fallback — a requirements
		// token with an empty challenge, no oai-sc cookie.
		value, _ := json.Marshal(sentinelValue{P: g.generateRequirementsToken(), ID: deviceID, Flow: sentinelFlowPasswordVerify})
		return sentinelAcquisition{token: string(value)}
	}
	token, _ := payload["token"].(string)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(token) == "" {
		// sentinel_req_failed_<status>: the reference raises and the caller
		// swallows → no header at all.
		return sentinelAcquisition{}
	}
	p := g.generateRequirementsToken()
	if pow, _ := payload["proofofwork"].(map[string]any); pow != nil {
		required, _ := pow["required"].(bool)
		seed, _ := pow["seed"].(string)
		difficulty, _ := pow["difficulty"].(string)
		if difficulty == "" {
			difficulty = "0"
		}
		if required && seed != "" {
			solved, err := g.generateToken(seed, difficulty)
			if err != nil {
				return sentinelAcquisition{err: errSentinelPoWCap}
			}
			p = solved
		}
	}
	value, _ := json.Marshal(sentinelValue{P: p, C: token, ID: deviceID, Flow: sentinelFlowPasswordVerify})
	return sentinelAcquisition{token: string(value), oaiSC: "0" + token}
}

// jsonQuoted marshals s as a JSON string (for the reference-spaced req body).
func jsonQuoted(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// uuid4String returns a random UUIDv4 in the reference's lowercase hyphenated
// form (str(uuid.uuid4())).
func uuid4String() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// tokenURLSafe returns n base64url chars of crypto randomness — the shape of
// Python's secrets.token_urlsafe(n) (n bytes of entropy → n*3/4 random bytes).
func tokenURLSafe(n int) (string, error) {
	buf := make([]byte, n*3/4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
