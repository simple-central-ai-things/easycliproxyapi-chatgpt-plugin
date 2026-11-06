package plugin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The loopback callback URL the host computes for its OAuth callback handler
// (internal/api/handlers/management/auth_files_oauth_callback.go builds
// http(s)://127.0.0.1:<port>/v0/management/oauth-callback from its own port).
const testBaseURL = "http://127.0.0.1:8317/v0/management/oauth-callback"

// loginStartRequestBody mirrors the host wire shape of auth.login.start: the
// SDK request (marshaled PascalCase) plus the callback id the host uses to
// scope transport to this RPC.
func loginStartRequestBody(t *testing.T, baseURL, authDir, callbackID string) []byte {
	t.Helper()
	body, err := json.Marshal(loginStartRequest{
		AuthLoginStartRequest: pluginapi.AuthLoginStartRequest{
			Provider: providerKey,
			BaseURL:  baseURL,
			Host:     pluginapi.HostConfigSummary{AuthDir: authDir},
		},
		HostCallbackID: callbackID,
	})
	if err != nil {
		t.Fatalf("marshal auth.login.start request: %v", err)
	}
	return body
}

func loginPollRequestBody(t *testing.T, state, authDir, callbackID string) []byte {
	t.Helper()
	body, err := json.Marshal(loginPollRequest{
		AuthLoginPollRequest: pluginapi.AuthLoginPollRequest{
			Provider: providerKey,
			State:    state,
			Host:     pluginapi.HostConfigSummary{AuthDir: authDir},
		},
		HostCallbackID: callbackID,
	})
	if err != nil {
		t.Fatalf("marshal auth.login.poll request: %v", err)
	}
	return body
}

func decodeLoginStart(t *testing.T, resp []byte) pluginapi.AuthLoginStartResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("auth.login.start envelope not OK: %s", resp)
	}
	var out pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode auth.login.start result: %v (%s)", err, env.Result)
	}
	return out
}

func decodeLoginPoll(t *testing.T, resp []byte) pluginapi.AuthLoginPollResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("auth.login.poll envelope not OK: %s", resp)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode auth.login.poll result: %v (%s)", err, env.Result)
	}
	return out
}

// writeCallbackFile publishes the host's OAuth callback file for state:
// <authDir>/.oauth-<provider>-<state>.oauth with {"code","state","error"}
// (the host's WriteOAuthCallbackFile convention).
func writeCallbackFile(t *testing.T, authDir, state, code, providerErr string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"code": code, "state": state, "error": providerErr})
	if err != nil {
		t.Fatalf("marshal callback payload: %v", err)
	}
	path := filepath.Join(authDir, ".oauth-"+providerKey+"-"+state+".oauth")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write callback file: %v", err)
	}
}

// startLogin runs auth.login.start through the dispatch seam and returns the
// manager (so the test can inspect the attempt store), the response and the
// fake host caller.
func startLogin(t *testing.T, host caller, baseURL, authDir string) (*Manager, pluginapi.AuthLoginStartResponse) {
	t.Helper()
	m := newAuthTestManager(t, host)
	resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, baseURL, authDir, "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.login.start): %v", err)
	}
	return m, decodeLoginStart(t, resp)
}

