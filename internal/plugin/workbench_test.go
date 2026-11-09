package plugin

// workbench_test.go pins F6: the drawing workbench. The registration reply
// (routes + the one resource) is pinned by TestManagementRegisterListsImportRoutes
// (import_test.go); this file pins the dispatch — the GET resource path
// served through management.handle (the host builds the request itself for
// resources, full path, no body), the credential acquisition for the
// management context (host.auth.list/get, the import's knownAccessTokens
// pattern), the synchronous generate/edit turns onto the existing image
// pipeline, the models route, the n validation, routing refusals, and the
// data-free shell.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// handleWorkbenchResource drives one management.handle round the way the
// host dispatches a browser resource: GET, the FULL resource path, no body
// (the host's ServeResourceHTTP builds the ManagementRequest itself).
func handleWorkbenchResource(t *testing.T, m *Manager, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	raw, _ := json.Marshal(managementHandleRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Body: body},
		HostCallbackID:    "cb-workbench-test",
	})
	resp, err := m.Handle(pluginabi.MethodManagementHandle, raw)
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

// workbenchStub builds a registered manager with a scripted store and the
// image pipeline seams stubbed to the stub.
func workbenchManager(t *testing.T, fake *importFakeCaller, s *imgStub) *Manager {
	t.Helper()
	stubImageSeams(t, s)
	return execManager(t, fake, "")
}

// TestWorkbenchResourceServesShell pins the resource dispatch: the GET
// answer is the pinned shell bytes, text/html, 200.
func TestWorkbenchResourceServesShell(t *testing.T) {
	m := workbenchManager(t, newImportFakeCaller(), &imgStub{})

	out := handleWorkbenchResource(t, m, http.MethodGet, workbenchResourceFullPath, nil)
	if out.StatusCode != http.StatusOK {
		t.Fatalf("resource answered %d, want 200", out.StatusCode)
	}
	if ct := out.Headers.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("resource Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	if !bytes.Equal(out.Body, workbenchShellBody) {
		t.Fatalf("the served body is not the pinned shell bytes (%d vs %d bytes)", len(out.Body), len(workbenchShellBody))
	}
	if !bytes.HasPrefix(out.Body, []byte("<!doctype html")) {
		t.Fatalf("shell does not start with an HTML doctype: %q", out.Body[:min(32, len(out.Body))])
	}
}

// TestWorkbenchResourceRouting pins the wrong-method / unknown-path refusals
// on the resource path (the host's own lookup filters both before
// dispatching; these are the offline pin).
func TestWorkbenchResourceRouting(t *testing.T) {
	m := workbenchManager(t, newImportFakeCaller(), &imgStub{})

	out := handleWorkbenchResource(t, m, http.MethodPost, workbenchResourceFullPath, nil)
	if out.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST on the resource path answered %d, want 405: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_method_not_allowed" {
		t.Fatalf("wrong-method body is not the typed 405: %s", out.Body)
	}

	out = handleWorkbenchResource(t, m, http.MethodGet, "/v0/resource/plugins/"+pluginID+"/chatgpt-proxy/no-such-resource", nil)
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resource path answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_not_found" {
		t.Fatalf("unknown-resource body is not the typed 404: %s", out.Body)
	}
}

