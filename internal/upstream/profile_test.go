package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// profile_test.go pins the account-status triple against the oracle
// (openai_backend_api.py get_user_info, lines 289-376): three parallel
// backend-api calls whose payloads are merged into one Profile, the
// image_gen quota derivation, the plan default, and the typed 401.

// The oracle's exact probe URLs and the byte-pinned init body, written as
// literals in the test so a drift in the implementation's constants fails
// loudly instead of silently moving both sides.
const (
	wantProfileMeURL      = "https://chatgpt.com/backend-api/me"
	wantProfileInitURL    = "https://chatgpt.com/backend-api/conversation/init"
	wantProfileAccountURL = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480"

	profileInitBodyPin = `{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":-480}`
)

// profileInitFixture carries both reset_after dialects the parser must
// accept: an RFC3339 absolute string and a numeric seconds-from-now value.
const profileInitFixture = `{"default_model_slug":"gpt-5-2-prod","limits_progress":[{"feature_name":"primary_limits","remaining":10,"max":40,"reset_after":"2026-11-07T09:00:00Z"},{"feature_name":"image_gen","remaining":7,"max":100,"reset_after":259}]}`

const profileAccountFixture = `{"accounts":{"default":{"account":{"plan_type":"plus","account_user_role":"admin","is_deactivated":false}}}}`

// parallelProbeUpstream is the offline Upstream double for the probe. The
// three legs run in parallel, so responses are URL-keyed (a FIFO queue would
// be order-nondeterministic). It also acts as a rendezvous: every leg holds
// its response open until all three legs have entered Do. A genuinely
// parallel implementation releases all three at once; a sequential one
// blocks its first leg until the probe's context deadline, so the test fails
// (and proves the leg was never concurrent) instead of passing.
type parallelProbeUpstream struct {
	mu       sync.Mutex
	scripts  map[string]Resp
	requests []Req

	entered int32
	release chan struct{}
}

func newParallelProbeUpstream(scripts map[string]Resp) *parallelProbeUpstream {
	return &parallelProbeUpstream{scripts: scripts, release: make(chan struct{})}
}

func (u *parallelProbeUpstream) Do(ctx context.Context, req Req) (Resp, error) {
	u.mu.Lock()
	u.requests = append(u.requests, req)
	u.mu.Unlock()

	// Rendezvous: record this leg's arrival; the third arrival releases all.
	if n := atomic.AddInt32(&u.entered, 1); n == 3 {
		close(u.release)
	}
	select {
	case <-u.release:
	case <-ctx.Done():
		return Resp{}, fmt.Errorf("parallelProbeUpstream: leg %s never ran concurrently with the others: %w", req.URL, ctx.Err())
	}

	u.mu.Lock()
	resp, ok := u.scripts[req.URL]
	u.mu.Unlock()
	if !ok {
		return Resp{}, fmt.Errorf("parallelProbeUpstream: no script for %s", req.URL)
	}
	return resp, nil
}

func (u *parallelProbeUpstream) DoStream(context.Context, Req) (*Stream, error) {
	return nil, errors.New("parallelProbeUpstream: DoStream is not used by the profile probe")
}

func (u *parallelProbeUpstream) recorded() []Req {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Req(nil), u.requests...)
}

func probeScripts(t *testing.T, overrides map[string]Resp) map[string]Resp {
	t.Helper()
	scripts := map[string]Resp{
		wantProfileMeURL:      scriptResp(http.StatusOK, `{"email":"quota-user@example.com","id":"user-12345"}`),
		wantProfileInitURL:    scriptResp(http.StatusOK, profileInitFixture),
		wantProfileAccountURL: scriptResp(http.StatusOK, profileAccountFixture),
	}
	for url, resp := range overrides {
		scripts[url] = resp
	}
	return scripts
}

func probeCredential() model.Credential {
	return model.Credential{
		AccessToken: "test-access-token",
		Fp: model.Fingerprint{
			OAIDeviceID:  "device-id-1",
			OAISessionID: "session-id-1",
		},
	}
}

