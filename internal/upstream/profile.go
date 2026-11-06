package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// profile.go ports the account-status triple from the oracle
// (openai_backend_api.py get_user_info, lines 289-376): three backend-api
// calls run IN PARALLEL — GET /backend-api/me (identity),
// POST /backend-api/conversation/init (limits_progress + default model) and
// GET /backend-api/accounts/check (plan_type + is_deactivated) — merged into
// one Profile. A 401 on any leg is the typed invalid_token the executor uses
// to force-refresh (Task 12).

// The probe's pinned URLs. The oracle builds the accounts.check query inline
// (line 315); the init body and query share the pinned -480 offset
// (Global Constraint wire pinning).
const (
	pinnedTimezoneOffsetMin = -480

	profileMeURL   = chatGPTBaseURL + "/backend-api/me"
	profileInitURL = chatGPTBaseURL + "/backend-api/conversation/init"

	// X-OpenAI-Target-Path/Route carry the path WITHOUT the query string,
	// matching the oracle's _headers(path) convention (line 315 passes the
	// bare path while the URL carries the query).
	profileMePath      = "/backend-api/me"
	profileInitPath    = "/backend-api/conversation/init"
	profileAccountPath = "/backend-api/accounts/check/v4-2023-04-27"
)

// profileAccountURL carries the pinned query string; it is a var only
// because the offset is formatted from the constant (the test pins the
// exact wire value).
var profileAccountURL = fmt.Sprintf("%s/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=%d", chatGPTBaseURL, pinnedTimezoneOffsetMin)

// probeNow anchors relative reset_after values to "now"; tests override it
// for deterministic restore times.
var probeNow = time.Now

// conversationInitRequest is the byte-pinned init body (oracle lines
// 301-306): all three id fields are null, the timezone offset is pinned.
// Struct field order fixes the JSON key order.
type conversationInitRequest struct {
	GizmoID               any `json:"gizmo_id"`
	RequestedDefaultModel any `json:"requested_default_model"`
	ConversationID        any `json:"conversation_id"`
	TimezoneOffsetMin     int `json:"timezone_offset_min"`
}

func conversationInitBody() []byte {
	out, _ := json.Marshal(conversationInitRequest{TimezoneOffsetMin: pinnedTimezoneOffsetMin})
	return out
}

// LimitWindow is one limits_progress entry from /backend-api/conversation/
// init. The oracle reads feature_name, remaining and reset_after per entry
// (lines 279-281); max is the window capacity the quota cards divide by.
type LimitWindow struct {
	FeatureName string
	Remaining   int
	// Max is the window capacity upstream reported; 0 = absent/unknown —
	// the card then renders a full bar (1.0) rather than an invented
	// fraction or an exhausted reading (commandcode unknown-cap precedent).
	Max int
	// RestoreAt is the absolute reset time derived from reset_after; zero
	// when upstream returned none or an unparseable value.
	RestoreAt time.Time
}

// Profile is the merged account-status triple: identity from /me, limits and
// default model from conversation/init, plan and deactivation from
// accounts/check. Plan is never "" after a successful probe — the oracle
// defaults a missing plan_type to "free" (line 351); card mapping treats ""
// as unknown either way.
type Profile struct {
	Email          string
	UserID         string
	Plan           string
	LimitsProgress []LimitWindow
	// ImageQuota is the remaining count of the limits_progress entry with
	// feature_name == "image_gen" (0 when absent — oracle lines 277-282).
	ImageQuota       int
	ImageRestoreAt   time.Time
	DefaultModelSlug string
	// Deactivated is accounts.check is_deactivated (oracle line 326).
	Deactivated bool
}

// ProbeProfile runs the three legs in parallel (the oracle's
// ThreadPoolExecutor(max_workers=3)) and merges their payloads. Any leg's
// error — 401 included — wins; the other legs' successes are discarded.
func ProbeProfile(ctx context.Context, u Upstream, cred model.Credential) (Profile, error) {
	// Materialize the per-credential fingerprint BEFORE any leg runs:
	// ensureFingerprint lazily writes generated ids back into cred.Fp, and
	// the three parallel legs would otherwise race on that write (the
	// SessionHeaders contract — persist the credential after first use).
	ensureFingerprint(&cred)

	var (
		wg          sync.WaitGroup
		meBody      []byte
		initBody    []byte
		accountBody []byte
		meErr       error
		initErr     error
		accountErr  error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		meBody, meErr = fetchProfileLeg(ctx, u, &cred, http.MethodGet, profileMeURL, profileMePath, nil)
	}()
	go func() {
		defer wg.Done()
		initBody, initErr = fetchProfileLeg(ctx, u, &cred, http.MethodPost, profileInitURL, profileInitPath, conversationInitBody())
	}()
	go func() {
		defer wg.Done()
		accountBody, accountErr = fetchProfileLeg(ctx, u, &cred, http.MethodGet, profileAccountURL, profileAccountPath, nil)
	}()
	wg.Wait()

	for _, err := range []error{meErr, initErr, accountErr} {
		if err != nil {
			return Profile{}, err
		}
	}
	return mergeProfile(meBody, initBody, accountBody)
}

