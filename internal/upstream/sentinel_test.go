package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// scriptedUpstream is the offline Upstream double for handshake tests: it
// records every Req and answers Do from a pre-scripted response queue.
type scriptedUpstream struct {
	requests  []Req
	responses []Resp
}

func (s *scriptedUpstream) Do(_ context.Context, req Req) (Resp, error) {
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return Resp{}, fmt.Errorf("scriptedUpstream: response queue exhausted after %d calls", len(s.requests))
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func (s *scriptedUpstream) DoStream(context.Context, Req) (*Stream, error) {
	return nil, errors.New("scriptedUpstream: DoStream is not used by the sentinel handshake")
}

func scriptResp(status int, body string) Resp {
	return Resp{StatusCode: status, Headers: http.Header{}, Body: []byte(body)}
}

type bootstrapVector struct {
	Name              string
	HTML              string
	ExpectedSources   []string
	ExpectedDataBuild string
}

func loadBootstrapVectors(t *testing.T) []bootstrapVector {
	t.Helper()
	var out []bootstrapVector
	for _, obj := range fixtureObjects(t, "bootstrap") {
		out = append(out, bootstrapVector{
			Name:              fixtureString(t, obj, "name"),
			HTML:              fixtureString(t, obj, "html"),
			ExpectedSources:   fixtureStrings(t, obj, "expected_sources"),
			ExpectedDataBuild: fixtureString(t, obj, "expected_data_build"),
		})
	}
	return out
}

// TestParsePowResources is the differential check for the homepage scan:
// script source collection order, the c/-pattern data-build extraction
// (last match wins) and the <html data-build> fallback.
func TestParsePowResources(t *testing.T) {
	for _, v := range loadBootstrapVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			sources, dataBuild := parsePowResources(v.HTML)
			if len(sources) != len(v.ExpectedSources) {
				t.Fatalf("sources = %v, want %v", sources, v.ExpectedSources)
			}
			for i := range sources {
				if sources[i] != v.ExpectedSources[i] {
					t.Fatalf("sources[%d] = %q, want %q", i, sources[i], v.ExpectedSources[i])
				}
			}
			if dataBuild != v.ExpectedDataBuild {
				t.Fatalf("dataBuild = %q, want %q", dataBuild, v.ExpectedDataBuild)
			}
		})
	}
}

// sentinelRequests asserts the exact request sequence AcquireSentinel issued.
func sentinelRequests(t *testing.T, u *scriptedUpstream, wantPreparePath, wantFinalizePath string) {
	t.Helper()
	if len(u.requests) != 3 {
		t.Fatalf("AcquireSentinel issued %d requests, want 3 (bootstrap, prepare, finalize)", len(u.requests))
	}
	boot, prepare, finalize := u.requests[0], u.requests[1], u.requests[2]

	if boot.Method != http.MethodGet || boot.URL != "https://chatgpt.com/" {
		t.Fatalf("bootstrap request = %s %s, want GET https://chatgpt.com/", boot.Method, boot.URL)
	}
	// The bootstrap carries the reference's page-load header set, not the
	// session headers — and never an Authorization header.
	if got := boot.Headers.Get("Sec-Fetch-Dest"); got != "document" {
		t.Fatalf("bootstrap Sec-Fetch-Dest = %q, want document", got)
	}
	if got := boot.Headers.Get("Accept-Language"); got != "zh-CN,zh;q=0.9,en;q=0.8" {
		t.Fatalf("bootstrap Accept-Language = %q, want the page-load variant", got)
	}
	if got := boot.Headers.Get("Authorization"); got != "" {
		t.Fatalf("bootstrap must not carry Authorization (got %q)", got)
	}

	if prepare.Method != http.MethodPost || prepare.URL != "https://chatgpt.com"+wantPreparePath {
		t.Fatalf("prepare request = %s %s, want POST https://chatgpt.com%s", prepare.Method, prepare.URL, wantPreparePath)
	}
	if finalize.Method != http.MethodPost || finalize.URL != "https://chatgpt.com"+wantFinalizePath {
		t.Fatalf("finalize request = %s %s, want POST https://chatgpt.com%s", finalize.Method, finalize.URL, wantFinalizePath)
	}

	for _, r := range []Req{prepare, finalize} {
		// Content-Type travels in the Req field; the transports apply it as a
		// header only when one is absent (the fake Upstream returns Req raw).
		if r.ContentType != "application/json" {
			t.Fatalf("%s ContentType = %q, want application/json", r.Method, r.ContentType)
		}
		for header, want := range map[string]string{
			"OAI-Client-Version":      "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887",
			"OAI-Client-Build-Number": "6708908",
			"OAI-Device-Id":           "11111111-1111-4111-8111-111111111111",
		} {
			if got := r.Headers.Get(header); got != want {
				t.Fatalf("%s %s = %q, want %q", r.Method, header, got, want)
			}
		}
		if got := r.Headers.Get("X-OpenAI-Target-Path"); got != strings.TrimPrefix(r.URL, "https://chatgpt.com") {
			t.Fatalf("X-OpenAI-Target-Path = %q, want the request path", got)
		}
		if got := r.Headers.Get("X-OpenAI-Target-Route"); got != strings.TrimPrefix(r.URL, "https://chatgpt.com") {
			t.Fatalf("X-OpenAI-Target-Route = %q, want the request path", got)
		}
	}
}

