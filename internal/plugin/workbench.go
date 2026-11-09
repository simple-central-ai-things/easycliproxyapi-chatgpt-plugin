package plugin

// The drawing workbench (F6): the plugin-owned image console. The
// management.register reply carries one browser RESOURCE — the single
// self-contained HTML shell — plus three management routes that drive the
// existing openai-image pipeline (executor_image.go) synchronously.
//
// The interaction oracle is the reference's web image app (compose →
// results grid → history), ported dependency-free. Two deliberate,
// documented simplifications vs the oracle:
//   - SYNCHRONOUS: the reference is a separate HTTP server with background
//     workers (create task → poll), so it answers a task id. This plugin
//     runs the pipeline inline in the host process — one management.handle
//     call runs the whole turn and returns the result grid; there is no
//     task/poll loop.
//   - CLIENT-SIDE HISTORY: the shell keeps its history in the browser's
//     localStorage (prompt/config per turn, like the reference's
//     image-conversations store). There is NO server-side history store.
//
// Credential acquisition: management.handle is NOT a model execution, so the
// request carries no host-injected StorageJSON. The workbench acquires a
// credential itself from the host store — the import's knownAccessTokens
// pattern (host.auth.list + host.auth.get, import.go): the body's optional
// `credential` name selects that exact own-provider entry, an absent name
// defaults to the FIRST own-provider entry (store order), and an empty store
// is a typed refusal pointing at the import routes. The acquired file name
// becomes the turn's authFileName, so the run loop's 401 refresh-retry
// persists to the right record.
//
// The shell is DATA-FREE (security rule, pinned by test): its bytes carry
// no credential, token, email or config value. All dynamic data arrives
// from the authenticated workbench routes via same-origin fetches with
// credentials. The host's resource path (ServeResourceHTTP) writes the body
// verbatim — no CSP injection, no sanitizing — which is what makes the
// single-file inline-CSS/JS shell servable raw; the data routes themselves
// are management-authenticated by the host before dispatch.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// workbenchShellBody is the data-free console: a single self-contained HTML
// page (inline CSS + JS, no external assets or build step). The host's
// resource path serves the bytes verbatim (no CSP injection, no sanitizing),
// so embed is the exact-serving mechanism.
//
//go:embed workbench_shell.html
var workbenchShellBody []byte

// Workbench route paths (exact, relative under /v0/management/ — the host
// resolves and authenticates them before dispatching to management.handle).
const (
	workbenchRouteGenerate = providerKey + "/workbench/generate"
	workbenchRouteEdit     = providerKey + "/workbench/edit"
	workbenchRouteModels   = providerKey + "/workbench/models"
)

// workbenchResourcePath is the resource route's relative path; the host
// resolves it under /v0/resource/plugins/<pluginID>/ and dispatches the GET
// through management.handle with the FULL path and no body (the host's
// ServeResourceHTTP builds the ManagementRequest itself).
const workbenchResourcePath = providerKey + "/workbench"

// workbenchResourceMenu is the panel Menu label for the resource.
const workbenchResourceMenu = "ChatGPT Image Workbench"

// workbenchResourceFullPath is the internal dispatch key: exactly what the
// host sends back on the GET resource path.
const workbenchResourceFullPath = "/v0/resource/plugins/" + pluginID + "/" + workbenchResourcePath

// Typed error classes the workbench surface answers (README error-matrix
// pinned; "workbench" pins). Pipeline failures reuse the executor's codes
// (invalid_request, model_unknown, invalid_credential, invalid_token,
// content_policy_violation, upstream_error, ...) — the same rule that fires
// on /v1/images keeps the same vocabulary here.
const (
	// errWorkbenchCredentialMissing: no own-provider credential exists yet
	// (the operator must add/import an account first).
	errWorkbenchCredentialMissing = "workbench_credential_missing"
	// errWorkbenchCredentialNotFound: the explicit `credential` name is not
	// an own-provider entry in the host store.
	errWorkbenchCredentialNotFound = "workbench_credential_not_found"
	// errWorkbenchStoreUnavailable: a host.auth.list/get leg refused (store
	// outage, a disk-only entry without a runtime index, an unreadable
	// record) — retryable, never a blind pipeline start.
	errWorkbenchStoreUnavailable = "workbench_store_unavailable"
)

