package plugin

// Credential, Fingerprint and the storage helpers were moved to
// internal/model in Task 8 (the upstream package consumes them too, and the
// dependency direction is fixed: plugin → upstream → model). The aliases and
// wrappers below keep every existing plugin-side reference byte-identical.

import (
	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

type Credential = model.Credential
type Fingerprint = model.Fingerprint

func FromStorageJSON(raw []byte) (Credential, error) {
	return model.FromStorageJSON(raw)
}

func FileNameFor(cred Credential) string {
	return model.FileNameFor(cred)
}

func isOwnCredentialFile(raw []byte) bool {
	return model.IsOwnCredentialFile(raw)
}

func hostManagedMetadata(record map[string]any) map[string]any {
	return model.HostManagedMetadata(record)
}