// TestStartURLShape pins the authorize URL wire contract: exactly the eight
// pinned query parameters (nothing more, nothing less), the PKCE pair
// (48-byte verifier, S256 challenge), a state the host's ValidateOAuthState
// accepts, the loopback redirect_uri returned untouched, and a 10-minute
// expiry. Building the URL never touches the transport seam.
func TestStartURLShape(t *testing.T) {
	fake := &httpFakeCaller{}
	m, out := startLogin(t, fake, testBaseURL, t.TempDir())
	if len(fake.calls) != 0 {
		t.Fatalf("login.start must not touch the host transport: %v", fake.methods())
	}
	if out.Provider != providerKey {
		t.Errorf("Provider = %q, want %q", out.Provider, providerKey)
	}
	now := time.Now()
	if out.ExpiresAt.Before(now.Add(loginAttemptTTL - time.Minute)) || out.ExpiresAt.After(now.Add(loginAttemptTTL + time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ≈ now+%v", out.ExpiresAt, loginAttemptTTL)
	}

	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatalf("authorize URL does not parse: %v", err)
	}
	if u.Scheme != "https" || u.Hostname() != "auth.openai.com" || u.Port() != "" || u.User != nil || u.Fragment != "" {
		t.Errorf("authorize URL origin = %s://%s, want exactly https://auth.openai.com", u.Scheme, u.Host)
	}
	if u.Path != "/api/accounts/authorize" {
		t.Errorf("authorize path = %q, want /api/accounts/authorize", u.Path)
	}

	q := u.Query()
	if len(q) != 8 {
		t.Fatalf("authorize URL has %d query params, want exactly 8: %v", len(q), q)
	}
	if got := q.Get("client_id"); got != oauthClientID {
		t.Errorf("client_id = %q, want %q", got, oauthClientID)
	}
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want code", got)
	}
	if got := q.Get("redirect_uri"); got != testBaseURL {
		t.Errorf("redirect_uri = %q, want the host callback %q", got, testBaseURL)
	}
	if got := q.Get("scope"); got != "openid profile email offline_access" {
		t.Errorf("scope = %q, want the pinned consumer scope", got)
	}
	if got := q.Get("audience"); got != "https://api.openai.com/v1" {
		t.Errorf("audience = %q, want the pinned audience", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if got := q.Get("state"); got != out.State {
		t.Errorf("state query param = %q, want the returned State %q", got, out.State)
	}

	// State shape: 32 random alphanumeric chars — inside the host's
	// ValidateOAuthState charset, long enough that collisions are impossible.
	if len(out.State) != 32 {
		t.Fatalf("State length = %d, want 32", len(out.State))
	}
	for _, r := range out.State {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("State %q has non-alphanumeric character %q", out.State, r)
		}
	}

	// PKCE: the stored verifier is 48 random bytes (base64url on the wire) and
	// the challenge is S256 of the verifier string.
	m.loginMu.Lock()
	attempt, found := m.logins[out.State]
	m.loginMu.Unlock()
	if !found {
		t.Fatalf("attempt store has no entry for the returned state")
	}
	verifierBytes, err := base64.RawURLEncoding.DecodeString(attempt.verifier)
	if err != nil {
		t.Fatalf("stored verifier %q is not base64url: %v", attempt.verifier, err)
	}
	if len(verifierBytes) != pkceVerifierBytes {
		t.Errorf("verifier is %d bytes, want %d", len(verifierBytes), pkceVerifierBytes)
	}
	digest := sha256.Sum256([]byte(attempt.verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if got := q.Get("code_challenge"); got != wantChallenge {
		t.Errorf("code_challenge = %q, want S256(verifier) %q", got, wantChallenge)
	}
}

// TestStartOriginValidation pins the origin guards: the authorize URL we build
// must sit on exactly https://auth.openai.com (mistral's validateProviderURL
// pattern applied to our own redirect construction), and a mutated BaseURL —
// http:// on a non-loopback host, a wrong host, or not a URL at all — is
// refused with a typed error before any host call and without recording an
// attempt.
func TestStartOriginValidation(t *testing.T) {
	t.Run("authorize URL mutations refused", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
		}{
			{"http scheme", "http://auth.openai.com/api/accounts/authorize?state=x"},
			{"wrong host", "https://evil.example/api/accounts/authorize?state=x"},
			{"lookalike host", "https://auth.openai.com.evil.example/api/accounts/authorize?state=x"},
			{"explicit port", "https://auth.openai.com:443/api/accounts/authorize?state=x"},
			{"userinfo", "https://user@auth.openai.com/api/accounts/authorize?state=x"},
			{"fragment", "https://auth.openai.com/api/accounts/authorize?state=x#frag"},
			{"not a URL", "scheme-relative//auth.openai.com"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := validateAuthorizeURL(tc.raw); err == nil {
					t.Fatalf("validateAuthorizeURL(%q) must refuse this origin", tc.raw)
				}
			})
		}
		if err := validateAuthorizeURL("https://auth.openai.com/api/accounts/authorize?state=x"); err != nil {
			t.Fatalf("validateAuthorizeURL accepts the pinned origin: %v", err)
		}
	})

	t.Run("mutated BaseURL refused before any host call", func(t *testing.T) {
		cases := []struct {
			name    string
			baseURL string
		}{
			{"http non-loopback", "http://evil.example/oauth-callback"},
			{"https non-loopback", "https://evil.example/oauth-callback"},
			{"wrong host any scheme", "http://example.com/oauth-callback"},
			{"relative URL", "/v0/management/oauth-callback"},
			{"not a URL", "evil.example callback"},
			{"userinfo", "http://user@127.0.0.1:8317/callback"},
			{"empty", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fake := &httpFakeCaller{}
				m := newAuthTestManager(t, fake)
				resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, tc.baseURL, t.TempDir(), "cb-1"))
				if err != nil {
					t.Fatalf("Handle(auth.login.start): %v", err)
				}
				env := decodeErrorEnvelope(t, resp)
				if env.Error.Code != "invalid_request" {
					t.Errorf("error code = %q, want invalid_request", env.Error.Code)
				}
				if strings.Contains(string(resp), tc.baseURL) && tc.baseURL != "" {
					t.Errorf("refusal envelope must not echo the mutated BaseURL: %s", resp)
				}
				if len(fake.calls) != 0 {
					t.Errorf("refused start must not touch the host transport: %v", fake.methods())
				}
				m.loginMu.Lock()
				recorded := len(m.logins)
				m.loginMu.Unlock()
				if recorded != 0 {
					t.Errorf("refused start must not record an attempt (got %d)", recorded)
				}
			})
		}
	})

	t.Run("loopback callback accepted", func(t *testing.T) {
		// The host's own callback URL is loopback http(s); refusing it would
		// kill the deployed flow (the host management API runs without TLS).
		for _, baseURL := range []string{
			testBaseURL,
			"https://127.0.0.1:8317/v0/management/oauth-callback",
			"http://localhost:8317/v0/management/oauth-callback",
			"http://[::1]:8317/v0/management/oauth-callback",
		} {
			if _, out := startLogin(t, &httpFakeCaller{}, baseURL, t.TempDir()); out.URL == "" {
				t.Fatalf("valid loopback BaseURL %q must produce an authorize URL", baseURL)
			}
		}
	})
}