// TestWorkbenchGenerateRunsPipeline pins the synchronous generate turn: the
// body fields reach the pipeline unchanged, the credential is acquired from
// the host store (the management context has no host-injected record), and
// the reply is the pinned images envelope.
func TestWorkbenchGenerateRunsPipeline(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-workbench", Email: "wb@example.com", SourceType: "codex"})
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a lighthouse"},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse","size":"1024x1024","quality":"high","n":2}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("generate answered %d: %s", out.StatusCode, out.Body)
	}
	if ct := out.Headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("generate Content-Type = %q, want application/json", ct)
	}

	var ir imageResponse
	if err := json.Unmarshal(out.Body, &ir); err != nil {
		t.Fatalf("reply is not the images envelope: %v (%s)", err, out.Body)
	}
	if ir.Created <= 0 {
		t.Fatalf("created = %d, want a unix timestamp", ir.Created)
	}
	if len(ir.Data) != 2 {
		t.Fatalf("data = %d, want 2 (n is a sequential loop)", len(ir.Data))
	}
	assertPNGInEntry(t, ir.Data[0].B64JSON, png)
	if ir.Usage.TotalTokens <= 0 {
		t.Fatalf("usage = %+v, want a documented estimate", ir.Usage)
	}

	// The pipeline ran with the store-acquired credential, on the drawing
	// route (the default), with the body's fields and NO references.
	if s.lastCred.AccessToken != "at-workbench" {
		t.Fatalf("pipeline credential = %+v, want the store-acquired record", s.lastCred)
	}
	if s.generateCalls != 2 || s.codexCalls != 0 {
		t.Fatalf("route calls = {generate:%d codex:%d}, want 2 drawing runs", s.generateCalls, s.codexCalls)
	}
	if s.acquireCalls != 1 {
		t.Fatalf("acquireCalls = %d, want 1 sentinel bundle for the request", s.acquireCalls)
	}
	if s.lastPrompt != "a lighthouse" || s.lastSize != "1024x1024" || s.lastQuality != "high" {
		t.Fatalf("pipeline args = {prompt:%q size:%q quality:%q}", s.lastPrompt, s.lastSize, s.lastQuality)
	}
	if len(s.lastRefs) != 0 {
		t.Fatalf("generate carries no references, want none: %v", s.lastRefs)
	}
	// A successful turn persists nothing (no refresh happened).
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostAuthSave {
			t.Fatal("a successful workbench turn must not save a credential")
		}
	}
}

// TestWorkbenchGenerateCodexModel pins the model routing: the published codex
// id selects the codex pipeline (no sentinel), with the stored plan.
func TestWorkbenchGenerateCodexModel(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-codex", Plan: "pro", SourceType: "codex"})
	png := solidPNG()
	s := &imgStub{
		codexResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a codex cat"},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a cat","model":"chatgpt-proxy/codex-gpt-image-2"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("generate answered %d: %s", out.StatusCode, out.Body)
	}
	if s.codexCalls != 1 || s.generateCalls != 0 || s.acquireCalls != 0 {
		t.Fatalf("route calls = {codex:%d generate:%d acquire:%d}, want exactly the codex route", s.codexCalls, s.generateCalls, s.acquireCalls)
	}
	if s.lastProfile.Plan != "pro" {
		t.Fatalf("codex profile plan = %q, want the stored credential plan", s.lastProfile.Plan)
	}
	if s.lastCred.AccessToken != "at-codex" {
		t.Fatalf("pipeline credential = %+v, want the store-acquired record", s.lastCred)
	}
}

// TestWorkbenchGenerateModelUnknown pins the typed model refusal BEFORE any
// store or pipeline call (route refusals cost zero network).
func TestWorkbenchGenerateModelUnknown(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-model", SourceType: "codex"})
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse","model":"chatgpt-proxy/foreign-model"}`))
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "model_unknown" {
		t.Fatalf("body is not the typed model refusal: %s", out.Body)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a model refusal must not touch the host store: %v", fake.calls)
	}
	if s.generateCalls+s.codexCalls != 0 {
		t.Fatal("a model refusal must not run the pipeline")
	}
}

