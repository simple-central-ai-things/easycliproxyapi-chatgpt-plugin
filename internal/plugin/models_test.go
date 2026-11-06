package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// models_test.go pins the model_provider surface (Task 11): model.for_auth
// discovers the per-credential upstream text catalog (oracle
// /backend-api/models shape: slug / created / owned_by), publishes it as
// chatgpt-proxy/openai/<slug> plus the two image models, caches per
// credential fingerprint with a 300s TTL and serves the last good snapshot
// stale-while-unavailable; model.static serves the union without any fetch.
// Review Focus #4 is pinned by TestBaseNamesNotHardcoded.

// modelsRouteURL is the oracle's authed catalog route, query included
// (openai_backend_api.py list_models, line 2733).
const modelsRouteURL = "https://chatgpt.com/backend-api/models?history_and_training_disabled=false"

// catalogFixture is a raw /backend-api/models body in the oracle's shape: a
// duplicate slug (first occurrence wins), a blank slug, a slug-less entry
// and a non-object entry — all skipped per the oracle's tolerant loop
// (openai_backend_api.py lines 2747-2758).
const catalogFixture = `{
	"models": [
		{"slug": "gpt-5-5", "created": 1750000000, "owned_by": "chatgpt"},
		{"slug": "gpt-5.4-mini", "created": 1751000000},
		{"slug": "gpt-5-5", "created": 999, "owned_by": "openai"},
		{"slug": "  ", "created": 1},
		{"display_name": "no-slug-entry"},
		"not-an-object"
	]
}`

// catalogCredential builds one test credential; the wire fingerprint ids are
// deterministic per email so two credentials yield two distinct cache keys.
func catalogCredential(email, plan string) Credential {
	return Credential{
		AccessToken:  "test-access-token-" + email,
		RefreshToken: "test-refresh-token-" + email,
		Email:        email,
		Plan:         plan,
		Fp: Fingerprint{
			OAIDeviceID:  email + "-device",
			OAISessionID: email + "-session",
		},
	}
}

// modelFakeCaller is the offline host seam for the catalog tests. Scripts
// are keyed by "url\x00authorization" so two credentials hitting the same
// route can carry independent responses (TestFingerprintCacheScoping).
type modelFakeCaller struct {
	mu      sync.Mutex
	calls   []hostCallRecord
	scripts map[string]hostHTTPResponse
	hits    map[string]int
}

func modelScriptKey(url string, cred Credential) string {
	return url + "\x00" + "Bearer " + cred.AccessToken
}

