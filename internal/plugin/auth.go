package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// Refresh surface wire contract, pinned to the reference values.
const (
	// oauthTokenURL is the refresh_token grant endpoint.
	oauthTokenURL = "https://auth.openai.com/oauth/token"
	// oauthClientID is the Platform consumer client id shared with the
	// reference so imported tokens stay refresh-compatible.
	oauthClientID = "app_2SKx67EdpoN0G6j64rFvigXD"

	// accessTokenRefreshSkew is how long after a successful refresh the host
	// is told not to refresh again (reference _ACCESS_TOKEN_REFRESH_SKEW).
	accessTokenRefreshSkew = 24 * time.Hour
	// refreshErrorBackoff is the retry delay recorded after a failed refresh
	// (reference _TOKEN_REFRESH_ERROR_BACKOFF).
	refreshErrorBackoff = 5 * time.Minute

	// errCodeCredentialTerminated marks accounts whose OAuth session the
	// provider terminated; only a fresh login recovers them.
	errCodeCredentialTerminated = "credential_terminated"

	// planProbeTimeout bounds the best-effort plan probe login and refresh
	// run after a credential lifecycle success (the oracle's get_user_info
	// legs use timeout=30).
	planProbeTimeout = 30 * time.Second
)

// Sentinels refreshToken returns so handleAuthRefresh can pick the reply
// shape. Their text never carries token material — only the provider's own
// detail message, which is safe to persist and display.
var (
	errCredentialTerminated = errors.New("provider terminated the account OAuth session")
	errNoRefreshToken       = errors.New("credential has no refresh token")
)

// hostHTTPRequest mirrors the host wire shape of host.http.do (snake_case
// keys; body base64-encoded by encoding/json). The host ignores fields we
// do not set (operation ids, wire profile).
type hostHTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// hostHTTPResponse mirrors the host http.do result payload: the host
// marshals pluginapi.HTTPResponse without json tags, so the keys are
// PascalCase and the body is base64.
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// refreshRequest mirrors the host wire shape of auth.refresh: the SDK
// request (marshaled PascalCase) plus the callback id the host uses to scope
// transport to this RPC.
type refreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse implements auth.parse: it claims only type=chatgpt-proxy
// records, normalizes partial ones from the access token's JWT claims, and
// answers Handled:false for everything else so built-in codex files stay the
// host's. The input file name is returned unchanged (stability contract).
func (m *Manager) handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed auth.parse request", 400)), nil
	}
	if strings.TrimSpace(req.Provider) != "" && req.Provider != providerKey {
		return ok(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	if !isOwnCredentialFile(req.RawJSON) {
		return ok(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	cred, err := FromStorageJSON(req.RawJSON)
	if err != nil {
		return errEnvelope(typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)), nil
	}

	var rawRecord map[string]any
	_ = json.Unmarshal(req.RawJSON, &rawRecord)
	auth := pluginapi.AuthData{
		Provider:    providerKey,
		ID:          req.FileName,
		FileName:    req.FileName,
		StorageJSON: normalizeCredential(cred).StorageJSON(),
		Metadata:    hostManagedMetadata(rawRecord),
	}
	if label, ok := rawRecord["label"].(string); ok {
		auth.Label = label
	}
	if disabled, ok := rawRecord["disabled"].(bool); ok {
		auth.Disabled = disabled
	}
	return ok(pluginapi.AuthParseResponse{Handled: true, Auth: auth}), nil
}

// normalizeCredential fills identity facts missing from the record from the
// access token's JWT claims. Claims are read from the base64url payload
// segment only — never signature-verified (same as the reference). Values
// already present in the record always win.
func normalizeCredential(cred Credential) Credential {
	if strings.TrimSpace(cred.Email) != "" && strings.TrimSpace(cred.AccountID) != "" {
		return cred
	}
	claims := decodeJWTPayload(cred.AccessToken)
	if claims == nil {
		return cred
	}
	if strings.TrimSpace(cred.Email) == "" {
		if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
			cred.Email = claimString(profile["email"])
		}
	}
	if strings.TrimSpace(cred.AccountID) == "" {
		if authClaim, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			cred.AccountID = claimString(authClaim["chatgpt_account_id"])
		}
	}
	return cred
}

