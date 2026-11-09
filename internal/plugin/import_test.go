package plugin

// import_test.go pins the F5 account imports: the Management API plumbing
// (management.register lists the exact import routes, management.handle
// routes them) and the import handlers themselves (CPA file / remote,
// sub2api accounts / import) against a scripted host seam that models the
// credential store and records every ABI crossing for the secret-hygiene
// assertions.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// importFakeCaller is the offline host seam for the import tests: it scripts
// host.http.do results in order and models the credential store with plain
// maps (host.auth.list/get/save). It records every call — request bytes
// included — so the hygiene assertions can scan every ABI crossing.
type importFakeCaller struct {
	calls       []hostCallRecord
	httpResults []hostHTTPResponse

	store      map[string]json.RawMessage // file name → credential JSON
	storeOrder []string

	// noIndex holds file names that appear in host.auth.list WITHOUT a
	// runtime auth_index (the disk-only listing form): the record cannot be
	// read through host.auth.get.
	noIndex map[string]bool

	listError string // when set, host.auth.list / host.auth.get refuse
	saveError string // when set, host.auth.save refuses
}

func newImportFakeCaller() *importFakeCaller {
	return &importFakeCaller{store: map[string]json.RawMessage{}}
}

// seedCredential places one credential in the modeled store (used to
// exercise the dedupe path). Returns the stable file name.
func (f *importFakeCaller) seedCredential(cred Credential) string {
	name := FileNameFor(cred)
	f.store[name] = cred.StorageJSON()
	f.storeOrder = append(f.storeOrder, name)
	return name
}

func (f *importFakeCaller) Call(method string, request []byte) ([]byte, error) {
	f.calls = append(f.calls, hostCallRecord{method: method, request: append([]byte(nil), request...)})

	switch method {
	case pluginabi.MethodHostHTTPDo:
		if len(f.httpResults) > 0 {
			result := f.httpResults[0]
			f.httpResults = f.httpResults[1:]
			return hostOK(result), nil
		}
		return hostOK(map[string]any{}), nil
	case pluginabi.MethodHostAuthList:
		if f.listError != "" {
			return hostErr(f.listError), nil
		}
		entries := make([]hostAuthFileEntry, 0, len(f.storeOrder))
		for i, name := range f.storeOrder {
			entry := hostAuthFileEntry{Name: name, Type: providerKey}
			if !f.noIndex[name] {
				entry.AuthIndex = strconv.Itoa(i + 1)
			}
			entries = append(entries, entry)
		}
		return hostOK(map[string]any{"files": entries}), nil
	case pluginabi.MethodHostAuthGet:
		if f.listError != "" {
			return hostErr(f.listError), nil
		}
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return hostOK(map[string]any{}), nil
		}
		if index, err := strconv.Atoi(req.AuthIndex); err == nil && index >= 1 && index <= len(f.storeOrder) {
			name := f.storeOrder[index-1]
			return hostOK(map[string]any{
				"auth_index": req.AuthIndex, "name": name, "json": f.store[name],
			}), nil
		}
		return hostErr("auth_index not found"), nil
	case pluginabi.MethodHostAuthSave:
		if f.saveError != "" {
			return hostErr(f.saveError), nil
		}
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return hostErr("invalid auth save request"), nil
		}
		if _, exists := f.store[req.Name]; !exists {
			f.storeOrder = append(f.storeOrder, req.Name)
		}
		f.store[req.Name] = req.JSON
		return hostOK(map[string]any{}), nil
	default:
		return hostOK(map[string]any{}), nil
	}
}

func (f *importFakeCaller) savedNames() []string {
	out := make([]string, 0, len(f.store))
	for name := range f.store {
		out = append(out, name)
	}
	return out
}

// managementHandleBody builds the wire shape the host sends to
// management.handle: the SDK request (PascalCase keys — the SDK struct
// carries no json tags) plus the transport callback id, with the
// authenticated client's method/path/body as the host delivers them.
func managementHandleBody(method, path string, body []byte) []byte {
	raw, _ := json.Marshal(managementHandleRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Body: body},
		HostCallbackID:    "cb-import-test",
	})
	return raw
}

// handleManagement drives one management.handle round through the Manager
// dispatcher and returns the decoded ManagementResponse.
func handleManagement(t *testing.T, m *Manager, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	resp, err := m.Handle(pluginabi.MethodManagementHandle,
		managementHandleBody(method, "/v0/management/"+path, body))
	if err != nil {
		t.Fatalf("management.handle surfaced a Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("management.handle envelope not OK: %s", resp)
	}
	var out pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode management response: %v (%s)", err, env.Result)
	}
	return out
}

func mustImportSummary(t *testing.T, body []byte) importSummary {
	t.Helper()
	var summary importSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("decode import summary: %v (%s)", err, body)
	}
	return summary
}

