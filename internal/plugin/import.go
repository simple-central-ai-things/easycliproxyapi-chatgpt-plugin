package plugin

// Account imports (F5): bring ChatGPT OAuth accounts in from a CLIProxyAPI
// (CPA) codex export or a sub2api admin server and write them into the
// host's credential store as type:"chatgpt-proxy" auth files — the SAME
// record shape a login stores, so auth.parse/auth.refresh, the executor's
// 401 refresh-retry and quota work on them unchanged.
//
// The routes hang on the host Management API (management.go). Each handler
// takes the source config INLINE in the request body — the reference's
// pool/server dict passed by value. Pool/server persistence (CRUD) is a
// follow-up, as is the reference's async job record: these handlers run to
// completion inside one management.handle call and answer the job-summary
// shape ({added, skipped, errors}) inline.
//
// Deliberate superset vs the reference: the reference's CPA download step
// extracts ONLY the access token and then keepalives the imported accounts
// (refresh_accounts). This plugin captures the FULL token set the payload
// provides (access token required; refresh/id tokens when present, flat or
// nested under "tokens") and does NOT reimplement keepalive — the saved
// credential joins the plugin's existing auth.refresh lifecycle and
// refreshes on demand (executor 401 retry, host-scheduled refresh).
//
// Secret hygiene: the CPA secret_key, the sub2api password and the sub2api
// JWT cross the ABI only as management.handle request input and appear
// only in the outbound Authorization headers / the sub2api login body.
// Every reply, error, summary entry and saved credential carries FIXED
// typed text — upstream bodies are read only to pick the error branch,
// never echoed (an upstream-controlled body could echo credential
// material), exactly the F4 classifyPasswordVerifyError pattern.
//
// Source typing: both import routes tag records source_type "codex" — the
// reference does the same for both (the token SHAPE both sources carry is
// the codex OAuth triple), and the plugin's SourceType vocabulary already
// names "codex".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Import route paths (exact, relative under /v0/management/; the host
// resolves and authenticates them before dispatching to management.handle).
const (
	importRouteCpaFile         = providerKey + "/import/cpa-file"
	importRouteCpaRemoteFiles  = providerKey + "/import/cpa-remote/files"
	importRouteCpaRemote       = providerKey + "/import/cpa-remote"
	importRouteSub2apiAccounts = providerKey + "/import/sub2api/accounts"
	importRouteSub2api         = providerKey + "/import/sub2api"
)

// Typed error classes the import surface answers (README error-matrix
// pinned; "import" pins).
const (
	// errImportInvalidRequest: the request body itself is malformed or a
	// required inline config field is missing.
	errImportInvalidRequest = "import_invalid_request"
	// errImportUnauthorized: the remote server rejected the credentials
	// (CPA secret_key 401/403, sub2api login refusal).
	errImportUnauthorized = "import_unauthorized"
	// errImportRemoteFailed: transport unavailable or any other remote
	// refusal; retryable.
	errImportRemoteFailed = "import_remote_failed"
	// errImportPayloadInvalid: a payload — inline export or remote
	// response — does not carry the expected shape.
	errImportPayloadInvalid = "import_payload_invalid"
	// errImportSaveFailed: the host credential store (list/get/save)
	// refused; retryable.
	errImportSaveFailed = "import_save_failed"
)

// importSourceType tags every imported record the way the reference tags
// its CPA / sub2api imports (see the file doc).
const importSourceType = "codex"

// importError is the typed refusal an import leg answers; Message is always
// fixed text — no upstream body, no secret material.
type importError struct {
	code       string
	httpStatus int
	message    string
	retryable  bool
}

func importErrorResponse(e importError) pluginapi.ManagementResponse {
	return managementError(e.httpStatus, e.code, e.message, e.retryable)
}

func importTransportError() importError {
	return importError{
		code: errImportRemoteFailed, httpStatus: http.StatusBadGateway,
		message:   "the remote request could not be completed through the host transport",
		retryable: true,
	}
}

