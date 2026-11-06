package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// hostCallRecord captures one caller.Call invocation (the transport seam).
type hostCallRecord struct {
	method  string
	request []byte
}

// httpFakeCaller is the offline host seam for auth tests: it records every
// Call, answers queued results to host.http.do and replays them as success
// envelopes (or a single failure envelope when errorCode is set).
type httpFakeCaller struct {
	mu          sync.Mutex
	calls       []hostCallRecord
	httpResults []hostHTTPResponse
	errorCode   string
}

func (f *httpFakeCaller) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hostCallRecord{method: method, request: append([]byte(nil), request...)})
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	if f.errorCode != "" {
		return hostErr(f.errorCode), nil
	}
	if len(f.httpResults) > 0 {
		result := f.httpResults[0]
		f.httpResults = f.httpResults[1:]
		return hostOK(result), nil
	}
	return hostOK(map[string]any{}), nil
}

func (f *httpFakeCaller) callFor(t *testing.T, method string) hostCallRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.calls {
		if record.method == method {
			return record
		}
	}
	t.Fatalf("caller never received %s (got %v)", method, methodsOf(f.calls))
	return hostCallRecord{}
}

func methodsOf(calls []hostCallRecord) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.method)
	}
	return out
}

func hostErr(code string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: &pluginabi.Error{Code: code, Message: "transport unavailable"},
	})
	return out
}

func newAuthTestManager(t *testing.T, host caller) *Manager {
	t.Helper()
	m, err := New(host)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func authParseRequestBody(t *testing.T, provider, fileName string, rawJSON []byte) []byte {
	t.Helper()
	body, err := json.Marshal(pluginapi.AuthParseRequest{
		Provider: provider,
		FileName: fileName,
		RawJSON:  rawJSON,
	})
	if err != nil {
		t.Fatalf("marshal auth.parse request: %v", err)
	}
	return body
}

func authRefreshRequestBody(t *testing.T, cred Credential, authID, callbackID string) []byte {
	t.Helper()
	body, err := json.Marshal(refreshRequest{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:       authID,
			AuthProvider: providerKey,
			StorageJSON:  cred.StorageJSON(),
		},
		HostCallbackID: callbackID,
	})
	if err != nil {
		t.Fatalf("marshal auth.refresh request: %v", err)
	}
	return body
}

func decodeRefreshResponse(t *testing.T, resp []byte) pluginapi.AuthRefreshResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("auth.refresh envelope not OK: %s", resp)
	}
	var out pluginapi.AuthRefreshResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode auth.refresh result: %v (%s)", err, env.Result)
	}
	return out
}

func decodeErrorEnvelope(t *testing.T, resp []byte) pluginabi.Envelope {
	t.Helper()
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil {
		t.Fatalf("expected an error envelope: %s", resp)
	}
	return env
}

func fullCredential() Credential {
	return Credential{
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
		IDToken:      "id-token-value",
		Email:        "owner@example.com",
		UserID:       "user-1",
		AccountID:    "acc-1",
		Plan:         "plus",
		SourceType:   "oauth_login",
		Fp: Fingerprint{
			UserAgent:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
			Impersonate:  "chrome136",
			OAIDeviceID:  "oai-device-1",
			OAISessionID: "oai-session-1",
		},
		LastRefreshAt:    time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		LastRefreshError: "oauth refresh failed with http 400: invalid_grant",
	}
}