// assertCanariesAbsent scans every management response body and every ABI
// crossing EXCEPT host.http.do (the outbound calls to the remote servers are
// required to carry the secrets) and asserts none of the canaries
// round-tripped.
func assertCanariesAbsent(t *testing.T, fake *importFakeCaller, bodies [][]byte, canaries ...string) {
	t.Helper()
	for _, body := range bodies {
		for _, canary := range canaries {
			if strings.Contains(string(body), canary) {
				t.Errorf("canary %q round-tripped into a management response: %s", canary, body)
			}
		}
	}
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostHTTPDo {
			continue
		}
		for _, canary := range canaries {
			if strings.Contains(string(record.request), canary) {
				t.Errorf("canary %q crossed the ABI in %s: %s", canary, record.method, record.request)
			}
		}
	}
}

// outboundHTTPRequest decodes one scripted host.http.do call.
func (f *importFakeCaller) outboundHTTPRequest(t *testing.T) hostHTTPRequest {
	t.Helper()
	for _, record := range f.calls {
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(record.request, &req); err != nil {
			t.Fatalf("decode host.http.do request: %v", err)
		}
		return req
	}
	t.Fatal("no host.http.do call was recorded")
	return hostHTTPRequest{}
}

// assertCapabilityManagementAPI pins the new capability on the registration
// envelope (TestRegisterEnvelope covers the full surface).
func TestRegisterEnvelopeManagementAPI(t *testing.T) {
	m := newTestManager(t)
	resp, err := m.Handle(pluginabi.MethodPluginRegister, lifecycleRequestBody(t, "{}"))
	if err != nil {
		t.Fatalf("Handle(plugin.register): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("register envelope not OK: %s", resp)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities.management_api must be true so the host registers the import routes")
	}
}

// TestManagementRegisterListsImportRoutes pins the management.register
// reply: exactly the five import routes plus the three workbench routes, by
// method + path + description — no handler (it cannot cross the ABI), no Menu
// on a management route (a Menu-tagged GET would be diverted to the
// unauthenticated resource surface) — and exactly the one workbench browser
// resource (the data-free HTML shell).
func TestManagementRegisterListsImportRoutes(t *testing.T) {
	m := newTestManager(t)
	body, _ := json.Marshal(managementRegistrationRequest{BasePath: "/v0/management"})
	resp, err := m.Handle(pluginabi.MethodManagementRegister, body)
	if err != nil {
		t.Fatalf("management.register surfaced a Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("management.register envelope not OK: %s", resp)
	}
	var result managementRegistrationResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode registration result: %v (%s)", err, env.Result)
	}

	want := map[string]string{
		importRouteCpaFile:         http.MethodPost,
		importRouteCpaRemoteFiles:  http.MethodGet,
		importRouteCpaRemote:       http.MethodPost,
		importRouteSub2apiAccounts: http.MethodGet,
		importRouteSub2api:         http.MethodPost,
		workbenchRouteGenerate:     http.MethodPost,
		workbenchRouteEdit:         http.MethodPost,
		workbenchRouteModels:       http.MethodGet,
	}
	if len(result.Routes) != len(want) {
		t.Fatalf("register reply lists %d routes, want exactly %d: %+v", len(result.Routes), len(want), result.Routes)
	}
	for _, route := range result.Routes {
		wantMethod, known := want[route.Path]
		if !known {
			t.Fatalf("register reply lists unexpected route path %q", route.Path)
		}
		if route.Method != wantMethod {
			t.Fatalf("route %q method = %q, want %q", route.Path, route.Method, wantMethod)
		}
		if route.Menu != "" {
			t.Fatalf("route %q carries a Menu label; GET routes with a Menu are diverted to the resource surface", route.Path)
		}
		if route.Handler != nil {
			t.Fatalf("route %q carries a Handler; function pointers do not cross the ABI", route.Path)
		}
		if route.Description == "" {
			t.Fatalf("route %q has no description for the panel", route.Path)
		}
	}

	// The one browser resource: the workbench shell, relative under
	// /v0/resource/plugins/<pluginID>/ (the host resolves and authenticates
	// nothing for it — the data lives on the management routes).
	if len(result.Resources) != 1 {
		t.Fatalf("register reply lists %d resources, want exactly 1: %+v", len(result.Resources), result.Resources)
	}
	resource := result.Resources[0]
	if resource.Path != workbenchResourcePath {
		t.Fatalf("resource path = %q, want %q", resource.Path, workbenchResourcePath)
	}
	if resource.Menu != workbenchResourceMenu {
		t.Fatalf("resource Menu = %q, want %q", resource.Menu, workbenchResourceMenu)
	}
	if resource.Description == "" {
		t.Fatal("resource has no description for the panel")
	}
	if resource.Handler != nil {
		t.Fatal("resource carries a Handler; function pointers do not cross the ABI")
	}
}