func importStoreUnavailable() importError {
	return importError{
		code: errImportSaveFailed, httpStatus: http.StatusInternalServerError,
		message:   "the host credential store is unavailable",
		retryable: true,
	}
}

// importRefusal classifies a remote HTTP status by branch only (F4 pattern):
// the body is NEVER echoed. 401/403 are credential refusals; anything else
// non-OK is a retryable remote failure.
func importRefusal(status int) (importError, bool) {
	switch status {
	case http.StatusOK:
		return importError{}, false
	case http.StatusUnauthorized, http.StatusForbidden:
		return importError{
			code: errImportUnauthorized, httpStatus: http.StatusUnauthorized,
			message: "the remote server rejected the credentials",
		}, true
	default:
		return importError{
			code: errImportRemoteFailed, httpStatus: http.StatusBadGateway,
			message:   "the remote server refused the request",
			retryable: true,
		}, true
	}
}

// --- request / response shapes -------------------------------------------

// cpaRemoteConfig is the inline CPA source config (the reference's pool
// dict passed by value — no pool persistence in F5).
type cpaRemoteConfig struct {
	BaseURL   string `json:"base_url"`
	SecretKey string `json:"secret_key"`
}

// sub2apiConfig is the inline sub2api source config (the reference's server
// dict, email+password legs; the reference's api_key alternative is a
// follow-up).
type sub2apiConfig struct {
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// cpaRemoteFile is one entry of the CPA auth-file list reply.
type cpaRemoteFile struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// sub2apiAccount is one entry of the sub2api account list reply (the
// reference's flat list shape, has_refresh_token flag included).
type sub2apiAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	PlanType        string `json:"plan_type"`
	Status          string `json:"status"`
	ExpiresAt       string `json:"expires_at"`
	HasRefreshToken bool   `json:"has_refresh_token"`
}

// importSummaryError is one per-item failure of a batch import. Name is the
// item's display label (file name / account id — never a secret) and
// Message is fixed typed text.
type importSummaryError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// importSummary is the job-summary shape the reference persists per pool;
// F5 answers it inline (no persisted job — pool/server CRUD and the async
// job record are follow-ups).
type importSummary struct {
	Added   int                  `json:"added"`
	Skipped int                  `json:"skipped"`
	Errors  []importSummaryError `json:"errors"`
}

// --- shared validation ----------------------------------------------------

// validateImportBaseURL fails closed on anything that is not an http(s)
// URL with a host — the outbound target is operator input, not a constant.
func validateImportBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base_url must be an http(s) URL")
	}
	return nil
}

// importBaseURLErr answers the typed 400 for a refused base_url.
func importBaseURLErr() pluginapi.ManagementResponse {
	return managementError(http.StatusBadRequest, errImportInvalidRequest,
		"base_url must be an http(s) URL", false)
}

// --- token-set extraction -------------------------------------------------

// importTokens is the token set one import payload yields. The reference
// extracts the access token only; this is the deliberate superset (see the
// file doc) — refresh/id tokens let the saved credential join the plugin's
// auth.refresh lifecycle.
type importTokens struct {
	accessToken  string
	refreshToken string
	idToken      string
}

// importTokenAt reads the first non-empty string among keys.
func importTokenAt(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, isString := record[key].(string); isString {
			if s := strings.TrimSpace(value); s != "" {
				return s
			}
		}
	}
	return ""
}

// extractImportTokens pulls the token set from one import payload: the flat
// sub2api fallback chain (access_token / accessToken / token, plus the
// refresh/id keys when present) first, then the same keys nested under
// "tokens" (a CPA codex-export layout). nil and empty maps answer empty.
func extractImportTokens(record map[string]any) importTokens {
	if record == nil {
		return importTokens{}
	}
	tokens := importTokens{
		accessToken:  importTokenAt(record, "access_token", "accessToken", "token"),
		refreshToken: importTokenAt(record, "refresh_token", "refreshToken"),
		idToken:      importTokenAt(record, "id_token", "idToken"),
	}
	if tokens.accessToken == "" {
		if nested, isMap := record["tokens"].(map[string]any); isMap {
			tokens = extractImportTokens(nested)
		}
	}
	return tokens
}

