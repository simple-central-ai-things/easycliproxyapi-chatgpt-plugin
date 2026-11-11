package plugin

// matrix_test.go pins the README.md contract matrices to the actual
// dispatcher, identity, and config surface (the commandcode-plugin
// matrix_test.go precedent). The README is the operator-facing contract;
// these tests fail when a documented matrix and the code disagree in
// either direction — except the error matrix, which is pinned
// one-directionally (each documented row against the code) — so a
// dispatcher change and its README update land in the same change or CI
// goes red.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// readReadme returns the whole README for the substring pins.
func readReadme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	return string(raw)
}

// readMatrix returns the data rows of the README table delimited by
// <!-- matrix:<name> --> / <!-- /matrix:<name> -->. The header row and the
// separator row are skipped; cells are trimmed.
func readMatrix(t *testing.T, name string) [][]string {
	t.Helper()
	doc := readReadme(t)
	start := strings.Index(doc, "<!-- matrix:"+name+" -->")
	end := strings.Index(doc, "<!-- /matrix:"+name+" -->")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("README matrix %q markers missing or out of order", name)
	}
	var rows [][]string
	headerSeen := false
	for _, line := range strings.Split(doc[start:end], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		if !headerSeen {
			headerSeen = true
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatalf("README matrix %q has no data rows", name)
	}
	return rows
}

