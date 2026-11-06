package plugin

// models.go implements the model_provider surface: model.static and
// model.for_auth.
//
// model.for_auth discovers the credential's upstream text catalog —
// GET /backend-api/models?history_and_training_disabled=false, the oracle's
// list_models route — and publishes it alongside the plugin's two image
// models. Snapshots are cached per credential fingerprint: a 300s TTL
// (oracle ModelCatalogService), replacement on success, and
// stale-while-unavailable serving of the last good snapshot on fetch
// failure (commandcode catalog.fail policy, oracle model_service.py:130,
// 136-137). Text models publish as <prefix>/openai/<slug> with Type "chat"
// — the model-vendor segment is the constant openai because the upstream
// catalog carries no vendor field, so nothing is invented. The image models
// publish as <prefix>/<image-model-name> and <prefix>/<codex-image-model-name>
// with Type "openai-image"; the codex route is published only when the
// credential's plan gates it (plus/pro/team). model.static serves the
// last-known-good union across all credentials plus both image models, so
// the catalog is non-empty on first boot without any fetch.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

const (
	// modelsFetchTimeout bounds one catalog fetch (oracle list_models
	// timeout=30, openai_backend_api.py:2742).
	modelsFetchTimeout = 30 * time.Second

	// modelCatalogTTL is how long one credential's snapshot stays fresh
	// before the next discovery re-fetches (oracle model_service.py:33
	// cache_ttl_seconds=300).
	modelCatalogTTL = 5 * time.Minute

	// upstreamModelsURL is the oracle's authed catalog route, query
	// included (openai_backend_api.py:2733). The base mirrors
	// upstream's pinned chatGPTBaseURL.
	upstreamModelsURL = "https://chatgpt.com/backend-api/models?history_and_training_disabled=false"

	// modelsPathTemplate carries the query-less route in the pinned
	// X-OpenAI-Target-Path/Route pair (oracle _headers(route) convention,
	// as in profile.go's pinned paths).
	modelsPathTemplate = "/backend-api/models"

	// textModelType classifies catalog text models. imageModelType mirrors
	// the host registry constant OpenAIImageModelType (SDK v8.0.0
	// internal/registry/model_registry.go:21) and is what routes our image
	// base names on the registry/normalizing path instead of the host's
	// hardcoded built-in set (Review Focus #4).
	textModelType  = "chat"
	imageModelType = "openai-image"
)

// textModel is one normalized /backend-api/models entry: the oracle reads
// only slug, created and owned_by (openai_backend_api.py:2753-2757) and
// defaults owned_by to "chatgpt" — never invented otherwise.
type textModel struct {
	slug    string
	created int64
	ownedBy string
}

// catalogEntry is one credential's last good snapshot.
type catalogEntry struct {
	models    []textModel
	expiresAt time.Time
}

// modelCatalog keeps per-credential snapshots keyed by credential
// fingerprint. All accessors are safe for concurrent use.
type modelCatalog struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{entries: make(map[string]catalogEntry)}
}

// reset drops every snapshot (shutdown: a fresh process starts fresh).
func (c *modelCatalog) reset() {
	c.mu.Lock()
	c.entries = make(map[string]catalogEntry)
	c.mu.Unlock()
}

// refresh returns the credential's current snapshot: the cached entry while
// it is fresh, a newly fetched entry when it is not, or — stale-while-
// unavailable — the last good entry when the fetch fails. err is non-nil
// only when there is no snapshot to serve.
func (c *modelCatalog) refresh(ctx context.Context, u upstream.Upstream, key string, cred *model.Credential, callbackID string) (catalogEntry, error) {
	c.mu.Lock()
	if prev, ok := c.entries[key]; ok && time.Now().Before(prev.expiresAt) {
		c.mu.Unlock()
		return prev, nil
	}
	c.mu.Unlock()

	fresh, err := fetchModels(ctx, u, cred, callbackID)
	if err != nil {
		c.mu.Lock()
		if prev, ok := c.entries[key]; ok {
			// Serve the stale snapshot (commandcode SWR policy; the oracle
			// keeps cached catalogs on upstream failure, model_service.py
			// lines 130 and 136-137).
			c.mu.Unlock()
			return prev, nil
		}
		c.mu.Unlock()
		return catalogEntry{}, err
	}

	c.mu.Lock()
	c.entries[key] = fresh
	c.mu.Unlock()
	return fresh, nil
}