// --- the store: dedupe + save ---------------------------------------------

// hostAuthListResult / hostAuthGetResult mirror the host's host.auth.list /
// host.auth.get callback wire shapes (snake_case keys, raw JSON payload).
type hostAuthListResult struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthFileEntry struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetResult struct {
	JSON json.RawMessage `json:"json"`
}

// isOwnAuthFileEntry reports whether a store entry belongs to this provider
// (host-managed Type/Provider keys, falling back to the stable-hash name
// prefix for a disk-only listing).
func isOwnAuthFileEntry(entry hostAuthFileEntry) bool {
	return entry.Type == providerKey || entry.Provider == providerKey ||
		strings.HasPrefix(entry.Name, providerKey+"-")
}

// knownAccessTokens returns the access tokens of every credential already
// in the host store under this provider: host.auth.list names the files,
// host.auth.get yields their raw JSON. Entries without a runtime index
// (disk-only listing) cannot be read and stay "unknown" — re-importing them
// is harmless, the stable-hash file name makes the save idempotent.
// Individual get/parse failures are tolerated the same way.
func (m *Manager) knownAccessTokens() (map[string]bool, importError) {
	raw, err := m.host.Call(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return nil, importStoreUnavailable()
	}
	known, ok := decodeKnownAccessTokens(raw)
	if !ok {
		return nil, importStoreUnavailable()
	}
	for _, entry := range known.entries {
		if !isOwnAuthFileEntry(entry) || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		getReq, err := json.Marshal(map[string]string{"auth_index": entry.AuthIndex})
		if err != nil {
			continue
		}
		rawGet, err := m.host.Call(pluginabi.MethodHostAuthGet, getReq)
		if err != nil {
			continue
		}
		result, okGet := decodeHostAuthGetResult(rawGet)
		if !okGet {
			continue
		}
		cred, err := FromStorageJSON(result.JSON)
		if err != nil || strings.TrimSpace(cred.AccessToken) == "" {
			continue
		}
		known.tokens[cred.AccessToken] = true
	}
	return known.tokens, importError{}
}

// hostAuthListDecode is the decoded host.auth.list result; tokens is filled
// by knownAccessTokens.
type hostAuthListDecode struct {
	entries []hostAuthFileEntry
	tokens  map[string]bool
}

func decodeKnownAccessTokens(raw []byte) (hostAuthListDecode, bool) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return hostAuthListDecode{}, false
	}
	var result hostAuthListResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return hostAuthListDecode{}, false
	}
	return hostAuthListDecode{
		entries: result.Files,
		tokens:  map[string]bool{},
	}, true
}

func decodeHostAuthGetResult(raw []byte) (hostAuthGetResult, bool) {
	var result hostAuthGetResult
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return result, false
	}
	if len(env.Result) == 0 {
		return result, false
	}
	if err := json.Unmarshal(env.Result, &result); err != nil || len(result.JSON) == 0 {
		return result, false
	}
	return result, true
}

// importItem is one account a batch import picked up: the display label
// (file name / account id — never a secret), its tokens, the email the
// source listed, and an optional fixed failure message.
type importItem struct {
	label    string
	tokens   importTokens
	email    string
	failWith string
}