// TestManagementHandleRouting pins the internal exact-path router: a known
// path + right method reaches the handler; an unknown path answers 404 with
// a typed body; a known path + wrong method answers 405.
func TestManagementHandleRouting(t *testing.T) {
	m := newAuthTestManager(t, newImportFakeCaller())

	out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
		[]byte(`{"access_token":"at-route"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("known path + right method answered %d, want 200: %s", out.StatusCode, out.Body)
	}
	if contentType := out.Headers.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("management response content type = %q", contentType)
	}

	out = handleManagement(t, m, http.MethodGet, "chatgpt-proxy/import/no-such-route", []byte(`{}`))
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_not_found" {
		t.Fatalf("unknown path body is not the typed 404: %s", out.Body)
	}

	out = handleManagement(t, m, http.MethodGet, importRouteCpaFile, nil)
	if out.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("known path + wrong method answered %d, want 405: %s", out.StatusCode, out.Body)
	}
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_method_not_allowed" {
		t.Fatalf("wrong-method body is not the typed 405: %s", out.Body)
	}

	// The host sends the full path under /v0/management/ (every check above
	//); the relative form must route too, since the table keys are relative.
	resp, err := m.Handle(pluginabi.MethodManagementHandle,
		managementHandleBody(http.MethodPost, importRouteCpaFile, []byte(`{`)))
	if err != nil {
		t.Fatalf("management.handle surfaced a Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("management.handle envelope not OK: %s", resp)
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("relative path did not route to the handler: %d %s", out.StatusCode, out.Body)
	}
}

// TestImportCpaFile pins the inline CPA codex-export import: one credential
// saved through host.auth.save with the login-shaped record, the stable-hash
// name (never the email), source_type "codex", and an auth.parse
// round-trip; refusals save nothing.
func TestImportCpaFile(t *testing.T) {
	t.Run("saves one codex credential", func(t *testing.T) {
		fake := newImportFakeCaller()
		m := newAuthTestManager(t, fake)

		export := []byte(`{"access_token":"at-cpa-1","refresh_token":"rt-cpa-1","id_token":"idt-cpa-1"}`)
		out := handleManagement(t, m, http.MethodPost, importRouteCpaFile, export)
		if out.StatusCode != http.StatusOK {
			t.Fatalf("import answered %d: %s", out.StatusCode, out.Body)
		}
		summary := mustImportSummary(t, out.Body)
		if summary.Added != 1 || summary.Skipped != 0 || len(summary.Errors) != 0 {
			t.Fatalf("summary = %+v, want one added", summary)
		}

		if len(fake.savedNames()) != 1 {
			t.Fatalf("store holds %d files, want 1: %v", len(fake.savedNames()), fake.savedNames())
		}
		name := fake.savedNames()[0]
		if !strings.HasPrefix(name, providerKey+"-") || !strings.HasSuffix(name, ".json") {
			t.Fatalf("saved file name %q is not the stable-hash form", name)
		}
		if strings.Contains(strings.ToLower(name), "example") {
			t.Fatalf("saved file name must never carry the email: %q", name)
		}

		stored := fake.store[name]
		cred, err := FromStorageJSON(stored)
		if err != nil {
			t.Fatalf("stored payload is not a credential record: %v (%s)", err, stored)
		}
		if cred.AccessToken != "at-cpa-1" || cred.RefreshToken != "rt-cpa-1" || cred.IDToken != "idt-cpa-1" {
			t.Fatalf("stored tokens = %+v", cred)
		}
		if cred.SourceType != importSourceType {
			t.Fatalf("source_type = %q, want %q", cred.SourceType, importSourceType)
		}

		// auth.parse round-trip: the host reloads this file and must claim
		// it, normalized, unchanged.
		parseResp, err := m.Handle(pluginabi.MethodAuthParse,
			authParseRequestBody(t, "", name, stored))
		if err != nil {
			t.Fatalf("auth.parse: %v", err)
		}
		env := decodeEnv(t, parseResp)
		if !env.OK {
			t.Fatalf("auth.parse envelope not OK: %s", parseResp)
		}
		var parsed pluginapi.AuthParseResponse
		if err := json.Unmarshal(env.Result, &parsed); err != nil || !parsed.Handled {
			t.Fatalf("auth.parse did not claim the imported credential: %s", parseResp)
		}
		if parsed.Auth.FileName != name {
			t.Fatalf("auth.parse renamed the file: %q", parsed.Auth.FileName)
		}
	})

	t.Run("nested tokens block is captured", func(t *testing.T) {
		fake := newImportFakeCaller()
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
			[]byte(`{"tokens":{"access_token":"at-nested","refresh_token":"rt-nested"}}`))
		if out.StatusCode != http.StatusOK {
			t.Fatalf("import answered %d: %s", out.StatusCode, out.Body)
		}
		name := fake.savedNames()[0]
		cred, err := FromStorageJSON(fake.store[name])
		if err != nil {
			t.Fatalf("stored payload: %v", err)
		}
		if cred.AccessToken != "at-nested" || cred.RefreshToken != "rt-nested" {
			t.Fatalf("nested tokens not captured: %+v", cred)
		}
	})

	t.Run("missing access token refuses typed, saves nothing", func(t *testing.T) {
		fake := newImportFakeCaller()
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
			[]byte(`{"refresh_token":"rt-only"}`))
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil ||
			errBody.Error.Code != errImportPayloadInvalid {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
		if len(fake.savedNames()) != 0 {
			t.Fatalf("a refusal must save nothing: %v", fake.savedNames())
		}
	})

	t.Run("malformed body refuses typed", func(t *testing.T) {
		fake := newImportFakeCaller()
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteCpaFile, []byte(`{`))
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil ||
			errBody.Error.Code != errImportInvalidRequest {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
	})
}

// TestImportCpaRemoteFiles pins the CPA remote file list: the GET to
// /v0/management/auth-files with the Bearer secret header, the
// {files:[...]} decode with the email|account fallback, and the typed
// refusals.
func TestImportCpaRemoteFiles(t *testing.T) {
	cpaConfigBody := []byte(`{"base_url":"http://cpa.local/","secret_key":"cpa-secret-key"}`)

	t.Run("lists files", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusOK, Body: []byte(`{"files":[
			{"name":"codex-a.json","email":"a@example.com"},
			{"name":"codex-b.json","account":"b@example.com"},
			{"name":"   "},
			{"email":"no-name@example.com"}
		]}`)}}
		m := newAuthTestManager(t, fake)

		out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfigBody)
		if out.StatusCode != http.StatusOK {
			t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
		}
		var list struct {
			Files []cpaRemoteFile `json:"files"`
		}
		if err := json.Unmarshal(out.Body, &list); err != nil {
			t.Fatalf("decode list: %v (%s)", err, out.Body)
		}
		if len(list.Files) != 2 {
			t.Fatalf("listed %d files, want 2: %+v", len(list.Files), list.Files)
		}
		if list.Files[0].Name != "codex-a.json" || list.Files[0].Email != "a@example.com" {
			t.Fatalf("file 0 = %+v", list.Files[0])
		}
		if list.Files[1].Name != "codex-b.json" || list.Files[1].Email != "b@example.com" {
			t.Fatalf("account key must backfill the email: %+v", list.Files[1])
		}

		req := fake.outboundHTTPRequest(t)
		if req.Method != http.MethodGet {
			t.Fatalf("outbound method = %q, want GET", req.Method)
		}
		if req.URL != "http://cpa.local/v0/management/auth-files" {
			t.Fatalf("outbound URL = %q (base_url trailing slash must be trimmed)", req.URL)
		}
		if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer cpa-secret-key" {
			t.Fatalf("outbound Authorization header = %v", got)
		}
	})

	t.Run("secret rejection is typed unauthorized", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusUnauthorized, Body: []byte(`{"detail":"no"}`)}}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfigBody)
		if out.StatusCode != http.StatusUnauthorized {
			t.Fatalf("answered %d, want 401: %s", out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errImportUnauthorized {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
	})

	t.Run("refusal never echoes the upstream body", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusUnauthorized,
			Body: []byte(`{"detail":"the secret was " + "echo-canary-xyz"}`)}}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfigBody)
		if strings.Contains(string(out.Body), "echo-canary-xyz") {
			t.Fatalf("the refusal echoed the upstream body: %s", out.Body)
		}
	})

	t.Run("invalid payload is typed", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusOK, Body: []byte(`{"no_files":true}`)}}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfigBody)
		if out.StatusCode != http.StatusBadGateway {
			t.Fatalf("answered %d, want 502: %s", out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errImportPayloadInvalid {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
	})

	t.Run("missing config refuses typed", func(t *testing.T) {
		fake := newImportFakeCaller()
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles,
			[]byte(`{"base_url":"http://cpa.local"}`))
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
		}
	})
}

// TestImportCpaRemote pins the CPA remote batch import: one download per
// selected file (?name=<escaped>), the full token-set capture, dedupe
// against the store AND the batch, and per-file errors that never abort the
// rest.
func TestImportCpaRemote(t *testing.T) {
	fake := newImportFakeCaller()
	// The store already knows at-existing.
	fake.seedCredential(Credential{AccessToken: "at-existing", SourceType: importSourceType})
	fake.httpResults = []hostHTTPResponse{
		{StatusCode: http.StatusOK, Body: []byte(`{"access_token":"at-existing"}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"access_token":"at-new","refresh_token":"rt-new"}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"tokens":{"access_token":"at-new","id_token":"idt-new"}}`)},
		{StatusCode: http.StatusNotFound, Body: []byte(`{"detail":"missing"}`)},
	}
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, importRouteCpaRemote, []byte(`{
		"base_url":"http://cpa.local",
		"secret_key":"cpa-secret-key",
		"files":["f1.json","f2.json","f3.json","f4.json","  ","f1.json"]
	}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
	}
	summary := mustImportSummary(t, out.Body)
	if summary.Added != 1 || summary.Skipped != 2 || len(summary.Errors) != 1 {
		t.Fatalf("summary = %+v, want added 1, skipped 2, one error", summary)
	}
	if summary.Errors[0].Name != "f4.json" || summary.Errors[0].Message == "" {
		t.Fatalf("error entry = %+v", summary.Errors[0])
	}

	// The good file was saved; the bad one did not abort it.
	names := fake.savedNames()
	if len(names) != 2 {
		t.Fatalf("store holds %d files, want the seeded one + the import: %v", len(names), names)
	}
	for _, name := range names {
		if name == fake.storeOrder[0] {
			continue
		}
		cred, err := FromStorageJSON(fake.store[name])
		if err != nil || cred.AccessToken != "at-new" || cred.RefreshToken != "rt-new" {
			t.Fatalf("imported credential = %+v (err %v); the full token set must be captured", cred, err)
		}
	}

	// Duplicate names are de-duped in the request; blank ones dropped.
	downloads := 0
	for _, record := range fake.calls {
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(record.request, &req); err != nil {
			t.Fatalf("decode outbound: %v", err)
		}
		if strings.Contains(req.URL, "/auth-files/download?name=") {
			downloads++
			if !strings.Contains(req.URL, "name=f1.json") && !strings.Contains(req.URL, "name=f2.json") &&
				!strings.Contains(req.URL, "name=f3.json") && !strings.Contains(req.URL, "name=f4.json") {
				t.Fatalf("unexpected download URL %q", req.URL)
			}
		}
	}
	if downloads != 4 {
		t.Fatalf("%d downloads issued, want 4 (f1 deduped in the request): %v", downloads, fake.calls)
	}

	// Missing files refuses typed before any remote call.
	m2 := newAuthTestManager(t, newImportFakeCaller())
	out2 := handleManagement(t, m2, http.MethodPost, importRouteCpaRemote,
		[]byte(`{"base_url":"http://cpa.local","secret_key":"s","files":[]}`))
	if out2.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty files answered %d, want 400: %s", out2.StatusCode, out2.Body)
	}
}

