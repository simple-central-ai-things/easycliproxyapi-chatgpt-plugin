package plugin

// Management API plumbing (F5+F6): the import surface (import.go), the
// password recovery route (password_login.go) and the drawing workbench
// (workbench.go) are exposed through the host's Management API. The host invokes management.register once at startup; the
// reply lists the plugin-owned routes by method + path + description ONLY —
// the SDK's Handler field is a Go function pointer and does not cross the
// ABI, and the host's RPC adapter attaches itself as the callback target.
// Every hit on a registered route then comes back as a management.handle RPC
// carrying an already-authenticated ManagementRequest (the host
// authenticates management traffic before dispatching it). The plugin
// therefore owns an internal exact-path router: paths are exact (no path
// params), relative under /v0/management/, and the host sends them back
// full-path.
//
// The registration reply also carries browser RESOURCES (F6): the host
// resolves each relative path under /v0/resource/plugins/<pluginID>/ and
// dispatches the GET through management.handle with the FULL resource path
// and no body (the host's ServeResourceHTTP builds the request itself).
// Resource paths therefore travel through the internal router un-normalized
// (normalizeManagementPath keeps anything outside /v0/management/ as-is).
//
// Management routes carry NO Menu label: a GET route with a Menu would be
// diverted by the host to the browser-resource surface (/v0/resource/...),
// which is NOT management-authenticated — the resource itself is declared
// in the reply's `resources` instead.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementBasePath is the only prefix the host lets plugins register
// under; the host resolves the plugin's relative paths against it and sends
// the full path back on every management.handle.
const managementBasePath = "/v0/management"

// managementRegistrationRequest mirrors the host's management.register wire
// shape (PascalCase — the SDK request carries no json tags). The fields are
// host context; the reply is independent of them.
type managementRegistrationRequest struct {
	Plugin           pluginapi.Metadata
	BasePath         string
	ResourceBasePath string
}

// managementRegistrationResult mirrors the host's
// rpcManagementRegistrationResponse: the reply lists routes and resources
// only (method + path + description — no handler to cross the ABI). The
// host's RPC adapter (rpc_client.go RegisterManagement) attaches itself as
// the callback target for both.
type managementRegistrationResult struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// managementHandleRequest mirrors the host's rpcManagementRequest: the SDK
// request (PascalCase keys — no json tags) plus the callback id the host
// uses to scope the transport to this RPC. Method/Path/Headers/Query/Body
// arrive exactly as the authenticated client sent them.
type managementHandleRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// managementResponseJSON renders one management reply: raw JSON bytes (the
// host serves Body as-is — schema_version 6 skips the legacy HTML escape),
// the content type, and the status (0 would mean 200, so every reply sets
// it explicitly).
func managementResponseJSON(status int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
	}
}

// managementErrorBody is the typed body for every refusal — routing
// (404/405) and handler errors alike: fixed code + message + retryable,
// never payload material.
type managementErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable,omitempty"`
	} `json:"error"`
}

// managementError builds a typed refusal response.
func managementError(status int, code, message string, retryable bool) pluginapi.ManagementResponse {
	body := managementErrorBody{}
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Retryable = retryable
	return managementResponseJSON(status, body)
}

// managementRouteSpec is one plugin-owned management route: the exact path
// (relative under /v0/management/), its single method, the panel
// description, and the handler that answers management.handle hits on it.
type managementRouteSpec struct {
	method      string
	path        string
	description string
	handle      func(m *Manager, req managementHandleRequest) pluginapi.ManagementResponse
}

// managementRouteSpecs is the complete management route set (import.go +
// password_login.go + workbench.go). The host registers each (METHOD,
// fullPath) pair exactly; anything not listed never reaches the plugin.
func managementRouteSpecs() []managementRouteSpec {
	return []managementRouteSpec{
		{
			method:      http.MethodPost,
			path:        importRouteCpaFile,
			description: "Import one CLIProxyAPI codex auth-file export (the JSON body) as a credential",
			handle:      (*Manager).handleImportCpaFile,
		},
		{
			method:      http.MethodGet,
			path:        importRouteCpaRemoteFiles,
			description: "List the auth files on a CLIProxyAPI management server (body: base_url + secret_key)",
			handle:      (*Manager).handleImportCpaRemoteFiles,
		},
		{
			method:      http.MethodPost,
			path:        importRouteCpaRemote,
			description: "Import selected auth files from a CLIProxyAPI management server (body: base_url + secret_key + files)",
			handle:      (*Manager).handleImportCpaRemote,
		},
		{
			method:      http.MethodGet,
			path:        importRouteSub2apiAccounts,
			description: "List the OpenAI OAuth accounts on a sub2api admin server (body: base_url + email + password)",
			handle:      (*Manager).handleImportSub2apiAccounts,
		},
		{
			method:      http.MethodPost,
			path:        importRouteSub2api,
			description: "Import selected accounts from a sub2api admin server (body: base_url + email + password + accounts)",
			handle:      (*Manager).handleImportSub2api,
		},
		{
			method:      http.MethodPost,
			path:        loginRoutePassword,
			description: "Recover a ChatGPT account via password re-login (body: email + password) — the auth.login.password core",
			handle:      (*Manager).handleLoginPasswordRoute,
		},
		{
			method:      http.MethodPost,
			path:        workbenchRouteGenerate,
			description: "Generate images synchronously (body: prompt, model?, size?, quality?, n?, credential?) — the drawing workbench",
			handle:      (*Manager).handleWorkbenchGenerate,
		},
		{
			method:      http.MethodPost,
			path:        workbenchRouteEdit,
			description: "Edit images from reference_images synchronously (body: prompt, model?, size?, quality?, n?, reference_images[], credential?) — the drawing workbench",
			handle:      (*Manager).handleWorkbenchEdit,
		},
		{
			method:      http.MethodGet,
			path:        workbenchRouteModels,
			description: "List the published image model ids the workbench can drive",
			handle:      (*Manager).handleWorkbenchModels,
		},
	}
}