func TestSentinelHappyPath(t *testing.T) {
	bootstrap := loadBootstrapVectors(t)[0]
	t.Run("authed", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{
			scriptResp(200, bootstrap.HTML),
			scriptResp(200, `{"prepare_token":"pt-fixture","arkose":{"required":false},"proofofwork":{"required":false},"turnstile":{"required":false}}`),
			scriptResp(200, `{"token":"cr-fixture","so_token":"so-fixture"}`),
		}}
		cred := fixtureCredential()
		tokens, err := AcquireSentinel(context.Background(), u, cred, false)
		if err != nil {
			t.Fatalf("AcquireSentinel: %v", err)
		}
		want := SentinelTokens{ChatRequirements: "cr-fixture", SO: "so-fixture"}
		if tokens != want {
			t.Fatalf("tokens = %+v, want %+v (Proof/Turnstile stay empty when not required)", tokens, want)
		}

		sentinelRequests(t, u,
			"/backend-api/sentinel/chat-requirements/prepare",
			"/backend-api/sentinel/chat-requirements/finalize")
		if got := u.requests[1].Headers.Get("Authorization"); got != "Bearer fixture-access-token" {
			t.Fatalf("prepare Authorization = %q, want the fixture bearer token", got)
		}

		// prepare body: exactly {"p": <legacy token>}, a gAAAAAC token whose
		// decoded 25-field config carries the bootstrap extraction.
		var prepareBody map[string]any
		if err := json.Unmarshal(u.requests[1].Body, &prepareBody); err != nil {
			t.Fatalf("prepare body is not JSON: %v", err)
		}
		if len(prepareBody) != 1 {
			t.Fatalf("prepare body has %d fields, want exactly one (\"p\")", len(prepareBody))
		}
		p, ok := prepareBody["p"].(string)
		if !ok || !strings.HasPrefix(p, "gAAAAAC") {
			t.Fatalf("prepare body p = %v, want a gAAAAAC legacy token", prepareBody["p"])
		}
		payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(p, "gAAAAAC"))
		if err != nil {
			t.Fatalf("legacy token is not base64: %v", err)
		}
		var config []any
		if err := json.Unmarshal(payload, &config); err != nil || len(config) != 25 {
			t.Fatalf("legacy token decodes to %d fields, want 25", len(config))
		}
		if config[4] != defaultUserAgent {
			t.Fatalf("legacy config[4] = %v, want the pinned default user agent", config[4])
		}
		if config[6] != bootstrap.ExpectedDataBuild {
			t.Fatalf("legacy config[6] = %v, want the bootstrap data-build %q", config[6], bootstrap.ExpectedDataBuild)
		}
		sourceOK := false
		for _, s := range bootstrap.ExpectedSources {
			if config[5] == s {
				sourceOK = true
			}
		}
		if !sourceOK {
			t.Fatalf("legacy config[5] = %v, want one of the bootstrap script sources %v", config[5], bootstrap.ExpectedSources)
		}

		// finalize body: the exact three-field shape the oracle posts.
		wantFinalize := `{"prepare_token":"pt-fixture","proof_token":"","turnstile_token":""}`
		if !bytes.Equal(u.requests[2].Body, []byte(wantFinalize)) {
			t.Fatalf("finalize body = %s, want %s", u.requests[2].Body, wantFinalize)
		}
	})

	t.Run("anon", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{
			scriptResp(200, bootstrap.HTML),
			scriptResp(200, `{"prepare_token":"pt-anon","arkose":{"required":false},"proofofwork":{"required":false},"turnstile":{"required":false}}`),
			scriptResp(200, `{"token":"cr-anon","so_token":"so-anon"}`),
		}}
		cred := fixtureCredential()
		tokens, err := AcquireSentinel(context.Background(), u, cred, true)
		if err != nil {
			t.Fatalf("AcquireSentinel(anon): %v", err)
		}
		if tokens.ChatRequirements != "cr-anon" || tokens.SO != "so-anon" {
			t.Fatalf("anon tokens = %+v, want cr-anon/so-anon", tokens)
		}
		sentinelRequests(t, u,
			"/backend-anon/sentinel/chat-requirements/prepare",
			"/backend-anon/sentinel/chat-requirements/finalize")
		if got := u.requests[1].Headers.Get("Authorization"); got != "" {
			t.Fatalf("anon prepare must not carry Authorization (got %q)", got)
		}
	})
}