// runProbe calls ProbeProfile under a bounded context so a sequential
// implementation trips the rendezvous deadline and fails fast instead of
// hanging the suite.
func runProbe(t *testing.T, u *parallelProbeUpstream) (Profile, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ProbeProfile(ctx, u, probeCredential())
}

// ---------------------------------------------------------------------------
// TestProbeTripleParallel — the three legs run in parallel, each hitting the
// oracle's exact URL (method, query, target-path header, bearer), and the
// payloads merge into one Profile.
// ---------------------------------------------------------------------------

func TestProbeTripleParallel(t *testing.T) {
	u := newParallelProbeUpstream(probeScripts(t, nil))

	profile, err := runProbe(t, u)
	if err != nil {
		t.Fatalf("ProbeProfile: %v", err)
	}

	requests := u.recorded()
	if len(requests) != 3 {
		t.Fatalf("probe issued %d requests, want 3", len(requests))
	}

	want := map[string]struct {
		method string
		path   string
	}{
		wantProfileMeURL:      {http.MethodGet, "/backend-api/me"},
		wantProfileInitURL:    {http.MethodPost, "/backend-api/conversation/init"},
		wantProfileAccountURL: {http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27"},
	}
	seen := make(map[string]bool, 3)
	var initReq Req
	for _, req := range requests {
		w, ok := want[req.URL]
		if !ok {
			t.Fatalf("probe hit unexpected URL %q (oracle paths are the three literals above)", req.URL)
		}
		seen[req.URL] = true
		if req.Method != w.method {
			t.Fatalf("method for %s = %s, want %s", req.URL, req.Method, w.method)
		}
		if got := req.Headers.Get("X-OpenAI-Target-Path"); got != w.path {
			t.Fatalf("X-OpenAI-Target-Path for %s = %q, want %q", req.URL, got, w.path)
		}
		if got := req.Headers.Get("Authorization"); got != "Bearer test-access-token" {
			t.Fatalf("Authorization for %s = %q, want the credential bearer token", req.URL, got)
		}
		if req.URL == wantProfileInitURL {
			initReq = req
		}
	}
	if len(seen) != 3 {
		t.Fatalf("probe hit %d of 3 URLs: %v", len(seen), seen)
	}

	// The conversation/init body is byte-pinned to the oracle (lines 301-306)
	// and the only leg that carries a JSON body + Content-Type.
	if string(initReq.Body) != profileInitBodyPin {
		t.Fatalf("conversation/init body = %s, want byte-pinned %s", initReq.Body, profileInitBodyPin)
	}
	if got := initReq.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("conversation/init Content-Type = %q, want application/json", got)
	}

	if profile.Email != "quota-user@example.com" || profile.UserID != "user-12345" {
		t.Fatalf("merged identity = %q/%q, want the me payload's email/id", profile.Email, profile.UserID)
	}
	if profile.Plan != "plus" {
		t.Fatalf("Plan = %q, want the accounts.check plan_type", profile.Plan)
	}
	if profile.DefaultModelSlug != "gpt-5-2-prod" {
		t.Fatalf("DefaultModelSlug = %q, want the init payload's slug", profile.DefaultModelSlug)
	}
	if profile.Deactivated {
		t.Fatalf("Deactivated = true, want false for the fixture account")
	}
	if len(profile.LimitsProgress) != 2 {
		t.Fatalf("LimitsProgress = %d windows, want the 2 the init payload returned", len(profile.LimitsProgress))
	}
}

// ---------------------------------------------------------------------------
// TestProbeImageQuotaExtraction — ImageQuota/ImageRestoreAt come from the
// first limits_progress entry with feature_name == "image_gen" (oracle
// _extract_quota_and_restore_at, lines 277-282); 0 + zero time when absent.
// ---------------------------------------------------------------------------