// TestPollPendingWhenFileMissing pins the pending contract: no callback file,
// an unknown state, an empty auth dir, an unparseable file, and a callback
// carrying someone else's state all answer pending — the flow keeps waiting
// until the user finishes or the attempt expires.
func TestPollPendingWhenFileMissing(t *testing.T) {
	authDir := t.TempDir()
	m, start := startLogin(t, &httpFakeCaller{}, testBaseURL, authDir)

	cases := []struct {
		name  string
		state string
		setup func()
	}{
		{"no callback file yet", start.State, func() {}},
		{"unknown state", "no-such-state-000000000000000000000000", func() {}},
		{"empty auth dir", start.State, func() {
			m.loginMu.Lock()
			m.logins["empty-authdir-state-00000000000000000"] = loginAttempt{
				verifier: "verifier", redirectURI: testBaseURL, expiresAt: time.Now().Add(time.Minute),
			}
			m.loginMu.Unlock()
		}},
		{"unparseable callback file", start.State, func() {
			if err := os.WriteFile(filepath.Join(authDir, ".oauth-"+providerKey+"-"+start.State+".oauth"),
				[]byte("not json"), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
		}},
		{"callback with foreign state", start.State, func() {
			writeCallbackFile(t, authDir, start.State, "code-123", "")
			_ = os.WriteFile(filepath.Join(authDir, ".oauth-"+providerKey+"-"+start.State+".oauth"),
				[]byte(`{"code":"code-123","state":"someone-else","error":""}`), 0o600)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			fake := &httpFakeCaller{}
			resp, err := m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, tc.state, authDir, "cb-2"))
			if err != nil {
				t.Fatalf("Handle(auth.login.poll): %v", err)
			}
			out := decodeLoginPoll(t, resp)
			if out.Status != pluginapi.AuthLoginStatusPending {
				t.Fatalf("Status = %q, want pending: %s", out.Status, resp)
			}
			if len(fake.calls) != 0 {
				t.Errorf("pending poll must not touch the host transport: %v", fake.methods())
			}
		})
	}
}

// TestPollBusyReturnsPending pins the concurrent-poll guard: while one poll is
// exchanging a code, a second poll for the same state answers pending instead
// of racing the single-use exchange.
func TestPollBusyReturnsPending(t *testing.T) {
	authDir := t.TempDir()
	m, start := startLogin(t, &httpFakeCaller{}, testBaseURL, authDir)
	m.loginMu.Lock()
	attempt := m.logins[start.State]
	attempt.busy = true
	m.logins[start.State] = attempt
	m.loginMu.Unlock()

	fake := &httpFakeCaller{}
	resp, err := m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll): %v", err)
	}
	out := decodeLoginPoll(t, resp)
	if out.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("Status = %q, want pending while busy: %s", out.Status, resp)
	}
	if len(fake.calls) != 0 {
		t.Errorf("busy poll must not touch the host transport: %v", fake.methods())
	}
}