func (f *modelFakeCaller) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, hostCallRecord{method: method, request: append([]byte(nil), request...)})
	f.mu.Unlock()
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	var req struct {
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
	}
	_ = json.Unmarshal(request, &req)
	key := req.URL + "\x00" + req.Headers.Get("Authorization")
	f.mu.Lock()
	if f.hits == nil {
		f.hits = make(map[string]int)
	}
	f.hits[key]++
	resp, scripted := f.scripts[key]
	f.mu.Unlock()
	if !scripted {
		return hostOK(hostHTTPResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"no script"}`)}), nil
	}
	return hostOK(resp), nil
}

// fetches counts upstream fetches issued under one script key — the TTL
// cache pin (a second discovery within the TTL must not re-fetch).
func (f *modelFakeCaller) fetches(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

// lastFetch returns the recorded wire request of the latest host.http.do
// under one script key.
func (f *modelFakeCaller) lastFetch(t *testing.T, key string) struct {
	URL     string
	Authz   string
	Headers http.Header
} {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out struct {
		URL     string
		Authz   string
		Headers http.Header
	}
	for i := len(f.calls) - 1; i >= 0; i-- {
		record := f.calls[i]
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if json.Unmarshal(record.request, &req) != nil {
			continue
		}
		if req.URL+"\x00"+req.Headers.Get("Authorization") == key {
			out.URL = req.URL
			out.Authz = req.Headers.Get("Authorization")
			out.Headers = req.Headers
			return out
		}
	}
	t.Fatalf("caller never fetched under key %q", key)
	return out
}

func modelsScript(status int, body string) hostHTTPResponse {
	return hostHTTPResponse{StatusCode: status, Body: []byte(body)}
}

func modelForAuthBody(t *testing.T, cred Credential) []byte {
	t.Helper()
	body, err := json.Marshal(pluginapi.AuthModelRequest{
		AuthID:       "auth-1",
		AuthProvider: providerKey,
		StorageJSON:  cred.StorageJSON(),
	})
	if err != nil {
		t.Fatalf("marshal model.for_auth request: %v", err)
	}
	return body
}

// modelManager builds a Manager the way the host does: plugin.register with
// the default config first (the model prefix and image model names come
// from the served config snapshot), then the Manager is ready.
func modelManager(t *testing.T, host caller) *Manager {
	t.Helper()
	m := quotaManager(t, host)
	resp, err := m.Handle(pluginabi.MethodPluginRegister, lifecycleRequestBody(t, ""))
	if err != nil {
		t.Fatalf("Handle(plugin.register): %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("plugin.register envelope not OK: %s", resp)
	}
	return m
}

func handleModel(t *testing.T, m *Manager, method string, request []byte) []byte {
	t.Helper()
	resp, err := m.Handle(method, request)
	if err != nil {
		t.Fatalf("Handle(%s): %v", method, err)
	}
	return resp
}

// decodeModelResponse unwraps a success envelope into the SDK wire shape —
// pluginapi.ModelResponse marshals PascalCase (no json tags), exactly what
// the host decodes (pluginhost rpc_client.go callPlugin).
func decodeModelResponse(t *testing.T, resp []byte) pluginapi.ModelResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("model envelope not OK: %s", resp)
	}
	var out pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode model response: %v (%s)", err, env.Result)
	}
	return out
}

func modelsByID(resp pluginapi.ModelResponse) map[string]pluginapi.ModelInfo {
	out := make(map[string]pluginapi.ModelInfo, len(resp.Models))
	for _, info := range resp.Models {
		out[info.ID] = info
	}
	return out
}

func assertNoAuthUpdate(t *testing.T, out pluginapi.ModelResponse) {
	t.Helper()
	if out.AuthUpdate.Provider != "" ||
		len(out.AuthUpdate.StorageJSON) != 0 ||
		len(out.AuthUpdate.Metadata) != 0 {
		t.Fatalf("AuthUpdate = %+v, want zero (nothing for the host to re-save)", out.AuthUpdate)
	}
}

// ---------------------------------------------------------------------------
// TestForAuthPublishesTextCatalog — fixture upstream catalog → published
// chatgpt-proxy/openai/<slug> ids, Type chat, the CONSTANT openai vendor
// segment (the upstream catalog carries no vendor field — nothing invented),
// oracle-tolerant parsing, the exact fetch route + bearer, and the 300s TTL
// cache (a second discovery does not re-fetch).
// ---------------------------------------------------------------------------

func TestForAuthPublishesTextCatalog(t *testing.T) {
	cred := catalogCredential("alice@example.com", "plus")
	key := modelScriptKey(modelsRouteURL, cred)
	fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{key: modelsScript(http.StatusOK, catalogFixture)}}
	m := modelManager(t, fake)

	out := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
	if out.Provider != providerKey {
		t.Fatalf("Provider = %q, want %q", out.Provider, providerKey)
	}

	byID := modelsByID(out)
	if len(byID) != 4 {
		t.Fatalf("published %d models %v, want 2 text + 2 image", len(byID), keysOf(byID))
	}
	for id, info := range byID {
		parts := strings.Split(id, "/")
		if parts[0] != providerKey {
			t.Fatalf("model id %q does not start with %q", id, providerKey)
		}
		switch info.Type {
		case "chat":
			// Text models publish as <prefix>/openai/<slug>: the vendor
			// segment is the constant openai (the upstream catalog carries
			// no vendor field — nothing invented).
			if len(parts) != 3 || parts[1] != "openai" {
				t.Fatalf("text model id %q is not <prefix>/openai/<slug>", id)
			}
		case "openai-image":
			if len(parts) != 2 {
				t.Fatalf("image model id %q is not <prefix>/<image-model-name>", id)
			}
		default:
			t.Fatalf("model %q Type = %q, want chat or openai-image", id, info.Type)
		}
		if info.Object != "model" {
			t.Fatalf("model %q Object = %q, want model", id, info.Object)
		}
	}

	// First occurrence wins on duplicate slugs; owned_by defaults to the
	// oracle's "chatgpt" fallback when upstream omits it.
	gpt55 := byID["chatgpt-proxy/openai/gpt-5-5"]
	if gpt55.Created != 1750000000 || gpt55.OwnedBy != "chatgpt" {
		t.Fatalf("gpt-5-5 = %+v, want created 1750000000 / owned_by chatgpt (first occurrence)", gpt55)
	}
	gpt54 := byID["chatgpt-proxy/openai/gpt-5.4-mini"]
	if gpt54.Created != 1751000000 || gpt54.OwnedBy != "chatgpt" {
		t.Fatalf("gpt-5.4-mini = %+v, want created 1751000000 / owned_by chatgpt (defaulted)", gpt54)
	}
	assertNoAuthUpdate(t, out)

	// The fetch went to the exact oracle route, authed with the credential
	// bearer, under the pinned session headers.
	fetch := fake.lastFetch(t, key)
	if fetch.URL != modelsRouteURL {
		t.Fatalf("fetched %q, want %q", fetch.URL, modelsRouteURL)
	}
	if fetch.Authz != "Bearer test-access-token-alice@example.com" {
		t.Fatalf("Authorization = %q, want the credential bearer", fetch.Authz)
	}
	// Header names canonicalize on the wire (http.Header); Get looks them
	// up case-insensitively, so the pinned semantic name is what we assert.
	if fetch.Headers.Get("X-OpenAI-Target-Path") != "/backend-api/models" {
		t.Fatalf("X-OpenAI-Target-Path = %q, want the bare route (oracle _headers convention)", fetch.Headers.Get("X-OpenAI-Target-Path"))
	}

	// TTL: a second discovery for the same credential serves the cache —
	// no new upstream fetch.
	out2 := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
	if len(modelsByID(out2)) != len(byID) {
		t.Fatalf("cached re-discovery published %d models, want %d", len(modelsByID(out2)), len(byID))
	}
	if n := fake.fetches(key); n != 1 {
		t.Fatalf("upstream fetched %d times, want 1 (300s TTL cache)", n)
	}
}

// keysOf lists a map's keys for failure messages.
func keysOf(m map[string]pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// ---------------------------------------------------------------------------
// TestForAuthImageModels — both image models publish with Type openai-image
// for every credential; the codex route only for the gating plans.
// ---------------------------------------------------------------------------

func TestForAuthImageModels(t *testing.T) {
	const emptyCatalog = `{"models":[]}`
	cases := []struct {
		plan  string
		codex bool
	}{
		{plan: "free", codex: false},
		{plan: "", codex: false}, // unknown plan gates out
		{plan: "plus", codex: true},
		{plan: "pro", codex: true},
		{plan: "team", codex: true},
	}
	for _, tc := range cases {
		t.Run("plan="+tc.plan, func(t *testing.T) {
			cred := catalogCredential("bob@"+tc.plan+"example.com", tc.plan)
			key := modelScriptKey(modelsRouteURL, cred)
			fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{key: modelsScript(http.StatusOK, emptyCatalog)}}
			m := modelManager(t, fake)
			out := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
			byID := modelsByID(out)

			web := byID["chatgpt-proxy/gpt-image-2-proxy"]
			if web.Type != "openai-image" || web.Object != "model" {
				t.Fatalf("web-drawing image model = %+v, want Type openai-image / Object model", web)
			}
			codex, ok := byID["chatgpt-proxy/codex-gpt-image-2"]
			if ok != tc.codex {
				t.Fatalf("codex image model present = %v, want %v for plan %q", ok, tc.codex, tc.plan)
			}
			if ok && codex.Type != "openai-image" {
				t.Fatalf("codex image model Type = %q, want openai-image", codex.Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestStaleWhileUnavailable — a previous snapshot keeps serving when the
// fetch fails (commandcode SWR policy); the reply carries no AuthUpdate; a
// failure with NO snapshot is a typed model_failure envelope.
// ---------------------------------------------------------------------------

func TestStaleWhileUnavailable(t *testing.T) {
	cred := catalogCredential("carol@example.com", "plus")
	key := modelScriptKey(modelsRouteURL, cred)
	fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{key: modelsScript(http.StatusOK, catalogFixture)}}
	m := modelManager(t, fake)

	first := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
	assertNoAuthUpdate(t, first)

	// Upstream goes away: the last good snapshot is served instead.
	fake.mu.Lock()
	fake.scripts[key] = modelsScript(http.StatusInternalServerError, `{"error":"upstream down"}`)
	fake.mu.Unlock()
	out := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
	if len(modelsByID(out)) != len(modelsByID(first)) {
		t.Fatalf("stale reply published %d models, want the previous snapshot's %d", len(modelsByID(out)), len(modelsByID(first)))
	}
	assertNoAuthUpdate(t, out)

	t.Run("failure without a snapshot is typed", func(t *testing.T) {
		stranger := catalogCredential("dave@example.com", "free")
		fake2 := &modelFakeCaller{scripts: map[string]hostHTTPResponse{
			modelScriptKey(modelsRouteURL, stranger): modelsScript(http.StatusInternalServerError, `{"error":"down"}`),
		}}
		m2 := modelManager(t, fake2)
		env := decodeErrorEnvelope(t, handleModel(t, m2, pluginabi.MethodModelForAuth, modelForAuthBody(t, stranger)))
		if env.Error.Code != "model_failure" {
			t.Fatalf("error code = %q, want model_failure", env.Error.Code)
		}
		if env.Error.HTTPStatus != http.StatusInternalServerError || !env.Error.Retryable {
			t.Fatalf("error = %+v, want status 500 retryable", env.Error)
		}
	})
}

// ---------------------------------------------------------------------------
// TestStaticFirstBoot — with no cache, model.static publishes exactly the
// two image models (text union empty, no fetch); after a discovery it serves
// the union.
// ---------------------------------------------------------------------------

func TestStaticFirstBoot(t *testing.T) {
	m := modelManager(t, &fakeCaller{})
	out := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelStatic, nil))
	if out.Provider != providerKey {
		t.Fatalf("Provider = %q, want %q", out.Provider, providerKey)
	}
	ids := make([]string, 0, len(out.Models))
	for _, info := range out.Models {
		ids = append(ids, info.ID)
		if info.Type != "openai-image" {
			t.Fatalf("first-boot model %q Type = %q, want only openai-image", info.ID, info.Type)
		}
	}
	want := []string{"chatgpt-proxy/codex-gpt-image-2", "chatgpt-proxy/gpt-image-2-proxy"}
	if len(ids) != len(want) {
		t.Fatalf("first-boot published %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("first-boot published %v, want %v (sorted by id)", ids, want)
		}
	}

	t.Run("union after discovery", func(t *testing.T) {
		cred := catalogCredential("erin@example.com", "plus")
		key := modelScriptKey(modelsRouteURL, cred)
		fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{key: modelsScript(http.StatusOK, catalogFixture)}}
		m2 := modelManager(t, fake)
		handleModel(t, m2, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred))
		out2 := decodeModelResponse(t, handleModel(t, m2, pluginabi.MethodModelStatic, nil))
		byID := modelsByID(out2)
		if _, ok := byID["chatgpt-proxy/openai/gpt-5-5"]; !ok {
			t.Fatalf("static after discovery = %v, want the discovered text union", keysOf(byID))
		}
		if _, ok := byID["chatgpt-proxy/gpt-image-2-proxy"]; !ok || len(byID) != 4 {
			t.Fatalf("static after discovery = %v, want union + both image models", keysOf(byID))
		}
	})
}

// ---------------------------------------------------------------------------
// TestBaseNamesNotHardcoded — Review Focus #4. The host's images endpoint
// treats these base names as BUILT-IN codex models and routes them verbatim
// to the built-in executor before any plugin/registry lookup: the set is
// pinned at CLIProxyAPI v8.0.0 sdk/api/handlers/openai/openai_images_handlers.go
// :243-249 (isCodexImagesToolModel, case on imagesModelBase) and repeated in
// sdk/api/handlers/handlers_routing.go:242 (isOpenAIImageOnlyModel) with the
// XAI models. Our two published image BASE names must stay outside it so
// they resolve on the registry path (Type openai-image → our executor) —
// which is why the web-drawing model carries the deliberate -proxy suffix.
// ---------------------------------------------------------------------------

func TestBaseNamesNotHardcoded(t *testing.T) {
	// Pinned from the SDK literals above; membership of gpt-image-2 asserts
	// the pin itself did not drift.
	hardcoded := map[string]bool{
		"gpt-image-1.5":              true,
		"gpt-image-2":                true,
		"gpt-image-2.5-flare":        true,
		"gpt-image-2.5-sunburst":     true,
		"gpt-image-2.5":              true,
		"grok-imagine-image":         true,
		"grok-imagine-image-quality": true,
		"grok-imagine-image-2.0":     true,
	}
	if !hardcoded["gpt-image-2"] {
		t.Fatalf("pinned hardcoded set literal drifted from the SDK")
	}
	for _, base := range []string{"gpt-image-2-proxy", "codex-gpt-image-2"} {
		if hardcoded[base] {
			t.Fatalf("image base name %q is in the host's hardcoded set — the built-in executor would claim it (Review Focus #4)", base)
		}
	}
}

// ---------------------------------------------------------------------------
// TestFingerprintCacheScoping — two credentials keep independent caches:
// one credential's snapshot (or failure) never leaks into the other.
// ---------------------------------------------------------------------------

func TestFingerprintCacheScoping(t *testing.T) {
	const aliceCatalog = `{"models":[{"slug":"alice-only","owned_by":"chatgpt"}]}`
	const bobCatalog = `{"models":[{"slug":"bob-only","owned_by":"chatgpt"}]}`

	alice := catalogCredential("alice@example.com", "plus")
	bob := catalogCredential("bob@example.com", "free")
	aliceKey, bobKey := modelScriptKey(modelsRouteURL, alice), modelScriptKey(modelsRouteURL, bob)

	fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{
		aliceKey: modelsScript(http.StatusOK, aliceCatalog),
		bobKey:   modelsScript(http.StatusInternalServerError, `{"error":"down"}`),
	}}
	m := modelManager(t, fake)

	outAlice := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, alice)))
	if _, ok := modelsByID(outAlice)["chatgpt-proxy/openai/alice-only"]; !ok {
		t.Fatalf("alice published %v, want her catalog", keysOf(modelsByID(outAlice)))
	}

	// Bob's failure (no snapshot) is a typed error and must not touch
	// Alice's cache.
	envBob := decodeErrorEnvelope(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, bob)))
	if envBob.Error.Code != "model_failure" {
		t.Fatalf("bob error code = %q, want model_failure", envBob.Error.Code)
	}
	outAlice2 := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, alice)))
	if _, ok := modelsByID(outAlice2)["chatgpt-proxy/openai/alice-only"]; !ok {
		t.Fatalf("bob's 500 leaked into alice's cache: %v", keysOf(modelsByID(outAlice2)))
	}
	if n := fake.fetches(aliceKey); n != 1 {
		t.Fatalf("alice fetched %d times, want 1 (her cache is intact)", n)
	}

	// Bob recovers with his OWN catalog — no alice models.
	fake.mu.Lock()
	fake.scripts[bobKey] = modelsScript(http.StatusOK, bobCatalog)
	fake.mu.Unlock()
	outBob := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, bob)))
	byBob := modelsByID(outBob)
	if _, ok := byBob["chatgpt-proxy/openai/bob-only"]; !ok {
		t.Fatalf("bob published %v, want his own catalog", keysOf(byBob))
	}
	if _, ok := byBob["chatgpt-proxy/openai/alice-only"]; ok {
		t.Fatalf("alice's snapshot leaked into bob's catalog: %v", keysOf(byBob))
	}

	// The static union merges both independent snapshots.
	outStatic := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelStatic, nil))
	byStatic := modelsByID(outStatic)
	for _, slug := range []string{"alice-only", "bob-only"} {
		if _, ok := byStatic["chatgpt-proxy/openai/"+slug]; !ok {
			t.Fatalf("static union = %v, want %q", keysOf(byStatic), slug)
		}
	}
}

// ---------------------------------------------------------------------------
// TestForAuthPersistsFingerprint — a credential without wire fingerprint ids
// gets them materialized for the session headers; the AuthUpdate carries the
// persisted StorageJSON back (headers.go contract) so the cache key derived
// from the fingerprint is stable across calls.
// ---------------------------------------------------------------------------

func TestForAuthPersistsFingerprint(t *testing.T) {
	cred := catalogCredential("frank@example.com", "plus")
	cred.Fp = Fingerprint{} // fresh import: no wire fingerprint yet
	key := modelScriptKey(modelsRouteURL, cred)
	fake := &modelFakeCaller{scripts: map[string]hostHTTPResponse{key: modelsScript(http.StatusOK, catalogFixture)}}
	m := modelManager(t, fake)

	out := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, cred)))
	if out.AuthUpdate.Provider != providerKey || len(out.AuthUpdate.StorageJSON) == 0 {
		t.Fatalf("AuthUpdate = %+v, want the materialized fingerprint persisted back", out.AuthUpdate)
	}
	saved, err := FromStorageJSON(out.AuthUpdate.StorageJSON)
	if err != nil {
		t.Fatalf("AuthUpdate.StorageJSON does not round-trip: %v", err)
	}
	if saved.Fp.OAIDeviceID == "" || saved.Fp.OAISessionID == "" {
		t.Fatalf("AuthUpdate fingerprint = %+v, want generated ids", saved.Fp)
	}

	// The persisted credential hits the same cache key: no re-fetch, no
	// second AuthUpdate.
	out2 := decodeModelResponse(t, handleModel(t, m, pluginabi.MethodModelForAuth, modelForAuthBody(t, saved)))
	assertNoAuthUpdate(t, out2)
	if n := fake.fetches(key); n != 1 {
		t.Fatalf("upstream fetched %d times, want 1 (fingerprint cache key stable)", n)
	}
}