// TestWorkbenchGenerateNoCredential pins the empty-store refusal: typed,
// pointing at the import routes, with nothing upstream and nothing in the
// pipeline.
func TestWorkbenchGenerateNoCredential(t *testing.T) {
	fake := newImportFakeCaller() // no credentials at all
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse"}`))
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errWorkbenchCredentialMissing {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
	if !strings.Contains(errBody.Error.Message, "/import") {
		t.Fatalf("the message must point at the import routes: %q", errBody.Error.Message)
	}
	if s.generateCalls+s.codexCalls != 0 {
		t.Fatal("no pipeline run without a credential")
	}
}

// TestWorkbenchCredentialSelection pins the explicit credential name: the
// named record is fetched and the pipeline runs with it; a foreign name
// (not an own-provider entry) is a typed refusal.
func TestWorkbenchCredentialSelection(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-first", Email: "first@example.com", SourceType: "codex"})
	second := fake.seedCredential(Credential{AccessToken: "at-second", Email: "second@example.com", SourceType: "codex"})
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: solidPNG()}}},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse","credential":`+`"`+second+`"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("explicit credential answered %d: %s", out.StatusCode, out.Body)
	}
	if s.lastCred.AccessToken != "at-second" {
		t.Fatalf("pipeline credential = %+v, want the explicitly named record", s.lastCred)
	}

	// Default mode picked the FIRST own-provider entry (store order).
	s2 := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: solidPNG()}}},
	}
	stubImageSeams(t, s2)
	out = handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("default credential answered %d: %s", out.StatusCode, out.Body)
	}
	if s2.lastCred.AccessToken != "at-first" {
		t.Fatalf("default pipeline credential = %+v, want the first own-provider entry", s2.lastCred)
	}

	// A name that is not an own-provider credential is a typed refusal.
	out = handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse","credential":"openai-someone-else.json"}`))
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign credential answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != errWorkbenchCredentialNotFound {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
}

// TestAcquireWorkbenchCredential pins the management-context acquisition
// directly: list + get on the own-provider entries, the returned file name
// (the turn's authFileName — the refresh-retry persists there), and the
// store-leg refusals.
func TestAcquireWorkbenchCredential(t *testing.T) {
	fake := newImportFakeCaller()
	name := fake.seedCredential(Credential{AccessToken: "at-a", Email: "a@example.com", SourceType: "codex"})
	m := workbenchManager(t, fake, &imgStub{})

	fileName, cred, e := m.acquireWorkbenchCredential("")
	if e.code != "" {
		t.Fatalf("acquisition failed: %+v", e)
	}
	if fileName != name {
		t.Fatalf("file name = %q, want the seeded entry (the refresh-retry must persist there)", fileName)
	}
	if cred.AccessToken != "at-a" {
		t.Fatalf("credential = %+v, want the seeded record", cred)
	}

	// An explicit name fetches THAT record.
	_, cred2, e2 := m.acquireWorkbenchCredential(name)
	if e2.code != "" || cred2.AccessToken != "at-a" {
		t.Fatalf("explicit acquisition = (%+v, %+v)", cred2, e2)
	}

	// A disk-only entry (no runtime index) cannot be read: typed refusal,
	// never a blind pass-through.
	const diskOnly = "chatgpt-proxy-diskonly.json"
	fake.store[diskOnly] = Credential{AccessToken: "at-disk"}.StorageJSON()
	fake.storeOrder = append(fake.storeOrder, diskOnly)
	if fake.noIndex == nil {
		fake.noIndex = map[string]bool{}
	}
	fake.noIndex[diskOnly] = true
	_, _, e3 := m.acquireWorkbenchCredential(diskOnly)
	if e3.code != errWorkbenchStoreUnavailable {
		t.Fatalf("disk-only entry answered %+v, want the store refusal", e3)
	}

	// A store outage (list refused) is a typed retryable refusal.
	fake.listError = "store-denied"
	_, _, e4 := m.acquireWorkbenchCredential("")
	if e4.code != errWorkbenchStoreUnavailable || !e4.retryable {
		t.Fatalf("store outage answered %+v, want the retryable store refusal", e4)
	}
}