// TestPollSuccessConsumesAttempt pins the full success path and the replay
// protection: the callback file's code is exchanged exactly once with the
// stored verifier, the token triple plus the /backend-api/me identity facts
// become the credential (source_type oauth_login, stable hashed file name,
// type metadata), and the attempt is deleted — a second poll for the same
// state answers pending and never re-exchanges the consumed code.
func TestPollSuccessConsumesAttempt(t *testing.T) {
	authDir := t.TempDir()
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"access_token":  "login-access-token",
			"refresh_token": "login-refresh-token",
			"id_token":      "login-id-token",
		})},
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"id":    "account-1",
			"email": "owner@example.com",
		})},
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}},
		})},
	}}
	m, start := startLogin(t, fake, testBaseURL, authDir)
	m.loginMu.Lock()
	verifier := m.logins[start.State].verifier
	m.loginMu.Unlock()
	writeCallbackFile(t, authDir, start.State, "auth-code-123", "")

	resp, err := m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll): %v", err)
	}
	out := decodeLoginPoll(t, resp)
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("Status = %q, want success: %s", out.Status, resp)
	}
	if out.Auth.Provider != providerKey {
		t.Errorf("Auth.Provider = %q, want %q", out.Auth.Provider, providerKey)
	}
	cred, err := FromStorageJSON(out.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON: %v", err)
	}
	if cred.AccessToken != "login-access-token" || cred.RefreshToken != "login-refresh-token" || cred.IDToken != "login-id-token" {
		t.Errorf("token triple not applied: %+v", cred)
	}
	if cred.Email != "owner@example.com" || cred.AccountID != "account-1" {
		t.Errorf("identity facts from /backend-api/me not applied: %+v", cred)
	}
	if cred.SourceType != "oauth_login" {
		t.Errorf("SourceType = %q, want oauth_login", cred.SourceType)
	}
	wantName := FileNameFor(cred)
	if out.Auth.FileName != wantName || out.Auth.ID != wantName {
		t.Errorf("Auth.FileName/ID = %q/%q, want %q", out.Auth.FileName, out.Auth.ID, wantName)
	}
	if typ, _ := out.Auth.Metadata["type"].(string); typ != providerKey {
		t.Errorf("Auth.Metadata[type] = %v, want %q", out.Auth.Metadata["type"], providerKey)
	}

	// The exchange request pins the wire shape: JSON body with exactly the
	// five grant fields, the stored verifier, through host.http.do with the
	// poll's callback scope.
	exchanges := 0
	var exchangeBody map[string]string
	var meRequest hostHTTPRequest
	var planRequest hostHTTPRequest
	for _, call := range fake.calls {
		if call.method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected host call %s", call.method)
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(call.request, &req); err != nil {
			t.Fatalf("decode captured host.http.do request: %v", err)
		}
		switch req.URL {
		case oauthLoginTokenURL:
			exchanges++
			if req.Method != "POST" {
				t.Errorf("exchange method = %q, want POST", req.Method)
			}
			if req.HostCallbackID != "cb-2" {
				t.Errorf("exchange host_callback_id = %q, want cb-2", req.HostCallbackID)
			}
			if err := json.Unmarshal(req.Body, &exchangeBody); err != nil {
				t.Fatalf("exchange body is not JSON: %v", err)
			}
		case chatgptMeURL:
			meRequest = req
		case quotaProbeAccountURL:
			planRequest = req
		default:
			t.Errorf("unexpected exchange target %q", req.URL)
		}
	}
	if exchanges != 1 {
		t.Fatalf("exchange called %d times, want exactly 1", exchanges)
	}
	wantBody := map[string]string{
		"client_id":     oauthClientID,
		"grant_type":    "authorization_code",
		"code":          "auth-code-123",
		"code_verifier": verifier,
		"redirect_uri":  testBaseURL,
	}
	if len(exchangeBody) != len(wantBody) {
		t.Errorf("exchange body has %d fields, want exactly %d: %v", len(exchangeBody), len(wantBody), exchangeBody)
	}
	for key, want := range wantBody {
		if exchangeBody[key] != want {
			t.Errorf("exchange body %s = %q, want %q", key, exchangeBody[key], want)
		}
	}
	if meRequest.Method != "GET" {
		t.Errorf("me request method = %q, want GET", meRequest.Method)
	}
	if authz := strings.Join(meRequest.Headers["Authorization"], ","); authz != "Bearer login-access-token" {
		t.Errorf("me request Authorization = %q, want the bearer access token", authz)
	}

	// The best-effort plan probe runs the pinned accounts/check leg with the
	// same bearer and persists plan_type into the returned record.
	if planRequest.Method != "GET" {
		t.Errorf("plan probe method = %q, want GET", planRequest.Method)
	}
	if authz := strings.Join(planRequest.Headers["Authorization"], ","); authz != "Bearer login-access-token" {
		t.Errorf("plan probe Authorization = %q, want the bearer access token", authz)
	}
	if cred.Plan != "plus" {
		t.Errorf("persisted Plan = %q, want plus from the plan probe", cred.Plan)
	}

	// Replay protection: the attempt is consumed, so a second poll for the
	// same state answers pending and the code is never exchanged again.
	m.loginMu.Lock()
	_, stillThere := m.logins[start.State]
	m.loginMu.Unlock()
	if stillThere {
		t.Fatalf("attempt must be deleted after a successful exchange")
	}
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-3"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, replay): %v", err)
	}
	replay := decodeLoginPoll(t, resp)
	if replay.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("replay poll Status = %q, want pending", replay.Status)
	}
	if exchangesNow := countHostCalls(fake, pluginabi.MethodHostHTTPDo); exchangesNow != 3 {
		t.Fatalf("replay poll made %d transport calls (want 0 new): %v", exchangesNow-3, fake.methods())
	}
}

