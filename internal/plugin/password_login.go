package plugin

// Password re-login (account recovery): the auth method that recovers a
// ChatGPT account whose refresh token died, using the account's email +
// password through the auth-flow sentinel + PKCE flow. Ported from the
// reference _login_with_password (account_service.py:585-834) over the same
// host transport seam the browser login uses (doHostHTTP).
//
// RPC SURFACE DECISION: this is a SINGLE synchronous method
// (auth.login.password), not a start/poll pair. The browser flow needs the
// attempt store because the AUTHORIZE step is answered by the user's browser
// minutes later via the host's callback file — there is an async gap to
// bridge. The password flow has no external party: authorize bootstrap,
// sentinel PoW, verify, exchange and account verification all complete inside
// one RPC round, so a start/poll pair would be fake asynchrony (start would
// do all the work, poll would only read it back). There is therefore no
// in-memory attempt record either — nothing outlives the call, so there is
// nothing to store, GC or replay-protect. Concurrency needs no guard: each
// call owns a fresh device id, PKCE pair and sid, and the Manager holds no
// mutable state the call writes.
//
// SECRET HYGIENE: the password crosses the ABI only as this RPC's input and
// appears only in the verify request body. Every other outbound request,
// every error message and every envelope carries fixed text — verify
// refusals never echo the upstream body (an error body could echo
// credential material), and storage goes through the same record shape the
// browser login produces (source_type "password").

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// methodAuthLoginPassword is the RPC the host/panel invokes for password
// recovery. It is plugin-defined (the SDK carries no wire type for it yet)
// and dispatched by name like every other method.
const methodAuthLoginPassword = "auth.login.password"

const (
	// passwordVerifyURL is the email+password verify endpoint
	// (account_service.py:704).
	passwordVerifyURL = "https://auth.openai.com/api/accounts/password/verify"
	// passwordPlatformBase is the platform origin used in the exchange
	// origin/referer headers (account_service.py:749).
	passwordPlatformBase = "https://platform.openai.com"
	// passwordAuth0Client is the auth0-spa-js challenge JSON the reference
	// pins (account_service.py:592) — public client metadata, not a secret.
	// It rides as the authorize query param auth0Client and as the token
	// exchange header auth0-client.
	passwordAuth0Client = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	// passwordPKCEBytes is the reference's verifier entropy: 64 random bytes
	// → 86 base64url chars (utils/pkce.py:15). Wider than the browser login's
	// 48-byte verifier, kept distinct so Phase 1 stays byte-stable.
	passwordPKCEBytes = 64
	// passwordSourceLogin is the recovered credential's source_type
	// (account_service.py:828 pins "password").
	passwordSourceLogin = "password"
	// passwordLoginMessage names the recovered-account success reply.
	passwordLoginMessage = "ChatGPT account recovered via password login"
)

// Typed error codes the password flow answers (README error-matrix pinned).
const (
	errInvalidPassword      = "invalid_password"
	errUnsupportedRegion    = "unsupported_region"
	errLoginStateInvalid    = "login_state_invalid"
	errPasswordVerifyFailed = "password_verify_failed"
	errNeedVerificationCode = "need_verification_code"
	errNoAuthCode           = "no_auth_code"
	errSentinelPowFailed    = "sentinel_pow_failed"
	errLoginRateLimited     = "rate_limit_exceeded"
)

// passwordLoginRequest is the auth.login.password wire shape: PascalCase
// fields (the SDK's request marshaling convention) plus the snake_case
// host_callback_id the host uses to scope transport to this RPC — the same
// pairing loginStartRequest uses.
type passwordLoginRequest struct {
	Provider       string `json:"Provider"`
	Email          string `json:"Email"`
	Password       string `json:"Password"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// passwordTokenExchangeBody mirrors the reference exchange body field order
// (account_service.py:770-776).
type passwordTokenExchangeBody struct {
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier"`
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
}