// workbenchError is the typed refusal a workbench route answers; Message is
// always fixed text — no store material, no secret (the importError shape).
type workbenchError struct {
	code       string
	httpStatus int
	message    string
	retryable  bool
}

func workbenchErrorResponse(e workbenchError) pluginapi.ManagementResponse {
	return managementError(e.httpStatus, e.code, e.message, e.retryable)
}

func workbenchStoreUnavailable() workbenchError {
	return workbenchError{
		code: errWorkbenchStoreUnavailable, httpStatus: http.StatusInternalServerError,
		message:   "the host credential store is unavailable",
		retryable: true,
	}
}

// workbenchInvalidCred answers a typed refusal for an unreadable own
// credential record: the existing invalid_credential class (400), fixed text.
func workbenchInvalidCred() workbenchError {
	return workbenchError{
		code:       "invalid_credential",
		httpStatus: http.StatusBadRequest,
		message:    "malformed " + providerKey + " credential record",
	}
}

// acquireWorkbenchCredential fetches one own-provider credential for the
// management context (see the file doc): host.auth.list names the files,
// the explicit name selects that entry (or the first own-provider entry
// when absent), and host.auth.get yields the raw record. The returned file
// name is the turn's authFileName.
func (m *Manager) acquireWorkbenchCredential(explicit string) (string, Credential, workbenchError) {
	raw, err := m.host.Call(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return "", Credential{}, workbenchStoreUnavailable()
	}
	known, ok := decodeKnownAccessTokens(raw)
	if !ok {
		return "", Credential{}, workbenchStoreUnavailable()
	}
	own := make([]hostAuthFileEntry, 0, len(known.entries))
	for _, entry := range known.entries {
		if isOwnAuthFileEntry(entry) {
			own = append(own, entry)
		}
	}

	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		for _, entry := range own {
			if entry.Name != explicit {
				continue
			}
			if strings.TrimSpace(entry.AuthIndex) == "" {
				// Disk-only listing: the record cannot be read through
				// host.auth.get — typed refusal, never a blind start.
				return "", Credential{}, workbenchStoreUnavailable()
			}
			cred, e := m.fetchOwnCredential(entry.AuthIndex)
			return explicit, cred, e
		}
		return "", Credential{}, workbenchError{
			code:       errWorkbenchCredentialNotFound,
			httpStatus: http.StatusNotFound,
			message:    "no such credential exists in the host store",
		}
	}

	if len(own) == 0 {
		return "", Credential{}, workbenchError{
			code:       errWorkbenchCredentialMissing,
			httpStatus: http.StatusNotFound,
			message: "no " + providerKey + " credential exists yet; add an account first " +
				"(the " + providerKey + "/import routes or the panel login)",
		}
	}

	// Default: the first READABLE own-provider entry (store order). Entries
	// without a runtime index (disk-only listing) cannot be read and are
	// skipped — the knownAccessTokens precedent; a failed read never aborts
	// the rest (one unreadable record must not hide a healthy account).
	for _, entry := range own {
		if strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		cred, e := m.fetchOwnCredential(entry.AuthIndex)
		if e.code != "" {
			continue
		}
		return entry.Name, cred, workbenchError{}
	}
	return "", Credential{}, workbenchStoreUnavailable()
}

// fetchOwnCredential reads one entry's record through host.auth.get and
// parses it (the knownAccessTokens get/parse legs, minus the tolerance —
// here a failed leg is a typed refusal, not a skip).
func (m *Manager) fetchOwnCredential(authIndex string) (Credential, workbenchError) {
	getReq, err := json.Marshal(map[string]string{"auth_index": authIndex})
	if err != nil {
		return Credential{}, workbenchStoreUnavailable()
	}
	raw, err := m.host.Call(pluginabi.MethodHostAuthGet, getReq)
	if err != nil {
		return Credential{}, workbenchStoreUnavailable()
	}
	result, ok := decodeHostAuthGetResult(raw)
	if !ok {
		return Credential{}, workbenchStoreUnavailable()
	}
	cred, perr := FromStorageJSON(result.JSON)
	if perr != nil {
		return Credential{}, workbenchInvalidCred()
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return Credential{}, workbenchInvalidCred()
	}
	return cred, workbenchError{}
}

// --- request / response shapes --------------------------------------------

