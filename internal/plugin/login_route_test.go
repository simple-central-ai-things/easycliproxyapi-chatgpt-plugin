package plugin

// login_route_test.go pins the password re-login management route
// (POST chatgpt-proxy/login/password): it exposes the SAME recovery core
// the auth.login.password RPC runs (handlePasswordLogin — no fork of the
// flow) through the F5 management plumbing, so the panel gets a
// built-in UI path to account recovery (the SDK's auth contract knows only
// auth.login.start / auth.login.poll). The registration pin lives in
// import_test.go's TestManagementRegisterListsImportRoutes (the exact route
// set); this file pins routing, validation reuse, the success + persistence
// contract, and the secret-hygiene canaries.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// passwordLoginSuccessScript is the six scripted legs every successful
// recovery runs (authorize bootstrap, sentinel challenge without PoW,
// verify with a continue_url code, token exchange, /backend-api/me, plan
// probe) — the F4 tests' fixture pattern (password_login_test.go).
func passwordLoginSuccessScript(t *testing.T) []hostHTTPResponse {
	t.Helper()
	script := passwordLoginAuthorizeScript(t)
	return append(script,
		hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"continue_url": "https://platform.openai.com/auth/callback?code=PASS-CODE-123&state=st",
		})},
		hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"access_token":  "pw-access-token",
			"refresh_token": "pw-refresh-token",
			"id_token":      "pw-id-token",
		})},
		hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"id":    "account-9",
			"email": testPasswordEmail,
		})},
		hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}},
		})},
	)
}

// TestLoginRouteRouting pins the internal exact-path router for the new
// route: POST the known path reaches the handler (the core's fail-closed
// validation answers), GET/PUT on the POST-only route answer 405 with the
// typed body, and an unknown path still answers 404.
func TestLoginRouteRouting(t *testing.T) {
	m := newAuthTestManager(t, newImportFakeCaller())

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword, []byte(`{}`))
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("known path + right method did not reach the handler: %d %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
		t.Fatalf("empty body is not the core's typed invalid_request 400: %s", out.Body)
	}

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		out = handleManagement(t, m, method, loginRoutePassword, nil)
		if out.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("known path + %s answered %d, want 405: %s", method, out.StatusCode, out.Body)
		}
		errBody = managementErrorBody{}
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_method_not_allowed" {
			t.Fatalf("wrong-method body is not the typed 405: %s", out.Body)
		}
	}

	out = handleManagement(t, m, http.MethodPost, providerKey+"/login/no-such-route", nil)
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	errBody = managementErrorBody{}
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_not_found" {
		t.Fatalf("unknown path body is not the typed 404: %s", out.Body)
	}
}