func TestProbeImageQuotaExtraction(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { probeNow = time.Now })
	probeNow = func() time.Time { return fixed }

	t.Run("image_gen window wins, other windows ignored", func(t *testing.T) {
		u := newParallelProbeUpstream(probeScripts(t, nil))
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if profile.ImageQuota != 7 {
			t.Fatalf("ImageQuota = %d, want the image_gen remaining (7), not the decoy's (10)", profile.ImageQuota)
		}
		want := fixed.Add(259 * time.Second)
		if !profile.ImageRestoreAt.Equal(want) {
			t.Fatalf("ImageRestoreAt = %v, want reset_after 259s from now (%v)", profile.ImageRestoreAt, want)
		}
	})

	t.Run("first image_gen entry wins", func(t *testing.T) {
		scripts := probeScripts(t, map[string]Resp{
			wantProfileInitURL: scriptResp(http.StatusOK,
				`{"limits_progress":[{"feature_name":"image_gen","remaining":4,"reset_after":100},{"feature_name":"image_gen","remaining":99,"reset_after":200}]}`),
		})
		u := newParallelProbeUpstream(scripts)
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if profile.ImageQuota != 4 {
			t.Fatalf("ImageQuota = %d, want the FIRST image_gen entry's remaining (4)", profile.ImageQuota)
		}
	})

	t.Run("absent image_gen yields zero quota and zero restore", func(t *testing.T) {
		scripts := probeScripts(t, map[string]Resp{
			wantProfileInitURL: scriptResp(http.StatusOK,
				`{"limits_progress":[{"feature_name":"primary_limits","remaining":10,"reset_after":600}]}`),
		})
		u := newParallelProbeUpstream(scripts)
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if profile.ImageQuota != 0 || !profile.ImageRestoreAt.IsZero() {
			t.Fatalf("absent image_gen: ImageQuota=%d ImageRestoreAt=%v, want 0 + zero time", profile.ImageQuota, profile.ImageRestoreAt)
		}
	})
}

// ---------------------------------------------------------------------------
// TestProbeDeactivated — is_deactivated flows into Profile.Deactivated; a
// missing plan_type defaults to "free" (oracle line 351) and a null
// accounts hierarchy is tolerated (oracle lines 320, 345-347).
// ---------------------------------------------------------------------------

func TestProbeDeactivated(t *testing.T) {
	t.Run("is_deactivated true", func(t *testing.T) {
		scripts := probeScripts(t, map[string]Resp{
			wantProfileAccountURL: scriptResp(http.StatusOK,
				`{"accounts":{"default":{"account":{"plan_type":"free","is_deactivated":true}}}}`),
		})
		u := newParallelProbeUpstream(scripts)
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if !profile.Deactivated {
			t.Fatalf("Deactivated = false, want true (is_deactivated: true)")
		}
	})

	t.Run("missing plan_type defaults to free", func(t *testing.T) {
		scripts := probeScripts(t, map[string]Resp{
			wantProfileAccountURL: scriptResp(http.StatusOK,
				`{"accounts":{"default":{"account":{"account_user_role":"admin"}}}}`),
		})
		u := newParallelProbeUpstream(scripts)
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if profile.Plan != "free" {
			t.Fatalf("Plan = %q, want the oracle default \"free\"", profile.Plan)
		}
		if profile.Deactivated {
			t.Fatalf("Deactivated = true, want false when is_deactivated is absent")
		}
	})

	t.Run("null accounts hierarchy tolerated", func(t *testing.T) {
		scripts := probeScripts(t, map[string]Resp{
			wantProfileAccountURL: scriptResp(http.StatusOK, `{"accounts":null}`),
		})
		u := newParallelProbeUpstream(scripts)
		profile, err := runProbe(t, u)
		if err != nil {
			t.Fatalf("ProbeProfile: %v", err)
		}
		if profile.Plan != "free" || profile.Deactivated {
			t.Fatalf("null accounts: Plan=%q Deactivated=%v, want free + false", profile.Plan, profile.Deactivated)
		}
	})
}

// singleLegUpstream scripts one URL for the single-call ProbePlan (no
// rendezvous — there is nothing to parallelize).
type singleLegUpstream struct {
	resp     Resp
	requests []Req
}

func (u *singleLegUpstream) Do(_ context.Context, req Req) (Resp, error) {
	u.requests = append(u.requests, req)
	return u.resp, nil
}

func (u *singleLegUpstream) DoStream(context.Context, Req) (*Stream, error) {
	return nil, errors.New("singleLegUpstream: DoStream is not used by ProbePlan")
}

