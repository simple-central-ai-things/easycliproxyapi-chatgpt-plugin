package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// quota_test.go pins the native-panel quota card surface: quota.describe
// metadata, quota.fetch's Profile→cards mapping (the "never invent data"
// rule), quota.reset's typed unsupported refusal, and Review Focus #6 — no
// token bytes in any response field.

const (
	quotaMeBody      = `{"email":"card-user@example.com","id":"user-987"}`
	quotaAccountBody = `{"accounts":{"default":{"account":{"plan_type":"plus","account_user_role":"admin","is_deactivated":false}}}}`

	quotaProbeMeURL      = "https://chatgpt.com/backend-api/me"
	quotaProbeInitURL    = "https://chatgpt.com/backend-api/conversation/init"
	quotaProbeAccountURL = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480"
)

// quotaFakeCaller is the offline host seam for quota tests. The probe's
// three legs arrive in nondeterministic order (they run in parallel), so
// responses are URL-keyed rather than queued.
type quotaFakeCaller struct {
	mu      sync.Mutex
	calls   []hostCallRecord
	scripts map[string]hostHTTPResponse
}

func (f *quotaFakeCaller) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, hostCallRecord{method: method, request: append([]byte(nil), request...)})
	f.mu.Unlock()
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	var req struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(request, &req)
	f.mu.Lock()
	resp, ok := f.scripts[req.URL]
	f.mu.Unlock()
	if !ok {
		return hostOK(hostHTTPResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"no script"}`)}), nil
	}
	return hostOK(resp), nil
}

func (f *quotaFakeCaller) probeURLs() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, record := range f.calls {
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(record.request, &req) == nil {
			out[req.URL] = true
		}
	}
	return out
}

func quotaScript(body string) hostHTTPResponse {
	return hostHTTPResponse{StatusCode: http.StatusOK, Body: []byte(body)}
}

func quotaFetchBody(t *testing.T, cred Credential) []byte {
	t.Helper()
	body, err := json.Marshal(pluginapi.QuotaFetchRequest{
		AuthIndex:   "0",
		AuthID:      "auth-1",
		Provider:    providerKey,
		StorageJSON: cred.StorageJSON(),
	})
	if err != nil {
		t.Fatalf("marshal quota.fetch request: %v", err)
	}
	return body
}

func quotaManager(t *testing.T, host caller) *Manager {
	t.Helper()
	m, err := New(host)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func decodeQuotaFetchResponse(t *testing.T, resp []byte) pluginapi.QuotaFetchResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("quota.fetch envelope not OK: %s", resp)
	}
	var out pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode quota.fetch result: %v (%s)", err, env.Result)
	}
	return out
}

// ---------------------------------------------------------------------------
// TestQuotaDescribe — static metadata, exact values.
// ---------------------------------------------------------------------------

func TestQuotaDescribe(t *testing.T) {
	m := quotaManager(t, &quotaFakeCaller{})
	resp, err := m.Handle(pluginabi.MethodQuotaDescribe, nil)
	if err != nil {
		t.Fatalf("Handle(quota.describe): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("quota.describe envelope not OK: %s", resp)
	}
	var out pluginapi.QuotaDescribeResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode quota.describe result: %v (%s)", err, env.Result)
	}
	if len(out.SupportedProviders) != 1 || out.SupportedProviders[0] != providerKey {
		t.Fatalf("SupportedProviders = %v, want [%s]", out.SupportedProviders, providerKey)
	}
	if out.DisplayName != "ChatGPT (chatgpt-proxy)" {
		t.Fatalf("DisplayName = %q, want ChatGPT (chatgpt-proxy)", out.DisplayName)
	}
	if out.SupportsReset {
		t.Fatalf("SupportsReset = true, want false")
	}
}

// ---------------------------------------------------------------------------
// TestQuotaFetchMapsWindows — one bucket per limits_progress window with the
// exact fraction (remaining/max), ResetTime (RFC3339) from reset_after and
// the feature_name as Window label.
// ---------------------------------------------------------------------------

func TestQuotaFetchMapsWindows(t *testing.T) {
	cred := Credential{AccessToken: "fetch-access-token"}
	initBody := `{"default_model_slug":"gpt-5-2-prod","limits_progress":[{"feature_name":"primary_limits","remaining":10,"max":40,"reset_after":"2026-11-07T09:00:00Z"},{"feature_name":"image_gen","remaining":27,"max":100,"reset_after":"2026-11-07T10:30:00Z"}]}`

	fake := &quotaFakeCaller{scripts: map[string]hostHTTPResponse{
		quotaProbeMeURL:      quotaScript(quotaMeBody),
		quotaProbeInitURL:    quotaScript(initBody),
		quotaProbeAccountURL: quotaScript(quotaAccountBody),
	}}
	m := quotaManager(t, fake)
	resp, err := m.Handle(pluginabi.MethodQuotaFetch, quotaFetchBody(t, cred))
	if err != nil {
		t.Fatalf("Handle(quota.fetch): %v", err)
	}
	out := decodeQuotaFetchResponse(t, resp)

	// The handler ran the full probe triple against the host transport.
	urls := fake.probeURLs()
	if len(urls) != 3 || !urls[quotaProbeMeURL] || !urls[quotaProbeInitURL] || !urls[quotaProbeAccountURL] {
		t.Fatalf("quota.fetch probed %v, want the exact triple", urls)
	}

	if out.Subscription == nil || out.Subscription.Plan != "plus" {
		t.Fatalf("Subscription = %+v, want plan plus from the accounts.check leg", out.Subscription)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("Groups = %d, want one group holding the windows", len(out.Groups))
	}
	buckets := out.Groups[0].Buckets
	if len(buckets) != 2 {
		t.Fatalf("Buckets = %d, want one per limits_progress window", len(buckets))
	}
	if buckets[0].Window != "primary_limits" || buckets[0].RemainingFraction != 0.25 || buckets[0].ResetTime != "2026-11-07T09:00:00Z" {
		t.Fatalf("bucket[0] = %+v, want primary_limits / 10 of 40 / 2026-11-07T09:00:00Z", buckets[0])
	}
	if buckets[1].Window != "image_gen" || buckets[1].RemainingFraction != 0.27 || buckets[1].ResetTime != "2026-11-07T10:30:00Z" {
		t.Fatalf("bucket[1] = %+v, want image_gen / 27 of 100 / 2026-11-07T10:30:00Z", buckets[1])
	}

	t.Run("unknown-capacity window renders full, not invented", func(t *testing.T) {
		fake2 := &quotaFakeCaller{scripts: map[string]hostHTTPResponse{
			quotaProbeMeURL:      quotaScript(quotaMeBody),
			quotaProbeInitURL:    quotaScript(`{"limits_progress":[{"feature_name":"weekly","remaining":3}]}`),
			quotaProbeAccountURL: quotaScript(quotaAccountBody),
		}}
		resp2, err := quotaManager(t, fake2).Handle(pluginabi.MethodQuotaFetch, quotaFetchBody(t, cred))
		if err != nil {
			t.Fatalf("Handle(quota.fetch): %v", err)
		}
		out2 := decodeQuotaFetchResponse(t, resp2)
		if len(out2.Groups) != 1 || len(out2.Groups[0].Buckets) != 1 {
			t.Fatalf("Groups = %+v, want the single weekly window", out2.Groups)
		}
		b := out2.Groups[0].Buckets[0]
		if b.RemainingFraction != 1.0 {
			t.Fatalf("RemainingFraction = %v, want 1.0 for a window without a max (commandcode unknown-cap precedent)", b.RemainingFraction)
		}
		if b.ResetTime != "" {
			t.Fatalf("ResetTime = %q, want empty when upstream returned no reset_after", b.ResetTime)
		}
	})
}

// ---------------------------------------------------------------------------
// TestQuotaFetchNeverInvents — the card reflects only what upstream returned:
// an empty Profile yields no subscription, no groups and no summary; an empty
// limits_progress yields zero groups.
// ---------------------------------------------------------------------------

func TestQuotaFetchNeverInvents(t *testing.T) {
	t.Run("empty profile invents nothing", func(t *testing.T) {
		out := quotaCards(upstream.Profile{})
		if out.Subscription != nil {
			t.Fatalf("Subscription = %+v, want nil for an unknown plan", out.Subscription)
		}
		if len(out.Groups) != 0 || len(out.Summary) != 0 {
			t.Fatalf("Groups=%d Summary=%d, want zero (absent window → no bucket)", len(out.Groups), len(out.Summary))
		}
	})

	t.Run("empty limits_progress yields zero groups", func(t *testing.T) {
		cred := Credential{AccessToken: "never-invent-token"}
		fake := &quotaFakeCaller{scripts: map[string]hostHTTPResponse{
			quotaProbeMeURL:      quotaScript(quotaMeBody),
			quotaProbeInitURL:    quotaScript(`{"default_model_slug":"gpt-5-2-prod","limits_progress":[]}`),
			quotaProbeAccountURL: quotaScript(quotaAccountBody),
		}}
		resp, err := quotaManager(t, fake).Handle(pluginabi.MethodQuotaFetch, quotaFetchBody(t, cred))
		if err != nil {
			t.Fatalf("Handle(quota.fetch): %v", err)
		}
		out := decodeQuotaFetchResponse(t, resp)
		if out.Subscription == nil || out.Subscription.Plan != "plus" {
			t.Fatalf("Subscription = %+v, want the plan the account leg returned", out.Subscription)
		}
		if len(out.Groups) != 0 {
			t.Fatalf("Groups = %d, want zero (the init leg returned no windows)", len(out.Groups))
		}
	})
}

// ---------------------------------------------------------------------------
// TestQuotaResetUnsupported — typed envelope code unsupported.
// ---------------------------------------------------------------------------

func TestQuotaResetUnsupported(t *testing.T) {
	m := quotaManager(t, &quotaFakeCaller{})
	body, _ := json.Marshal(pluginapi.QuotaResetRequest{
		AuthID:   "auth-1",
		Provider: providerKey,
	})
	resp, err := m.Handle(pluginabi.MethodQuotaReset, body)
	if err != nil {
		t.Fatalf("Handle(quota.reset): %v", err)
	}
	env := decodeErrorEnvelope(t, resp)
	if env.Error.Code != "unsupported" {
		t.Fatalf("error code = %q, want unsupported", env.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// TestQuotaNoSecretLeak (Review Focus #6) — the request carries a full
// credential StorageJSON; no token bytes may appear in any response field,
// on the success path OR a failure path.
// ---------------------------------------------------------------------------

func TestQuotaNoSecretLeak(t *testing.T) {
	secrets := []string{
		"SECRET-ACCESS-TOKEN-0123456789",
		"SECRET-REFRESH-TOKEN-0123456789",
		"SECRET-ID-TOKEN-0123456789",
	}
	cred := Credential{
		AccessToken:  secrets[0],
		RefreshToken: secrets[1],
		IDToken:      secrets[2],
		Email:        "leak@example.com",
	}

	assertNoLeak := func(t *testing.T, resp []byte) {
		t.Helper()
		for _, secret := range secrets {
			if strings.Contains(string(resp), secret) {
				t.Fatalf("response envelope carries %s bytes: %s", secret[:13], resp)
			}
		}
	}

	t.Run("fetch success", func(t *testing.T) {
		fake := &quotaFakeCaller{scripts: map[string]hostHTTPResponse{
			quotaProbeMeURL:      quotaScript(quotaMeBody),
			quotaProbeInitURL:    quotaScript(`{"limits_progress":[]}`),
			quotaProbeAccountURL: quotaScript(quotaAccountBody),
		}}
		resp, err := quotaManager(t, fake).Handle(pluginabi.MethodQuotaFetch, quotaFetchBody(t, cred))
		if err != nil {
			t.Fatalf("Handle(quota.fetch): %v", err)
		}
		assertNoLeak(t, resp)
	})

	t.Run("fetch failure", func(t *testing.T) {
		unauthorized := hostHTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":"unauthorized"}`)}
		fake := &quotaFakeCaller{scripts: map[string]hostHTTPResponse{
			quotaProbeMeURL:      unauthorized,
			quotaProbeInitURL:    unauthorized,
			quotaProbeAccountURL: unauthorized,
		}}
		resp, err := quotaManager(t, fake).Handle(pluginabi.MethodQuotaFetch, quotaFetchBody(t, cred))
		if err != nil {
			t.Fatalf("Handle(quota.fetch): %v", err)
		}
		env := decodeErrorEnvelope(t, resp)
		if env.Error.Code != "quota_failure" {
			t.Fatalf("error code = %q, want quota_failure", env.Error.Code)
		}
		assertNoLeak(t, resp)
	})

	t.Run("reset", func(t *testing.T) {
		body, _ := json.Marshal(pluginapi.QuotaResetRequest{
			AuthID:      "auth-1",
			Provider:    providerKey,
			StorageJSON: cred.StorageJSON(),
		})
		resp, err := quotaManager(t, &quotaFakeCaller{}).Handle(pluginabi.MethodQuotaReset, body)
		if err != nil {
			t.Fatalf("Handle(quota.reset): %v", err)
		}
		assertNoLeak(t, resp)
	})
}
