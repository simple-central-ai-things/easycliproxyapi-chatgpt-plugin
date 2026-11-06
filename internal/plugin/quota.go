package plugin

// quota.go implements the native-panel quota card surface: quota.describe
// (static metadata), quota.fetch (the account-status triple probe mapped to
// normalized cards) and quota.reset (typed refusal).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// quotaMaxTimeout bounds one quota.fetch: the management UI is a synchronous
// user-initiated call (commandcode's quotaMaxTimeout precedent).
const quotaMaxTimeout = 30 * time.Second

// handleQuotaDescribe returns the static quota metadata. It cannot fail —
// the cards render on the host's native auth-file surface.
func handleQuotaDescribe() []byte {
	return ok(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        "ChatGPT (" + providerKey + ")",
		SupportsReset:      false,
	})
}

// quotaFetchRequest mirrors the host wire shape of quota.fetch
// (rpcQuotaFetchRequest): the SDK request, no host_callback_id — the host
// scopes this RPC's transport to the plugin instance context.
type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
}

// handleQuotaFetch decodes the credential from the request's StorageJSON,
// runs the account-status triple probe over the host transport and answers
// with the normalized cards for whatever the probe returned.
func (m *Manager) handleQuotaFetch(request []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errEnvelope(typedErr("invalid_request", "malformed quota.fetch request", 400)), nil
	}
	if req.Provider != "" && req.Provider != providerKey {
		return errEnvelope(typedErr("quota_failure", "quota.fetch handles "+providerKey+" credentials only", 400)), nil
	}
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return errEnvelope(typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)), nil
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return errEnvelope(typedErr("quota_failure", "upstream transport unavailable: "+err.Error(), 500)), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), quotaMaxTimeout)
	defer cancel()

	profile, err := upstream.ProbeProfile(ctx, u, cred)
	if err != nil {
		return errEnvelope(quotaFailureError(err)), nil
	}
	return ok(quotaCards(profile)), nil
}

// quotaFailureError wraps a probe failure as the quota_failure envelope.
// Typed ConvError statuses propagate (invalid_token keeps its 401 so the
// host can react); error text carries only the typed message — ConvError
// messages never embed upstream payload bytes (secret rule).
func quotaFailureError(err error) *pluginabi.Error {
	e := typedErr("quota_failure", err.Error())
	var convErr *upstream.ConvError
	if errors.As(err, &convErr) {
		if convErr.HTTPStatus != 0 {
			e.HTTPStatus = convErr.HTTPStatus
		}
		e.Retryable = convErr.Retryable
	}
	return e
}

// handleQuotaReset refuses quota.reset with the typed unsupported code: the
// upstream windows are rate limits that reset on their own schedule, and the
// oracle exposes no reset endpoint.
func handleQuotaReset() []byte {
	return errEnvelope(typedErr("unsupported", "quota.reset is not supported by "+providerKey, 501))
}

// quotaCards maps one profile to the host's normalized quota shape. The
// "never invent data" rule (commandcode quota_provider.go): the cards
// reflect only what upstream returned — an unknown plan yields no
// subscription, an absent window yields no bucket, and a window without a
// reported capacity renders full (1.0) rather than an invented fraction or
// an exhausted reading.
func quotaCards(profile upstream.Profile) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{}
	if profile.Plan != "" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: profile.Plan}
	}
	if len(profile.LimitsProgress) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(profile.LimitsProgress))
		for _, w := range profile.LimitsProgress {
			bucket := pluginapi.QuotaBucket{
				Window:            w.FeatureName,
				RemainingFraction: 1.0,
			}
			if w.Max > 0 {
				bucket.RemainingFraction = float64(w.Remaining) / float64(w.Max)
			}
			if !w.RestoreAt.IsZero() {
				bucket.ResetTime = w.RestoreAt.UTC().Format(time.RFC3339)
			}
			buckets = append(buckets, bucket)
		}
		out.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "Rate limits",
			Buckets:     buckets,
		}}
	}
	return out
}