// decodeJWTPayload reads the claims of a JWT from its middle base64url
// segment only — the signature is never verified (same as the reference).
// It returns nil for anything that is not a three-segment token with a JSON
// object payload.
func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func claimString(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

// handleAuthRefresh implements auth.refresh: it renews the record's token
// triple through the host transport and replies with the updated record and
// the next refresh deadline. Soft failures (any non-terminal error) reply
// OK with the error recorded and a refreshErrorBackoff deadline so the host
// persists the backoff; only session termination and a missing refresh
// token answer error envelopes.
func (m *Manager) handleAuthRefresh(request []byte) ([]byte, error) {
	var req refreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed auth.refresh request", 400)), nil
	}
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return errEnvelope(typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)), nil
	}

	updated, refreshErr := m.refreshToken(context.Background(), req.HostCallbackID, cred)
	if errors.Is(refreshErr, errCredentialTerminated) {
		return errEnvelope(typedErr(errCodeCredentialTerminated,
			"the provider terminated this account's OAuth session; log in again", 401)), nil
	}
	if errors.Is(refreshErr, errNoRefreshToken) {
		return errEnvelope(typedErr("invalid_credential",
			"credential has no refresh token; log in again", 400)), nil
	}

	auth := pluginapi.AuthData{
		Provider:    providerKey,
		ID:          req.AuthID,
		FileName:    refreshFileName(req),
		StorageJSON: updated.StorageJSON(),
		Metadata:    req.Metadata,
		Attributes:  req.Attributes,
	}
	if refreshErr != nil {
		return ok(pluginapi.AuthRefreshResponse{
			Auth:             auth,
			NextRefreshAfter: updated.LastRefreshAt.Add(refreshErrorBackoff),
		}), nil
	}
	return ok(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.Now().Add(accessTokenRefreshSkew),
	}), nil
}

// refreshFileName resolves the file name refresh must return unchanged: the
// pool identifier is the file name; Attributes carry the source path when
// the identifier is empty.
func refreshFileName(req refreshRequest) string {
	if req.AuthID != "" {
		return req.AuthID
	}
	if sourcePath := req.Attributes["path"]; sourcePath != "" {
		return path.Base(sourcePath)
	}
	return ""
}

// refreshToken exchanges the record's refresh token for a new token triple
// through the host transport seam (host.http.do) and returns the updated
// record. On failure the tokens are untouched and the error is recorded in
// LastRefreshError; the record is returned either way so the caller decides
// whether to persist it.
func (m *Manager) refreshToken(_ context.Context, callbackID string, cred Credential) (Credential, error) {
	now := time.Now().UTC()
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return recordRefreshFailure(cred, now, "credential has no refresh token"), errNoRefreshToken
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	form.Set("client_id", oauthClientID)

	resp, err := m.doHostHTTP(callbackID, http.MethodPost, oauthTokenURL, map[string][]string{
		"Content-Type": {"application/x-www-form-urlencoded"},
		"Accept":       {"application/json"},
	}, []byte(form.Encode()))
	if err != nil {
		return recordRefreshFailure(cred, now, "host transport unavailable: "+err.Error()), err
	}
	if resp.StatusCode != http.StatusOK {
		refreshErr := oauthRefreshError(resp.StatusCode, resp.Body)
		return recordRefreshFailure(cred, now, refreshErr.Error()), refreshErr
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil || strings.TrimSpace(tokenResp.AccessToken) == "" {
		refreshErr := fmt.Errorf("oauth refresh response has no access token (http %d)", resp.StatusCode)
		return recordRefreshFailure(cred, now, refreshErr.Error()), refreshErr
	}

	updated := cred
	updated.AccessToken = strings.TrimSpace(tokenResp.AccessToken)
	if newRT := strings.TrimSpace(tokenResp.RefreshToken); newRT != "" {
		updated.RefreshToken = newRT
	}
	if newID := strings.TrimSpace(tokenResp.IDToken); newID != "" {
		updated.IDToken = newID
	}
	updated.LastRefreshAt = now
	updated.LastRefreshError = ""
	updated = normalizeCredential(updated)
	// Persist the plan on a SUCCESSFUL refresh (the oracle's background
	// watcher refreshes plan_type on every cycle): the record is the source
	// model.for_auth's codex image gate reads. Best-effort — a probe failure
	// leaves Plan untouched and never fails the refresh.
	if plan, err := m.probePlan(updated); err == nil {
		updated.Plan = plan
	}
	return updated, nil
}

// probePlan fetches the account's plan_type through the pinned
// accounts/check leg (oracle get_user_info plan source) with the record's
// bearer. Login and refresh call it best-effort: ANY error — including a
// typed 401 invalid_token — is returned for the caller to discard, so a plan
// probe can never fail the credential lifecycle or surface as a
// credential_terminated-style typed error.
func (m *Manager) probePlan(cred Credential) (string, error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), planProbeTimeout)
	defer cancel()
	return upstream.ProbePlan(ctx, u, cred)
}