// TestSentinelWithPoWAndTurnstile scripts a prepare response that requires
// BOTH the proof-of-work and the turnstile VM: both solvers run, and the
// finalize body carries the exact tokens they produced. The turnstile dx is
// fixture vector t01, so its expected token is the oracle's byte-for-byte.
// The proof config is randomized per run, so the proof token is verified by
// its shape plus the mined-nonce predicate (the proof payload embeds every
// config field the hash touches, so the static pieces are reconstructed
// from it directly).
func TestSentinelWithPoWAndTurnstile(t *testing.T) {
	bootstrap := loadBootstrapVectors(t)[0]
	var dx, wantTurnstile, dxKey string
	for _, v := range loadTurnstileVectors(t) {
		if v.Name == "t01-literal-copy" {
			dx = v.DX
			dxKey = v.P
			wantTurnstile = *v.ExpectedToken
		}
	}
	if dx == "" {
		t.Fatal("fixture lost the t01-literal-copy turnstile vector")
	}

	// The fixture dx is XORed with dxKey. AcquireSentinel XORs with the legacy
	// token, so pin the seam to dxKey to make that token deterministic here.
	previousBuildLegacyToken := buildLegacyToken
	buildLegacyToken = func(string, []string, string) (string, error) { return dxKey, nil }
	t.Cleanup(func() { buildLegacyToken = previousBuildLegacyToken })

	const wantSeed, wantDifficulty = "fixture-seed-001", "00aa"

	u := &scriptedUpstream{responses: []Resp{
		scriptResp(200, bootstrap.HTML),
		scriptResp(200, fmt.Sprintf(
			`{"prepare_token":"pt-fixture","arkose":{"required":false},"proofofwork":{"required":true,"seed":%q,"difficulty":%q},"turnstile":{"required":true,"dx":%q}}`,
			wantSeed, wantDifficulty, dx)),
		scriptResp(200, `{"token":"cr-fixture","so_token":"so-fixture"}`),
	}}
	cred := fixtureCredential()
	tokens, err := AcquireSentinel(context.Background(), u, cred, false)
	if err != nil {
		t.Fatalf("AcquireSentinel: %v", err)
	}

	if tokens.ChatRequirements != "cr-fixture" || tokens.SO != "so-fixture" {
		t.Fatalf("tokens = %+v, want cr-fixture/so-fixture", tokens)
	}
	if tokens.Turnstile != wantTurnstile {
		t.Fatalf("turnstile token = %q, want the oracle's %q", tokens.Turnstile, wantTurnstile)
	}
	if !strings.HasPrefix(tokens.Proof, "gAAAAAB") {
		t.Fatalf("proof token = %q, want the gAAAAAB prefix", tokens.Proof)
	}

	// Reconstruct the hashed static pieces from the mined proof payload and
	// verify the nonce is first-acceptable (indices 3 and 9 of the payload are
	// the nonce pair; every other field is the config verbatim).
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(tokens.Proof, "gAAAAAB"))
	if err != nil {
		t.Fatalf("proof token is not base64: %v", err)
	}
	decoded, err := decodePyJSON(payload)
	if err != nil {
		t.Fatalf("proof payload is not JSON: %v", err)
	}
	fields, ok := decoded.([]any)
	if !ok || len(fields) != 25 {
		t.Fatalf("proof payload has %d fields, want 25", len(fields))
	}
	// The proof JSON is [c0,c1,c2, nonce, c4,c5,c6,c7,c8, nonce>>1, c10...c24].
	nonce, ok := fields[3].(int)
	if !ok {
		t.Fatalf("proof payload[3] = %#v, want the mined nonce (int)", fields[3])
	}
	if fields[9] != nonce>>1 {
		t.Fatalf("proof payload[9] = %#v, want nonce>>1 (%d)", fields[9], nonce>>1)
	}
	config := make([]any, 25)
	copy(config, fields)
	if _, err := hex.DecodeString(wantDifficulty); err != nil {
		t.Fatalf("fixture difficulty is not hex: %v", err)
	}
	static1, static2, static3, err := proofStaticPieces(config)
	if err != nil {
		t.Fatalf("proofStaticPieces: %v", err)
	}
	for j := 0; j <= nonce; j++ {
		if proofAccepts(wantSeed, wantDifficulty, static1, static2, static3, j) && j < nonce {
			t.Fatalf("proof attempt %d before the mined %d already accepts", j, nonce)
		}
	}
	if !proofAccepts(wantSeed, wantDifficulty, static1, static2, static3, nonce) {
		t.Fatalf("mined nonce %d does not satisfy the predicate", nonce)
	}

	var finalizeBody map[string]any
	if err := json.Unmarshal(u.requests[2].Body, &finalizeBody); err != nil {
		t.Fatalf("finalize body is not JSON: %v", err)
	}
	if finalizeBody["prepare_token"] != "pt-fixture" {
		t.Fatalf("finalize prepare_token = %v, want pt-fixture", finalizeBody["prepare_token"])
	}
	if finalizeBody["proof_token"] != tokens.Proof {
		t.Fatal("finalize body does not carry the mined proof token")
	}
	if finalizeBody["turnstile_token"] != wantTurnstile {
		t.Fatalf("finalize turnstile_token = %v, want the oracle's %q", finalizeBody["turnstile_token"], wantTurnstile)
	}
	sentinelRequests(t, u,
		"/backend-api/sentinel/chat-requirements/prepare",
		"/backend-api/sentinel/chat-requirements/finalize")
}