// importCredentials saves one batch of imported accounts into the host's
// credential store via saveCredential (host.auth.save — the executor's
// existing helper). Dedupe by access_token runs against BOTH the existing
// store and the batch itself (the reference's dict.fromkeys semantics): an
// import whose access token is already known answers "skipped", never an
// error, and never a save. One bad item records a fixed-text error and
// never aborts the rest. The returned file name is set only when the batch
// added exactly one credential (the single-file routes report it).
func (m *Manager) importCredentials(items []importItem) (importSummary, string, importError) {
	known, storeErr := m.knownAccessTokens()
	if storeErr.code != "" {
		return importSummary{}, "", storeErr
	}
	summary := importSummary{Errors: []importSummaryError{}}
	fileName := ""
	added := 0
	for _, item := range items {
		if item.failWith != "" {
			summary.Errors = append(summary.Errors, importSummaryError{Name: item.label, Message: item.failWith})
			continue
		}
		tokens := item.tokens
		if strings.TrimSpace(tokens.accessToken) == "" {
			summary.Errors = append(summary.Errors,
				importSummaryError{Name: item.label, Message: "missing access token"})
			continue
		}
		if known[tokens.accessToken] {
			summary.Skipped++
			continue
		}
		cred := Credential{
			AccessToken:  tokens.accessToken,
			RefreshToken: tokens.refreshToken,
			IDToken:      tokens.idToken,
			Email:        strings.TrimSpace(item.email),
			SourceType:   importSourceType,
		}
		// JWT-claim backfill (the auth.parse precedent): imported exports
		// usually carry no identity, so the record's stable file name
		// locks onto the same facts a later parse would recover.
		cred = normalizeCredential(cred)
		fileName = FileNameFor(cred)
		if err := m.saveCredential(fileName, cred); err != nil {
			summary.Errors = append(summary.Errors, importSummaryError{
				Name: item.label, Message: "the host refused to save the credential",
			})
			continue
		}
		known[tokens.accessToken] = true
		summary.Added++
		added++
	}
	if added != 1 {
		fileName = ""
	}
	return summary, fileName, importError{}
}

// --- CPA (CLIProxyAPI) import ---------------------------------------------

// cpaManagementHeaders is the reference's _management_headers: the secret
// rides ONLY in this outbound header.
func cpaManagementHeaders(secretKey string) map[string][]string {
	return map[string][]string{
		"Authorization": {"Bearer " + secretKey},
		"Accept":        {"application/json"},
	}
}

func cpaAuthFilesURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/v0/management/auth-files"
}

func cpaAuthFileDownloadURL(baseURL, fileName string) string {
	return cpaAuthFilesURL(baseURL) + "/download?name=" + url.QueryEscape(fileName)
}