// union merges every credential's last good snapshot into one text catalog
// the way the oracle's list_models does (model_service.py:155-161,
// setdefault = first occurrence wins). Cache keys iterate in sorted order
// so the merge is deterministic.
func (c *modelCatalog) union() catalogEntry {
	c.mu.Lock()
	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]bool)
	models := make([]textModel, 0)
	for _, key := range keys {
		for _, tm := range c.entries[key].models {
			if seen[tm.slug] {
				continue
			}
			seen[tm.slug] = true
			models = append(models, tm)
		}
	}
	c.mu.Unlock()
	return catalogEntry{models: models}
}

// modelForAuthRequest mirrors the host wire shape of model.for_auth
// (pluginhost rpcAuthModelRequest, rpc_schema.go:75-78): the SDK request
// plus the callback id that scopes the host transport to this RPC.
type modelForAuthRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleModelForAuth implements model.for_auth: discover this credential's
// upstream text catalog and publish it with the plugin's image models.
func (m *Manager) handleModelForAuth(request []byte) ([]byte, error) {
	var req modelForAuthRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed model.for_auth request", 400)), nil
	}
	if req.AuthProvider != "" && req.AuthProvider != providerKey {
		return errEnvelope(typedErr("invalid_request", "model.for_auth handles "+providerKey+" credentials only", 400)), nil
	}
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return errEnvelope(typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)), nil
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	// Materialize the wire fingerprint BEFORE the fetch: the session headers
	// need it, and the per-credential cache key is derived from it. When the
	// ids were empty they are fresh random values this call generated — the
	// AuthUpdate below persists them back (headers.go contract) so the cache
	// key and the upstream fingerprint stay stable across calls.
	hadFingerprint := cred.Fp.OAIDeviceID != "" && cred.Fp.OAISessionID != ""
	upstream.SessionHeaders(&cred, modelsPathTemplate)
	key := fingerprintKey(&cred)

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return errEnvelope(typedErr("model_failure", "upstream transport unavailable: "+err.Error(), 500)), nil
	}

	entry, err := m.models.refresh(context.Background(), u, key, &cred, req.HostCallbackID)
	if err != nil {
		return errEnvelope(modelFailureError(err)), nil
	}

	resp := publishedModels(cfg, entry, codexPlanAllowed(cred.Plan))
	if !hadFingerprint {
		resp.AuthUpdate = pluginapi.AuthData{
			Provider:    providerKey,
			StorageJSON: cred.StorageJSON(),
			Metadata:    req.Metadata,
		}
	}
	return ok(resp), nil
}

// handleModelStatic implements model.static: the last-known-good union of
// every credential's snapshot plus both image models — non-empty on first
// boot without any fetch. The host sends a StaticModelRequest; nothing in
// it is needed.
func (m *Manager) handleModelStatic(_ []byte) ([]byte, error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	return ok(publishedModels(cfg, m.models.union(), true)), nil
}

// fingerprintKey derives the per-credential cache key from the wire
// fingerprint ids (headers.go ensureFingerprint): random per credential,
// stable for the account's lifetime once persisted.
func fingerprintKey(cred *model.Credential) string {
	sum := sha256.Sum256([]byte(cred.Fp.OAIDeviceID + "\x00" + cred.Fp.OAISessionID))
	return hex.EncodeToString(sum[:8])
}

// codexPlanAllowed gates the codex image route: Plus/Team/Pro publish it;
// free, enterprise and an unknown plan do not.
func codexPlanAllowed(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "plus", "pro", "team":
		return true
	default:
		return false
	}
}

