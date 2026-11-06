package plugin

import (
	"strings"
	"testing"
)

// TestConfigDefaults pins the fail-closed config surface: an empty document
// ({} or no bytes at all) must yield exactly the defaults every later task
// routes models, polling and timeouts through.
func TestConfigDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"empty map", []byte("{}")},
		{"no bytes", []byte("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig(tc.raw)
			if err != nil {
				t.Fatalf("ParseConfig(%q): %v", tc.raw, err)
			}
			if c.ModelPrefix != "chatgpt-proxy" {
				t.Errorf("ModelPrefix = %q, want chatgpt-proxy", c.ModelPrefix)
			}
			if c.ImageModelName != "gpt-image-2-proxy" {
				t.Errorf("ImageModelName = %q, want gpt-image-2-proxy", c.ImageModelName)
			}
			if c.CodexImageModelName != "codex-gpt-image-2" {
				t.Errorf("CodexImageModelName = %q, want codex-gpt-image-2", c.CodexImageModelName)
			}
			if c.DrawingUpstreamModel != "gpt-5-5" {
				t.Errorf("DrawingUpstreamModel = %q, want gpt-5-5", c.DrawingUpstreamModel)
			}
			if c.ThinkingEffort != "auto" {
				t.Errorf("ThinkingEffort = %q, want auto", c.ThinkingEffort)
			}
			if c.BaseURL != "" {
				t.Errorf("BaseURL = %q, want empty (b64_json fallback)", c.BaseURL)
			}
			if !c.MessageAsError {
				t.Errorf("MessageAsError = false, want true")
			}
			if c.ImagePollTimeoutSecs != 120 {
				t.Errorf("ImagePollTimeoutSecs = %d, want 120", c.ImagePollTimeoutSecs)
			}
			if c.ImagePollIntervalSecs != 10 {
				t.Errorf("ImagePollIntervalSecs = %d, want 10", c.ImagePollIntervalSecs)
			}
			if c.RequestTimeoutSecs != 600 {
				t.Errorf("RequestTimeoutSecs = %d, want 600", c.RequestTimeoutSecs)
			}
			if c.TLSMode != "stdlib" {
				t.Errorf("TLSMode = %q, want stdlib", c.TLSMode)
			}
			if !c.Retry401Refresh {
				t.Errorf("Retry401Refresh = false, want true")
			}
		})
	}
}

// TestConfigRejects pins the fail-closed validation: every operator-visible
// misconfiguration must be refused at parse time with a message that names
// the offending key, never applied silently.
func TestConfigRejects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yamlText string
		wantSub  string
	}{
		{"empty model-prefix", "model-prefix: \"\"\n", "model-prefix"},
		{"bad tls-mode", "tls-mode: openssl\n", "tls-mode"},
		{"zero request-timeout", "request-timeout-secs: 0\n", "request-timeout-secs"},
		{"negative request-timeout", "request-timeout-secs: -5\n", "request-timeout-secs"},
		{"unknown key", "bogus-key: 1\n", "bogus-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tc.yamlText))
			if err == nil {
				t.Fatalf("ParseConfig(%q) = %+v, want error", tc.yamlText, c)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not name %q", err, tc.wantSub)
			}
		})
	}
}

// TestConfigHostInjectedKeys pins that the host-owned keys CLIProxyAPI v8
// injects into every plugin's config_yaml (normalizedConfigNode in the
// host's internal/pluginhost/config.go appends "enabled" and "priority"
// before handing the block to the plugin) are accepted and ignored — the
// sibling plugins tolerate them; a fail-closed parser that rejected them
// would refuse registration of any block the live host sends.
func TestConfigHostInjectedKeys(t *testing.T) {
	raw := []byte(
		"enabled: true\n" +
			"priority: 0\n" +
			"tls-mode: stdlib\n" +
			"model-prefix: chatgpt-proxy\n",
	)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig with host-injected keys: %v", err)
	}
	if c.TLSMode != "stdlib" || c.ModelPrefix != "chatgpt-proxy" {
		t.Errorf("plugin keys drifted next to host keys: %+v", c)
	}
	// The rest stays at defaults.
	if c.RequestTimeoutSecs != 600 || c.ImagePollTimeoutSecs != 120 {
		t.Errorf("unset defaults drifted: %+v", c)
	}
}

// TestConfigOverrides pins that explicit values are honored, so the defaults
// above are proven to be defaults and not constants.
func TestConfigOverrides(t *testing.T) {
	raw := []byte(
		"tls-mode: utls\n" +
			"model-prefix: custom-prefix\n" +
			"base-url: https://upstream.invalid/api\n" +
			"message-as-error: false\n" +
			"image-poll-timeout-secs: 30\n" +
			"retry-401-refresh: false\n",
	)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if c.TLSMode != "utls" {
		t.Errorf("TLSMode = %q, want utls", c.TLSMode)
	}
	if c.ModelPrefix != "custom-prefix" {
		t.Errorf("ModelPrefix = %q, want custom-prefix", c.ModelPrefix)
	}
	if c.BaseURL != "https://upstream.invalid/api" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.MessageAsError {
		t.Errorf("MessageAsError = true, want false")
	}
	if c.ImagePollTimeoutSecs != 30 {
		t.Errorf("ImagePollTimeoutSecs = %d, want 30", c.ImagePollTimeoutSecs)
	}
	if c.Retry401Refresh {
		t.Errorf("Retry401Refresh = true, want false")
	}
	// Unset fields keep their defaults.
	if c.ImagePollIntervalSecs != 10 || c.RequestTimeoutSecs != 600 {
		t.Errorf("unset defaults drifted: %+v", c)
	}
}