// TestImportSub2apiAccounts pins the sub2api account list: the login leg
// (POST /api/v1/auth/login, envelope unwrapped, the JWT captured only for
// the outbound headers), the paged GET /api/v1/admin/accounts with the
// reference params, and the flat list with has_refresh_token.
func TestImportSub2apiAccounts(t *testing.T) {
	const jwt = "sub2api-jwt-token"
	body := []byte(`{"base_url":"http://sub2api.local/","email":"admin@example.com","password":"sub2api-password"}`)

	fake := newImportFakeCaller()
	fake.httpResults = []hostHTTPResponse{
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"message":"ok","data":{"access_token":"` + jwt + `","expires_in":86400}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"items":[
			{"id":"acc-1","name":"One","status":"active","credentials":{"email":"one@example.com","plan_type":"plus","refresh_token":"rt-1","expires_at":"2026-12-01T00:00:00Z"}},
			{"id":"acc-2","name":"Two","credentials":{"email":"two@example.com"}}
		],"total":2}}`)},
	}
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodGet, importRouteSub2apiAccounts, body)
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
	}
	var list struct {
		Accounts []sub2apiAccount `json:"accounts"`
	}
	if err := json.Unmarshal(out.Body, &list); err != nil {
		t.Fatalf("decode accounts: %v (%s)", err, out.Body)
	}
	if len(list.Accounts) != 2 {
		t.Fatalf("listed %d accounts, want 2: %+v", len(list.Accounts), list.Accounts)
	}
	if !list.Accounts[0].HasRefreshToken || list.Accounts[1].HasRefreshToken {
		t.Fatalf("has_refresh_token flags wrong: %+v", list.Accounts)
	}
	if list.Accounts[0].ID != "acc-1" || list.Accounts[0].Email != "one@example.com" ||
		list.Accounts[0].PlanType != "plus" {
		t.Fatalf("account 0 = %+v", list.Accounts[0])
	}

	// Login leg: POST with the credential body.
	var loginReq, listReq hostHTTPRequest
	for _, record := range fake.calls {
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(record.request, &req); err != nil {
			t.Fatalf("decode outbound: %v", err)
		}
		switch {
		case strings.HasSuffix(req.URL, "/api/v1/auth/login"):
			if req.Method != http.MethodPost {
				t.Fatalf("login method = %q, want POST", req.Method)
			}
			loginReq = req
		case strings.HasPrefix(req.URL, "http://sub2api.local/api/v1/admin/accounts?"):
			listReq = req
		}
	}
	if loginReq.URL == "" || listReq.URL == "" {
		t.Fatalf("missing scripted legs: login=%q list=%q", loginReq.URL, listReq.URL)
	}
	var loginBody map[string]string
	if err := json.Unmarshal(loginReq.Body, &loginBody); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if loginBody["email"] != "admin@example.com" || loginBody["password"] != "sub2api-password" {
		t.Fatalf("login body = %+v", loginBody)
	}
	if got := loginReq.Headers["Authorization"]; len(got) != 0 {
		t.Fatalf("the login leg must not carry an Authorization header: %v", got)
	}
	if got := listReq.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer "+jwt {
		t.Fatalf("admin list Authorization header = %v", got)
	}
	if listReq.Method != http.MethodGet {
		t.Fatalf("admin list method = %q, want GET", listReq.Method)
	}
	query := listReq.URL[strings.IndexByte(listReq.URL, '?')+1:]
	for _, wantParam := range []string{"platform=openai", "type=oauth", "page=1", "page_size=200"} {
		if !strings.Contains(query, wantParam) {
			t.Fatalf("admin list query %q misses %q", query, wantParam)
		}
	}

	// The JWT never round-trips into a response or a saved file.
	assertCanariesAbsent(t, fake, [][]byte{out.Body}, jwt)
	if strings.Contains(string(out.Body), jwt) {
		t.Fatalf("the JWT leaked into the response: %s", out.Body)
	}
}

// TestImportSub2api pins the sub2api batch import: login once, GET
// /api/v1/admin/accounts/data with the reference query, the token-key
// fallback chain (access_token / accessToken / token), per-account errors,
// the exported-less-than-requested entry, and the wrong-password refusal.
func TestImportSub2api(t *testing.T) {
	body := []byte(`{"base_url":"http://sub2api.local","email":"admin@example.com","password":"sub2api-password","accounts":["acc-1","acc-2","acc-3"]}`)

	t.Run("imports accounts", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{
			{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"sub2api-jwt"}}`)},
			{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"accounts":[
				{"id":"acc-1","name":"One","credentials":{"accessToken":"at-s2-1","refresh_token":"rt-s2-1","email":"one@example.com"}},
				{"id":"acc-2","name":"Two","credentials":{"token":"at-s2-2"}},
				{"id":"acc-3","name":"Three","credentials":{}}
			]}}`)},
		}
		// Three ids requested, three accounts exported, one of which
		// (acc-3) carries no token — so the summary must hold exactly the
		// per-account error. (The exported-less-than-requested entry has
		// its own scenario below.)
		m := newAuthTestManager(t, fake)

		out := handleManagement(t, m, http.MethodPost, importRouteSub2api, body)
		if out.StatusCode != http.StatusOK {
			t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
		}
		summary := mustImportSummary(t, out.Body)
		if summary.Added != 2 || summary.Skipped != 0 || len(summary.Errors) != 1 {
			t.Fatalf("summary = %+v, want added 2 and the one per-account error", summary)
		}
		if summary.Errors[0].Name != "acc-3" || summary.Errors[0].Message != "missing access token" {
			t.Fatalf("per-account error entry = %+v", summary.Errors[0])
		}

		// The accessToken and token fallbacks were captured.
		tokens := map[string]bool{}
		for _, name := range fake.savedNames() {
			cred, err := FromStorageJSON(fake.store[name])
			if err != nil {
				t.Fatalf("stored payload: %v", err)
			}
			tokens[cred.AccessToken] = true
		}
		if !tokens["at-s2-1"] || !tokens["at-s2-2"] {
			t.Fatalf("fallback chain failed: %v", tokens)
		}

		// The data leg: GET with the reference query shape.
		var dataReq hostHTTPRequest
		for _, record := range fake.calls {
			if record.method != pluginabi.MethodHostHTTPDo {
				continue
			}
			var req hostHTTPRequest
			if err := json.Unmarshal(record.request, &req); err != nil {
				t.Fatalf("decode outbound: %v", err)
			}
			if strings.Contains(req.URL, "/api/v1/admin/accounts/data?") {
				dataReq = req
			}
		}
		if dataReq.URL == "" {
			t.Fatalf("the accounts/data leg was never issued")
		}
		if dataReq.Method != http.MethodGet {
			t.Fatalf("accounts/data method = %q, want GET (the reference shape)", dataReq.Method)
		}
		query := dataReq.URL[strings.IndexByte(dataReq.URL, '?')+1:]
		if !strings.Contains(query, "ids=acc-1%2Cacc-2%2Cacc-3") || !strings.Contains(query, "timezone=Asia%2FShanghai") {
			t.Fatalf("accounts/data query = %q", query)
		}
		if got := dataReq.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer sub2api-jwt" {
			t.Fatalf("accounts/data Authorization header = %v", got)
		}
	})

	t.Run("export fewer than requested records the shortfall", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{
			{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"sub2api-jwt"}}`)},
			{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"accounts":[
				{"id":"acc-1","credentials":{"access_token":"at-short"}}
			]}}`)},
		}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteSub2api, body)
		if out.StatusCode != http.StatusOK {
			t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
		}
		summary := mustImportSummary(t, out.Body)
		if summary.Added != 1 || len(summary.Errors) != 1 {
			t.Fatalf("summary = %+v, want added 1 and the shortfall error", summary)
		}
		if summary.Errors[0].Name != "acc-1,acc-2,acc-3" || summary.Errors[0].Message != "exported 1/3 accounts" {
			t.Fatalf("shortfall entry = %+v", summary.Errors[0])
		}
	})

	t.Run("wrong password refuses typed and saves nothing", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{
			{StatusCode: http.StatusUnauthorized, Body: []byte(`{"code":1,"message":"bad credentials"}`)},
		}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteSub2api, body)
		if out.StatusCode != http.StatusUnauthorized {
			t.Fatalf("answered %d, want 401: %s", out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errImportUnauthorized {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
		if len(fake.savedNames()) != 0 {
			t.Fatalf("a login refusal must save nothing: %v", fake.savedNames())
		}
	})

	t.Run("export refusal records every requested id", func(t *testing.T) {
		fake := newImportFakeCaller()
		fake.httpResults = []hostHTTPResponse{
			{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"sub2api-jwt"}}`)},
			{StatusCode: http.StatusForbidden, Body: []byte(`{"code":1,"message":"no access"}`)},
		}
		m := newAuthTestManager(t, fake)
		out := handleManagement(t, m, http.MethodPost, importRouteSub2api, body)
		if out.StatusCode != http.StatusOK {
			t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
		}
		summary := mustImportSummary(t, out.Body)
		if summary.Added != 0 || len(summary.Errors) != 3 {
			t.Fatalf("summary = %+v, want three per-id errors", summary)
		}
		for _, e := range summary.Errors {
			if e.Message != "the remote server rejected the credentials" {
				t.Fatalf("error %q carries non-fixed text: %+v", e.Name, e)
			}
		}
	})

	t.Run("empty accounts refuses typed", func(t *testing.T) {
		m := newAuthTestManager(t, newImportFakeCaller())
		out := handleManagement(t, m, http.MethodPost, importRouteSub2api,
			[]byte(`{"base_url":"http://sub2api.local","email":"a","password":"b","accounts":[]}`))
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
		}
	})
}