// TestSentinelArkoseRefused pins the arkose refusal: prepare advertising
// arkose.required fails typed and finalize is NEVER attempted.
func TestSentinelArkoseRefused(t *testing.T) {
	bootstrap := loadBootstrapVectors(t)[0]
	u := &scriptedUpstream{responses: []Resp{
		scriptResp(200, bootstrap.HTML),
		scriptResp(200, `{"prepare_token":"pt-fixture","arkose":{"required":true}}`),
	}}
	cred := fixtureCredential()
	_, err := AcquireSentinel(context.Background(), u, cred, false)
	if err == nil {
		t.Fatal("AcquireSentinel with arkose.required must error")
	}
	if !errors.Is(err, ErrChallengeArkoseUnsupported) {
		t.Fatalf("error = %v, want ErrChallengeArkoseUnsupported", err)
	}
	if len(u.requests) != 2 {
		t.Fatalf("arkose refusal issued %d requests, want 2 (bootstrap + prepare; finalize never)", len(u.requests))
	}
}

// TestSentinelFailureTyped pins the typed-failure mapping: 403 at any step
// is sentinel_denied with Retryable=false; 429/5xx are retryable
// sentinel_unavailable; a finalize without a token is sentinel_missing_token.
func TestSentinelFailureTyped(t *testing.T) {
	bootstrap := loadBootstrapVectors(t)[0]
	prepare200 := scriptResp(200, `{"prepare_token":"pt-fixture","arkose":{"required":false},"proofofwork":{"required":false},"turnstile":{"required":false}}`)

	cases := []struct {
		name          string
		responses     []Resp
		wantCode      string
		wantRetryable bool
		wantStatus    int
	}{
		{"prepare 403", []Resp{scriptResp(200, bootstrap.HTML), scriptResp(403, `{}`), prepare200}, "sentinel_denied", false, 403},
		{"finalize 403", []Resp{scriptResp(200, bootstrap.HTML), prepare200, scriptResp(403, `{}`)}, "sentinel_denied", false, 403},
		{"bootstrap 500", []Resp{scriptResp(500, `{}`), prepare200, prepare200}, "sentinel_unavailable", true, 500},
		{"prepare 429", []Resp{scriptResp(200, bootstrap.HTML), scriptResp(429, `{}`), prepare200}, "sentinel_unavailable", true, 429},
		{"finalize missing token", []Resp{scriptResp(200, bootstrap.HTML), prepare200, scriptResp(200, `{"so_token":"so-fixture"}`)}, "sentinel_missing_token", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &scriptedUpstream{responses: tc.responses}
			cred := fixtureCredential()
			_, err := AcquireSentinel(context.Background(), u, cred, false)
			if err == nil {
				t.Fatal("AcquireSentinel must error")
			}
			var se *SentinelError
			if !errors.As(err, &se) {
				t.Fatalf("error = %v, want *SentinelError", err)
			}
			if se.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", se.Code, tc.wantCode)
			}
			if se.Retryable != tc.wantRetryable {
				t.Fatalf("error retryable = %v, want %v", se.Retryable, tc.wantRetryable)
			}
			if se.HTTPStatus != tc.wantStatus {
				t.Fatalf("error http status = %d, want %d", se.HTTPStatus, tc.wantStatus)
			}
		})
	}
}
