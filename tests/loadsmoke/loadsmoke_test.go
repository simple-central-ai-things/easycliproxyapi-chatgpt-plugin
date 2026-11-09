//go:build linux && cgo

package loadsmoke

// Offline load dry-run of the REAL artifact: the c-shared .so is dlopen'ed,
// cliproxy_plugin_init is called with a fake host ABI whose callbacks are
// answered in memory, and the plugin is driven through cliproxyPluginCall
// exactly as EasyCLI drives it.
//
// No EasyCLI binary, deploy host, network or credential is involved. The
// needles are the registration identity, the pinned capability surface and
// the provider identifiers — the contracts every later task builds on. The
// test skips when the artifact is absent; CI builds it before go test.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginID   = "easycliproxyapi-chatgpt-provider"
	providerID = "chatgpt-proxy"
	githubRepo = "https://github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin"
)

// smokeHost is the fake EasyCLI host: it records every host.* callback and
// answers with an empty OK envelope.
type smokeHost struct {
	mu    sync.Mutex
	calls []string
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func (h *smokeHost) call(method string, _ []byte) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.mu.Unlock()
	return hostOK(map[string]any{}), nil
}

func pluginPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "plugins", "linux", "amd64", pluginID+".so"))
	if err != nil {
		t.Fatalf("resolve plugin path: %v", err)
	}
	return path
}

func TestLoadDryRunRealPluginArtifact(t *testing.T) {
	path := pluginPath(t)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("plugin artifact not built (%s); CI builds it before go test", path)
	}

	host := &smokeHost{}
	SetHost(host.call)
	t.Cleanup(func() { SetHost(nil) })

	loader, err := Open(path)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	t.Cleanup(loader.Close)
	if err := loader.Init(); err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	if got := loader.ABI(); got != int(pluginabi.ABIVersion) {
		t.Fatalf("plugin ABI version = %d, want %d", got, pluginabi.ABIVersion)
	}

	call := func(method string, request []byte) pluginabi.Envelope {
		t.Helper()
		raw, err := loader.RPC(method, request)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if len(raw) == 0 {
			t.Fatalf("%s: empty response", method)
		}
		var env pluginabi.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: undecodable envelope %s", method, raw)
		}
		return env
	}

	// --- registration identity ------------------------------------------
	registerRequest, _ := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte("{}")),
		"schema_version": int(pluginabi.SchemaVersion),
	})
	env := call(pluginabi.MethodPluginRegister, registerRequest)
	if !env.OK || env.Error != nil {
		t.Fatalf("plugin.register rejected by the loaded plugin: %s", rawResult(env))
	}
	var registration struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  struct {
			AuthProvider       bool     `json:"auth_provider"`
			ModelProvider      bool     `json:"model_provider"`
			Executor           bool     `json:"executor"`
			ExecutorModelScope string   `json:"executor_model_scope"`
			InputFormats       []string `json:"executor_input_formats"`
			OutputFormats      []string `json:"executor_output_formats"`
			QuotaProvider      bool     `json:"quota_provider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Name != pluginID ||
		registration.Metadata.Version == "" ||
		registration.Metadata.GitHubRepository != githubRepo {
		t.Fatalf("identity = %+v", registration.Metadata)
	}
	if !registration.Capabilities.AuthProvider || !registration.Capabilities.ModelProvider ||
		!registration.Capabilities.Executor || !registration.Capabilities.QuotaProvider {
		t.Fatalf("capabilities booleans wrong: %+v", registration.Capabilities)
	}
	if registration.Capabilities.ExecutorModelScope != "oauth" {
		t.Fatalf("executor_model_scope = %q, want oauth", registration.Capabilities.ExecutorModelScope)
	}
	if strings.Join(registration.Capabilities.InputFormats, ",") != "openai,openai-image,openai-response" ||
		strings.Join(registration.Capabilities.OutputFormats, ",") != "openai,openai-image,openai-response" {
		t.Fatalf("executor formats in=%v out=%v", registration.Capabilities.InputFormats, registration.Capabilities.OutputFormats)
	}

	// --- provider identifiers -------------------------------------------
	assertIdentifier := func(method string) {
		t.Helper()
		env := call(method, nil)
		if !env.OK {
			t.Fatalf("%s rejected: %s", method, rawResult(env))
		}
		var id struct {
			Identifier string `json:"identifier"`
		}
		if err := json.Unmarshal(env.Result, &id); err != nil {
			t.Fatalf("%s decode result: %v", method, err)
		}
		if id.Identifier != providerID {
			t.Fatalf("%s identifier = %q, want %q", method, id.Identifier, providerID)
		}
	}
	assertIdentifier(pluginabi.MethodAuthIdentifier)
	assertIdentifier(pluginabi.MethodExecutorIdentifier)

	// --- quota describe surface ------------------------------------------
	env = call(pluginabi.MethodQuotaDescribe, []byte(`{}`))
	if !env.OK {
		t.Fatalf("quota.describe rejected: %s", rawResult(env))
	}
	var describe pluginapi.QuotaDescribeResponse
	if err := json.Unmarshal(env.Result, &describe); err != nil {
		t.Fatalf("decode quota.describe: %v", err)
	}
	if len(describe.SupportedProviders) != 1 || describe.SupportedProviders[0] != providerID || describe.SupportsReset {
		t.Fatalf("quota.describe = %+v", describe)
	}

	// --- clean shutdown ----------------------------------------------------
	env = call(pluginabi.MethodPluginShutdown, nil)
	if !env.OK {
		t.Fatalf("plugin.shutdown failed: %s", rawResult(env))
	}
	loader.Shutdown()
}

func rawResult(env pluginabi.Envelope) string {
	if len(env.Result) > 0 {
		return string(env.Result)
	}
	if env.Error != nil {
		return env.Error.Code + ": " + env.Error.Message
	}
	return "(empty)"
}