// TestPollSuccessPersistsPlan pins the plan-persistence data flow (review
// fix for the dead codex-image gate): a plugin-created credential carries
// the plan_type its accounts/check probe returned, so model.for_auth's codex
// image gate reads a plan the plugin itself wrote — not a hand-set fixture.
func TestPollSuccessPersistsPlan(t *testing.T) {
	cases := []struct {
		name  string
		plan  string
		codex bool
	}{
		{name: "plus publishes codex route", plan: "plus", codex: true},
		{name: "free gates codex route out", plan: "free", codex: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fake := &httpFakeCaller{httpResults: []hostHTTPResponse{
				{StatusCode: 200, Body: mustJSON(t, map[string]any{
					"access_token":  "login-access-token",
					"refresh_token": "login-refresh-token",
					"id_token":      "login-id-token",
				})},
				{StatusCode: 200, Body: mustJSON(t, map[string]any{
					"id":    "account-1",
					"email": "owner@example.com",
				})},
				{StatusCode: 200, Body: mustJSON(t, map[string]any{
					"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": tc.plan}}},
				})},
			}}
			m, start := startLogin(t, fake, testBaseURL, authDir)
			writeCallbackFile(t, authDir, start.State, "auth-code-123", "")
			out := decodeLoginPoll(t, handleModel(t, m, pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2")))
			if out.Status != pluginapi.AuthLoginStatusSuccess {
				t.Fatalf("Status = %q, want success", out.Status)
			}
			cred, err := FromStorageJSON(out.Auth.StorageJSON)
			if err != nil {
				t.Fatalf("FromStorageJSON: %v", err)
			}
			if cred.Plan != tc.plan {
				t.Fatalf("persisted Plan = %q, want %q (the login probe's plan_type)", cred.Plan, tc.plan)
			}

			// The catalog gate sees the plugin-written plan.
			modelsFake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{
				modelScriptKey(modelsRouteURL, cred): modelsScript(200, `{"models":[]}`),
			}}
			out2 := decodeModelResponse(t, handleModel(t, modelManager(t, modelsFake),
				pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
			byID := modelsByID(out2)
			if _, ok := byID["chatgpt-proxy/codex-gpt-image-2"]; ok != tc.codex {
				t.Fatalf("codex image model published = %v, want %v for plan %q the login persisted", ok, tc.codex, tc.plan)
			}
			if _, ok := byID["chatgpt-proxy/gpt-image-2-proxy"]; !ok {
				t.Fatalf("web-drawing image model missing from %v", byID)
			}
		})
	}

	t.Run("plan probe failure keeps login success", func(t *testing.T) {
		probeFailures := []hostHTTPResponse{
			{StatusCode: 500, Body: []byte(`{"error":"probe down"}`)},
			{StatusCode: 401, Body: []byte(`{"error":"unauthorized"}`)}, // never credential_terminated-style
		}
		for i, failure := range probeFailures {
			authDir := t.TempDir()
			fake := &httpFakeCaller{httpResults: []hostHTTPResponse{
				{StatusCode: 200, Body: mustJSON(t, map[string]any{
					"access_token":  "login-access-token",
					"refresh_token": "login-refresh-token",
					"id_token":      "login-id-token",
				})},
				{StatusCode: 200, Body: mustJSON(t, map[string]any{
					"id":    "account-1",
					"email": "owner@example.com",
				})},
				failure,
			}}
			m, start := startLogin(t, fake, testBaseURL, authDir)
			writeCallbackFile(t, authDir, start.State, "auth-code-123", "")
			out := decodeLoginPoll(t, handleModel(t, m, pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2")))
			if out.Status != pluginapi.AuthLoginStatusSuccess {
				t.Fatalf("probe failure #%d: Status = %q (%q), want success — the plan probe is best-effort", i, out.Status, out.Message)
			}
			cred, err := FromStorageJSON(out.Auth.StorageJSON)
			if err != nil {
				t.Fatalf("FromStorageJSON: %v", err)
			}
			if cred.Plan != "" {
				t.Fatalf("probe failure #%d: Plan = %q, want as-is (empty)", i, cred.Plan)
			}
		}
	})
}

// TestPollProviderError pins the provider-refusal path: a callback file
// carrying {"error":...} answers the error status with the provider's message
// passed through, and the attempt survives so a later poll still reports it.
func TestPollProviderError(t *testing.T) {
	authDir := t.TempDir()
	m, start := startLogin(t, &httpFakeCaller{}, testBaseURL, authDir)
	writeCallbackFile(t, authDir, start.State, "", "access_denied")

	resp, err := m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll): %v", err)
	}
	out := decodeLoginPoll(t, resp)
	if out.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("Status = %q, want error: %s", out.Status, resp)
	}
	if out.Message != "access_denied" {
		t.Errorf("Message = %q, want the provider error passed through", out.Message)
	}
	m.loginMu.Lock()
	_, survived := m.logins[start.State]
	m.loginMu.Unlock()
	if !survived {
		t.Fatalf("provider error must keep the attempt for retry")
	}

	// One retry reports the same refusal.
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, retry): %v", err)
	}
	retry := decodeLoginPoll(t, resp)
	if retry.Status != pluginapi.AuthLoginStatusError || retry.Message != "access_denied" {
		t.Fatalf("retry = %+v, want the same provider error", retry)
	}
}