// TestWorkbenchEditDecodesReferences pins the edit turn: the data-URL
// references decode through the existing path (decodeReferenceImages +
// applyImageDims) and reach the pipeline as references.
func TestWorkbenchEditDecodesReferences(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-edit", SourceType: "codex"})
	png := solidPNG()
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "edit it"},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteEdit, []byte(
		`{"prompt":"edit it","size":"1536x1024","reference_images":[`+`"`+dataURL+`"]}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("edit answered %d: %s", out.StatusCode, out.Body)
	}
	if s.generateCalls != 1 {
		t.Fatalf("generateCalls = %d, want 1", s.generateCalls)
	}
	if len(s.lastRefs) != 1 {
		t.Fatalf("references = %d, want 1 decoded from reference_images", len(s.lastRefs))
	}
	ref := s.lastRefs[0]
	if !bytes.Equal(ref.Data, png) {
		t.Fatal("the data-URL was not decoded to the reference bytes")
	}
	if ref.Width != 2 || ref.Height != 2 {
		t.Fatalf("reference dims = %dx%d, want the decoded 2x2", ref.Width, ref.Height)
	}
	if ref.MimeType != "image/png" {
		t.Fatalf("reference mime = %q, want image/png", ref.MimeType)
	}
	if s.lastSize != "1536x1024" {
		t.Fatalf("pipeline size = %q, want the body field", s.lastSize)
	}
}

// TestWorkbenchEditRequiresReference pins the typed refusals: zero
// reference images (an edit needs at least one) and the 10-image cap (the
// existing decode path enforces it).
func TestWorkbenchEditRequiresReference(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-edit", SourceType: "codex"})
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteEdit,
		[]byte(`{"prompt":"edit it"}`))
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero references answered %d, want 400: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
	if s.generateCalls+s.codexCalls != 0 {
		t.Fatal("a reference refusal must not run the pipeline")
	}

	// The cap: 11 references refuse before any pipeline call (decode
	// bounds check).
	refs := make([]string, 11)
	for i := range refs {
		refs[i] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(solidPNG())
	}
	body, _ := json.Marshal(map[string]any{"prompt": "edit it", "reference_images": refs})
	out = handleManagement(t, m, http.MethodPost, workbenchRouteEdit, body)
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("11 references answered %d, want 400: %s", out.StatusCode, out.Body)
	}
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
}

// TestWorkbenchEditPromptRequired pins that the edit route refuses an empty
// prompt (the executor's edits contract: an edit always carries intent).
func TestWorkbenchEditPromptRequired(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-edit", SourceType: "codex"})
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(solidPNG())
	out := handleManagement(t, m, http.MethodPost, workbenchRouteEdit,
		[]byte(`{"reference_images":[`+`"`+dataURL+`"]}`))
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty prompt answered %d, want 400: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
		t.Fatalf("body is not the typed refusal: %s", out.Body)
	}
}

// TestWorkbenchModelsRoute pins the models route: the two published image
// ids, from the registered config.
func TestWorkbenchModelsRoute(t *testing.T) {
	m := workbenchManager(t, newImportFakeCaller(), &imgStub{})

	out := handleManagement(t, m, http.MethodGet, workbenchRouteModels, nil)
	if out.StatusCode != http.StatusOK {
		t.Fatalf("models answered %d: %s", out.StatusCode, out.Body)
	}
	var res struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(out.Body, &res); err != nil {
		t.Fatalf("decode models reply: %v (%s)", err, out.Body)
	}
	want := []string{
		"chatgpt-proxy/gpt-image-2-proxy",
		"chatgpt-proxy/codex-gpt-image-2",
	}
	if len(res.Models) != 2 || res.Models[0] != want[0] || res.Models[1] != want[1] {
		t.Fatalf("models = %v, want %v", res.Models, want)
	}
}

// TestWorkbenchModelsRouteCustomConfig pins that the route answers the
// PUBLISHED ids of the registered config, not hardcoded defaults.
func TestWorkbenchModelsRouteCustomConfig(t *testing.T) {
	fake := newImportFakeCaller()
	stub := &imgStub{}
	stubImageSeams(t, stub)
	m := execManager(t, fake, "model-prefix: custom-proxy\nimage-model-name: draw-x\ncodex-image-model-name: codex-x")

	out := handleManagement(t, m, http.MethodGet, workbenchRouteModels, nil)
	if out.StatusCode != http.StatusOK {
		t.Fatalf("models answered %d: %s", out.StatusCode, out.Body)
	}
	var res struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(out.Body, &res); err != nil {
		t.Fatalf("decode models reply: %v (%s)", err, out.Body)
	}
	if len(res.Models) != 2 || res.Models[0] != "custom-proxy/draw-x" || res.Models[1] != "custom-proxy/codex-x" {
		t.Fatalf("models = %v, want the config-published ids", res.Models)
	}
}

// TestWorkbenchRouting pins the management-route dispatch: known path +
// right method reaches the handler (relative and full forms), wrong method
// on a known path answers 405, unknown answers 404.
func TestWorkbenchRouting(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-route", SourceType: "codex"})
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	// Wrong methods on the workbench paths.
	for _, methodPath := range []struct{ method, path string }{
		{http.MethodGet, workbenchRouteGenerate},
		{http.MethodGet, workbenchRouteEdit},
		{http.MethodPost, workbenchRouteModels},
	} {
		out := handleManagement(t, m, methodPath.method, methodPath.path, []byte(`{}`))
		if out.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s answered %d, want 405: %s", methodPath.method, methodPath.path, out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_method_not_allowed" {
			t.Fatalf("wrong-method body is not the typed 405: %s", out.Body)
		}
	}

	// The full management path form routes too (the host sends it full-path).
	out := handleWorkbenchResource(t, m, http.MethodPost, "/v0/management/"+workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("full management path answered %d, want 200: %s", out.StatusCode, out.Body)
	}

	// An unknown path is the typed 404.
	out = handleManagement(t, m, http.MethodGet, "chatgpt-proxy/workbench/nope", nil)
	if out.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path answered %d, want 404: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "management_not_found" {
		t.Fatalf("unknown-path body is not the typed 404: %s", out.Body)
	}
}

// TestWorkbenchNValidation pins the executor's n contract on the workbench:
// n=5 and n=0 refuse typed before any pipeline call (validateImageCount is
// reused — the workbench count IS the executor's n, sequential in one
// attempt; NOT the reference web's 1..100-per-task).
func TestWorkbenchNValidation(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-n", SourceType: "codex"})
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	for _, n := range []any{float64(5), float64(0)} {
		out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
			mustJSON(t, map[string]any{"prompt": "a lighthouse", "n": n}))
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("n=%v answered %d, want 400: %s", n, out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
		if !strings.Contains(errBody.Error.Message, "1 and 4") {
			t.Fatalf("message %q is not the executor's n contract", errBody.Error.Message)
		}
	}
	if s.generateCalls+s.codexCalls != 0 {
		t.Fatal("n refusals must not run the pipeline")
	}
}

// TestWorkbenchPipelineErrorMapping pins that a pipeline failure (a
// content-policy refusal from the stub) surfaces as the SAME typed code +
// status on the management surface — the executor's error contract, not a
// new vocabulary.
func TestWorkbenchPipelineErrorMapping(t *testing.T) {
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "at-policy", SourceType: "codex"})
	s := &imgStub{
		tokens: upstream.SentinelTokens{ChatRequirements: "req-1"},
		genErr: &upstream.ImageError{Code: upstream.ImageCodeContentPolicy,
			Message: "This prompt may violate our usage policies."},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"draw a weapon"}`))
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400: %s", out.StatusCode, out.Body)
	}
	var errBody managementErrorBody
	if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "content_policy_violation" {
		t.Fatalf("body is not the typed pipeline refusal: %s", out.Body)
	}
}