// fetchProfileLeg issues one probe leg with the pinned session headers plus
// the credential bearer. The oracle sends Content-Type only on the POST leg
// (line 300); non-2xx maps through the seam's typed statusError (401 →
// invalid_token).
func fetchProfileLeg(ctx context.Context, u Upstream, cred *model.Credential, method, url, pathTemplate string, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headers := Authed(cred, SessionHeaders(cred, pathTemplate))
	if body != nil {
		// Set on the header (not Req.ContentType) so the wire value is
		// pinned by the seam doubles; the transport only applies
		// ContentType when the header is absent.
		headers.Set("Content-Type", "application/json")
	}
	req := Req{
		Method:  method,
		URL:     url,
		Headers: headers,
		Body:    body,
	}
	resp, err := u.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := statusError(resp.StatusCode, nil); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// mePayload is the identity the /me leg returns (oracle lines 357-358).
type mePayload struct {
	Email string `json:"email"`
	ID    string `json:"id"`
}

// initPayload is what the init leg returns for the probe; limits_progress is
// decoded loosely (oracle line 354: a non-list value means no windows).
type initPayload struct {
	DefaultModelSlug json.RawMessage `json:"default_model_slug"`
	LimitsProgress   any             `json:"limits_progress"`
}

// accountInfoPayload mirrors accounts.default.account (oracle lines 323-326).
type accountInfoPayload struct {
	PlanType      *string `json:"plan_type"`
	IsDeactivated *bool   `json:"is_deactivated"`
}

// accountPayload mirrors the accounts.check response with null-tolerant
// pointer levels (oracle line 320: every hierarchy level may be absent).
type accountPayload struct {
	Accounts *struct {
		Default *struct {
			Account *accountInfoPayload `json:"account"`
		} `json:"default"`
	} `json:"accounts"`
}

// mergeProfile derives the Profile from the three leg bodies. Error text
// names only the seam — never payload material (secret rule).
func mergeProfile(meBody, initBody, accountBody []byte) (Profile, error) {
	var me mePayload
	if err := json.Unmarshal(meBody, &me); err != nil {
		return Profile{}, errors.New("the /backend-api/me response is not a valid JSON object")
	}

	var init initPayload
	if err := json.Unmarshal(initBody, &init); err != nil {
		return Profile{}, errors.New("the /backend-api/conversation/init response is not a valid JSON object")
	}

	var account accountPayload
	if err := json.Unmarshal(accountBody, &account); err != nil {
		return Profile{}, errors.New("the /backend-api/accounts/check response is not a valid JSON object")
	}

	profile := Profile{
		Email:            me.Email,
		UserID:           me.ID,
		DefaultModelSlug: defaultModelSlug(init.DefaultModelSlug),
	}

	// Oracle line 351: a missing (or null/empty) plan_type defaults to "free".
	profile.Plan = "free"
	if account.Accounts != nil && account.Accounts.Default != nil && account.Accounts.Default.Account != nil {
		acc := account.Accounts.Default.Account
		if acc.PlanType != nil && strings.TrimSpace(*acc.PlanType) != "" {
			profile.Plan = *acc.PlanType
		}
		if acc.IsDeactivated != nil {
			profile.Deactivated = *acc.IsDeactivated
		}
	}

	profile.LimitsProgress = decodeLimitWindows(init.LimitsProgress, probeNow())
	for _, w := range profile.LimitsProgress {
		if w.FeatureName == "image_gen" { // first match wins (oracle lines 279-281)
			profile.ImageQuota = w.Remaining
			profile.ImageRestoreAt = w.RestoreAt
			break
		}
	}
	return profile, nil
}

// defaultModelSlug coerces the init payload's slug to a string: missing or
// null → "", a non-string value → "" (never invent one).
func defaultModelSlug(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var slug string
	if err := json.Unmarshal(raw, &slug); err != nil {
		return ""
	}
	return slug
}

// decodeLimitWindows normalizes the init payload's limits_progress list into
// windows. Non-list values yield no windows (oracle line 354); non-object
// entries are skipped (oracle line 280).
func decodeLimitWindows(value any, now time.Time) []LimitWindow {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var windows []LimitWindow
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		windows = append(windows, decodeLimitWindow(fields, now))
	}
	return windows
}

func decodeLimitWindow(fields map[string]any, now time.Time) LimitWindow {
	w := LimitWindow{}
	if name, ok := fields["feature_name"].(string); ok {
		w.FeatureName = name
	}
	w.Remaining = asWindowInt(fields["remaining"])
	w.Max = asWindowInt(fields["max"])
	w.RestoreAt = parseResetAfter(fields["reset_after"], now)
	return w
}

// asWindowInt is the oracle's `int(item.get(key) or 0)` (line 281): missing,
// null or unparseable values read as 0.
func asWindowInt(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

// parseResetAfter derives the absolute reset time. The oracle passes the
// value through raw (line 281); the cards need a time, so: a number is
// seconds-from-now (0 reads as absent, matching the oracle's falsy `or`), a
// string is tried as RFC3339 first then as integer seconds, anything else is
// absent — never invented.
func parseResetAfter(value any, now time.Time) time.Time {
	switch v := value.(type) {
	case float64:
		if v <= 0 {
			return time.Time{}
		}
		return now.Add(time.Duration(v) * time.Second)
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil && i > 0 {
			return now.Add(time.Duration(i) * time.Second)
		}
	}
	return time.Time{}
}
