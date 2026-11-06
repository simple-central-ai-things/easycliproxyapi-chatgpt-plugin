package plugin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Browser OAuth login wire contract, pinned to the reference values.
const (
	// oauthAuthorizeBase is the exact origin of the authorize endpoint; the
	// URL we build is refused unless it sits on it (validateAuthorizeURL).
	oauthAuthorizeBase = "https://auth.openai.com"
	// oauthAuthorizePath is the consumer authorize endpoint.
	oauthAuthorizePath = "/api/accounts/authorize"
	// oauthLoginTokenURL exchanges the one-time auth code for the token triple.
	oauthLoginTokenURL = "https://auth.openai.com/api/accounts/oauth/token"
	// chatgptMeURL verifies the fresh access token and yields the account
	// identity facts (email + account id).
	chatgptMeURL = "https://chatgpt.com/backend-api/me"

	// oauthLoginAudience and oauthLoginScope are the pinned authorize params.
	oauthLoginAudience = "https://api.openai.com/v1"
	oauthLoginScope    = "openid profile email offline_access"

	// pkceVerifierBytes is the PKCE verifier entropy (RFC 7636 S256).
	pkceVerifierBytes = 48
	// loginStateBytes is the state entropy; 32 random alphanumeric chars sit
	// inside the host's ValidateOAuthState charset and make collisions
	// impossible.
	loginStateBytes = 32

	// loginAttemptTTL bounds one browser sign-in round; the reference gives
	// the user the same window.
	loginAttemptTTL = 10 * time.Minute
	// maxLoginAttempts caps the in-memory attempt store (reference _MAX_SESSIONS).
	maxLoginAttempts = 64

	// loginMessageCap bounds provider text passed through into poll replies.
	loginMessageCap = 300
)

// loginAttempt is one in-flight browser sign-in: the PKCE verifier and the
// redirect_uri the authorize URL carried, plus the lifecycle flags. It never
// crosses the ABI — only its State (the map key) does.
type loginAttempt struct {
	verifier    string
	redirectURI string
	expiresAt   time.Time
	busy        bool
}

// errLoginTransport marks a host-transport seam failure during the exchange;
// the auth code is then NOT consumed, so the attempt stays retryable and the
// poll answers pending instead of error.
var errLoginTransport = errors.New("host transport unavailable")

// loginStartRequest mirrors the host wire shape of auth.login.start: the SDK
// request (marshaled PascalCase) plus the callback id the host uses to scope
// transport to this RPC.
type loginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// loginPollRequest mirrors the host wire shape of auth.login.poll.
type loginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// oauthCallbackPayload mirrors the host's WriteOAuthCallbackFile content:
// {"code","state","error"} published at
// <authDir>/.oauth-<provider>-<state>.oauth when the browser redirects back.
type oauthCallbackPayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

// validateAuthorizeURL refuses any authorize URL whose origin is not exactly
// https://auth.openai.com (mistral validateProviderURL pattern applied to our
// own redirect construction): https scheme, the pinned host, no explicit
// port, no userinfo, no fragment.
func validateAuthorizeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "auth.openai.com" ||
		u.Port() != "" || u.User != nil || u.Fragment != "" {
		return errors.New("unexpected authorize URL origin")
	}
	return nil
}

// validateRedirectBaseURL refuses anything that is not the host's own
// loopback callback URL (http(s)://127.0.0.1|localhost|::1:<port>/...). The
// host computes it from its own port and runs the management API without TLS,
// so loopback http is the valid production shape; non-loopback hosts never
// become a redirect_uri.
func validateRedirectBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.Fragment != "" {
		return errors.New("invalid redirect base URL")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return errors.New("redirect base URL must target the host's loopback callback")
	}
	return nil
}