func unquoteCell(cell string) string {
	return strings.Trim(strings.TrimSpace(cell), "`")
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// registerForMatrix registers the plugin offline (empty config block) and
// returns the decoded registration result.
func registerForMatrix(t *testing.T, m *Manager) registrationResult {
	t.Helper()
	resp, err := m.Handle(pluginabi.MethodPluginRegister, lifecycleRequestBody(t, "{}"))
	if err != nil {
		t.Fatalf("Handle(plugin.register): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("register envelope not OK: %s", resp)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v (%s)", err, env.Result)
	}
	return reg
}

// TestReadmeIdentityAndRegistryPins pins the headline identity claims: the
// README must name the provider key, the plugin id, the version, the four
// capability names and both executor formats, plus the contract snippets
// the matrices do not carry (usage estimate, image inlining, wire-format
// fidelity, credential record). registry.json must hold exactly the store
// identity the registration publishes.
func TestReadmeIdentityAndRegistryPins(t *testing.T) {
	doc := readReadme(t)
	for _, want := range []string{
		providerKey, pluginID, pluginVersion,
		"`auth_provider`", "`model_provider`", "`executor`", "`quota_provider`",
		"`openai`", "`openai-image`", "`openai-response`", "`claude`",
		"bytes / 4",         // token usage is a documented estimate, never exact
		"b64_json",          // image responses are always served inline
		"byte-identical",    // wire-format fidelity to the reference
		"access_token",      // credential record shape
		"refresh_token",     // credential record shape
		"gpt-image-2-proxy", // the deliberate image base name
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("README does not name %q", want)
		}
	}

	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var store struct {
		Plugins []struct {
			ID         string `json:"id"`
			Version    string `json:"version"`
			Repository string `json:"repository"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatalf("decode registry.json: %v", err)
	}
	if len(store.Plugins) != 1 {
		t.Fatalf("registry.json carries %d plugins, want 1", len(store.Plugins))
	}
	entry := store.Plugins[0]
	if entry.ID != pluginID {
		t.Fatalf("registry.json id = %q, want %q", entry.ID, pluginID)
	}
	if entry.Version != pluginVersion {
		t.Fatalf("registry.json version = %q, want %q", entry.Version, pluginVersion)
	}
	if entry.Repository != githubRepo {
		t.Fatalf("registry.json repository = %q, want %q", entry.Repository, githubRepo)
	}
}

// TestDocumentedCapabilityMatrixMatchesRegistration pins the capabilities
// matrix: each row's advertised cell must agree with the registration
// envelope, and the executor row must state the exact model scope and the
// exact input/output format set.
func TestDocumentedCapabilityMatrixMatchesRegistration(t *testing.T) {
	reg := registerForMatrix(t, newTestManager(t))
	bools := map[string]bool{
		"auth_provider":  reg.Capabilities.AuthProvider,
		"model_provider": reg.Capabilities.ModelProvider,
		"executor":       reg.Capabilities.Executor,
		"quota_provider": reg.Capabilities.QuotaProvider,
		"management_api": reg.Capabilities.ManagementAPI,
	}

	advertisedFormats := map[string]bool{}
	for _, f := range reg.Capabilities.ExecutorInputFormats {
		advertisedFormats[f] = true
	}
	if len(advertisedFormats) != len(reg.Capabilities.ExecutorOutputFormats) {
		t.Fatalf("input formats %v and output formats %v disagree", reg.Capabilities.ExecutorInputFormats, reg.Capabilities.ExecutorOutputFormats)
	}
	for _, f := range reg.Capabilities.ExecutorOutputFormats {
		if !advertisedFormats[f] {
			t.Fatalf("output format %q is not also an input format: %v", f, reg.Capabilities.ExecutorInputFormats)
		}
	}

	seen := map[string]bool{}
	for _, row := range readMatrix(t, "capabilities") {
		if len(row) < 4 {
			t.Fatalf("capability matrix row has %d cells, want 4: %v", len(row), row)
		}
		capability := unquoteCell(row[0])
		want, known := bools[capability]
		if !known {
			t.Fatalf("capability matrix names capability %q that registration does not know", capability)
		}
		if seen[capability] {
			t.Fatalf("capability matrix documents %q twice", capability)
		}
		seen[capability] = true
		if got := row[1]; got != yesno(want) {
			t.Fatalf("capability %q documented as %q, registration advertises %q", capability, got, yesno(want))
		}
		if capability == "executor" {
			if got := unquoteCell(row[2]); got != string(reg.Capabilities.ExecutorModelScope) {
				t.Fatalf("executor scope documented as %q, registration advertises %q", got, reg.Capabilities.ExecutorModelScope)
			}
			documented := map[string]bool{}
			for _, part := range strings.Split(row[3], ",") {
				if f := unquoteCell(part); f != "—" {
					documented[f] = true
				}
			}
			if len(documented) != len(advertisedFormats) {
				t.Fatalf("executor formats documented as %s, registration advertises %v", row[3], reg.Capabilities.ExecutorInputFormats)
			}
			for f := range documented {
				if !advertisedFormats[f] {
					t.Fatalf("documented executor format %q is not advertised: %v", f, reg.Capabilities.ExecutorInputFormats)
				}
			}
		}
	}
	if len(seen) != len(bools) {
		t.Fatalf("capability matrix documents %v, registration has %v", seen, bools)
	}
}

// TestDocumentedModelMatrixMatchesDefaults pins the published model id
// matrix to the config defaults: the text id pattern and both image ids
// must be exactly what ParseConfig derives from an empty block.
func TestDocumentedModelMatrixMatchesDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("{}"))
	if err != nil {
		t.Fatalf("ParseConfig(defaults): %v", err)
	}
	want := map[string]string{
		"text":        cfg.ModelPrefix + "/openai/", // pattern: prefix to verify
		"web-drawing": cfg.ModelPrefix + "/" + cfg.ImageModelName,
		"codex":       cfg.ModelPrefix + "/" + cfg.CodexImageModelName,
	}
	seen := map[string]bool{}
	for _, row := range readMatrix(t, "models") {
		if len(row) < 2 {
			t.Fatalf("models matrix row has %d cells, want 2: %v", len(row), row)
		}
		route := unquoteCell(row[0])
		id := unquoteCell(row[1])
		wantID, known := want[route]
		if !known {
			t.Fatalf("models matrix names route %q that no executor serves", route)
		}
		if seen[route] {
			t.Fatalf("models matrix documents route %q twice", route)
		}
		seen[route] = true
		if route == "text" {
			if !strings.HasPrefix(id, wantID) {
				t.Fatalf("text models documented as %q, want the %q pattern", id, wantID)
			}
		} else if id != wantID {
			t.Fatalf("%s route documented as %q, want %q", route, id, wantID)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("models matrix documents %v, want all of %v", seen, want)
	}
}

// TestDocumentedConfigMatrixMatchesParseConfig pins the config defaults
// matrix: every documented key/default pair must equal what ParseConfig
// derives from an empty block, all plugin-owned keys must be documented,
// and the parser must stay fail-closed on keys the matrix does not name.
func TestDocumentedConfigMatrixMatchesParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("{}"))
	if err != nil {
		t.Fatalf("ParseConfig(defaults): %v", err)
	}
	expected := map[string]string{
		"model-prefix":              fmt.Sprintf("%q", cfg.ModelPrefix),
		"image-model-name":          fmt.Sprintf("%q", cfg.ImageModelName),
		"codex-image-model-name":    fmt.Sprintf("%q", cfg.CodexImageModelName),
		"drawing-upstream-model":    fmt.Sprintf("%q", cfg.DrawingUpstreamModel),
		"thinking-effort":           fmt.Sprintf("%q", cfg.ThinkingEffort),
		"base-url":                  fmt.Sprintf("%q", cfg.BaseURL),
		"message-as-error":          strconv.FormatBool(cfg.MessageAsError),
		"image-poll-timeout-secs":   strconv.Itoa(cfg.ImagePollTimeoutSecs),
		"image-poll-interval-secs":  strconv.Itoa(cfg.ImagePollIntervalSecs),
		"request-timeout-secs":      strconv.Itoa(cfg.RequestTimeoutSecs),
		"tls-mode":                  fmt.Sprintf("%q", cfg.TLSMode),
		"retry-401-refresh":         strconv.FormatBool(cfg.Retry401Refresh),
		"search-upstream-model":     fmt.Sprintf("%q", cfg.SearchUpstreamModel),
		"search-timeout-secs":       strconv.Itoa(cfg.SearchTimeoutSecs),
		"search-poll-interval-secs": strconv.Itoa(cfg.SearchPollIntervalSecs),
	}
	seen := map[string]bool{}
	for _, row := range readMatrix(t, "config") {
		if len(row) < 2 {
			t.Fatalf("config matrix row has %d cells, want 2: %v", len(row), row)
		}
		key := unquoteCell(row[0])
		want, known := expected[key]
		if !known {
			t.Fatalf("config matrix names key %q that ParseConfig does not own", key)
		}
		if seen[key] {
			t.Fatalf("config matrix documents %q twice", key)
		}
		seen[key] = true
		if got := unquoteCell(row[1]); got != want {
			t.Fatalf("config matrix documents %q default %q, want %q", key, got, want)
		}
	}
	if len(seen) != len(expected) {
		t.Fatalf("config matrix documents %v, want all %d plugin-owned keys", seen, len(expected))
	}
	if _, err := ParseConfig([]byte("bogus-key: 1")); err == nil {
		t.Fatal("ParseConfig accepted a key the matrix does not document — the fail-closed contract changed")
	}
}

// TestDocumentedConfigMatrixMatchesRegistrationFields pins the README config
// matrix's key set to the registration's Metadata.ConfigFields in both
// directions: the host panel's plugin-settings surface and the documented
// contract must stay the same set (minus the host-owned enabled/priority),
// so a new key in either place goes red. Each advertised field must also
// carry a description, since that text is what the panel renders.
func TestDocumentedConfigMatrixMatchesRegistrationFields(t *testing.T) {
	reg := registerForMatrix(t, newTestManager(t))
	advertised := map[string]bool{}
	for _, f := range reg.Metadata.ConfigFields {
		if advertised[f.Name] {
			t.Fatalf("registration advertises config field %q twice", f.Name)
		}
		advertised[f.Name] = true
		if f.Description == "" {
			t.Fatalf("registration config field %q has no description for the panel", f.Name)
		}
		if f.Type == pluginapi.ConfigFieldTypeEnum && len(f.EnumValues) == 0 {
			t.Fatalf("registration config field %q is an enum without enum values", f.Name)
		}
		if f.Type != pluginapi.ConfigFieldTypeEnum && len(f.EnumValues) != 0 {
			t.Fatalf("registration config field %q carries enum values but type %q", f.Name, f.Type)
		}
	}
	seen := map[string]bool{}
	for _, row := range readMatrix(t, "config") {
		if len(row) < 2 {
			t.Fatalf("config matrix row has %d cells, want 2: %v", len(row), row)
		}
		key := unquoteCell(row[0])
		if seen[key] {
			t.Fatalf("config matrix documents %q twice", key)
		}
		seen[key] = true
		if !advertised[key] {
			t.Fatalf("config matrix documents key %q that registration does not advertise", key)
		}
	}
	for key := range advertised {
		if !seen[key] {
			t.Fatalf("registration advertises config field %q the config matrix does not document", key)
		}
	}
	if len(seen) != len(advertised) {
		t.Fatalf("config matrix documents %d keys, registration advertises %d fields", len(seen), len(advertised))
	}
	for _, hostOwned := range []string{"enabled", "priority"} {
		if advertised[hostOwned] {
			t.Fatalf("registration advertises host-owned key %q: the host injects it", hostOwned)
		}
	}
}

// TestDocumentedErrorMatrixMatchesDispatch pins the error code matrix:
// each documented row's code and HTTP status must be exactly what the
// dispatcher (rows pinned by "dispatch") or the upstream error mapping
// (rows pinned by "error-map") produces offline; unknown codes or pins
// fail. The pinning is one-directional — a new code error class has no
// row to fail against — and the lifecycle/transport classes
// (invalid_config, plugin_error, model_failure) are intentionally out of
// scope.
func TestDocumentedErrorMatrixMatchesDispatch(t *testing.T) {
	m := newTestManager(t)
	registerForMatrix(t, m) // defaults must be served before executor turns resolve models

	cred := execCredential()
	chatPayload := func(extra map[string]any) map[string]any {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	handle := func(method string, body []byte) *pluginabi.Error {
		t.Helper()
		resp, err := m.Handle(method, body)
		if err != nil {
			t.Fatalf("%s surfaced a Go error: %v", method, err)
		}
		return decodeErrorEnvelope(t, resp).Error
	}
	execBody := func(mutate func(*pluginapi.ExecutorRequest), streamID string) []byte {
		t.Helper()
		req := execRequest(t, cred, "chatgpt-proxy/openai/gpt-5-5", chatPayload(nil), false)
		mutate(&req)
		return execRequestBody(t, req, streamID)
	}

	actual := func(row []string) *pluginabi.Error {
		code := unquoteCell(row[0])
		switch row[3] {
		case "dispatch":
			switch code {
			case "unknown_method":
				return handle("bogus.method", nil)
			case "invalid_request":
				return handle(pluginabi.MethodExecutorExecute, []byte("{"))
			case "model_unknown":
				return handle(pluginabi.MethodExecutorExecute, execBody(func(r *pluginapi.ExecutorRequest) {
					r.Model = "foreign-model"
				}, ""))
			case "tools_unsupported":
				req := execRequest(t, cred, "chatgpt-proxy/openai/gpt-5-5", chatPayload(map[string]any{
					"tools": []any{map[string]any{"type": "function"}},
				}), false)
				return handle(pluginabi.MethodExecutorExecute, execRequestBody(t, req, ""))
			case "unsupported_protocol":
				return handle(pluginabi.MethodExecutorExecute, execBody(func(r *pluginapi.ExecutorRequest) {
					r.Format = "gemini"
				}, ""))
			case "invalid_credential":
				return handle(pluginabi.MethodExecutorExecute, execBody(func(r *pluginapi.ExecutorRequest) {
					r.AuthProvider = "someone-else"
				}, ""))
			case "invalid_stream":
				return handle(pluginabi.MethodExecutorExecuteStream, execBody(func(_ *pluginapi.ExecutorRequest) {}, ""))
			case "unsupported":
				return handle(pluginabi.MethodQuotaReset, nil)
			case "quota_failure":
				return handle(pluginabi.MethodQuotaFetch, []byte(`{"Provider":"someone-else"}`))
			case "invalid_password", "unsupported_region", "login_state_invalid",
				"password_verify_failed", "need_verification_code", "no_auth_code",
				"sentinel_pow_failed", "rate_limit_exceeded":
				return passwordLoginMatrixError(t, code)
			default:
				t.Fatalf("matrix pin %q has no dispatch fixture", code)
			}
		case "import":
			return importMatrixError(t, code)
		case "error-map":
			switch code {
			case "invalid_token":
				return mapUpstreamError(upstream.ErrInvalidToken)
			case "rate_limited":
				return mapUpstreamError(&upstream.ConvError{Code: "rate_limited", HTTPStatus: 429, RetryAfter: 5})
			case "upstream_stream_timeout":
				return mapUpstreamError(upstream.ErrStreamTimeout)
			case "payload_too_large":
				return mapUpstreamError(upstream.ErrPayloadTooLarge)
			case "no_available_model":
				return mapUpstreamError(&upstream.ConvError{Code: "moderation", HTTPStatus: 404})
			case "upstream_error":
				return mapUpstreamError(fmt.Errorf("transport down"))
			case "content_policy_violation":
				return contentPolicyViolation()
			case "no_assistant_content":
				return noAssistantContent()
			case "search_poll_timeout":
				return mapSearchError(&upstream.SearchError{
					Code: upstream.SearchCodePollTimeout, HTTPStatus: http.StatusGatewayTimeout,
					Retryable: true, Message: "timed out waiting for the search result",
				})
			case "search_response_malformed":
				return mapSearchError(&upstream.SearchError{
					Code: upstream.SearchCodeMalformed, HTTPStatus: http.StatusBadGateway,
					Message: "the upstream search response is malformed",
				})
			default:
				t.Fatalf("matrix pin %q has no error-map fixture", code)
			}
		default:
			t.Fatalf("matrix pin %q unknown, want dispatch or error-map", row[3])
		}
		return nil // unreachable: every pin fatals or returns above
	}

	for _, row := range readMatrix(t, "errors") {
		if len(row) < 4 {
			t.Fatalf("errors matrix row has %d cells, want 4: %v", len(row), row)
		}
		code := unquoteCell(row[0])
		wantStatuses := map[int]bool{}
		for _, part := range strings.Split(row[1], ",") {
			status, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				t.Fatalf("errors matrix %q status cell %q is not a number", code, row[1])
			}
			wantStatuses[status] = true
		}
		wantRetryable := unquoteCell(row[2])
		e := actual(row)
		if e.Code != code {
			t.Fatalf("errors matrix row for %q: the pinned path answered %q", code, e.Code)
		}
		if !wantStatuses[e.HTTPStatus] {
			t.Fatalf("errors matrix %q documents statuses %s, the pinned path answered %d", code, row[1], e.HTTPStatus)
		}
		switch wantRetryable {
		case "yes":
			if !e.Retryable {
				t.Fatalf("errors matrix %q documents retryable=yes, the pinned path answered retryable=false", code)
			}
		case "no":
			if e.Retryable {
				t.Fatalf("errors matrix %q documents retryable=no, the pinned path answered retryable=true", code)
			}
		case "varies":
			// conditional retryability: documented, not asserted
		default:
			t.Fatalf("errors matrix %q retryable cell %q, want yes, no or varies", code, wantRetryable)
		}
	}
}

// passwordLoginMatrixError drives auth.login.password offline against a
// scripted host transport shaped to produce the given typed error, and
// returns that error. The flow's first two legs (authorize bootstrap,
// sentinel challenge without PoW) always succeed; the scenario varies the
// third (verify) or the generator budget (PoW cap).
func passwordLoginMatrixError(t *testing.T, code string) *pluginabi.Error {
	t.Helper()
	authorize := hostHTTPResponse{StatusCode: 200, Body: []byte("<html>login page</html>")}
	sentinel := hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
		"token": "matrix-challenge", "proofofwork": map[string]any{"required": false},
	})}
	verify := hostHTTPResponse{StatusCode: 400, Body: []byte(`{"error":{"code":"x","message":"refused"}}`)}
	switch code {
	case "invalid_password":
		verify = hostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":{"code":"a200","message":"Invalid credentials"}}`)}
	case "unsupported_region":
		verify = hostHTTPResponse{StatusCode: 403, Body: []byte(`{"error":{"code":"unsupported_country_region_territory"}}`)}
	case "login_state_invalid":
		verify = hostHTTPResponse{StatusCode: 400, Body: []byte(`{"error":{"code":"invalid_state"}}`)}
	case "need_verification_code":
		verify = hostHTTPResponse{StatusCode: 200, Body: []byte(`{"page":{"type":"email_otp_verification"}}`)}
	case "no_auth_code":
		verify = hostHTTPResponse{StatusCode: 200, Body: []byte(`{}`)}
	case "sentinel_pow_failed":
		sentinel = hostHTTPResponse{StatusCode: 200, Body: mustJSON(t, map[string]any{
			"token": "matrix-challenge",
			// "-" sorts below every hex digit: unsatisfiable, so the shrunk
			// budget below must trip the typed cap error.
			"proofofwork": map[string]any{"required": true, "seed": "0", "difficulty": "-"},
		})}
	case "rate_limit_exceeded":
		payload := base64.StdEncoding.EncodeToString([]byte(`{"errorCode":"rate_limit_exceeded"}`))
		authorize = hostHTTPResponse{
			StatusCode: 302,
			Headers:    map[string][]string{"Location": {oauthAuthorizeBase + "/error?payload=" + payload}},
		}
	}
	fake := &httpFakeCaller{httpResults: []hostHTTPResponse{authorize, sentinel, verify}}
	m := newAuthTestManager(t, fake)
	if code == "sentinel_pow_failed" {
		// Shrink the solve budget: an unsatisfiable difficulty must answer
		// the typed cap error instead of running the production 500k loop.
		m.sentinelFor = func(deviceID, sid string) *sentinelGenerator {
			g := newSentinelGenerator(deviceID, sentinelUserAgent, sid)
			g.maxAttempts = 5
			return g
		}
	}
	body, err := json.Marshal(passwordLoginRequest{
		Provider: providerKey, Email: "matrix@example.com",
		Password: "matrix-password", HostCallbackID: "cb-matrix",
	})
	if err != nil {
		t.Fatalf("marshal auth.login.password request: %v", err)
	}
	resp, err := m.Handle(methodAuthLoginPassword, body)
	if err != nil {
		t.Fatalf("Handle(auth.login.password): %v", err)
	}
	return decodeErrorEnvelope(t, resp).Error
}