// handlePasswordLogin implements auth.login.password end to end. Provider
// refusals travel as typed error envelopes; the account-verification outcome
// travels IN the response (the poll response shape) exactly like the browser
// login does, so the host renders both uniformly.
func (m *Manager) handlePasswordLogin(request []byte) ([]byte, error) {
	var req passwordLoginRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed auth.login.password request", 400)), nil
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || req.Password == "" {
		return errEnvelope(typedErr("invalid_request", "email and password are required", 400)), nil
	}
	callbackID := req.HostCallbackID

	deviceID, err := uuid4String()
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}
	verifier, challenge, err := pkcePairBytes(passwordPKCEBytes)
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}
	state, err := tokenURLSafe(32)
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}
	nonce, err := tokenURLSafe(32)
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}

	// ① authorize bootstrap (account_service.py:607-673): session setup keyed
	// by the oai-did cookie; the email travels as login_hint. The redirect is
	// followed by the host transport; only the status and a Location to an
	// error page matter here.
	authorizeURL := passwordAuthorizeURL(email, deviceID, state, nonce, challenge)
	// Defense in depth: the URL is built from constants + validated input,
	// but it is refused unless it lands on the pinned origin (login.go).
	if err := validateAuthorizeURL(authorizeURL); err != nil {
		return errEnvelope(typedErr("login_failed", err.Error(), 502)), nil
	}
	resp, err := m.doHostHTTP(callbackID, http.MethodGet, authorizeURL, map[string][]string{
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"Accept-Language":           {"zh-CN,zh;q=0.9,en;q=0.8"},
		"User-Agent":                {sentinelUserAgent},
		"sec-ch-ua":                 {sentinelSecCHUA},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {sentinelSecCHUAPlatform},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-site":            {"cross-site"},
		"sec-fetch-user":            {"?1"},
		"Upgrade-Insecure-Requests": {"1"},
		"Referer":                   {passwordPlatformBase + "/"},
		"Cookie":                    {"oai-did=" + deviceID},
	}, nil)
	if err != nil {
		return passwordLoginTransportError(), nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		e := typedErr("login_failed", "the provider refused the authorize bootstrap", 502)
		e.Retryable = true
		return errEnvelope(e), nil
	}
	if resp.StatusCode == http.StatusFound {
		if location := firstHeader(resp.Headers, "Location"); strings.Contains(location, "/error") && strings.Contains(location, "payload=") {
			return errEnvelope(passwordAuthorizeError(location)), nil
		}
	}

	// ② auth-flow sentinel (account_service.py:693-701): best-effort except
	// the PoW cap breach, which is typed (F4 decision — no silent marker).
	sid, err := uuid4String()
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}
	acq := m.acquireAuthSentinel(callbackID, deviceID, m.sentinelFor(deviceID, sid))
	if acq.err != nil {
		e := typedErr(errSentinelPowFailed, "the provider's proof-of-work budget was exhausted; retry later", 502)
		e.Retryable = true
		return errEnvelope(e), nil
	}

	// ③ password verify (account_service.py:676-746): JSON body carrying ONLY
	// the password — the email already traveled as the authorize login_hint.
	cookie := "oai-did=" + deviceID
	if acq.oaiSC != "" {
		cookie += "; oai-sc=" + acq.oaiSC
	}
	verifyHeaders := map[string][]string{
		"Accept":             {"application/json"},
		"Accept-Language":    {"zh-CN,zh;q=0.9"},
		"Content-Type":       {"application/json"},
		"Origin":             {oauthAuthorizeBase},
		"Priority":           {"u=1, i"},
		"User-Agent":         {sentinelUserAgent},
		"sec-ch-ua":          {sentinelSecCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sentinelSecCHUAPlatform},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"same-origin"},
		"Referer":            {oauthAuthorizeBase + "/email-verification"},
		"oai-device-id":      {deviceID},
		"Cookie":             {cookie},
	}
	if acq.token != "" {
		verifyHeaders["openai-sentinel-token"] = []string{acq.token}
	}
	verifyBody, _ := json.Marshal(struct {
		Password string `json:"password"`
	}{Password: req.Password})
	resp, err = m.doHostHTTP(callbackID, http.MethodPost, passwordVerifyURL, verifyHeaders, verifyBody)
	if err != nil {
		return passwordLoginTransportError(), nil
	}
	if resp.StatusCode != http.StatusOK {
		return errEnvelope(classifyPasswordVerifyError(resp.Body)), nil
	}
	var verifyData struct {
		ContinueURL string `json:"continue_url"`
		Page        struct {
			Type string `json:"type"`
		} `json:"page"`
	}
	_ = json.Unmarshal(resp.Body, &verifyData)
	code := extractContinueURLCode(verifyData.ContinueURL)
	if code == "" {
		// The password was accepted but no authorization code issued: an
		// email-OTP page is unusable headless (typed refusal); anything else
		// is retryable. NO token exchange is attempted without a code.
		if verifyData.Page.Type == "email_otp_verification" {
			return errEnvelope(typedErr(errNeedVerificationCode,
				"the account requires an email verification code; headless password login is not possible", 403)), nil
		}
		e := typedErr(errNoAuthCode, "the provider accepted the password but issued no authorization code; retry later", 502)
		e.Retryable = true
		return errEnvelope(e), nil
	}

	// ④ code → token triple (account_service.py:748-793): the same grant the
	// browser login exchanges, with the flow's own PKCE verifier.
	exchangeBody, _ := json.Marshal(passwordTokenExchangeBody{
		ClientID:     oauthClientID,
		CodeVerifier: verifier,
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  oauthLoginRedirectURI,
	})
	resp, err = m.doHostHTTP(callbackID, http.MethodPost, oauthLoginTokenURL, map[string][]string{
		"Accept":             {"*/*"},
		"Accept-Language":    {"zh-CN,zh;q=0.9"},
		"auth0-client":       {passwordAuth0Client},
		"Cache-Control":      {"no-cache"},
		"Content-Type":       {"application/json"},
		"Origin":             {passwordPlatformBase},
		"Pragma":             {"no-cache"},
		"Priority":           {"u=1, i"},
		"Referer":            {passwordPlatformBase + "/"},
		"sec-ch-ua":          {sentinelSecCHUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {sentinelSecCHUAPlatform},
		"sec-fetch-dest":     {"empty"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-site":     {"same-site"},
		"User-Agent":         {sentinelUserAgent},
		"Cookie":             {cookie},
	}, exchangeBody)
	if err != nil {
		return passwordLoginTransportError(), nil
	}
	if resp.StatusCode != http.StatusOK {
		e := typedErr("login_failed", "oauth token exchange failed: "+oauthErrorDetail(resp.Body), 502)
		e.Retryable = true
		return errEnvelope(e), nil
	}
	tokens, err := parseLoginTokens(resp.Body)
	if err != nil {
		e := typedErr("login_failed", err.Error(), 502)
		e.Retryable = true
		return errEnvelope(e), nil
	}

	// ⑤ store exactly like the browser login (login.go verifyLoginAccount):
	// /backend-api/me verification, JWT normalization, best-effort plan
	// probe, stable hashed file name. The executor refresh-retry and quota
	// then work on the record unchanged.
	return ok(m.verifyLoginAccount(callbackID, tokens, passwordSourceLogin, passwordLoginMessage)), nil
}

// passwordAuthorizeURL builds the authorize bootstrap URL with exactly the
// sixteen reference params in reference order (account_service.py:616-633).
// Query params are unordered per RFC 3986; the order is kept for readability
// and the tests pin every param value individually.
func passwordAuthorizeURL(email, deviceID, state, nonce, challenge string) string {
	pairs := [][2]string{
		{"issuer", oauthAuthorizeBase},
		{"client_id", oauthClientID},
		{"audience", oauthLoginAudience},
		{"redirect_uri", oauthLoginRedirectURI},
		{"device_id", deviceID},
		{"screen_hint", "login_or_signup"},
		{"max_age", "0"},
		{"login_hint", email},
		{"scope", oauthLoginScope},
		{"response_type", "code"},
		{"response_mode", "query"},
		{"state", state},
		{"nonce", nonce},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"auth0Client", passwordAuth0Client},
	}
	var b strings.Builder
	b.WriteString(oauthAuthorizeBase + oauthAuthorizePath + "?")
	for i, pair := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(pair[0])
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(pair[1]))
	}
	return b.String()
}