// oauthRefreshError turns a rejected refresh response into an error. The
// provider's detail text is safe to persist (it never carries our token
// material); the app_session_terminated marker is lifted to the typed
// errCredentialTerminated sentinel so the handler answers non-retryable.
func oauthRefreshError(status int, body []byte) error {
	detail := oauthErrorDetail(body)
	if strings.Contains(strings.ToLower(detail), "app_session_terminated") {
		return fmt.Errorf("%w: %s", errCredentialTerminated, detail)
	}
	return fmt.Errorf("oauth refresh failed with http %d: %s", status, detail)
}

// oauthTokenError turns a rejected login-exchange response into an error with
// the same safe-detail rule as oauthRefreshError.
func oauthTokenError(status int, body []byte) error {
	return fmt.Errorf("oauth login exchange failed with http %d: %s", status, oauthErrorDetail(body))
}

// oauthErrorDetail extracts the provider's own detail text from a rejected
// token response (error_description, then error, then message, then the raw
// body). Our token material never appears in the reply, so the detail is safe
// to surface in error messages — but it is upstream-controlled, so control
// runes are dropped and the result rune-capped before it is persisted.
func oauthErrorDetail(body []byte) string {
	var rejected struct {
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
		Message   string `json:"message"`
	}
	_ = json.Unmarshal(body, &rejected)
	detail := rejected.ErrorDesc
	if detail == "" {
		detail = rejected.Error
	}
	if detail == "" {
		detail = rejected.Message
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	return capText(dropControl(detail), 300)
}

// dropControl strips control and non-printable runes from upstream-controlled
// text (a provider error field) before it is persisted: the provider can
// answer with arbitrary bytes, and a persisted diagnostic must not carry
// control bytes.
func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// capText truncates s to at most limit runes. It is rune-safe: a byte slice
// at a rune boundary would leave a partial UTF-8 sequence that persists as
// U+FFFD mojibake in diagnostics and poll replies.
func capText(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func recordRefreshFailure(cred Credential, at time.Time, message string) Credential {
	cred.LastRefreshAt = at
	cred.LastRefreshError = message
	return cred
}

// doHostHTTP issues one upstream HTTP request through the host transport
// seam so host proxy policy applies. Errors name only the seam failure —
// never request or response payload material.
func (m *Manager) doHostHTTP(callbackID, method, requestURL string, headers map[string][]string, body []byte) (*hostHTTPResponse, error) {
	req, _ := json.Marshal(hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         method,
		URL:            requestURL,
		Headers:        headers,
		Body:           body,
	})
	raw, err := m.host.Call(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return nil, err
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("host http response is not a valid envelope")
	}
	if !env.OK {
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		return nil, fmt.Errorf("host http request failed (code %q)", code)
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, fmt.Errorf("host http response payload is invalid")
	}
	return &resp, nil
}
