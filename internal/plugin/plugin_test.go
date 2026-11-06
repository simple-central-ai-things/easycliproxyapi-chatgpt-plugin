package plugin

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeCaller records every host call and answers with an empty OK envelope —
// the offline host seam every Manager test runs against.
type fakeCaller struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeCaller) Call(method string, _ []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	return hostOK(map[string]any{}), nil
}

func (f *fakeCaller) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(&fakeCaller{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func decodeEnv(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return env
}

func lifecycleRequestBody(t *testing.T, yamlText string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte(yamlText)),
		"schema_version": int(pluginabi.SchemaVersion),
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	return body
}

// TestRegisterEnvelope pins the registration contract the host validPlugin
// gate and every later task build on: schema_version echo, plugin identity,
// non-empty GitHubRepository, and the exact capability surface.
func TestRegisterEnvelope(t *testing.T) {
	m := newTestManager(t)

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
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != pluginID {
		t.Fatalf("metadata.Name = %q, want %q", reg.Metadata.Name, pluginID)
	}
	if reg.Metadata.Version != pluginVersion {
		t.Fatalf("metadata.Version = %q, want %q", reg.Metadata.Version, pluginVersion)
	}
	if reg.Metadata.GitHubRepository != githubRepo {
		t.Fatalf("metadata.GitHubRepository = %q, want %q (host validPlugin rejects empty)", reg.Metadata.GitHubRepository, githubRepo)
	}

	caps := reg.Capabilities
	if !caps.AuthProvider || !caps.ModelProvider || !caps.Executor || !caps.QuotaProvider {
		t.Fatalf("capabilities booleans wrong: %+v", caps)
	}
	if caps.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor_model_scope = %q, want %q", caps.ExecutorModelScope, pluginapi.ExecutorModelScopeOAuth)
	}
	if strings.Join(caps.ExecutorInputFormats, ",") != "openai,openai-image" {
		t.Fatalf("executor_input_formats = %v, want [openai openai-image]", caps.ExecutorInputFormats)
	}
	if strings.Join(caps.ExecutorOutputFormats, ",") != "openai,openai-image" {
		t.Fatalf("executor_output_formats = %v, want [openai openai-image]", caps.ExecutorOutputFormats)
	}
}

// TestIdentifiers pins the three capability identifiers: the provider key is
// the auth-file type, the model prefix and the routing identity.
func TestIdentifiers(t *testing.T) {
	m := newTestManager(t)
	for _, method := range []string{
		pluginabi.MethodAuthIdentifier,
		pluginabi.MethodExecutorIdentifier,
		pluginabi.MethodQuotaIdentifier,
	} {
		resp, err := m.Handle(method, nil)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		env := decodeEnv(t, resp)
		if !env.OK {
			t.Fatalf("%s envelope not OK: %s", method, resp)
		}
		var id struct {
			Identifier string `json:"identifier"`
		}
		if err := json.Unmarshal(env.Result, &id); err != nil {
			t.Fatalf("%s decode result: %v", method, err)
		}
		if id.Identifier != providerKey {
			t.Fatalf("%s identifier = %q, want %q", method, id.Identifier, providerKey)
		}
	}
}

// TestUnknownMethod pins that an unrecognized RPC never panics and always
// answers with a typed error envelope carrying a code.
func TestUnknownMethod(t *testing.T) {
	m := newTestManager(t)

	resp, err := m.Handle("totally.bogus", nil)
	if err != nil {
		t.Fatalf("unknown method must not surface as a Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil {
		t.Fatalf("unknown method must yield an error envelope: %s", resp)
	}
	if env.Error.Code == "" {
		t.Fatalf("error envelope must carry a code: %s", resp)
	}
	if !strings.Contains(env.Error.Message, "totally.bogus") {
		t.Fatalf("error message should name the method: %s", resp)
	}
}

// TestMalformedLifecycle pins fail-closed lifecycle parsing: truncated JSON,
// bad base64 and invalid config are all typed error envelopes, never errors
// that escape Handle.
func TestMalformedLifecycle(t *testing.T) {
	m := newTestManager(t)

	cases := []struct {
		name string
		body []byte
		code string
	}{
		{"truncated json", []byte("{"), "invalid_request"},
		{"bad base64", []byte(`{"config_yaml":"!!!not-base64!!!"}`), "invalid_request"},
		{"invalid config", lifecycleRequestBody(t, "tls-mode: openssl\n"), "invalid_config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := m.Handle(pluginabi.MethodPluginRegister, tc.body)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			env := decodeEnv(t, resp)
			if env.OK || env.Error == nil || env.Error.Code != tc.code {
				t.Fatalf("%s: envelope = %s", tc.name, resp)
			}
		})
	}
}