// extractContinueURLCode pulls the code query param out of a continue_url
// (account_service.py:728-733). Any parse problem answers empty — the caller
// then classifies the missing code.
func extractContinueURLCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("code"))
}

// classifyPasswordVerifyError maps a refused verify response to its typed
// error (account_service.py:716-725). The messages are FIXED and carry no
// upstream body text — the provider's error body is upstream-controlled and
// could echo credential material, so only the provider's short machine
// code names the branch, never its message.
func classifyPasswordVerifyError(body []byte) *pluginabi.Error {
	var data struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &data)
	switch {
	case data.Error.Code == "unsupported_country_region_territory":
		return typedErr(errUnsupportedRegion, "the account's region is not supported for password login", 403)
	case data.Error.Code == "invalid_state":
		e := typedErr(errLoginStateInvalid, "the sign-in state expired; retry the password login", 400)
		e.Retryable = true
		return e
	case strings.Contains(data.Error.Message, "Invalid credentials") ||
		strings.Contains(strings.ToLower(data.Error.Message), "wrong password"):
		return typedErr(errInvalidPassword, "the password was rejected by the provider", 401)
	default:
		e := typedErr(errPasswordVerifyFailed, "password verification failed; retry later", 502)
		e.Retryable = true
		return e
	}
}

