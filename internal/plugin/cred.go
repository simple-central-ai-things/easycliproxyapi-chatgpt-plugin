package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Credential is one ChatGPT consumer-account credential record: the OAuth
// token triple plus the identity, plan, fingerprint and refresh-state facts
// every later task (login, upstream headers, quota, executor refresh-retry)
// reads and writes. It is the single source of truth for the auth-file
// payload; the executor receives it through AuthData.StorageJSON.
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	AccountID    string `json:"account_id"`
	// Plan is "free|plus|pro|team|enterprise"; "" = unknown.
	Plan string `json:"plan"`
	// SourceType is "web|codex|oauth_login".
	SourceType string      `json:"source_type"`
	Fp         Fingerprint `json:"fp"`
	// LastRefreshAt is when the token endpoint was last talked to, success
	// or failure; the refresh backoff is anchored on it.
	LastRefreshAt    time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshError string    `json:"last_refresh_error,omitempty"`
}

// Fingerprint holds the upstream wire fingerprint fields the executor keeps
// byte-identical to the reference so requests look like a stock web client.
type Fingerprint struct {
	UserAgent    string `json:"user_agent,omitempty"`
	Impersonate  string `json:"impersonate,omitempty"`
	OAIDeviceID  string `json:"oai_device_id,omitempty"`
	OAISessionID string `json:"oai_session_id,omitempty"`
}

// storageRecord is the on-disk auth-file payload for one credential. The
// host forces "type" to the provider key on persist, so the field is
// redundant on the live path but pinned here: it is what every consumer
// (parse, refresh, offline tooling) checks first.
type storageRecord struct {
	Type string `json:"type"`
	Credential
}

// StorageJSON serializes the credential in the auth-file shape:
// {"type":"chatgpt-proxy","access_token":...,"refresh_token":...,...,
// "fp":{...},"last_refresh_at":rfc3339}. Operational fields are omitted
// while empty so a fresh record stays lean; identity fields are always
// present.
func (c Credential) StorageJSON() []byte {
	raw, _ := json.Marshal(storageRecord{Type: providerKey, Credential: c})
	return raw
}

// FromStorageJSON decodes an auth-file payload and verifies the type
// discriminator. It tolerates host-managed extra keys (label, disabled,
// priority, weight, ...) — those are carried in AuthData.Metadata, not the
// Credential — and rejects anything else.
func FromStorageJSON(raw []byte) (Credential, error) {
	var rec storageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Credential{}, fmt.Errorf("credential storage is not valid JSON: %w", err)
	}
	if rec.Type != providerKey {
		return Credential{}, fmt.Errorf("credential storage type %q is not %q", rec.Type, providerKey)
	}
	return rec.Credential, nil
}

// isOwnCredentialFile reports whether raw is a JSON record discriminated as
// this provider. Anything else — built-in codex files, foreign providers,
// non-JSON payloads — stays the host's concern: parse answers
// Handled:false for it.
func isOwnCredentialFile(raw []byte) bool {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return false
	}
	return head.Type == providerKey
}

// credentialStorageKeys are the keys this plugin owns in the auth-file
// payload. Every other key present in a record is host-managed and must
// survive refresh rewrites, so it is carried in AuthData.Metadata instead.
var credentialStorageKeys = map[string]bool{
	"type":               true,
	"access_token":       true,
	"refresh_token":      true,
	"id_token":           true,
	"email":              true,
	"user_id":            true,
	"account_id":         true,
	"plan":               true,
	"source_type":        true,
	"fp":                 true,
	"last_refresh_at":    true,
	"last_refresh_error": true,
}

// hostManagedMetadata splits a decoded auth-file record into the keys the
// host manages (label, disabled, priority, weight, note, ...). The host
// merges this map over StorageJSON when it persists, so returning the
// credential-owned keys here would let stale values clobber a normalized
// record — they are filtered out.
func hostManagedMetadata(record map[string]any) map[string]any {
	if record == nil {
		return nil
	}
	out := make(map[string]any, len(record))
	for key, value := range record {
		if credentialStorageKeys[key] {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FileNameFor derives the stable auth-file name for a credential:
// chatgpt-proxy-<first 16 hex of sha256(seed)>.json where seed is the first
// known identity fact (email, then account id, then access token). Names are
// hashes — never emails — per the secret rule.
func FileNameFor(cred Credential) string {
	seed := strings.TrimSpace(cred.Email)
	if seed == "" {
		seed = strings.TrimSpace(cred.AccountID)
	}
	if seed == "" {
		seed = strings.TrimSpace(cred.AccessToken)
	}
	sum := sha256.Sum256([]byte(seed))
	return providerKey + "-" + hex.EncodeToString(sum[:8]) + ".json"
}