// TestWorkbenchSecretHygiene pins that the acquired credential's token never
// round-trips: only the image bytes + revised_prompt come back; the shell is
// static; and no ABI crossing (request side) carries the token — the
// host.auth.get request carries the auth_index only.
func TestWorkbenchSecretHygiene(t *testing.T) {
	const canary = "CANARY-WORKBENCH-ACCESS-TOKEN"
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{
		AccessToken:  canary,
		RefreshToken: "CANARY-WORKBENCH-REFRESH-TOKEN",
		Email:        "canary@example.com",
		SourceType:   "codex",
	})
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a lighthouse"},
	}
	m := workbenchManager(t, fake, s)

	out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate,
		[]byte(`{"prompt":"a lighthouse"}`))
	if out.StatusCode != http.StatusOK {
		t.Fatalf("generate answered %d: %s", out.StatusCode, out.Body)
	}
	for _, canaryBody := range []string{canary, "CANARY-WORKBENCH-REFRESH-TOKEN", "canary@example.com"} {
		if strings.Contains(string(out.Body), canaryBody) {
			t.Fatalf("canary %q round-tripped into the response: %s", canaryBody, out.Body)
		}
	}

	// The shell is static bytes — it can never carry the credential.
	if strings.Contains(string(workbenchShellBody), canary) {
		t.Fatal("the shell carries credential material")
	}

	// Every ABI crossing (request bytes) is canary-free: host.auth.get sends
	// the auth_index only, and nothing else leaves with the token.
	for _, record := range fake.calls {
		for _, canaryBody := range []string{canary, "CANARY-WORKBENCH-REFRESH-TOKEN"} {
			if strings.Contains(string(record.request), canaryBody) {
				t.Fatalf("canary %q crossed the ABI in %s: %s", canaryBody, record.method, record.request)
			}
		}
	}
}