// TestLoginRouteSuccess pins the panel contract: the lowercase {email,
// password} body runs the shared core to success, answers the poll-shape
// body with 200, and persists the recovered credential itself through
// host.auth.save (a management.handle reply is served as-is — there is no
// auth.login.poll persistence contract) with the stable-hash name and
// source_type "password"; the stored record passes auth.parse unchanged.
func TestLoginRouteSuccess(t *testing.T) {
	fake := newImportFakeCaller()
	fake.httpResults = passwordLoginSuccessScript(t)
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword,
		[]byte(`{"email":"`+testPasswordEmail+`","password":"`+testPasswordSecret+`"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d, want 200: %s", out.StatusCode, out.Body)
	}
	if contentType := out.Headers.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("management response content type = %q", contentType)
	}
	var poll pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(out.Body, &poll); err != nil {
		t.Fatalf("response body is not the poll shape: %v (%s)", err, out.Body)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("Status = %q, want success: %s", poll.Status, out.Body)
	}
	if poll.Message != passwordLoginMessage {
		t.Fatalf("Message = %q, want %q", poll.Message, passwordLoginMessage)
	}
	if poll.Auth.Provider != providerKey || poll.Auth.FileName != poll.Auth.ID {
		t.Fatalf("Auth = %+v, want the provider record", poll.Auth)
	}
	if strings.Contains(poll.Auth.FileName, "example.com") {
		t.Fatalf("file name carries the email: %q", poll.Auth.FileName)
	}

	// Exactly one host.auth.save, on the stable-hash name.
	saves := 0
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostAuthSave {
			saves++
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(record.request, &req); err != nil {
				t.Fatalf("decode host.auth.save request: %v", err)
			}
			if req.Name != poll.Auth.FileName {
				t.Fatalf("host.auth.save name = %q, want %q", req.Name, poll.Auth.FileName)
			}
		}
	}
	if saves != 1 {
		t.Fatalf("%d host.auth.save calls, want 1: %v", saves, methodsOf(fake.calls))
	}
	if len(fake.store) != 1 {
		t.Fatalf("store holds %d files, want 1: %v", len(fake.store), fake.savedNames())
	}
	stored := fake.store[poll.Auth.FileName]
	cred, err := FromStorageJSON(stored)
	if err != nil {
		t.Fatalf("stored payload is not a credential record: %v (%s)", err, stored)
	}
	if cred.AccessToken != "pw-access-token" || cred.RefreshToken != "pw-refresh-token" || cred.IDToken != "pw-id-token" {
		t.Fatalf("stored tokens = %+v", cred)
	}
	if cred.SourceType != passwordSourceLogin {
		t.Fatalf("source_type = %q, want %q", cred.SourceType, passwordSourceLogin)
	}
	if want := FileNameFor(cred); poll.Auth.FileName != want {
		t.Fatalf("file name %q is not the stable-hash form for the record", poll.Auth.FileName)
	}

	// The stored record passes auth.parse exactly like a browser-login
	// record does (the F4 fixture pattern).
	parseResp, err := m.Handle(pluginabi.MethodAuthParse,
		authParseRequestBody(t, providerKey, poll.Auth.FileName, stored))
	if err != nil {
		t.Fatalf("auth.parse: %v", err)
	}
	env := decodeEnv(t, parseResp)
	if !env.OK {
		t.Fatalf("auth.parse envelope not OK: %s", parseResp)
	}
	var parsed pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil || !parsed.Handled {
		t.Fatalf("auth.parse did not claim the recovered credential: %s", parseResp)
	}
	reparsed, err := FromStorageJSON(parsed.Auth.StorageJSON)
	if err != nil {
		t.Fatalf("FromStorageJSON(round-trip): %v", err)
	}
	if reparsed.AccessToken != cred.AccessToken || reparsed.SourceType != cred.SourceType {
		t.Fatalf("auth.parse round-trip altered the record: %+v vs %+v", reparsed, cred)
	}
}

// TestLoginRouteAcceptsRPCWireShape pins the type reuse: the RPC's
// PascalCase spelling (passwordLoginRequest) reaches the same core.
func TestLoginRouteAcceptsRPCWireShape(t *testing.T) {
	fake := newImportFakeCaller()
	fake.httpResults = passwordLoginSuccessScript(t)
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword,
		passwordLoginRequestBody(t, testPasswordEmail, testPasswordSecret, "ignored-in-the-envelope"))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d, want 200: %s", out.StatusCode, out.Body)
	}
	var poll pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(out.Body, &poll); err != nil || poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("RPC wire shape did not run the core to success: %s", out.Body)
	}
	if len(fake.store) != 1 {
		t.Fatalf("store holds %d files, want 1: %v", len(fake.store), fake.savedNames())
	}
}

// TestLoginRouteValidation pins the validation reuse: missing/blank email or
// password and a malformed body answer the core's OWN typed invalid_request
// 400 (the exact core messages prove the shared validation runs, not a
// fork) before any network call or store touch.
func TestLoginRouteValidation(t *testing.T) {
	cases := []struct {
		name        string
		body        []byte
		wantMessage string
	}{
		{"malformed json", []byte(`{`), "malformed auth.login.password request"},
		{"missing email", []byte(`{"password":"p"}`), "email and password are required"},
		{"blank email", []byte(`{"email":"   ","password":"p"}`), "email and password are required"},
		{"missing password", []byte(`{"email":"recovery@example.com"}`), "email and password are required"},
		{"empty object", []byte(`{}`), "email and password are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newImportFakeCaller()
			m := newAuthTestManager(t, fake)
			out := handleManagement(t, m, http.MethodPost, loginRoutePassword, tc.body)
			if out.StatusCode != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
			}
			var errBody managementErrorBody
			if err := json.Unmarshal(out.Body, &errBody); err != nil ||
				errBody.Error.Code != "invalid_request" || errBody.Error.Message != tc.wantMessage {
				t.Fatalf("body is not the core's typed invalid_request: %s", out.Body)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("invalid request touched the host seam: %v", methodsOf(fake.calls))
			}
		})
	}
}

// assertLoginRouteHygiene scans every ABI crossing the fake caller recorded
// and asserts the password canary traveled ONLY in the required outbound
// verify body — never in the response envelope, any other outbound call
// (URL + headers + body), any non-http crossing (including host.auth.save),
// or any saved credential. Mirrors the F4 core hygiene assertion
// (TestPasswordLoginSecretHygiene) and extends it to the route's surfaces.
func assertLoginRouteHygiene(t *testing.T, fake *importFakeCaller, responseBody []byte, canaryPassword string) {
	t.Helper()
	verifySeen := false
	httpDo := 0
	for _, record := range fake.calls {
		switch record.method {
		case pluginabi.MethodHostHTTPDo:
			httpDo++
			var req hostHTTPRequest
			if err := json.Unmarshal(record.request, &req); err != nil {
				t.Fatalf("decode host.http.do request: %v", err)
			}
			if req.URL == passwordVerifyURL {
				verifySeen = true
				if !strings.Contains(string(req.Body), canaryPassword) {
					t.Fatalf("the required outbound verify body never carried the password")
				}
				continue // the ONLY place the password is allowed to travel
			}
			onWire := req.URL + "\x00" + string(req.Body)
			for name, values := range req.Headers {
				onWire += "\x00" + name + ":" + strings.Join(values, ",")
			}
			if strings.Contains(onWire, canaryPassword) {
				t.Fatalf("the password crossed the ABI outside the verify body: %s", onWire)
			}
		default:
			if strings.Contains(string(record.request), canaryPassword) {
				t.Fatalf("the password crossed the ABI in %s: %s", record.method, record.request)
			}
		}
	}
	if !verifySeen {
		t.Fatalf("the flow never issued the verify request: %v", methodsOf(fake.calls))
	}
	if httpDo != 3 {
		t.Fatalf("failure path made %d transport calls, want 3 (authorize, sentinel, verify)", httpDo)
	}
	if strings.Contains(string(responseBody), canaryPassword) {
		t.Fatalf("the response envelope leaked the password: %s", responseBody)
	}
	for _, raw := range fake.store {
		if strings.Contains(string(raw), canaryPassword) {
			t.Fatalf("the saved credential carries the password: %s", raw)
		}
	}
}

// TestLoginRouteFailureHygiene is the canary test: plant a canary password
// in the route body and run a scripted invalid-credentials FAILURE path —
// whose upstream error body carries a second canary echo — through the
// route. The answer must be the FIXED typed message (branch-only
// classification), the canary password must appear only in the required
// outbound verify body, and the upstream echo must never round-trip.
func TestLoginRouteFailureHygiene(t *testing.T) {
	const canaryPassword = "CANARY-LOGIN-ROUTE-PASSWORD-7c9d2e"
	const upstreamEcho = "CANARY-ECHO-FROM-UPSTREAM-1a2b"

	script := passwordLoginAuthorizeScript(t)
	script = append(script, hostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":{"code":"a200","message":"Invalid credentials: ` + upstreamEcho + ` is wrong"}}`)})
	fake := newImportFakeCaller()
	fake.httpResults = script
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword,
		[]byte(`{"email":"recovery@example.com","password":"`+canaryPassword+`"}`))
	if out.StatusCode != http.StatusUnauthorized {
		t.Fatalf("answered %d, want 401: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil ||
		errBody.Error.Code != errInvalidPassword ||
		errBody.Error.Message != "the password was rejected by the provider" {
		t.Fatalf("body is not the FIXED typed invalid_password message: %s", out.Body)
	}
	if strings.Contains(string(out.Body), upstreamEcho) {
		t.Fatalf("the refusal echoed the upstream body: %s", out.Body)
	}
	if len(fake.store) != 0 {
		t.Fatalf("a refused recovery must save nothing: %v", fake.savedNames())
	}
	assertLoginRouteHygiene(t, fake, out.Body, canaryPassword)
}

// TestLoginRouteSuccessHygiene extends the canary to the success path, where
// the route's own host.auth.save crossing is the extra surface: the password
// may appear only in the verify body — not in the response, not in the save
// request, not in the stored credential, not in any other outbound call.
func TestLoginRouteSuccessHygiene(t *testing.T) {
	const canaryPassword = "CANARY-LOGIN-ROUTE-SUCCESS-5f3a9b"

	fake := newImportFakeCaller()
	fake.httpResults = passwordLoginSuccessScript(t)
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword,
		[]byte(`{"email":"recovery@example.com","password":"`+canaryPassword+`"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d, want 200: %s", out.StatusCode, out.Body)
	}
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostHTTPDo {
			var req hostHTTPRequest
			if err := json.Unmarshal(record.request, &req); err != nil {
				t.Fatalf("decode host.http.do request: %v", err)
			}
			if req.URL == passwordVerifyURL {
				if !strings.Contains(string(req.Body), canaryPassword) {
					t.Fatalf("the required outbound verify body never carried the password")
				}
				continue
			}
			onWire := req.URL + "\x00" + string(req.Body)
			for name, values := range req.Headers {
				onWire += "\x00" + name + ":" + strings.Join(values, ",")
			}
			if strings.Contains(onWire, canaryPassword) {
				t.Fatalf("the password crossed the ABI outside the verify body: %s", onWire)
			}
		} else if strings.Contains(string(record.request), canaryPassword) {
			t.Fatalf("the password crossed the ABI in %s: %s", record.method, record.request)
		}
	}
	if strings.Contains(string(out.Body), canaryPassword) {
		t.Fatalf("the response envelope leaked the password: %s", out.Body)
	}
	for _, raw := range fake.store {
		if strings.Contains(string(raw), canaryPassword) {
			t.Fatalf("the saved credential carries the password: %s", raw)
		}
	}
	if len(fake.store) != 1 {
		t.Fatalf("store holds %d files, want the one recovered credential: %v", len(fake.store), fake.savedNames())
	}
}

// TestLoginRouteStoreRefusal pins the persistence refusal: a host.auth.save
// refusal after a successful recovery answers a typed retryable
// login_failed 500 — the recovery itself succeeded, only persistence was
// refused, so the operator can retry the same route — and nothing half-saved
// is reported as success.
func TestLoginRouteStoreRefusal(t *testing.T) {
	fake := newImportFakeCaller()
	fake.httpResults = passwordLoginSuccessScript(t)
	fake.saveError = "disk-full"
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, loginRoutePassword,
		[]byte(`{"email":"`+testPasswordEmail+`","password":"`+testPasswordSecret+`"}`))
	if out.StatusCode != http.StatusInternalServerError {
		t.Fatalf("answered %d, want 500: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "login_failed" {
		t.Fatalf("body is not the typed store refusal: %s", out.Body)
	}
	if !errBody.Error.Retryable {
		t.Fatalf("a store refusal is retryable (recovery succeeded): %s", out.Body)
	}
	if len(fake.store) != 0 {
		t.Fatalf("a refused save must persist nothing: %v", fake.savedNames())
	}
}