// passwordAuthorizeError decodes the authorize error-page payload
// (account_service.py:658-673): rate_limit_exceeded is its own typed class;
// any other errorCode rides login_failed as a short machine token — the
// payload body is never echoed (it is upstream-controlled).
func passwordAuthorizeError(location string) *pluginabi.Error {
	u, err := url.Parse(location)
	if err != nil {
		e := typedErr("login_failed", "the authorize error redirect could not be parsed", 502)
		e.Retryable = true
		return e
	}
	payloadB64 := u.Query().Get("payload")
	payloadB64 += strings.Repeat("=", (4-len(payloadB64)%4)%4)
	raw, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		e := typedErr("login_failed", "the authorize error payload could not be decoded", 502)
		e.Retryable = true
		return e
	}
	var payload struct {
		ErrorCode string `json:"errorCode"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.ErrorCode == "rate_limit_exceeded" {
		e := typedErr(errLoginRateLimited, "the provider rate-limited the sign-in; retry later", 429)
		e.Retryable = true
		return e
	}
	e := typedErr("login_failed", "the provider refused the authorize bootstrap ("+payload.ErrorCode+")", 502)
	e.Retryable = true
	return e
}

// passwordLoginTransportError is the typed answer for a host-seam failure
// mid-flow. The password is already with the host (it crossed the ABI as the
// RPC input), so nothing extra is exposed by naming the seam.
func passwordLoginTransportError() []byte {
	e := typedErr("login_failed", "host transport unavailable during password login", 502)
	e.Retryable = true
	return errEnvelope(e)
}

// firstHeader reads one header value from the host response headers,
// canonicalizing the key the way Go's http layer does.
func firstHeader(headers map[string][]string, name string) string {
	values := headers[http.CanonicalHeaderKey(name)]
	if len(values) > 0 {
		return values[0]
	}
	return ""
}