// pkcePair returns a 48-random-byte verifier (base64url) and its S256
// challenge: BASE64URL(SHA256(ASCII(verifier))).
func pkcePair() (string, string, error) {
	buf := make([]byte, pkceVerifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// randomLoginState returns 32 random alphanumeric chars — inside the host's
// ValidateOAuthState charset, so the state the host registers and the file
// name it later derives never trip its own guard.
func randomLoginState() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, loginStateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// gcExpiredLoginAttempts drops expired attempts inline; callers hold
// m.loginMu.
func gcExpiredLoginAttempts(logins map[string]loginAttempt) {
	now := time.Now()
	for state, attempt := range logins {
		if now.After(attempt.expiresAt) {
			delete(logins, state)
		}
	}
}

// handleLoginStart implements auth.login.start: it validates the host
// callback URL, builds the pinned authorize URL with a fresh PKCE pair and
// state, records the attempt (capped, GC'd) and returns it. No transport
// call is made — the browser, not the host, completes the flow.
func (m *Manager) handleLoginStart(request []byte) ([]byte, error) {
	var req loginStartRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed auth.login.start request", 400)), nil
	}
	if err := validateRedirectBaseURL(req.BaseURL); err != nil {
		return errEnvelope(typedErr("invalid_request", err.Error(), 400)), nil
	}

	verifier, challenge, err := pkcePair()
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}
	state, err := randomLoginState()
	if err != nil {
		return errEnvelope(typedErr("login_failed", "secure random generator unavailable", 500)), nil
	}

	query := url.Values{}
	query.Set("client_id", oauthClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", req.BaseURL)
	query.Set("scope", oauthLoginScope)
	query.Set("audience", oauthLoginAudience)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	authorizeURL := oauthAuthorizeBase + oauthAuthorizePath + "?" + query.Encode()
	// Defense in depth: the URL is built from constants, but it is refused
	// unless it lands on the pinned origin — a constant or build drift can
	// never send the user's browser anywhere else.
	if err := validateAuthorizeURL(authorizeURL); err != nil {
		return errEnvelope(typedErr("login_failed", err.Error(), 502)), nil
	}

	expiresAt := time.Now().Add(loginAttemptTTL)
	m.loginMu.Lock()
	gcExpiredLoginAttempts(m.logins)
	if len(m.logins) >= maxLoginAttempts {
		m.loginMu.Unlock()
		return errEnvelope(typedErr("too_many_logins", "too many concurrent sign-in attempts", 429)), nil
	}
	m.logins[state] = loginAttempt{verifier: verifier, redirectURI: req.BaseURL, expiresAt: expiresAt}
	m.loginMu.Unlock()

	return ok(pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       authorizeURL,
		State:     state,
		ExpiresAt: expiresAt,
	}), nil
}

// handleLoginPoll implements auth.login.poll: one read of the host callback
// file for the state, then the exchange + verify when the code arrived.
// Login outcomes travel IN the response (status pending/success/error); only
// a malformed request answers an error envelope.
func (m *Manager) handleLoginPoll(request []byte) ([]byte, error) {
	var req loginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed auth.login.poll request", 400)), nil
	}
	if strings.TrimSpace(req.State) == "" {
		return errEnvelope(typedErr("invalid_request", "missing login state", 400)), nil
	}
	out, err := m.loginPoll(req.State, req.Host.AuthDir, req.HostCallbackID)
	if err != nil {
		return errEnvelope(typedErr("login_failed", err.Error())), nil
	}
	return ok(out), nil
}

func pendingLoginResponse(message string) pluginapi.AuthLoginPollResponse {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: message}
}

// loginPoll runs one poll round for state. The attempt store is replay
// protection: a consumed attempt is deleted on success, so a second poll for
// the same state answers pending and the one-time code is never exchanged
// twice.
func (m *Manager) loginPoll(state, authDir, callbackID string) (pluginapi.AuthLoginPollResponse, error) {
	m.loginMu.Lock()
	gcExpiredLoginAttempts(m.logins)
	attempt, found := m.logins[state]
	if !found {
		m.loginMu.Unlock()
		// Attempt gone — consumed by a successful exchange, expired, or never
		// ours. Waiting again is the only answer that cannot re-exchange a
		// consumed code.
		return pendingLoginResponse("waiting for the browser sign-in to complete"), nil
	}
	if attempt.busy {
		m.loginMu.Unlock()
		return pendingLoginResponse("sign-in request in progress"), nil
	}
	attempt.busy = true
	m.logins[state] = attempt
	m.loginMu.Unlock()
	defer func() {
		m.loginMu.Lock()
		if current, ok := m.logins[state]; ok {
			current.busy = false
			m.logins[state] = current
		}
		m.loginMu.Unlock()
	}()

	payload, err := readOAuthCallbackFile(authDir, state)
	if err != nil {
		// No callback file yet (or an unreadable one): the user has not
		// finished the browser step.
		return pendingLoginResponse("waiting for the browser sign-in to complete"), nil
	}
	if providerErr := strings.TrimSpace(payload.Error); providerErr != "" {
		// The provider refused the sign-in. The attempt is kept so a later
		// poll still reports the refusal (retryable from the panel).
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: capLoginMessage(providerErr),
		}, nil
	}
	code := strings.TrimSpace(payload.Code)
	if code == "" {
		return pendingLoginResponse("waiting for the browser sign-in to complete"), nil
	}
	if payload.State != "" && payload.State != state {
		// Not this flow's callback — someone else's state in the file.
		return pendingLoginResponse("waiting for the browser sign-in to complete"), nil
	}

	tokens, err := m.exchangeLoginCode(callbackID, code, attempt)
	if err != nil {
		if errors.Is(err, errLoginTransport) {
			// The seam is down and the code was never sent: still valid, so
			// keep waiting instead of failing the login.
			return pendingLoginResponse("sign-in status temporarily unavailable"), nil
		}
		// The provider rejected the exchange. The attempt is KEPT (mistral
		// pattern): a later poll re-reports the same refusal, and only a
		// successful exchange consumes the attempt — the replay-protection
		// invariant is "never re-exchange a successfully consumed code".
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: capLoginMessage(err.Error()),
		}, nil
	}
	// The code is consumed the moment the exchange succeeds: the attempt is
	// deleted BEFORE verify, so no later poll can exchange it again even if
	// verify fails.
	m.deleteLoginAttempt(state)
	return m.verifyLoginAccount(callbackID, tokens), nil
}