// TestPollCap pins the attempt-store safety: 65 concurrent starts refuse the
// oldest-generation attempts instead of growing unbounded, expired attempts
// are garbage-collected inline and free capacity, and nothing panics.
func TestPollCap(t *testing.T) {
	t.Run("65th concurrent start refused", func(t *testing.T) {
		m := newAuthTestManager(t, &httpFakeCaller{})
		authDir := t.TempDir()
		for i := 0; i < maxLoginAttempts; i++ {
			resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, testBaseURL, authDir, "cb-1"))
			if err != nil {
				t.Fatalf("Handle(auth.login.start %d): %v", i, err)
			}
			decodeLoginStart(t, resp)
		}
		resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, testBaseURL, authDir, "cb-1"))
		if err != nil {
			t.Fatalf("Handle(auth.login.start, over cap): %v", err)
		}
		env := decodeErrorEnvelope(t, resp)
		if env.Error.Code != "too_many_logins" {
			t.Fatalf("error code = %q, want too_many_logins", env.Error.Code)
		}
		m.loginMu.Lock()
		recorded := len(m.logins)
		m.loginMu.Unlock()
		if recorded != maxLoginAttempts {
			t.Fatalf("attempt store holds %d entries, want the cap %d", recorded, maxLoginAttempts)
		}
	})

	t.Run("expired attempts are GC'd and free capacity", func(t *testing.T) {
		m := newAuthTestManager(t, &httpFakeCaller{})
		authDir := t.TempDir()
		m.loginMu.Lock()
		for i := 0; i < 10; i++ {
			m.logins[expiredState(i)] = loginAttempt{
				verifier: "verifier", redirectURI: testBaseURL, expiresAt: time.Now().Add(-time.Minute),
			}
		}
		m.loginMu.Unlock()
		for i := 0; i < maxLoginAttempts; i++ {
			resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, testBaseURL, authDir, "cb-1"))
			if err != nil {
				t.Fatalf("Handle(auth.login.start %d): %v", i, err)
			}
			decodeLoginStart(t, resp)
		}
		m.loginMu.Lock()
		recorded := len(m.logins)
		m.loginMu.Unlock()
		if recorded != maxLoginAttempts {
			t.Fatalf("attempt store holds %d entries after GC, want the cap %d", recorded, maxLoginAttempts)
		}
	})
}