// TestImportDedupeSkipsKnownTokens pins the dedupe contract: an import whose
// access_token is already in the host store answers skipped — not added,
// not an error — and issues no save.
func TestImportDedupeSkipsKnownTokens(t *testing.T) {
	fake := newImportFakeCaller()
	seeded := fake.seedCredential(Credential{AccessToken: "at-existing", SourceType: importSourceType})
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
		[]byte(`{"access_token":"at-existing","refresh_token":"rt-different"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", out.StatusCode, out.Body)
	}
	summary := mustImportSummary(t, out.Body)
	if summary.Added != 0 || summary.Skipped != 1 || len(summary.Errors) != 0 {
		t.Fatalf("summary = %+v, want skipped 1", summary)
	}
	// The seeded record is untouched.
	if !bytes.Equal(fake.store[seeded], Credential{AccessToken: "at-existing", SourceType: importSourceType}.StorageJSON()) {
		t.Fatalf("a skipped import must not rewrite the existing record")
	}
	saves := 0
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostAuthSave {
			saves++
		}
	}
	if saves != 0 {
		t.Fatalf("%d saves issued for a deduped import", saves)
	}
}

// TestImportStoreUnavailable pins that a host credential-store failure is a
// typed retryable refusal, never a silent save.
func TestImportStoreUnavailable(t *testing.T) {
	fake := newImportFakeCaller()
	fake.listError = "store-denied"
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
		[]byte(`{"access_token":"at-1"}`))
	if out.StatusCode != http.StatusInternalServerError {
		t.Fatalf("answered %d, want 500: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errImportSaveFailed {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
	if !errBody.Error.Retryable {
		t.Fatalf("a store outage is retryable: %s", out.Body)
	}
	if len(fake.savedNames()) != 0 {
		t.Fatalf("nothing may be saved when the store is unavailable")
	}
}

// TestImportSaveRefusalPerFile pins that a refused save lands as a per-file
// error entry (fixed text) and the batch still answers 200 for the rest.
func TestImportSaveRefusalPerFile(t *testing.T) {
	fake := newImportFakeCaller()
	fake.saveError = "disk-full"
	m := newAuthTestManager(t, fake)

	out := handleManagement(t, m, http.MethodPost, importRouteCpaFile,
		[]byte(`{"access_token":"at-1"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("answered %d, want 200 with the error entry: %s", out.StatusCode, out.Body)
	}
	summary := mustImportSummary(t, out.Body)
	if summary.Added != 0 || len(summary.Errors) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Errors[0].Message != "the host refused to save the credential" {
		t.Fatalf("error entry carries non-fixed text: %+v", summary.Errors[0])
	}
}

// TestImportSecretHygiene plants canaries in every secret field and asserts
// none round-trip into a management response, an error body, a saved
// credential or any non-http ABI crossing — the secrets may appear only in
// the required outbound Authorization headers / the login body.
func TestImportSecretHygiene(t *testing.T) {
	const secretKey = "CANARY-CPA-SECRET-9f8e7d"
	const password = "CANARY-SUB2API-PASSWORD-1a2b3c"
	const jwt = "CANARY-SUB2API-JWT-4d5e6f"

	bodies := [][]byte{}
	fake := newImportFakeCaller()
	m := newAuthTestManager(t, fake)

	// CPA legs (success + a typed refusal).
	fake.httpResults = []hostHTTPResponse{
		{StatusCode: http.StatusOK, Body: []byte(`{"files":[{"name":"a.json","email":"a@example.com"}]}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"access_token":"at-hygiene","refresh_token":"rt-hygiene"}`)},
		{StatusCode: http.StatusUnauthorized, Body: []byte(`{"detail":"` + secretKey + ` must not echo"}`)},
	}
	out := handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles,
		[]byte(`{"base_url":"http://cpa.local","secret_key":"`+secretKey+`"}`))
	bodies = append(bodies, out.Body)
	out = handleManagement(t, m, http.MethodPost, importRouteCpaRemote,
		[]byte(`{"base_url":"http://cpa.local","secret_key":"`+secretKey+`","files":["a.json"]}`))
	bodies = append(bodies, out.Body)
	out = handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles,
		[]byte(`{"base_url":"http://cpa.local","secret_key":"`+secretKey+`"}`))
	bodies = append(bodies, out.Body)
	if out.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the scripted 401, got %d: %s", out.StatusCode, out.Body)
	}

	// sub2api legs (list, import, wrong password).
	fake.httpResults = []hostHTTPResponse{
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"` + jwt + `"}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"items":[],"total":0}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"access_token":"` + jwt + `"}}`)},
		{StatusCode: http.StatusOK, Body: []byte(`{"code":0,"data":{"accounts":[
			{"id":"acc-1","credentials":{"access_token":"at-hygiene-2"}}]}}`)},
		{StatusCode: http.StatusUnauthorized, Body: []byte(`{"code":1,"message":"` + password + ` must not echo"}`)},
	}
	out = handleManagement(t, m, http.MethodGet, importRouteSub2apiAccounts,
		[]byte(`{"base_url":"http://sub2api.local","email":"admin@example.com","password":"`+password+`"}`))
	bodies = append(bodies, out.Body)
	out = handleManagement(t, m, http.MethodPost, importRouteSub2api,
		[]byte(`{"base_url":"http://sub2api.local","email":"admin@example.com","password":"`+password+`","accounts":["acc-1"]}`))
	bodies = append(bodies, out.Body)
	out = handleManagement(t, m, http.MethodPost, importRouteSub2api,
		[]byte(`{"base_url":"http://sub2api.local","email":"admin@example.com","password":"`+password+`","accounts":["acc-1"]}`))
	bodies = append(bodies, out.Body)

	// Saved credentials carry tokens + source_type only.
	for _, raw := range fake.store {
		bodies = append(bodies, raw)
	}

	assertCanariesAbsent(t, fake, bodies, secretKey, password, jwt)

	// Positive side: the outbound carried the secrets exactly where required.
	for _, record := range fake.calls {
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req hostHTTPRequest
		if err := json.Unmarshal(record.request, &req); err != nil {
			t.Fatalf("decode outbound: %v", err)
		}
		if strings.Contains(req.URL, "cpa.local") {
			if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer "+secretKey {
				t.Fatalf("CPA outbound Authorization header = %v", got)
			}
		}
	}
}

// TestSub2apiEnvelopeHelpers pins the faithful port of the reference's
// envelope unwrap + paged-item extraction (sub2api_service.py
// _unwrap_envelope / _extract_paged_items).
func TestSub2apiEnvelopeHelpers(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		items   int
		total   int
	}{
		{"bare list", []any{"a", "b"}, 2, 2},
		{"items key", map[string]any{"items": []any{"a"}, "total": float64(7)}, 1, 7},
		{"data key list", map[string]any{"data": []any{"a", "b"}}, 2, 2},
		{"list key", map[string]any{"list": []any{"a"}}, 1, 1},
		{"envelope-wrapped", map[string]any{"code": float64(0), "data": map[string]any{"items": []any{"a", "b"}, "total": float64(5)}}, 2, 5},
		{"data-without-code passes through", map[string]any{"data": "plain"}, 0, 0},
		{"no items", map[string]any{"other": true}, 0, 0},
		{"nil", nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total := extractSub2apiPagedItems(tc.payload)
			if len(items) != tc.items || total != tc.total {
				t.Fatalf("got (%d items, total %d), want (%d, %d)", len(items), total, tc.items, tc.total)
			}
		})
	}

	// The unwrap: {code, data} → data; anything else passes through.
	if got := unwrapSub2apiEnvelope(map[string]any{"code": float64(0), "data": "inner"}); got != "inner" {
		t.Fatalf("unwrap = %v, want inner", got)
	}
	passThrough := map[string]any{"data": "inner"}
	// Interface comparison of maps panics, so identity is checked through
	// reflect: a pass-through must be the very same value, not an unwrapped
	// copy.
	if got := unwrapSub2apiEnvelope(passThrough); reflect.ValueOf(got).Pointer() != reflect.ValueOf(passThrough).Pointer() {
		t.Fatalf("data-without-code must pass through unchanged: %v", got)
	}
	if got := unwrapSub2apiEnvelope(nil); got != nil {
		t.Fatalf("nil payload must stay nil: %v", got)
	}
}