// readOAuthCallbackFile reads the host's callback file for state. Any error
// (missing file, empty auth dir, unparseable payload) means "not yet": the
// caller answers pending.
func readOAuthCallbackFile(authDir, state string) (oauthCallbackPayload, error) {
	if strings.TrimSpace(authDir) == "" {
		return oauthCallbackPayload{}, os.ErrNotExist
	}
	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", providerKey, state)
	data, err := os.ReadFile(filepath.Join(authDir, fileName))
	if err != nil {
		return oauthCallbackPayload{}, err
	}
	var payload oauthCallbackPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return oauthCallbackPayload{}, err
	}
	return payload, nil
}

// deleteLoginAttempt removes the attempt for state (replay protection).
func (m *Manager) deleteLoginAttempt(state string) {
	m.loginMu.Lock()
	delete(m.logins, state)
	m.loginMu.Unlock()
}

// loginTokens is the exchanged triple before identity verification.
type loginTokens struct {
	accessToken  string
	refreshToken string
	idToken      string
}

// exchangeLoginCode trades the one-time auth code for the token triple
// through the host transport seam (host.http.do). The body is JSON with
// exactly the five grant fields, byte-matched to the reference; errors carry
// only the provider's own detail text, never our code or verifier.
func (m *Manager) exchangeLoginCode(callbackID, code string, attempt loginAttempt) (loginTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     oauthClientID,
		"code_verifier": attempt.verifier,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  attempt.redirectURI,
	})
	resp, err := m.doHostHTTP(callbackID, http.MethodPost, oauthLoginTokenURL, map[string][]string{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}, body)
	if err != nil {
		return loginTokens{}, fmt.Errorf("%w: %v", errLoginTransport, err)
	}
	if resp.StatusCode != http.StatusOK {
		return loginTokens{}, oauthTokenError(resp.StatusCode, resp.Body)
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil || strings.TrimSpace(tokenResp.AccessToken) == "" {
		return loginTokens{}, errors.New("oauth login exchange returned no access token")
	}
	return loginTokens{
		accessToken:  strings.TrimSpace(tokenResp.AccessToken),
		refreshToken: strings.TrimSpace(tokenResp.RefreshToken),
		idToken:      strings.TrimSpace(tokenResp.IDToken),
	}, nil
}

// verifyLoginAccount verifies the fresh access token against /backend-api/me
// (a 401 there is error, never success) and turns the triple plus the account
// identity into the credential record. Errors never carry token material.
func (m *Manager) verifyLoginAccount(callbackID string, tokens loginTokens) pluginapi.AuthLoginPollResponse {
	resp, err := m.doHostHTTP(callbackID, http.MethodGet, chatgptMeURL, map[string][]string{
		"Accept":        {"application/json"},
		"Authorization": {"Bearer " + tokens.accessToken},
	}, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "could not verify the signed-in account; please try again",
		}
	}
	var me struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(resp.Body, &me)
	cred := Credential{
		AccessToken:  tokens.accessToken,
		RefreshToken: tokens.refreshToken,
		IDToken:      tokens.idToken,
		Email:        strings.TrimSpace(me.Email),
		AccountID:    strings.TrimSpace(me.ID),
		SourceType:   "oauth_login",
	}
	// JWT claims backfill any identity fact /me left out (Task 3 helper).
	cred = normalizeCredential(cred)
	// Persist the plan into the record the moment it is known: the codex
	// image route publishes per the credential's plan (model.for_auth), and
	// this record is its only source. Best-effort — a probe failure keeps
	// Plan as-is and still returns success; the record picks up the plan at
	// the first refresh.
	if plan, err := m.probePlan(cred); err == nil {
		cred.Plan = plan
	}
	fileName := FileNameFor(cred)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "ChatGPT account signed in",
		Auth: pluginapi.AuthData{
			Provider:    providerKey,
			ID:          fileName,
			FileName:    fileName,
			StorageJSON: cred.StorageJSON(),
			Metadata:    map[string]any{"type": providerKey},
		},
	}
}

func capLoginMessage(message string) string {
	if len(message) > loginMessageCap {
		return message[:loginMessageCap]
	}
	return message
}