// cpaListFiles ports the reference list_remote_files: GET
// /v0/management/auth-files → {files:[{name, email|account}]}; items without
// a name are dropped, the account key backfills the email.
func (m *Manager) cpaListFiles(callbackID string, cfg cpaRemoteConfig) ([]cpaRemoteFile, importError) {
	resp, err := m.doHostHTTP(callbackID, http.MethodGet, cpaAuthFilesURL(cfg.BaseURL), cpaManagementHeaders(cfg.SecretKey), nil)
	if err != nil {
		return nil, importTransportError()
	}
	if e, refused := importRefusal(resp.StatusCode); refused {
		return nil, e
	}
	var payload struct {
		Files []struct {
			Name    string `json:"name"`
			Email   string `json:"email"`
			Account string `json:"account"`
		} `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil || payload.Files == nil {
		return nil, importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote list payload is invalid",
		}
	}
	files := make([]cpaRemoteFile, 0, len(payload.Files))
	for _, item := range payload.Files {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		email := strings.TrimSpace(item.Email)
		if email == "" {
			email = strings.TrimSpace(item.Account)
		}
		files = append(files, cpaRemoteFile{Name: name, Email: email})
	}
	return files, importError{}
}

// cpaDownloadFile ports the reference fetch_remote_access_token (minus the
// access-token-only extraction): GET /v0/management/auth-files/download
// ?name=<file> → the raw auth-file payload for extractImportTokens.
func (m *Manager) cpaDownloadFile(callbackID string, cfg cpaRemoteConfig, fileName string) (map[string]any, importError) {
	resp, err := m.doHostHTTP(callbackID, http.MethodGet, cpaAuthFileDownloadURL(cfg.BaseURL, fileName), cpaManagementHeaders(cfg.SecretKey), nil)
	if err != nil {
		return nil, importTransportError()
	}
	if e, refused := importRefusal(resp.StatusCode); refused {
		return nil, e
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil || payload == nil {
		return nil, importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote download payload is invalid",
		}
	}
	return payload, importError{}
}

// handleImportCpaFile: POST cpa-file — the body IS the CLIProxyAPI codex
// export JSON; one credential is saved.
func (m *Manager) handleImportCpaFile(req managementHandleRequest) pluginapi.ManagementResponse {
	var payload map[string]any
	if err := json.Unmarshal(req.Body, &payload); err != nil || payload == nil {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"the request body must be the codex export JSON object", false)
	}
	tokens := extractImportTokens(payload)
	if strings.TrimSpace(tokens.accessToken) == "" {
		return managementError(http.StatusBadRequest, errImportPayloadInvalid,
			"the export carries no access token", false)
	}
	summary, fileName, e := m.importCredentials([]importItem{{
		label:  "cpa-file",
		tokens: tokens,
		// Imported exports occasionally carry the account email; it is
		// identity, not a secret.
		email: importTokenAt(payload, "email", "user_email"),
	}})
	if e.code != "" {
		return importErrorResponse(e)
	}
	out := struct {
		importSummary
		// File is the stable-hash auth file name the import saved — safe
		// to report (never the email).
		File string `json:"file,omitempty"`
	}{importSummary: summary, File: fileName}
	return managementResponseJSON(http.StatusOK, out)
}

// handleImportCpaRemoteFiles: GET cpa-remote/files — body {base_url,
// secret_key}; answers the reference's file list.
func (m *Manager) handleImportCpaRemoteFiles(req managementHandleRequest) pluginapi.ManagementResponse {
	var cfg cpaRemoteConfig
	if err := json.Unmarshal(req.Body, &cfg); err != nil ||
		strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"base_url and secret_key are required", false)
	}
	if err := validateImportBaseURL(cfg.BaseURL); err != nil {
		return importBaseURLErr()
	}
	files, e := m.cpaListFiles(req.HostCallbackID, cfg)
	if e.code != "" {
		return importErrorResponse(e)
	}
	return managementResponseJSON(http.StatusOK, struct {
		Files []cpaRemoteFile `json:"files"`
	}{Files: files})
}

// handleImportCpaRemote: POST cpa-remote — body {base_url, secret_key,
// files}; downloads each selected file (sequentially — deterministic
// summary order, unlike the reference's thread pool), captures the full
// token set, and answers the job summary. One bad file never aborts the
// rest.
func (m *Manager) handleImportCpaRemote(req managementHandleRequest) pluginapi.ManagementResponse {
	var body struct {
		cpaRemoteConfig
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"the request body must be JSON", false)
	}
	if strings.TrimSpace(body.BaseURL) == "" || strings.TrimSpace(body.SecretKey) == "" {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"base_url and secret_key are required", false)
	}
	if err := validateImportBaseURL(body.BaseURL); err != nil {
		return importBaseURLErr()
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(body.Files))
	for _, name := range body.Files {
		if s := strings.TrimSpace(name); s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"files is required", false)
	}

	items := make([]importItem, 0, len(names))
	for _, name := range names {
		payload, e := m.cpaDownloadFile(req.HostCallbackID, body.cpaRemoteConfig, name)
		if e.code != "" {
			// Per-file fixed text; the batch continues.
			items = append(items, importItem{label: name, failWith: e.message})
			continue
		}
		items = append(items, importItem{
			label:  name,
			tokens: extractImportTokens(payload),
			email:  importTokenAt(payload, "email"),
		})
	}
	summary, _, e := m.importCredentials(items)
	if e.code != "" {
		return importErrorResponse(e)
	}
	return managementResponseJSON(http.StatusOK, summary)
}

// --- sub2api import --------------------------------------------------------

// sub2apiPageSize pins the reference's paging constant.
const sub2apiPageSize = 200

func sub2apiURL(baseURL, path string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
}

// sub2apiAuthHeaders is the reference's JWT auth headers; the JWT rides ONLY
// in this outbound header.
func sub2apiAuthHeaders(jwt string) map[string][]string {
	return map[string][]string{
		"Authorization": {"Bearer " + jwt},
		"Accept":        {"application/json"},
	}
}

// sub2apiLogin ports the reference _login: POST /api/v1/auth/login with
// {email, password} → the admin JWT (envelope unwrapped). The reference
// caches the JWT per server; F5 acquires it per import call and never
// persists, logs, replies or saves it.
func (m *Manager) sub2apiLogin(callbackID, baseURL, email, password string) (string, importError) {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := m.doHostHTTP(callbackID, http.MethodPost, sub2apiURL(baseURL, "/api/v1/auth/login"), map[string][]string{
		"Accept":       {"application/json"},
		"Content-Type": {"application/json"},
	}, body)
	if err != nil {
		return "", importTransportError()
	}
	if e, refused := importRefusal(resp.StatusCode); refused {
		return "", e
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil || payload == nil {
		return "", importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote login payload is invalid",
		}
	}
	inner, isMap := unwrapSub2apiEnvelope(payload).(map[string]any)
	if !isMap {
		return "", importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote login payload is invalid",
		}
	}
	jwt := importTokenAt(inner, "access_token")
	if jwt == "" {
		return "", importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote login did not return an access token",
		}
	}
	return jwt, importError{}
}

// unwrapSub2apiEnvelope ports the reference _unwrap_envelope: peel
// sub2api's {code, message, data} envelope — return the inner data when the
// payload carries BOTH "data" and "code"; pass everything else through
// (older/alt versions answer unwrapped).
func unwrapSub2apiEnvelope(payload map[string]any) any {
	if payload == nil {
		return nil
	}
	if _, hasData := payload["data"]; hasData {
		if _, hasCode := payload["code"]; hasCode {
			return payload["data"]
		}
	}
	return any(payload)
}

// extractSub2apiPagedItems ports the reference _extract_paged_items: a
// paginated sub2api response → (items, total). It unwraps the envelope
// itself (the reference does) and then handles the wrapped shapes
// ({items|data|list: [...], total}) and bare lists.
func extractSub2apiPagedItems(payload any) ([]any, int) {
	if inner, isMap := payload.(map[string]any); isMap {
		payload = unwrapSub2apiEnvelope(inner)
	}
	switch inner := payload.(type) {
	case []any:
		return inner, len(inner)
	case map[string]any:
		for _, key := range []string{"items", "data", "list"} {
			if value, isList := inner[key].([]any); isList {
				total := 0
				if n, isNum := inner["total"].(float64); isNum {
					total = int(n)
				}
				if total == 0 {
					total = len(value)
				}
				return value, total
			}
		}
	}
	return nil, 0
}

// sub2apiString / sub2apiID read one reference field shape: JSON-decoded
// values arrive as strings or numbers (Python's str() over both).
func sub2apiString(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func sub2apiID(value any) string {
	switch id := value.(type) {
	case string:
		return strings.TrimSpace(id)
	case float64:
		if id == float64(int64(id)) {
			return strconv.FormatInt(int64(id), 10)
		}
		return strconv.FormatFloat(id, 'f', -1, 64)
	default:
		return ""
	}
}

// sub2apiListAccounts ports the reference list_remote_accounts: paged GET
// /api/v1/admin/accounts (platform=openai, type=oauth, page, page_size 200)
// → the flat list with the has_refresh_token flag.
func (m *Manager) sub2apiListAccounts(callbackID, baseURL, jwt string) ([]sub2apiAccount, importError) {
	headers := sub2apiAuthHeaders(jwt)
	accounts := []sub2apiAccount{}
	page := 1
	for {
		query := url.Values{}
		query.Set("platform", "openai")
		query.Set("type", "oauth")
		query.Set("page", strconv.Itoa(page))
		query.Set("page_size", strconv.Itoa(sub2apiPageSize))
		resp, err := m.doHostHTTP(callbackID, http.MethodGet,
			sub2apiURL(baseURL, "/api/v1/admin/accounts")+"?"+query.Encode(), headers, nil)
		if err != nil {
			return nil, importTransportError()
		}
		if e, refused := importRefusal(resp.StatusCode); refused {
			return nil, e
		}
		var payload map[string]any
		if err := json.Unmarshal(resp.Body, &payload); err != nil || payload == nil {
			return nil, importError{
				code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
				message: "the remote account list payload is invalid",
			}
		}
		items, total := extractSub2apiPagedItems(unwrapSub2apiEnvelope(payload))
		if len(items) == 0 {
			break
		}
		for _, raw := range items {
			account, isMap := raw.(map[string]any)
			if !isMap {
				continue
			}
			credentials, _ := account["credentials"].(map[string]any)
			id := sub2apiID(account["id"])
			if id == "" {
				continue
			}
			email := sub2apiString(credentials["email"])
			if email == "" {
				email = sub2apiString(account["name"])
			}
			accounts = append(accounts, sub2apiAccount{
				ID:              id,
				Name:            sub2apiString(account["name"]),
				Email:           email,
				PlanType:        sub2apiString(credentials["plan_type"]),
				Status:          sub2apiString(account["status"]),
				ExpiresAt:       sub2apiString(credentials["expires_at"]),
				HasRefreshToken: sub2apiString(credentials["refresh_token"]) != "",
			})
		}
		if page*sub2apiPageSize >= total || len(items) < sub2apiPageSize {
			break
		}
		page++
	}
	return accounts, importError{}
}

// sub2apiFetchAccountData ports the reference _fetch_access_tokens_for_accounts
// (the credential-bearing legs only): GET /api/v1/admin/accounts/data with
// ids=<csv> + timezone=Asia/Shanghai → {accounts:[...]} (envelope
// unwrapped). The reference's GET shape is ported as-is — the ids travel as
// a comma-joined query param, never a request body.
func (m *Manager) sub2apiFetchAccountData(callbackID, baseURL, jwt string, ids []string) ([]map[string]any, importError) {
	query := url.Values{}
	query.Set("ids", strings.Join(ids, ","))
	query.Set("timezone", "Asia/Shanghai")
	resp, err := m.doHostHTTP(callbackID, http.MethodGet,
		sub2apiURL(baseURL, "/api/v1/admin/accounts/data")+"?"+query.Encode(), sub2apiAuthHeaders(jwt), nil)
	if err != nil {
		return nil, importTransportError()
	}
	if e, refused := importRefusal(resp.StatusCode); refused {
		return nil, e
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil || payload == nil {
		return nil, importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote export payload is invalid",
		}
	}
	inner, isMap := unwrapSub2apiEnvelope(payload).(map[string]any)
	if !isMap {
		return nil, importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote export payload is invalid",
		}
	}
	rawAccounts, isList := inner["accounts"].([]any)
	if !isList {
		return nil, importError{
			code: errImportPayloadInvalid, httpStatus: http.StatusBadGateway,
			message: "the remote export payload is invalid",
		}
	}
	accounts := make([]map[string]any, 0, len(rawAccounts))
	for _, raw := range rawAccounts {
		if account, isMap := raw.(map[string]any); isMap {
			accounts = append(accounts, account)
		}
	}
	return accounts, importError{}
}

// handleImportSub2apiAccounts: GET sub2api/accounts — body {base_url,
// email, password}; logs in once and answers the flat account list.
func (m *Manager) handleImportSub2apiAccounts(req managementHandleRequest) pluginapi.ManagementResponse {
	var cfg sub2apiConfig
	if err := json.Unmarshal(req.Body, &cfg); err != nil {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"the request body must be JSON", false)
	}
	if e := validateSub2apiConfig(cfg); e != "" {
		return managementError(http.StatusBadRequest, errImportInvalidRequest, e, false)
	}
	if err := validateImportBaseURL(cfg.BaseURL); err != nil {
		return importBaseURLErr()
	}
	jwt, loginErr := m.sub2apiLogin(req.HostCallbackID, cfg.BaseURL, cfg.Email, cfg.Password)
	if loginErr.code != "" {
		return importErrorResponse(loginErr)
	}
	accounts, listErr := m.sub2apiListAccounts(req.HostCallbackID, cfg.BaseURL, jwt)
	if listErr.code != "" {
		return importErrorResponse(listErr)
	}
	return managementResponseJSON(http.StatusOK, struct {
		Accounts []sub2apiAccount `json:"accounts"`
	}{Accounts: accounts})
}

// handleImportSub2api: POST sub2api — body {base_url, email, password,
// accounts}; logs in once, fetches the selected accounts' credentials, and
// answers the job summary. A login refusal is a typed envelope error (the
// batch cannot start); a refused export records every requested id as a
// per-item error (the reference's job-failure semantics) and still answers
// the summary.
func (m *Manager) handleImportSub2api(req managementHandleRequest) pluginapi.ManagementResponse {
	var body struct {
		sub2apiConfig
		Accounts []string `json:"accounts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"the request body must be JSON", false)
	}
	if e := validateSub2apiConfig(body.sub2apiConfig); e != "" {
		return managementError(http.StatusBadRequest, errImportInvalidRequest, e, false)
	}
	if err := validateImportBaseURL(body.BaseURL); err != nil {
		return importBaseURLErr()
	}
	ids := make([]string, 0, len(body.Accounts))
	for _, id := range body.Accounts {
		if s := strings.TrimSpace(id); s != "" {
			ids = append(ids, s)
		}
	}
	if len(ids) == 0 {
		return managementError(http.StatusBadRequest, errImportInvalidRequest,
			"accounts is required", false)
	}

	jwt, loginErr := m.sub2apiLogin(req.HostCallbackID, body.BaseURL, body.Email, body.Password)
	if loginErr.code != "" {
		return importErrorResponse(loginErr)
	}
	accounts, dataErr := m.sub2apiFetchAccountData(req.HostCallbackID, body.BaseURL, jwt, ids)
	if dataErr.code != "" {
		// The reference records the export failure per requested id; the
		// fixed message travels, never the upstream body.
		items := make([]importItem, 0, len(ids))
		for _, id := range ids {
			items = append(items, importItem{label: id, failWith: dataErr.message})
		}
		summary, _, storeErr := m.importCredentials(items)
		if storeErr.code != "" {
			return importErrorResponse(storeErr)
		}
		return managementResponseJSON(http.StatusOK, summary)
	}

	items := make([]importItem, 0, len(accounts))
	for _, account := range accounts {
		credentials, _ := account["credentials"].(map[string]any)
		id := sub2apiID(account["id"])
		if id == "" {
			id = sub2apiString(credentials["chatgpt_account_id"])
		}
		if id == "" {
			id = sub2apiString(account["name"])
		}
		items = append(items, importItem{
			label:  id,
			tokens: extractImportTokens(credentials),
			email:  sub2apiString(credentials["email"]),
		})
	}
	if len(accounts) < len(ids) {
		items = append(items, importItem{
			label:    strings.Join(ids, ","),
			failWith: fmt.Sprintf("exported %d/%d accounts", len(accounts), len(ids)),
		})
	}
	summary, _, storeErr := m.importCredentials(items)
	if storeErr.code != "" {
		return importErrorResponse(storeErr)
	}
	return managementResponseJSON(http.StatusOK, summary)
}

// validateSub2apiConfig checks the inline sub2api config; answers a fixed
// message for the typed 400 (never the offending value).
func validateSub2apiConfig(cfg sub2apiConfig) string {
	switch {
	case strings.TrimSpace(cfg.BaseURL) == "":
		return "base_url is required"
	case strings.TrimSpace(cfg.Email) == "":
		return "email is required"
	case strings.TrimSpace(cfg.Password) == "":
		return "password is required"
	}
	return ""
}
