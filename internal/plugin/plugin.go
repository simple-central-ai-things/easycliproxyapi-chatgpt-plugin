// Package plugin owns the chatgpt-proxy provider's RPC surface: the
// Manager dispatches every host method, the caller seam carries all host I/O,
// and the registration/config contracts are pinned by this package's tests.
//
// This is the ABI skeleton (Task 1). The identity, the registration envelope
// and the config surface are the contracts every later task builds on; the
// protocol implementations land in later tasks and are dispatched from the
// same Manager. auth.parse / auth.refresh (Task 3) claim type=chatgpt-proxy
// credential records and run the OAuth refresh_token lifecycle;
// auth.login.start / auth.login.poll (Task 4) run the browser OAuth PKCE
// flow against the host's callback-file mechanism; auth.login.password
// (F4) runs password re-login — account recovery through the auth-flow
// sentinel + PKCE — as a single synchronous call;
// executor.execute / executor.execute_stream (Task 12) serve OpenAI-format
// text conversation turns over the sentinel handshake + conversation SSE;
// management.register / management.handle (F5) expose the CPA / sub2api
// account imports as host Management API routes (management.go) that write
// login-shaped credentials into the host store (import.go).
package plugin

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// Identity constants. The pluginID is the .so basename and the registration
// Name; providerKey aliases the canonical model.ProviderKey (the auth-file
// type, the model prefix and the routing identity); githubRepo satisfies the
// host validPlugin gate (it rejects an empty Metadata.GitHubRepository) and is
// the URL a plugin store polls.
const (
	pluginID      = "easycliproxyapi-chatgpt-provider"
	providerKey   = model.ProviderKey
	pluginVersion = "0.2.9"
	githubRepo    = "https://github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin"
)

// caller is the single host RPC bridge every Manager I/O flows through
// (host.http.do, host.http.do_stream, host.stream.emit/close, host.log,
// host.auth.list/get/save, ...). Tests inject a fake; main.go injects the C
// ABI. This seam is what makes the whole package testable offline.
type caller interface {
	Call(method string, request []byte) ([]byte, error)
}

// Manager owns the served configuration snapshot and routes every RPC method.
// It is safe for concurrent Handle use. host is immutable after New.
type Manager struct {
	host caller

	mu  sync.RWMutex
	cfg Config

	// loginMu guards logins, the in-memory browser-OAuth attempt store
	// (login.go). It is separate from mu: login lifecycle never needs the
	// config snapshot and vice versa, so the two locks stay unentangled.
	loginMu sync.Mutex
	logins  map[string]loginAttempt

	// models holds the per-credential catalog snapshots (models.go); it
	// carries its own lock for the same reason.
	models *modelCatalog

	// sentinelFor builds the auth-flow sentinel generator (password_login.go).
	// Tests may replace it to shrink the PoW budget; production wiring never
	// changes it after New.
	sentinelFor func(deviceID, sid string) *sentinelGenerator
}

// New returns a Manager whose outbound host calls flow through host. A nil
// host is refused because the Manager cannot function without the ABI bridge.
func New(host caller) (*Manager, error) {
	if host == nil {
		return nil, fmt.Errorf("host caller is required")
	}
	return &Manager{
		host:   host,
		logins: make(map[string]loginAttempt),
		models: newModelCatalog(),
		sentinelFor: func(deviceID, sid string) *sentinelGenerator {
			return newSentinelGenerator(deviceID, sentinelUserAgent, sid)
		},
	}, nil
}

// Handle dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// plugin_error envelope so the host process never dies with us.
func (m *Manager) Handle(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp = errEnvelope(typedErr("plugin_error", fmt.Sprintf("internal error handling %s", method)))
			err = nil
		}
	}()

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodAuthIdentifier:
		return ok(map[string]string{"identifier": providerKey}), nil
	case pluginabi.MethodExecutorIdentifier:
		return ok(map[string]string{"identifier": providerKey}), nil
	case pluginabi.MethodQuotaIdentifier:
		return ok(map[string]string{"identifier": providerKey}), nil
	case pluginabi.MethodQuotaDescribe:
		// Static metadata: the quota surface renders on the host's native
		// auth-file cards; this cannot fail.
		return handleQuotaDescribe(), nil
	case pluginabi.MethodQuotaFetch:
		return m.handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return handleQuotaReset(), nil
	case pluginabi.MethodAuthParse:
		return m.handleAuthParse(request)
	case pluginabi.MethodAuthRefresh:
		return m.handleAuthRefresh(request)
	case pluginabi.MethodAuthLoginStart:
		return m.handleLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return m.handleLoginPoll(request)
	case methodAuthLoginPassword:
		// Plugin-defined method (the SDK carries no wire type for it):
		// synchronous password re-login / account recovery.
		return m.handlePasswordLogin(request)
	case pluginabi.MethodModelStatic:
		return m.handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return m.handleModelForAuth(request)
	case pluginabi.MethodExecutorExecute:
		return m.handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecutorExecuteStream(request)
	case pluginabi.MethodManagementRegister:
		return m.handleManagementRegister(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagementHandle(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	default:
		return errEnvelope(typedErr("unknown_method", "unknown method: "+method, 404)), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64. The
// request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// handleLifecycle implements plugin.register / plugin.reconfigure: parse and
// validate the config fail-closed, then publish the served snapshot and reply
// with the registration envelope. Later tasks extend this to reconcile auth
// and warm the model catalog; the contract (typed errors in the envelope, no
// Go errors) is already pinned here.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed lifecycle request body", 400)), nil
	}
	cfg, err := ParseConfig(req.ConfigYAML)
	if err != nil {
		return errEnvelope(typedErr("invalid_config", err.Error(), 400)), nil
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return registrationEnvelope(), nil
}

// handleShutdown clears the served snapshot. Idempotent: the C glue may call
// the shutdown export after plugin.shutdown has already run.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.mu.Lock()
	m.cfg = Config{}
	m.mu.Unlock()
	m.models.reset()
	return ok(struct{}{}), nil
}