// workbenchRequest is one generate/edit body. N is validated (1..4) by
// decodeWorkbenchRequest through the executor's validateImageCount — the
// workbench count IS the executor's n (sequential in one attempt; NOT the
// reference web's 1..100-per-task).
type workbenchRequest struct {
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	Size            string   `json:"size"`
	Quality         string   `json:"quality"`
	N               *int     `json:"n"`
	Credential      string   `json:"credential"`
	ReferenceImages []string `json:"reference_images"`
}

// decodeWorkbenchRequest validates the body BEFORE any store or pipeline
// call (route refusals cost zero network). edit also bounds
// reference_images (>=1, <= the executor's 10-image cap).
func decodeWorkbenchRequest(raw []byte, edit bool) (workbenchRequest, *pluginabi.Error) {
	var body workbenchRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		return body, typedErr("invalid_request", "the request body must be JSON", http.StatusBadRequest)
	}
	if strings.TrimSpace(body.Prompt) == "" {
		return body, typedErr("invalid_request", "prompt is required", http.StatusBadRequest)
	}
	n, apiErr := imageCountPtr(body.N) // reuse the executor's n contract
	if apiErr != nil {
		return body, apiErr
	}
	body.N = &n
	if edit {
		if len(body.ReferenceImages) == 0 {
			return body, typedErr("invalid_request",
				"reference_images must carry at least one image", http.StatusBadRequest)
		}
		if len(body.ReferenceImages) > imageMaxReferences {
			return body, typedErr("invalid_request",
				"at most 10 images are supported", http.StatusBadRequest)
		}
	}
	return body, nil
}

// workbenchModelRoute maps the body's `model` field to the pipeline route:
// the published id (<prefix>/<ImageModelName> drawing,
// <prefix>/<CodexImageModelName> codex), an optional prefix on a base name,
// or absent → the drawing route (the default). Unknown models are the
// executor's typed model_unknown.
func workbenchModelRoute(raw string, cfg Config) (imageRoute, string, *pluginabi.Error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return imageRouteDrawing, cfg.ModelPrefix + "/" + cfg.ImageModelName, nil
	}
	base := model
	if strings.HasPrefix(model, cfg.ModelPrefix+"/") {
		base = strings.TrimPrefix(model, cfg.ModelPrefix+"/")
	}
	route, ok := imageRouteFor(base, cfg)
	if !ok {
		return imageRouteDrawing, "", typedErr("model_unknown", fmt.Sprintf(
			"model %q is not a published image model (%s/%s or %s/%s)",
			model, cfg.ModelPrefix, cfg.ImageModelName, cfg.ModelPrefix, cfg.CodexImageModelName), http.StatusBadRequest)
	}
	return route, cfg.ModelPrefix + "/" + base, nil
}

// workbenchPipelineError renders a pipeline typed error (finalizeImageTurn's
// refusal) as the SAME code + status + retryable on the management surface.
func workbenchPipelineError(apiErr *pluginabi.Error) pluginapi.ManagementResponse {
	status := apiErr.HTTPStatus
	if status == 0 {
		status = http.StatusBadGateway
	}
	return managementError(status, apiErr.Code, apiErr.Message, apiErr.Retryable)
}

// --- handlers ---------------------------------------------------------------

// handleWorkbenchGenerate: POST chatgpt-proxy/workbench/generate —
// {prompt, model?, size?, quality?, n?, credential?} → the drawing pipeline
// → {created, data:[{revised_prompt, b64_json}], usage} (the executor's
// pinned images envelope, assembled by assembleImageResponse).
func (m *Manager) handleWorkbenchGenerate(req managementHandleRequest) pluginapi.ManagementResponse {
	body, apiErr := decodeWorkbenchRequest(req.Body, false)
	if apiErr != nil {
		return workbenchPipelineError(apiErr)
	}
	return m.runWorkbenchTurn(req, body, false)
}

// handleWorkbenchEdit: POST chatgpt-proxy/workbench/edit — the same body
// plus reference_images[] (data-URLs / bare base64 / binary, decoded through
// the existing decodeReferenceImages path) → the pipeline with references →
// the same envelope.
func (m *Manager) handleWorkbenchEdit(req managementHandleRequest) pluginapi.ManagementResponse {
	body, apiErr := decodeWorkbenchRequest(req.Body, true)
	if apiErr != nil {
		return workbenchPipelineError(apiErr)
	}
	return m.runWorkbenchTurn(req, body, true)
}