// TestParseOwnRecord pins the credential schema: a full StorageJSON payload
// round-trips field-by-field through FromStorageJSON, the storage shape is
// discriminated by type=chatgpt-proxy, and the Manager's auth.parse handles
// the record with the file name returned unchanged.
func TestParseOwnRecord(t *testing.T) {
	original := fullCredential()
	raw := original.StorageJSON()

	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("decode storage head: %v", err)
	}
	if head.Type != providerKey {
		t.Fatalf("storage type = %q, want %q", head.Type, providerKey)
	}

	roundTripped, err := FromStorageJSON(raw)
	if err != nil {
		t.Fatalf("FromStorageJSON: %v", err)
	}
	if roundTripped.AccessToken != original.AccessToken {
		t.Errorf("AccessToken = %q, want %q", roundTripped.AccessToken, original.AccessToken)
	}
	if roundTripped.RefreshToken != original.RefreshToken {
		t.Errorf("RefreshToken = %q, want %q", roundTripped.RefreshToken, original.RefreshToken)
	}
	if roundTripped.IDToken != original.IDToken {
		t.Errorf("IDToken = %q, want %q", roundTripped.IDToken, original.IDToken)
	}
	if roundTripped.Email != original.Email {
		t.Errorf("Email = %q, want %q", roundTripped.Email, original.Email)
	}
	if roundTripped.UserID != original.UserID {
		t.Errorf("UserID = %q, want %q", roundTripped.UserID, original.UserID)
	}
	if roundTripped.AccountID != original.AccountID {
		t.Errorf("AccountID = %q, want %q", roundTripped.AccountID, original.AccountID)
	}
	if roundTripped.Plan != original.Plan {
		t.Errorf("Plan = %q, want %q", roundTripped.Plan, original.Plan)
	}
	if roundTripped.SourceType != original.SourceType {
		t.Errorf("SourceType = %q, want %q", roundTripped.SourceType, original.SourceType)
	}
	if roundTripped.Fp != original.Fp {
		t.Errorf("Fp = %+v, want %+v", roundTripped.Fp, original.Fp)
	}
	if !roundTripped.LastRefreshAt.Equal(original.LastRefreshAt) {
		t.Errorf("LastRefreshAt = %v, want %v", roundTripped.LastRefreshAt, original.LastRefreshAt)
	}
	if roundTripped.LastRefreshError != original.LastRefreshError {
		t.Errorf("LastRefreshError = %q, want %q", roundTripped.LastRefreshError, original.LastRefreshError)
	}

	fileName := FileNameFor(original)
	resp, err := newAuthTestManager(t, &httpFakeCaller{}).Handle(pluginabi.MethodAuthParse, authParseRequestBody(t, providerKey, fileName, raw))
	if err != nil {
		t.Fatalf("Handle(auth.parse): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("auth.parse envelope not OK: %s", resp)
	}
	var parsed pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatalf("decode auth.parse result: %v (%s)", err, env.Result)
	}
	if !parsed.Handled {
		t.Fatalf("auth.parse must handle a %s record", providerKey)
	}
	if parsed.Auth.FileName != fileName {
		t.Errorf("Auth.FileName = %q, want %q (stability contract)", parsed.Auth.FileName, fileName)
	}
	if parsed.Auth.Provider != providerKey {
		t.Errorf("Auth.Provider = %q, want %q", parsed.Auth.Provider, providerKey)
	}
	if !bytes.Equal(parsed.Auth.StorageJSON, raw) {
		t.Errorf("auth.parse rewrote a complete record's storage payload")
	}
}