// ---------------------------------------------------------------------------
// TestProbePlan — the single accounts/check leg login and refresh use to
// persist the plan into the credential record (oracle get_user_info plan
// source, openai_backend_api.py lines 315-326): plan_type with the "free"
// default, the pinned wire leg, and typed 401 / malformed-body failures.
// ---------------------------------------------------------------------------

func TestProbePlan(t *testing.T) {
	cred := model.Credential{AccessToken: "plan-probe-token"}

	t.Run("plan_type with pinned wire leg", func(t *testing.T) {
		u := &singleLegUpstream{resp: scriptResp(http.StatusOK, profileAccountFixture)}
		plan, err := ProbePlan(context.Background(), u, cred)
		if err != nil {
			t.Fatalf("ProbePlan: %v", err)
		}
		if plan != "plus" {
			t.Fatalf("plan = %q, want plus from the fixture plan_type", plan)
		}
		if len(u.requests) != 1 {
			t.Fatalf("ProbePlan issued %d transport calls, want exactly 1 (the accounts/check leg)", len(u.requests))
		}
		req := u.requests[0]
		if req.URL != wantProfileAccountURL || req.Method != http.MethodGet {
			t.Fatalf("leg = %s %s, want GET %s", req.Method, req.URL, wantProfileAccountURL)
		}
		if got := req.Headers.Get("Authorization"); got != "Bearer plan-probe-token" {
			t.Fatalf("Authorization = %q, want the credential bearer", got)
		}
		if got := req.Headers.Get("X-OpenAI-Target-Path"); got != "/backend-api/accounts/check/v4-2023-04-27" {
			t.Fatalf("X-OpenAI-Target-Path = %q, want the bare route (oracle _headers convention)", got)
		}
	})

	t.Run("missing plan_type defaults to free", func(t *testing.T) {
		u := &singleLegUpstream{resp: scriptResp(http.StatusOK, `{"accounts":null}`)}
		plan, err := ProbePlan(context.Background(), u, cred)
		if err != nil {
			t.Fatalf("ProbePlan: %v", err)
		}
		if plan != "free" {
			t.Fatalf("plan = %q, want the oracle free default", plan)
		}
	})

	t.Run("401 is the typed invalid token", func(t *testing.T) {
		u := &singleLegUpstream{resp: scriptResp(http.StatusUnauthorized, `{"error":"unauthorized"}`)}
		plan, err := ProbePlan(context.Background(), u, cred)
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("error = %v, want typed invalid_token", err)
		}
		if plan != "" {
			t.Fatalf("plan = %q, want empty on failure", plan)
		}
	})

	t.Run("malformed body is a typed error", func(t *testing.T) {
		u := &singleLegUpstream{resp: scriptResp(http.StatusOK, `not-json`)}
		if plan, err := ProbePlan(context.Background(), u, cred); err == nil || plan != "" {
			t.Fatalf("ProbePlan = (%q, %v), want a typed error and no plan", plan, err)
		}
	})
}

// ---------------------------------------------------------------------------
// TestProbe401Typed — a 401 on ANY leg is the typed invalid_token; the other
// legs' successes are discarded (the executor forces a refresh + one retry,
// Task 12).
// ---------------------------------------------------------------------------

func TestProbe401Typed(t *testing.T) {
	legs := map[string]struct {
		url string
	}{
		"me":      {wantProfileMeURL},
		"init":    {wantProfileInitURL},
		"account": {wantProfileAccountURL},
	}
	for name, leg := range legs {
		t.Run(name+" 401", func(t *testing.T) {
			scripts := probeScripts(t, nil)
			scripts[leg.url] = scriptResp(http.StatusUnauthorized, `{"error":"unauthorized"}`)
			u := newParallelProbeUpstream(scripts)

			profile, err := runProbe(t, u)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("error = %v, want typed invalid_token", err)
			}
			// The other legs' successes must be ignored entirely.
			if profile.Email != "" || profile.Plan != "" || profile.DefaultModelSlug != "" || len(profile.LimitsProgress) != 0 {
				t.Fatalf("profile %v merged from a failed probe; want the zero Profile", profile)
			}
		})
	}
}