// managementResourceSpec merges the one browser resource into the internal
// exact-path table: the host dispatches its GET through management.handle
// with the FULL /v0/resource/plugins/<pluginID>/ path (see the file doc), so
// the table key is the full path, unlike the management routes' relative
// keys.
func managementResourceSpec() managementRouteSpec {
	return managementRouteSpec{
		method:      http.MethodGet,
		path:        workbenchResourceFullPath,
		description: "Serve the data-free workbench shell (text/html)",
		handle:      (*Manager).handleWorkbenchShell,
	}
}

// managementRouteTable is the exact-path router management.handle dispatches
// through: byKey answers the method+path lookup, byPath the wrong-method
// check.
type managementRouteTable struct {
	byKey  map[string]managementRouteSpec
	byPath map[string]map[string]managementRouteSpec
}

var managementRoutes = buildManagementRouteTable()

func buildManagementRouteTable() managementRouteTable {
	byKey := map[string]managementRouteSpec{}
	byPath := map[string]map[string]managementRouteSpec{}
	// The management routes (relative keys) plus the one resource (full
	// key — the host sends the resource path back as-is).
	specs := append(managementRouteSpecs(), managementResourceSpec())
	for _, spec := range specs {
		byKey[spec.method+" "+spec.path] = spec
		if byPath[spec.path] == nil {
			byPath[spec.path] = map[string]managementRouteSpec{}
		}
		byPath[spec.path][spec.method] = spec
	}
	return managementRouteTable{byKey: byKey, byPath: byPath}
}

// normalizeManagementPath reduces a management.handle path to the table's
// key form: management paths arrive under the /v0/management prefix (how
// the host sends them) and are keyed relative — relative forms and trailing
// slashes are tolerated too. Resource paths arrive FULL (the host's
// ServeResourceHTTP builds the request itself) and are keyed as-is, so
// anything outside the management prefix passes through untouched.
func normalizeManagementPath(raw string) string {
	path := strings.TrimSpace(raw)
	if !strings.HasPrefix(path, managementBasePath) {
		return path
	}
	path = strings.TrimPrefix(path, managementBasePath)
	path = strings.TrimLeft(path, "/") // double slashes after the prefix tolerated
	return strings.TrimRight(path, "/")
}

// handleManagementRegister implements management.register: it replies with
// the exact management route set (import.go + password_login.go +
// workbench.go) and the one browser resource — method + path + description
// only (no handler to cross the ABI, no Menu label on a management route). The resource is registered
// by its relative path under /v0/resource/plugins/<pluginID>/ (the host
// resolves it); its Menu is what makes it browser-navigable.
func (m *Manager) handleManagementRegister(request []byte) ([]byte, error) {
	var req managementRegistrationRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed management.register request", 400)), nil
	}
	specs := managementRouteSpecs()
	routes := make([]pluginapi.ManagementRoute, 0, len(specs))
	for _, spec := range specs {
		routes = append(routes, pluginapi.ManagementRoute{
			Method:      spec.method,
			Path:        spec.path,
			Description: spec.description,
		})
	}
	return ok(managementRegistrationResult{
		Routes:    routes,
		Resources: managementResourceSpecs(),
	}), nil
}

// handleManagementHandle implements management.handle: decode the
// already-authenticated request, route by exact method+path, and pass the
// pluginapi.ManagementResponse through the success envelope (the host's
// RPC adapter unwraps it and serves Body raw with the given status).
// Unknown paths answer 404 and wrong methods on known paths answer 405 —
// the host's own lookup does the same before dispatching, so these
// branches are defense in depth (and the offline pin).
func (m *Manager) handleManagementHandle(request []byte) ([]byte, error) {
	var req managementHandleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed management.handle request", 400)), nil
	}
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	spec, found := managementRoutes.byKey[method+" "+path]
	if !found {
		if _, known := managementRoutes.byPath[path]; known {
			return ok(managementError(http.StatusMethodNotAllowed,
				"management_method_not_allowed",
				"this import route does not accept the "+method+" method", false)), nil
		}
		return ok(managementError(http.StatusNotFound,
			"management_not_found", "no such import route", false)), nil
	}
	return ok(spec.handle(m, req)), nil
}
