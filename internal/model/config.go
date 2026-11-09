package model

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Configuration defaults. Model names, polling cadence and the TLS mode are
// pinned here so every later task routes through one Config value; the wire
// contract (published ids, upstream model) is asserted by
// internal/plugin's config_test.go through the ParseConfig wrapper.
const (
	defaultModelPrefix       = "chatgpt-proxy"
	defaultImageModelName    = "gpt-image-2-proxy"
	defaultCodexImageModel   = "codex-gpt-image-2"
	defaultDrawingUpstream   = "gpt-5-5"
	defaultThinkingEffort    = "auto"
	defaultImagePollTimeout  = 120
	defaultImagePollInterval = 10
	defaultRequestTimeout    = 600
	defaultTLSMode           = "stdlib"
	tlsModeUtls              = "utls"

	// The oracle's search-flow constants (SEARCH_MODEL / SEARCH_TIMEOUT_SECS /
	// SEARCH_POLL_INTERVAL_SECS), surfaced as config so the poll budget is
	// tunable per deployment (and shrinkable to seconds in offline tests).
	defaultSearchUpstream = "gpt-5-5"
	defaultSearchTimeout  = 300
	defaultSearchInterval = 3
)

// Config is the validated plugin configuration. BaseURL is accepted but
// reserved — image responses are always inline b64_json regardless of its
// value (a real resolvable url would require a future pipeline that surfaces
// one); MessageAsError true means a drawing turn that replies with prose is
// surfaced as an error, not a silent 200.
// Declared here (not in internal/plugin) because internal/upstream consumes
// it too; internal/plugin aliases the type.
type Config struct {
	ModelPrefix           string
	ImageModelName        string
	CodexImageModelName   string
	DrawingUpstreamModel  string
	ThinkingEffort        string
	BaseURL               string
	MessageAsError        bool
	ImagePollTimeoutSecs  int
	ImagePollIntervalSecs int
	RequestTimeoutSecs    int
	TLSMode               string
	Retry401Refresh       bool

	SearchUpstreamModel    string
	SearchTimeoutSecs      int
	SearchPollIntervalSecs int
}

// rawConfig mirrors the YAML shape. Pointer fields distinguish "unset" (apply
// default) from an explicitly-set value, including empty strings, so validation
// can refuse them instead of silently defaulting.
type rawConfig struct {
	// Host-owned keys. CLIProxyAPI v8 injects "enabled" and "priority" into
	// every plugin's config_yaml before registration (normalizedConfigNode in
	// the host's internal/pluginhost/config.go); the host, not this plugin,
	// decides load state and order. Declared only so the fail-closed parser
	// accepts them; they never leak into Config.
	Enabled  *bool `yaml:"enabled"`
	Priority *int  `yaml:"priority"`

	ModelPrefix           *string `yaml:"model-prefix"`
	ImageModelName        *string `yaml:"image-model-name"`
	CodexImageModelName   *string `yaml:"codex-image-model-name"`
	DrawingUpstreamModel  *string `yaml:"drawing-upstream-model"`
	ThinkingEffort        *string `yaml:"thinking-effort"`
	BaseURL               *string `yaml:"base-url"`
	MessageAsError        *bool   `yaml:"message-as-error"`
	ImagePollTimeoutSecs  *int    `yaml:"image-poll-timeout-secs"`
	ImagePollIntervalSecs *int    `yaml:"image-poll-interval-secs"`
	RequestTimeoutSecs    *int    `yaml:"request-timeout-secs"`
	TLSMode               *string `yaml:"tls-mode"`
	Retry401Refresh       *bool   `yaml:"retry-401-refresh"`

	SearchUpstreamModel    *string `yaml:"search-upstream-model"`
	SearchTimeoutSecs      *int    `yaml:"search-timeout-secs"`
	SearchPollIntervalSecs *int    `yaml:"search-poll-interval-secs"`
}

// ParseConfig decodes the host-supplied config YAML, applies defaults and
// validates fail-closed. Unknown keys are rejected so a typo cannot silently
// disable a safety default; the error names the offending key but never
// echoes decoded values (secrets must not round-trip through messages).
func ParseConfig(rawYAML []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(rawYAML))
	dec.KnownFields(true)
	var raw rawConfig
	err := dec.Decode(&raw)
	if err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode config: %v", err)
	}

	c := Config{
		ModelPrefix:           orDefault(raw.ModelPrefix, defaultModelPrefix),
		ImageModelName:        orDefault(raw.ImageModelName, defaultImageModelName),
		CodexImageModelName:   orDefault(raw.CodexImageModelName, defaultCodexImageModel),
		DrawingUpstreamModel:  orDefault(raw.DrawingUpstreamModel, defaultDrawingUpstream),
		ThinkingEffort:        orDefault(raw.ThinkingEffort, defaultThinkingEffort),
		BaseURL:               orDefault(raw.BaseURL, ""),
		MessageAsError:        orDefault(raw.MessageAsError, true),
		ImagePollTimeoutSecs:  orDefault(raw.ImagePollTimeoutSecs, defaultImagePollTimeout),
		ImagePollIntervalSecs: orDefault(raw.ImagePollIntervalSecs, defaultImagePollInterval),
		RequestTimeoutSecs:    orDefault(raw.RequestTimeoutSecs, defaultRequestTimeout),
		TLSMode:               orDefault(raw.TLSMode, defaultTLSMode),
		Retry401Refresh:       orDefault(raw.Retry401Refresh, true),

		SearchUpstreamModel:    orDefault(raw.SearchUpstreamModel, defaultSearchUpstream),
		SearchTimeoutSecs:      orDefault(raw.SearchTimeoutSecs, defaultSearchTimeout),
		SearchPollIntervalSecs: orDefault(raw.SearchPollIntervalSecs, defaultSearchInterval),
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.ModelPrefix == "" {
		return fmt.Errorf("model-prefix: must not be empty")
	}
	if c.TLSMode != defaultTLSMode && c.TLSMode != tlsModeUtls {
		return fmt.Errorf("tls-mode: must be one of stdlib or utls")
	}
	if c.RequestTimeoutSecs <= 0 {
		return fmt.Errorf("request-timeout-secs: must be positive")
	}
	if c.ImagePollTimeoutSecs <= 0 {
		return fmt.Errorf("image-poll-timeout-secs: must be positive")
	}
	if c.ImagePollIntervalSecs <= 0 {
		return fmt.Errorf("image-poll-interval-secs: must be positive")
	}
	if c.ImagePollIntervalSecs > c.ImagePollTimeoutSecs {
		return fmt.Errorf("image-poll-interval-secs: must not exceed image-poll-timeout-secs")
	}
	if c.SearchTimeoutSecs <= 0 {
		return fmt.Errorf("search-timeout-secs: must be positive")
	}
	if c.SearchPollIntervalSecs <= 0 {
		return fmt.Errorf("search-poll-interval-secs: must be positive")
	}
	if c.SearchPollIntervalSecs > c.SearchTimeoutSecs {
		return fmt.Errorf("search-poll-interval-secs: must not exceed search-timeout-secs")
	}
	return nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