// runWorkbenchTurn builds the imageTurn for one workbench request and runs
// the existing pipeline (runImageTurn + finalizeImageTurn). The ONLY new
// logic vs prepareImageTurn is the credential acquisition for the
// management context — the host-injected StorageJSON has no analogue on
// management.handle, so the turn is built directly from the store record.
func (m *Manager) runWorkbenchTurn(req managementHandleRequest, body workbenchRequest, edit bool) pluginapi.ManagementResponse {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg.ModelPrefix == "" {
		// Unregistered manager (offline tests): serve the config defaults
		// — the same ids the model catalog publishes for an empty block.
		if def, err := ParseConfig([]byte("{}")); err == nil {
			cfg = def
		}
	}

	route, modelID, apiErr := workbenchModelRoute(body.Model, cfg)
	if apiErr != nil {
		return workbenchPipelineError(apiErr)
	}

	authFileName, cred, wbErr := m.acquireWorkbenchCredential(body.Credential)
	if wbErr.code != "" {
		return workbenchErrorResponse(wbErr)
	}

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return workbenchPipelineError(typedErr("upstream_error",
			"upstream transport unavailable: "+err.Error(), http.StatusInternalServerError))
	}

	turn := &imageTurn{
		cfg:          cfg,
		cred:         cred,
		upstream:     u,
		route:        route,
		modelID:      modelID,
		prompt:       body.Prompt,
		size:         body.Size,
		quality:      body.Quality,
		n:            *body.N,
		authFileName: authFileName,
	}

	if edit {
		// The reference decode may fetch http(s) image sources through the
		// upstream transport; it is bounded separately from the turn budget
		// (the prepareImageTurn precedent).
		parseCtx, parseCancel := context.WithTimeout(context.Background(), 60*time.Second)
		turn.references, apiErr = decodeReferenceImages(body.ReferenceImages, u, parseCtx)
		parseCancel()
		if apiErr != nil {
			return workbenchPipelineError(apiErr)
		}
		for i := range turn.references {
			applyImageDims(&turn.references[i])
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(cfg))
	defer cancel()
	results, runErr := m.runImageTurn(ctx, turn, req.HostCallbackID)
	payload, apiErr := finalizeImageTurn(turn, results, runErr)
	if apiErr != nil {
		return workbenchPipelineError(apiErr)
	}
	// The payload is ALREADY the images envelope (assembleImageResponse) —
	// serve the bytes verbatim, never re-marshal (a re-marshal would base64
	// the JSON).
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       payload,
	}
}

// handleWorkbenchModels: GET chatgpt-proxy/workbench/models — the two
// published image model ids (from the registered config) for the shell's
// select.
func (m *Manager) handleWorkbenchModels(req managementHandleRequest) pluginapi.ManagementResponse {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg.ModelPrefix == "" {
		if def, err := ParseConfig([]byte("{}")); err == nil {
			cfg = def
		}
	}
	return managementResponseJSON(http.StatusOK, struct {
		Models []string `json:"models"`
	}{Models: []string{
		cfg.ModelPrefix + "/" + cfg.ImageModelName,
		cfg.ModelPrefix + "/" + cfg.CodexImageModelName,
	}})
}

// handleWorkbenchShell answers the GET resource path with the pinned shell
// bytes. The host's ServeResourceHTTP writes the body verbatim (no CSP
// injection, no sanitizing on the resource path — see the file doc), so the
// single-file inline-CSS/JS shell is served as-is; the shell is data-free.
func (m *Manager) handleWorkbenchShell(req managementHandleRequest) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: workbenchShellBody,
	}
}

// managementResourceSpecs is the complete resource set (F6): the one
// browser-navigable workbench shell. Registered in the management.register
// reply's `resources` — the host resolves the relative path under
// /v0/resource/plugins/<pluginID>/ and dispatches the GET through
// management.handle.
func managementResourceSpecs() []pluginapi.ResourceRoute {
	return []pluginapi.ResourceRoute{{
		Path:        workbenchResourcePath,
		Menu:        workbenchResourceMenu,
		Description: "The drawing workbench console (data-free HTML shell; the generate/edit/models routes carry the data)",
	}}
}