// TestWorkbenchShellIsDataFree pins the security rule: the shell bytes are
// static — no credential, token, email, or config value. The strongest form:
// the served bytes are IDENTICAL under a config carrying canary values
// (nothing is ever rendered into the page), and the fixture credential
// material of the test suite is absent.
func TestWorkbenchShellIsDataFree(t *testing.T) {
	shell := string(workbenchShellBody)

	// No credential/token vocabulary at all.
	for _, want := range []string{
		"access_token", "refresh_token", "Bearer ", "SECRET-ACCESS-TOKEN-EXECUTOR",
		"executor@example.com", "wb@example.com",
	} {
		if strings.Contains(shell, want) {
			t.Fatalf("the shell carries dynamic material %q", want)
		}
	}

	// Served bytes are identical under a config with canary values: the
	// page renders nothing from the config or the credential store.
	fake := newImportFakeCaller()
	fake.seedCredential(Credential{AccessToken: "CANARY-IN-SHELL", Email: "inshell@example.com", SourceType: "codex"})
	m := execManager(t, fake, "model-prefix: canary-prefix\nbase-url: https://canary-config.example.com")
	out := handleWorkbenchResource(t, m, http.MethodGet, workbenchResourceFullPath, nil)
	if out.StatusCode != http.StatusOK {
		t.Fatalf("resource answered %d: %s", out.StatusCode, out.Body)
	}
	if !bytes.Equal(out.Body, workbenchShellBody) {
		t.Fatal("the served shell differs from the pinned bytes — the page is not static")
	}
	for _, canary := range []string{"CANARY-IN-SHELL", "canary-prefix", "canary-config.example.com", "inshell@example.com"} {
		if strings.Contains(string(out.Body), canary) {
			t.Fatalf("canary %q appears in the served shell", canary)
		}
	}

	// The static data contract the shell's JS relies on (fetch paths + the
	// localStorage keys) — pins that the shell is a real console, not an
	// empty page.
	for _, want := range []string{
		"<!doctype html",
		"/v0/management/chatgpt-proxy/workbench",
		"localStorage",
		"credentials: 'include'",
	} {
		if !strings.Contains(shell, want) {
			t.Fatalf("the shell is missing the static contract marker %q", want)
		}
	}
}

// TestWorkbenchGenerateMalformedBody pins the typed body refusals (malformed
// JSON, a non-object body) before any store call.
func TestWorkbenchGenerateMalformedBody(t *testing.T) {
	fake := newImportFakeCaller()
	s := &imgStub{}
	m := workbenchManager(t, fake, s)

	for _, body := range [][]byte{[]byte(`{`), []byte(`"just a string"`)} {
		out := handleManagement(t, m, http.MethodPost, workbenchRouteGenerate, body)
		if out.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s answered %d, want 400: %s", body, out.StatusCode, out.Body)
		}
		var errBody managementErrorBody
		if err := json.Unmarshal(out.Body, &errBody); err != nil || errBody.Error.Code != "invalid_request" {
			t.Fatalf("body is not the typed refusal: %s", out.Body)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("body refusals must not touch the host store: %v", fake.calls)
	}
}
