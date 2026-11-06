package plugin

// Config and ParseConfig were moved to internal/model in Task 8 (the
// upstream package consumes the config too — NewUpstream reads TLSMode,
// NewPollOptions reads the poll cadence — and the dependency direction is
// fixed: plugin → upstream → model). The alias and wrapper below keep every
// existing plugin-side reference byte-identical.

import (
	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

type Config = model.Config

func ParseConfig(rawYAML []byte) (Config, error) {
	return model.ParseConfig(rawYAML)
}