// capabilities is the exact capability surface the registration publishes.
// The host decodes this into its rpcCapabilities; unset capability booleans
// default to false, so the eight fields below are the complete surface.
// ManagementAPI must be true for the host to invoke management.register at
// all — it is what turns on the import routes (management.go).
type capabilities struct {
	AuthProvider          bool                         `json:"auth_provider"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
}

// registrationResult is the rpcRegistration-shaped body the host expects for
// plugin.register / plugin.reconfigure.
type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// configFields is the registration's Metadata.ConfigFields surface: one
// field per plugin-owned key ParseConfig accepts (the README config matrix
// pins the exact set, matrix_test.go), so the host panel's plugin-settings
// UI renders every key instead of the operator hand-editing the
// plugins.configs.<id> YAML block. Types mirror the parser: boolean keys →
// boolean, *-secs keys → integer, tls-mode → enum (the only key validate
// rejects outside its closed set), the rest → string. thinking-effort stays
// an open string — the executor maps its spelling, ParseConfig does not.
// The host-owned "enabled" and "priority" keys the host injects are
// deliberately absent: the host owns them and the panel renders its own.
// The Description text renders verbatim in the host's plugin-list JSON (the
// default Go JSON encoding HTML-escapes quotes) and in the management
// center panel: plain text only — no double quotes, no ampersand, no angle
// brackets anywhere.
var configFields = []pluginapi.ConfigField{
	{Name: "model-prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix of every published model id; the id is the prefix followed by the model base name. Default: chatgpt-proxy."},
	{Name: "image-model-name", Type: pluginapi.ConfigFieldTypeString, Description: "Base name of the published web-drawing image model. Default: gpt-image-2-proxy."},
	{Name: "codex-image-model-name", Type: pluginapi.ConfigFieldTypeString, Description: "Base name of the published codex image model (plus/pro/team plans). Default: codex-gpt-image-2."},
	{Name: "drawing-upstream-model", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream model used for web-drawing image turns. Default: gpt-5-5."},
	{Name: "thinking-effort", Type: pluginapi.ConfigFieldTypeString, Description: "Thinking effort applied to text turns; open string, the executor maps it to the upstream spelling. Default: auto."},
	{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "Reserved; image responses are always inline b64_json regardless of this value. Default: empty."},
	{Name: "message-as-error", Type: pluginapi.ConfigFieldTypeBoolean, Description: "True surfaces a drawing turn that replies with prose as an error instead of a silent 200. Default: true."},
	{Name: "image-poll-timeout-secs", Type: pluginapi.ConfigFieldTypeInteger, Description: "Seconds to wait for a web-drawing image turn before failing. Default: 120."},
	{Name: "image-poll-interval-secs", Type: pluginapi.ConfigFieldTypeInteger, Description: "Seconds between image-turn poll attempts; must not exceed image-poll-timeout-secs. Default: 10."},
	{Name: "request-timeout-secs", Type: pluginapi.ConfigFieldTypeInteger, Description: "Seconds for the overall upstream request budget of a turn. Default: 600."},
	{Name: "tls-mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"stdlib", "utls"}, Description: "TLS client for the direct upstream fallback leg: stdlib (Go crypto/tls) or utls (browser-like fingerprint). Default: stdlib."},
	{Name: "retry-401-refresh", Type: pluginapi.ConfigFieldTypeBoolean, Description: "True refreshes the credential once and retries a turn that fails with 401. Default: true."},
	{Name: "search-upstream-model", Type: pluginapi.ConfigFieldTypeString, Description: "Upstream model used for web-search turns. Default: gpt-5-5."},
	{Name: "search-timeout-secs", Type: pluginapi.ConfigFieldTypeInteger, Description: "Seconds to wait for a web-search result before failing. Default: 300."},
	{Name: "search-poll-interval-secs", Type: pluginapi.ConfigFieldTypeInteger, Description: "Seconds between search-result poll attempts; must not exceed search-timeout-secs. Default: 3."},
}

// registrationEnvelope builds the success envelope for register/reconfigure:
// schema_version echo, the pinned identity (with a non-empty GitHubRepository
// for the host validPlugin gate), the exact capability surface, and the
// config-field surface the host panel renders as plugin settings.
func registrationEnvelope() []byte {
	formats := []string{"openai", "openai-image", responseFormat, claudeFormat}
	return ok(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           pluginID,
			GitHubRepository: githubRepo,
			ConfigFields:     configFields,
		},
		Capabilities: capabilities{
			AuthProvider:          true,
			ModelProvider:         true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			QuotaProvider:         true,
			ManagementAPI:         true,
		},
	})
}