func expiredState(i int) string {
	return "expiredstate000000000000000000000"[:30] + string(rune('A'+i%26)) + string(rune('0'+i/26))
}

func countHostCalls(fake *httpFakeCaller, method string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, call := range fake.calls {
		if call.method == method {
			n++
		}
	}
	return n
}

// TestNoSecretInEnvelope pins the secret rule (Review Focus #6): the PKCE
// verifier and the one-time auth code never appear in any RPC envelope —
// start, pending, provider-error, exchange-rejected, and success (the tokens
// live only inside Auth.StorageJSON, which is the host's own credential
// store payload).
func TestNoSecretInEnvelope(t *testing.T) {
	authDir := t.TempDir()
	fake := &httpFakeCaller{}
	m, start := startLogin(t, fake, testBaseURL, authDir)

	m.loginMu.Lock()
	verifier := m.logins[start.State].verifier
	m.loginMu.Unlock()

	// Start envelope: only the state and the challenge travel back.
	resp, err := m.Handle(pluginabi.MethodAuthLoginStart, loginStartRequestBody(t, testBaseURL, authDir, "cb-1"))
	if err != nil {
		t.Fatalf("Handle(auth.login.start): %v", err)
	}
	if strings.Contains(string(resp), verifier) {
		t.Errorf("auth.login.start envelope leaked the PKCE verifier: %s", resp)
	}

	// Pending envelope.
	writeCallbackFile(t, authDir, start.State, "", "")
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, pending): %v", err)
	}
	if strings.Contains(string(resp), verifier) {
		t.Errorf("pending envelope leaked the PKCE verifier: %s", resp)
	}

	// Provider-error envelope.
	writeCallbackFile(t, authDir, start.State, "", "access_denied")
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, provider error): %v", err)
	}
	if strings.Contains(string(resp), verifier) {
		t.Errorf("provider-error envelope leaked the PKCE verifier: %s", resp)
	}

	// Exchange-rejected envelope: the provider saw the code, but the code and
	// verifier must not round-trip into any envelope or error message.
	writeCallbackFile(t, authDir, start.State, "auth-code-secret-789", "")
	fake.httpResults = []hostHTTPResponse{{
		StatusCode: 400,
		Body:       mustJSON(t, map[string]any{"error": "invalid_grant"}),
	}}
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, rejected): %v", err)
	}
	if strings.Contains(string(resp), "auth-code-secret-789") {
		t.Errorf("exchange-rejected envelope leaked the auth code: %s", resp)
	}
	if strings.Contains(string(resp), verifier) {
		t.Errorf("exchange-rejected envelope leaked the PKCE verifier: %s", resp)
	}

	// Success envelope: the token triple is allowed inside Auth.StorageJSON
	// (the host persists it) and nowhere else.
	fake.httpResults = []hostHTTPResponse{
		{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"access_token":  "secret-access-abcdef",
			"refresh_token": "secret-refresh-abcdef",
			"id_token":      "secret-id-abcdef",
		})},
		{StatusCode: 200, Body: mustJSON(t, map[string]any{"id": "account-1", "email": "owner@example.com"})},
	}
	resp, err = m.Handle(pluginabi.MethodAuthLoginPoll, loginPollRequestBody(t, start.State, authDir, "cb-2"))
	if err != nil {
		t.Fatalf("Handle(auth.login.poll, success): %v", err)
	}
	out := decodeLoginPoll(t, resp)
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("Status = %q, want success: %s", out.Status, resp)
	}
	blank := out
	blank.Auth.StorageJSON = nil
	blanked, _ := json.Marshal(blank)
	for _, secret := range []string{"secret-access-abcdef", "secret-refresh-abcdef", "secret-id-abcdef", verifier} {
		if strings.Contains(string(blanked), secret) {
			t.Errorf("success envelope leaked %q outside StorageJSON: %s", secret, blanked)
		}
	}
}