// publishedModels maps one snapshot to the host ModelInfos: text models as
// <prefix>/openai/<slug> with Type "chat", plus the plugin's image models
// with Type "openai-image". The list is sorted by ID for a stable wire
// shape (the oracle sorts its catalog by id too).
func publishedModels(cfg Config, entry catalogEntry, includeCodexImage bool) pluginapi.ModelResponse {
	models := make([]pluginapi.ModelInfo, 0, len(entry.models)+2)
	for _, tm := range entry.models {
		models = append(models, pluginapi.ModelInfo{
			ID:          cfg.ModelPrefix + "/openai/" + tm.slug,
			Object:      "model",
			Created:     tm.created,
			OwnedBy:     tm.ownedBy,
			Type:        textModelType,
			DisplayName: tm.slug,
		})
	}
	models = append(models, pluginapi.ModelInfo{
		ID:          cfg.ModelPrefix + "/" + cfg.ImageModelName,
		Object:      "model",
		OwnedBy:     providerKey,
		Type:        imageModelType,
		DisplayName: cfg.ImageModelName,
	})
	if includeCodexImage {
		models = append(models, pluginapi.ModelInfo{
			ID:          cfg.ModelPrefix + "/" + cfg.CodexImageModelName,
			Object:      "model",
			OwnedBy:     providerKey,
			Type:        imageModelType,
			DisplayName: cfg.CodexImageModelName,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return pluginapi.ModelResponse{Provider: providerKey, Models: models}
}

// modelFetchError is the typed fetch failure. Its text carries only the
// classification — never upstream body bytes (secret rule).
type modelFetchError struct {
	status    int
	message   string
	retryable bool
}

func (e *modelFetchError) Error() string {
	return e.message
}

// modelFailureError wraps a fetch failure as the model_failure envelope,
// propagating the upstream status and retryability when classified.
func modelFailureError(err error) *pluginabi.Error {
	e := typedErr("model_failure", err.Error())
	var fetchErr *modelFetchError
	if errors.As(err, &fetchErr) {
		if fetchErr.status != 0 {
			e.HTTPStatus = fetchErr.status
		}
		e.Retryable = fetchErr.retryable
	}
	return e
}

// fetchModels GETs the credential's catalog with the pinned session headers
// plus the credential bearer (oracle list_models) and normalizes the body.
func fetchModels(ctx context.Context, u upstream.Upstream, cred *model.Credential, callbackID string) (catalogEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()

	headers := upstream.Authed(cred, upstream.SessionHeaders(cred, modelsPathTemplate))
	resp, err := u.Do(ctx, upstream.Req{
		Method:         http.MethodGet,
		URL:            upstreamModelsURL,
		Headers:        headers,
		HostCallbackID: callbackID,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return catalogEntry{}, &modelFetchError{
				status:    http.StatusGatewayTimeout,
				message:   "model discovery timed out",
				retryable: true,
			}
		}
		return catalogEntry{}, &modelFetchError{
			message:   "model discovery request failed",
			retryable: true,
		}
	}
	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
	case resp.StatusCode == http.StatusUnauthorized:
		return catalogEntry{}, &modelFetchError{
			status:  resp.StatusCode,
			message: "the upstream rejected the bearer token",
		}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return catalogEntry{}, &modelFetchError{
			status:    resp.StatusCode,
			message:   fmt.Sprintf("the upstream returned status %d for model discovery", resp.StatusCode),
			retryable: true,
		}
	default:
		return catalogEntry{}, &modelFetchError{
			status:  resp.StatusCode,
			message: fmt.Sprintf("the upstream returned status %d for model discovery", resp.StatusCode),
		}
	}

	models, err := parseModels(resp.Body)
	if err != nil {
		return catalogEntry{}, err
	}
	return catalogEntry{models: models, expiresAt: time.Now().Add(modelCatalogTTL)}, nil
}

// upstreamModelsPayload is the raw /backend-api/models body: the oracle
// reads response.json().get("models", []) — a missing key is an EMPTY
// catalog (oracle line 2747), not an error.
type upstreamModelsPayload struct {
	Models []json.RawMessage `json:"models"`
}

// upstreamModelsItem is one catalog entry; the oracle reads only slug,
// created and owned_by (openai_backend_api.py:2753-2757).
type upstreamModelsItem struct {
	Slug    string `json:"slug"`
	Created any    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// parseModels normalizes the body the way the oracle does (lines
// 2747-2762): non-object entries skipped, blank slugs skipped, duplicate
// slugs deduped with the first occurrence winning, created coerced to an
// int (missing reads 0), owned_by defaulted to "chatgpt". Malformed JSON is
// a typed fetch failure — never an intended clear.
func parseModels(body []byte) ([]textModel, error) {
	var payload upstreamModelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &modelFetchError{message: "the upstream models response is not valid JSON"}
	}
	seen := make(map[string]bool, len(payload.Models))
	models := make([]textModel, 0, len(payload.Models))
	for _, raw := range payload.Models {
		var item upstreamModelsItem
		if err := json.Unmarshal(raw, &item); err != nil {
			continue // non-object entry (oracle isinstance check, line 2748)
		}
		slug := strings.TrimSpace(item.Slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		ownedBy := strings.TrimSpace(item.OwnedBy)
		if ownedBy == "" {
			ownedBy = "chatgpt"
		}
		models = append(models, textModel{slug: slug, created: asCreatedInt(item.Created), ownedBy: ownedBy})
	}
	return models, nil
}

// asCreatedInt is the oracle's `int(item.get("created") or 0)`: numbers pass
// through, numeric strings are parsed, everything else reads as 0
// (never invented).
func asCreatedInt(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}