// importMatrixError drives one import management route offline against a
// scripted host transport and returns the typed refusal it answers. The
// import classes refuse as management responses (a typed JSON body with an
// HTTP status), not RPC envelopes, so the pin decodes that body.
func importMatrixError(t *testing.T, code string) *pluginabi.Error {
	t.Helper()
	fake := newImportFakeCaller()
	m := newAuthTestManager(t, fake)
	cpaConfig := []byte(`{"base_url":"http://cpa.local","secret_key":"matrix-secret"}`)
	switch code {
	case "import_invalid_request":
		fake.httpResults = nil
		return importMatrixRefusal(t, handleManagement(t, m, http.MethodPost, importRouteCpaFile, []byte(`{`)))
	case "import_unauthorized":
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusUnauthorized, Body: []byte(`{"detail":"no"}`)}}
		return importMatrixRefusal(t, handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfig))
	case "import_remote_failed":
		fake.httpResults = []hostHTTPResponse{{StatusCode: http.StatusBadGateway, Body: []byte(`{"detail":"no"}`)}}
		return importMatrixRefusal(t, handleManagement(t, m, http.MethodGet, importRouteCpaRemoteFiles, cpaConfig))
	case "import_payload_invalid":
		return importMatrixRefusal(t, handleManagement(t, m, http.MethodPost, importRouteCpaFile, []byte(`{"refresh_token":"rt-only"}`)))
	case "import_save_failed":
		fake.listError = "store-denied"
		return importMatrixRefusal(t, handleManagement(t, m, http.MethodPost, importRouteCpaFile, []byte(`{"access_token":"at-matrix"}`)))
	default:
		t.Fatalf("matrix pin %q has no import fixture", code)
	}
	return nil // unreachable: every branch returns or fatals above
}

// importMatrixRefusal decodes a refusal management response into the
// pluginabi.Error shape the matrix rows pin (code + status + retryable).
func importMatrixRefusal(t *testing.T, resp pluginapi.ManagementResponse) *pluginabi.Error {
	t.Helper()
	var body managementErrorBody
	if err := json.Unmarshal(resp.Body, &body); err != nil || body.Error.Code == "" {
		t.Fatalf("management refusal body is not typed: %s", resp.Body)
	}
	return &pluginabi.Error{
		Code:       body.Error.Code,
		HTTPStatus: resp.StatusCode,
		Retryable:  body.Error.Retryable,
	}
}