// TestParseForeignRecord pins the discriminator: anything that is not
// type=chatgpt-proxy — built-in codex files included — stays the host's and
// answers Handled:false in an OK envelope.
func TestParseForeignRecord(t *testing.T) {
	m := newAuthTestManager(t, &httpFakeCaller{})

	cases := []struct {
		name     string
		provider string
		raw      []byte
	}{
		{"codex file", "", []byte(`{"type":"codex","access_token":"codex-token"}`)},
		{"openai file", "", []byte(`{"type":"openai","tokens":{"access_token":"x"}}`)},
		{"missing type", "", []byte(`{"access_token":"no-type"}`)},
		{"non json payload", "", []byte("not json at all")},
		{"other provider requested", "codex", []byte(`{"type":"codex"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := m.Handle(pluginabi.MethodAuthParse, authParseRequestBody(t, tc.provider, "file.json", tc.raw))
			if err != nil {
				t.Fatalf("Handle(auth.parse): %v", err)
			}
			env := decodeEnv(t, resp)
			if !env.OK {
				t.Fatalf("foreign record must answer with an OK envelope, got: %s", resp)
			}
			var parsed pluginapi.AuthParseResponse
			if err := json.Unmarshal(env.Result, &parsed); err != nil {
				t.Fatalf("decode auth.parse result: %v (%s)", err, env.Result)
			}
			if parsed.Handled {
				t.Fatalf("foreign record must yield Handled:false: %s", resp)
			}
		})
	}
}

// TestFileNameForRule pins the auth-file naming contract: provider key
// prefix, 16 lowercase hex digest chars, .json suffix, and the first-known
// identity priority (email, then account id, then access token). Names are
// hashes — never emails.
func TestFileNameForRule(t *testing.T) {
	base := Credential{Email: "owner@example.com", AccountID: "acc-1", AccessToken: "tok"}
	name := FileNameFor(base)
	if !strings.HasPrefix(name, providerKey+"-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("file name %q does not follow %s-<16 hex>.json", name, providerKey)
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(name, providerKey+"-"), ".json")
	if _, err := hex.DecodeString(digest); err != nil || len(digest) != 16 {
		t.Fatalf("digest %q is not 16 lowercase hex chars", digest)
	}
	if strings.Contains(name, "owner@example.com") {
		t.Fatalf("file name must never contain the email: %q", name)
	}
	// Email beats account id and access token.
	if FileNameFor(base) != FileNameFor(Credential{Email: "owner@example.com"}) {
		t.Fatalf("email-first identity priority broken")
	}
	// Without email, account id beats access token.
	if FileNameFor(Credential{AccountID: "acc-1", AccessToken: "tok"}) !=
		FileNameFor(Credential{AccountID: "acc-1"}) {
		t.Fatalf("account-id-second identity priority broken")
	}
	// Deterministic across calls.
	if FileNameFor(base) != name {
		t.Fatalf("file name not deterministic")
	}
}

// buildFixtureJWT assembles an unsigned-shape JWT (header.payload.signature)
// whose payload segment carries the given claims. The signature is never
// verified — decodeJWTPayload reads the base64url payload only.
func buildFixtureJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jwt claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		enc.EncodeToString(payload) + "." +
		enc.EncodeToString([]byte("fixture-signature"))
}

// TestParsePartial pins the normalization rule: an access-token-only record
// is accepted, and missing Email/AccountID are filled from the access token's
// JWT claims (https://api.openai.com/profile / .../auth) offline; values
// already present in the record are never overwritten.
func TestParsePartial(t *testing.T) {
	access := buildFixtureJWT(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "partial@example.com"},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acc-partial-1"},
	})
	partial := Credential{AccessToken: access, SourceType: "web"}

	fileName := FileNameFor(partial)
	resp, err := newAuthTestManager(t, &httpFakeCaller{}).Handle(
		pluginabi.MethodAuthParse, authParseRequestBody(t, providerKey, fileName, partial.StorageJSON()))
	if err != nil {
		t.Fatalf("Handle(auth.parse): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("auth.parse envelope not OK: %s", resp)
	}
	var parsed pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatalf("decode auth.parse result: %v (%s)", err, env.Result)
	}
	if !parsed.Handled {
		t.Fatalf("access-token-only record must be handled: %s", resp)
	}
	normalized, err := FromStorageJSON(parsed.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(normalized): %v", err)
	}
	if normalized.Email != "partial@example.com" {
		t.Errorf("Email = %q, want email filled from JWT profile claim", normalized.Email)
	}
	if normalized.AccountID != "acc-partial-1" {
		t.Errorf("AccountID = %q, want account id filled from JWT auth claim", normalized.AccountID)
	}
	if normalized.AccessToken != access {
		t.Errorf("AccessToken changed during normalization")
	}

	// Facts already present in the record win over the claims.
	kept := Credential{AccessToken: access, Email: "kept@example.com", AccountID: "kept-acc"}
	keptName := FileNameFor(kept)
	resp, err = newAuthTestManager(t, &httpFakeCaller{}).Handle(
		pluginabi.MethodAuthParse, authParseRequestBody(t, providerKey, keptName, kept.StorageJSON()))
	if err != nil {
		t.Fatalf("Handle(auth.parse, kept): %v", err)
	}
	env = decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("auth.parse envelope not OK: %s", resp)
	}
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatalf("decode auth.parse result: %v (%s)", err, env.Result)
	}
	keptBack, err := FromStorageJSON(parsed.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(kept): %v", err)
	}
	if keptBack.Email != "kept@example.com" || keptBack.AccountID != "kept-acc" {
		t.Errorf("record values overwritten by JWT claims: %+v", keptBack)
	}
}

// TestRefreshFormFields pins the refresh_token grant wire shape: the request
// goes to the exact endpoint through host.http.do as a POST form with exactly
// grant_type / refresh_token / client_id, and the callback scope is threaded.
func TestRefreshFormFields(t *testing.T) {
	cred := Credential{AccessToken: "old-access", RefreshToken: "refresh-secret-1", IDToken: "old-id"}
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 200,
		Body:       mustJSON(t, map[string]any{"access_token": "new-access"}),
	}}}
	m := newAuthTestManager(t, fake)

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	decodeRefreshResponse(t, resp)

	call := fake.callFor(t, pluginabi.MethodHostHTTPDo)
	var req hostHTTPRequest
	if err := json.Unmarshal(call.request, &req); err != nil {
		t.Fatalf("decode captured host.http.do request: %v", err)
	}
	if req.Method != "POST" {
		t.Errorf("host.http.do method = %q, want POST", req.Method)
	}
	if req.URL != oauthTokenURL {
		t.Errorf("host.http.do url = %q, want %q", req.URL, oauthTokenURL)
	}
	if req.HostCallbackID != "cb-1" {
		t.Errorf("host_callback_id = %q, want %q", req.HostCallbackID, "cb-1")
	}
	if contentType := strings.Join(req.Headers["Content-Type"], ","); contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", contentType)
	}
	values, err := url.ParseQuery(string(req.Body))
	if err != nil {
		t.Fatalf("decode form body: %v", err)
	}
	if got := values.Get("grant_type"); got != "refresh_token" {
		t.Errorf("form grant_type = %q, want refresh_token", got)
	}
	if got := values.Get("refresh_token"); got != "refresh-secret-1" {
		t.Errorf("form refresh_token = %q, want the record's refresh token", got)
	}
	if got := values.Get("client_id"); got != oauthClientID {
		t.Errorf("form client_id = %q, want %q", got, oauthClientID)
	}
	if len(values) != 3 {
		t.Errorf("form body has %d fields, want exactly 3: %v", len(values), values)
	}
}

// TestRefreshRenewsTriple pins the success path: the new access/refresh/id
// triple is applied, the storage type discriminator is preserved, the error
// field clears, and the next refresh is scheduled refreshSkew out.
func TestRefreshRenewsTriple(t *testing.T) {
	cred := fullCredential()
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 200,
		Body: mustJSON(t, map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"id_token":      "new-id-token",
			"expires_in":    28800,
		}),
	}}}
	m := newAuthTestManager(t, fake)
	before := time.Now()

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	after := time.Now()
	out := decodeRefreshResponse(t, resp)

	updated, err := FromStorageJSON(out.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(refreshed): %v", err)
	}
	if updated.AccessToken != "new-access-token" {
		t.Errorf("AccessToken = %q, want the renewed access token", updated.AccessToken)
	}
	if updated.RefreshToken != "new-refresh-token" {
		t.Errorf("RefreshToken = %q, want the renewed refresh token", updated.RefreshToken)
	}
	if updated.IDToken != "new-id-token" {
		t.Errorf("IDToken = %q, want the renewed id token", updated.IDToken)
	}
	if updated.LastRefreshError != "" {
		t.Errorf("LastRefreshError = %q, want cleared after success", updated.LastRefreshError)
	}
	if !updated.LastRefreshAt.After(before.Add(-time.Second)) || updated.LastRefreshAt.After(after.Add(time.Second)) {
		t.Errorf("LastRefreshAt = %v, want within [%v, %v]", updated.LastRefreshAt, before, after)
	}
	// Identity facts survive the rotation.
	if updated.Email != cred.Email || updated.AccountID != cred.AccountID {
		t.Errorf("identity facts lost during refresh: %+v", updated)
	}
	if out.NextRefreshAfter.Before(before.Add(accessTokenRefreshSkew-time.Minute)) ||
		out.NextRefreshAfter.After(after.Add(accessTokenRefreshSkew+time.Minute)) {
		t.Errorf("NextRefreshAfter = %v, want ≈ now+%v", out.NextRefreshAfter, accessTokenRefreshSkew)
	}

	// A provider response missing a token keeps the record's current value
	// for that slot (only access_token is mandatory).
	fake = &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 200,
		Body:       mustJSON(t, map[string]any{"access_token": "rotated-only"}),
	}}}
	m = newAuthTestManager(t, fake)
	resp, err = m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, updated, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh, partial response): %v", err)
	}
	out = decodeRefreshResponse(t, resp)
	partialUpdate, err := FromStorageJSON(out.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(partial update): %v", err)
	}
	if partialUpdate.AccessToken != "rotated-only" {
		t.Errorf("AccessToken = %q, want rotated-only", partialUpdate.AccessToken)
	}
	if partialUpdate.RefreshToken != updated.RefreshToken {
		t.Errorf("RefreshToken = %q, want unchanged when the response omits it", partialUpdate.RefreshToken)
	}
}

// TestRefreshPersistsPlan pins the refresh half of plan persistence (oracle
// background-watcher semantics: a successful refresh updates plan_type): the
// record's plan comes from the accounts/check leg, the persisted plan drives
// the for_auth codex image gate end to end, and refresh failures — including
// plan-probe failures — leave the plan untouched.
func TestRefreshPersistsPlan(t *testing.T) {
	cred := fullCredential()
	cred.Plan = "" // the record has no plan yet (e.g. imported)
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"id_token":      "new-id-token",
		})},
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "pro"}}},
		})},
	}}
	m := newAuthTestManager(t, fake)
	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	out := decodeRefreshResponse(t, resp)
	updated, err := FromStorageJSON(out.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(refreshed): %v", err)
	}
	if updated.Plan != "pro" {
		t.Fatalf("persisted Plan = %q, want pro from the accounts/check leg", updated.Plan)
	}
	// The persisted plan publishes the codex image route in for_auth.
	modelsFake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{
		modelScriptKey(modelsRouteURL, updated): modelsScript(200, `{"models":[]}`),
	}}
	out2 := decodeModelResponse(t, handleModel(t, modelManager(t, modelsFake),
		pluginabi.MethodModelForAuth, modelForAuthBody(t, updated)))
	if _, ok := modelsByID(out2)["chatgpt-proxy/codex-gpt-image-2"]; !ok {
		t.Fatalf("codex image model not published for the plan refresh persisted: %v", modelsByID(out2))
	}

	t.Run("refresh failure leaves plan untouched", func(t *testing.T) {
		cred := fullCredential()
		cred.Plan = ""
		fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
			StatusCode: 400,
			Body:       mustJSON(t, map[string]any{"error": "invalid_grant"}),
		}}}
		m := newAuthTestManager(t, fake)
		resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
		if err != nil {
			t.Fatalf("Handle(auth.refresh): %v", err)
		}
		out := decodeRefreshResponse(t, resp) // soft failure replies OK with backoff
		updated, err := FromStorageJSON(out.Auth.StorageJSON)
		if err != nil {
			t.Fatalf("FromStorageJSON: %v", err)
		}
		if updated.Plan != "" {
			t.Fatalf("Plan = %q after a failed refresh, want untouched (empty)", updated.Plan)
		}
	})

	t.Run("plan probe failure does not fail refresh", func(t *testing.T) {
		cred := fullCredential()
		cred.Plan = ""
		fake := &httpFakeCaller{httpResults: []hostHTTPResponse{
			{StatusCode: 200, Body: mustJSON(t, map[string]any{
				"access_token":  "new-access-token",
				"refresh_token": "new-refresh-token",
				"id_token":      "new-id-token",
			})},
			{StatusCode: 401, Body: mustJSON(t, map[string]any{"error": "unauthorized"})}, // typed upstream error, swallowed as best-effort
		}}
		m := newAuthTestManager(t, fake)
		resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
		if err != nil {
			t.Fatalf("Handle(auth.refresh): %v", err)
		}
		out := decodeRefreshResponse(t, resp)
		updated, err := FromStorageJSON(out.Auth.StorageJSON)
		if err != nil {
			t.Fatalf("FromStorageJSON: %v", err)
		}
		if updated.AccessToken != "new-access-token" {
			t.Fatalf("refresh did not renew the tokens despite the plan probe failure: %+v", updated)
		}
		if updated.Plan != "" {
			t.Fatalf("Plan = %q after a plan probe failure, want untouched (empty)", updated.Plan)
		}
	})
}

// TestRefreshFileNameStable pins the stability contract (Review Focus #3):
// refresh returns the input record's file name byte-identical, so a refresh
// can never rename the pool entry or race an in-flight rotation.
func TestRefreshFileNameStable(t *testing.T) {
	cred := fullCredential()
	fileName := "chatgpt-proxy-0123456789abcdef.json"
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 200,
		Body:       mustJSON(t, map[string]any{"access_token": "new-access-token"}),
	}}}
	m := newAuthTestManager(t, fake)

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, fileName, "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	out := decodeRefreshResponse(t, resp)
	if out.Auth.FileName != fileName {
		t.Fatalf("Auth.FileName = %q, want byte-identical input %q", out.Auth.FileName, fileName)
	}
	if out.Auth.ID != fileName {
		t.Errorf("Auth.ID = %q, want the pool identifier %q", out.Auth.ID, fileName)
	}
}

// TestRefreshBackoff pins the soft-failure path: an HTTP 400 keeps the
// tokens untouched, records the error in LastRefreshError and schedules the
// next refresh only refreshErrorBackoff out.
func TestRefreshBackoff(t *testing.T) {
	cred := fullCredential()
	before := time.Now()
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 400,
		Body:       mustJSON(t, map[string]any{"error": "invalid_grant", "error_description": "refresh token expired"}),
	}}}
	m := newAuthTestManager(t, fake)

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	after := time.Now()
	out := decodeRefreshResponse(t, resp)

	updated, err := FromStorageJSON(out.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(backoff): %v", err)
	}
	if updated.AccessToken != cred.AccessToken || updated.RefreshToken != cred.RefreshToken || updated.IDToken != cred.IDToken {
		t.Errorf("tokens changed on a failed refresh: %+v", updated)
	}
	if updated.LastRefreshError == "" {
		t.Errorf("LastRefreshError not set on failure")
	}
	if !strings.Contains(updated.LastRefreshError, "400") {
		t.Errorf("LastRefreshError = %q, want the http status in it", updated.LastRefreshError)
	}
	if out.NextRefreshAfter.Before(before.Add(refreshErrorBackoff-time.Minute)) ||
		out.NextRefreshAfter.After(after.Add(refreshErrorBackoff+time.Minute)) {
		t.Errorf("NextRefreshAfter = %v, want ≈ now+%v", out.NextRefreshAfter, refreshErrorBackoff)
	}
}

// TestRefreshSessionTerminated pins the terminal failure: an
// app_session_terminated body answers a typed, non-retryable
// credential_terminated error envelope (only a fresh login recovers it).
func TestRefreshSessionTerminated(t *testing.T) {
	cred := fullCredential()
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 401,
		Body:       mustJSON(t, map[string]any{"error": "invalid_grant", "error_description": "app_session_terminated"}),
	}}}
	m := newAuthTestManager(t, fake)

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	env := decodeErrorEnvelope(t, resp)
	if env.Error.Code != "credential_terminated" {
		t.Fatalf("error code = %q, want credential_terminated", env.Error.Code)
	}
	if env.Error.Retryable {
		t.Fatalf("credential_terminated must be non-retryable")
	}
	if env.Error.HTTPStatus != 401 {
		t.Errorf("http_status = %d, want 401", env.Error.HTTPStatus)
	}
}

// TestRefreshWithoutRefreshToken pins that a partial record (accepted by
// parse) is not silently retried forever: refresh answers a typed
// invalid_credential error instead of a soft backoff.
func TestRefreshWithoutRefreshToken(t *testing.T) {
	cred := Credential{AccessToken: buildFixtureJWT(t, map[string]any{}), SourceType: "web"}
	fake := &httpFakeCaller{}
	m := newAuthTestManager(t, fake)

	resp, err := m.Handle(pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	env := decodeErrorEnvelope(t, resp)
	if env.Error.Code != "invalid_credential" {
		t.Fatalf("error code = %q, want invalid_credential", env.Error.Code)
	}
	if len(fake.calls) != 0 {
		t.Errorf("missing refresh token must not touch the transport seam: %v", fake.methods())
	}
}

// TestRefreshNoSecretInError pins the secret rule (Review Focus #6): the
// refresh token appears only inside the credential payload the host already
// owns — never in an error message or any other envelope field, on either
// the backoff or the terminated path.
func TestRefreshNoSecretInError(t *testing.T) {
	const secretRT = "rt-secret-must-not-leak-0123456789"
	cred := fullCredential()
	cred.RefreshToken = secretRT

	// Soft-failure path: blank StorageJSON in the response, scan the rest.
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 400,
		Body:       mustJSON(t, map[string]any{"error": "invalid_grant"}),
	}}}
	resp, err := newAuthTestManager(t, fake).Handle(
		pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh): %v", err)
	}
	out := decodeRefreshResponse(t, resp)
	blank := out
	blank.Auth.StorageJSON = nil
	blanked, _ := json.Marshal(blank)
	if strings.Contains(string(blanked), secretRT) {
		t.Errorf("refresh token leaked outside StorageJSON on backoff: %s", blanked)
	}
	if decoded, err := FromStorageJSON(out.Auth.StorageJSON); err == nil &&
		strings.Contains(decoded.LastRefreshError, secretRT) {
		t.Errorf("LastRefreshError leaked the refresh token: %q", decoded.LastRefreshError)
	}

	// Terminated path: scan the whole error envelope.
	fake = &httpFakeCaller{httpResults: []hostHTTPResponse{{
		StatusCode: 401,
		Body:       mustJSON(t, map[string]any{"error": "invalid_grant", "error_description": "app_session_terminated"}),
	}}}
	resp, err = newAuthTestManager(t, fake).Handle(
		pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh, terminated): %v", err)
	}
	if strings.Contains(string(resp), secretRT) {
		t.Errorf("refresh token leaked into the error envelope: %s", resp)
	}

	// Transport failure path: the host callback itself errors.
	fake = &httpFakeCaller{errorCode: "transport_error"}
	resp, err = newAuthTestManager(t, fake).Handle(
		pluginabi.MethodAuthRefresh, authRefreshRequestBody(t, cred, "chatgpt-proxy-0123456789abcdef.json", "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.refresh, transport): %v", err)
	}
	out = decodeRefreshResponse(t, resp)
	blank = out
	blank.Auth.StorageJSON = nil
	blanked, _ = json.Marshal(blank)
	if strings.Contains(string(blanked), secretRT) {
		t.Errorf("refresh token leaked outside StorageJSON on transport failure: %s", blanked)
	}
}

func (f *httpFakeCaller) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return methodsOf(f.calls)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}
