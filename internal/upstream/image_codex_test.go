package upstream

// image_codex_test.go pins the Task 10 contract: the codex image route
// (oracle openai_backend_api.py iter_codex_image_response_events /
// _codex_responses_headers / _codex_image_input, and
// services/protocol/conversation.py _codex_response_images /
// stream_codex_image_outputs) — the byte-pinned request body, the minimal
// two-header set (bearer + content-type: no sentinel handshake, no session
// fingerprint, no X-OpenAI-Target-* pair), the plan gate (plus|pro|team;
// free refuses before any call; unknown plans attempt once and map 403/404
// to typed codex_image_unavailable), recursive base64 data URL extraction
// from SSE frames or a single JSON body, and Retry-After on a 429.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

const codexImageURL = chatGPTBaseURL + "/backend-api/codex/responses"

// The SSE fixture's deeper-nested event carries a second, distinct image:
// its data URL payload is base64 "AAEC", which decodes to these bytes.
var codexSecondImage = []byte{0x00, 0x01, 0x02}

func codexProfile(plan string) Profile { return Profile{Plan: plan} }

// codexJSONFixture scripts one successful single-JSON response at the codex
// URL, with the content-type that selects the JSON parse branch (the oracle
// branches purely on the content-type header).
func codexJSONFixture(fake *drawFake, fixture string) {
	fake.on("POST "+codexImageURL, func() Resp {
		raw, err := osReadFixture(fixture)
		if err != nil {
			return Resp{StatusCode: 500, Body: []byte(fmt.Sprintf("fixture read failed: %v", err))}
		}
		headers := http.Header{}
		headers.Set("Content-Type", "application/json")
		return Resp{StatusCode: 200, Headers: headers, Body: raw}
	})
}

// assertCodexHeaders pins the oracle _codex_responses_headers set EXACTLY:
// the plain bearer and content-type, and nothing else.
func assertCodexHeaders(t *testing.T, req Req) {
	t.Helper()
	if got := req.Headers.Get("Authorization"); got != "Bearer fixture-access-token" {
		t.Fatalf("Authorization = %q, want the plain bearer", got)
	}
	if got := req.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	for name := range req.Headers {
		if name != "Authorization" && name != "Content-Type" {
			t.Fatalf("header %q must not be sent (the codex route uses bearer + content-type only — no sentinel, no fingerprint, no X-OpenAI-Target-*)", name)
		}
	}
}

// assertCodexImageError unwraps the typed *ImageError and checks its code.
func assertCodexImageError(t *testing.T, err error, code string) *ImageError {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want typed %q", code)
	}
	var imgErr *ImageError
	if !errors.As(err, &imgErr) {
		t.Fatalf("error = %v, want typed *ImageError", err)
	}
	if imgErr.Code != code {
		t.Fatalf("error code = %q, want %q", imgErr.Code, code)
	}
	return imgErr
}

// ---------------------------------------------------------------------------
// TestCodexBodyGolden — the request body is byte-pinned against the oracle's
// json.dumps default dialect in its dict insertion order; any field, value,
// order or dialect drift fails loudly. The header set is pinned too.
// ---------------------------------------------------------------------------

func TestCodexBodyGolden(t *testing.T) {
	for _, tc := range []struct {
		name       string
		references []ReferenceImage
		size       string
		quality    string
		golden     string
	}{
		{name: "generate", size: "1024x1024", quality: "high", golden: "codex-image-body-generate.json"},
		// No references → action "generate"; one reference → action "edit"
		// and the data URL in input; empty size/quality fall back to the
		// oracle's "1024x1024"/"auto" defaults.
		{name: "edit", references: []ReferenceImage{{Data: pngBytes}}, golden: "codex-image-body-edit.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newDrawFake(t)
			codexJSONFixture(fake, "codex-image-json.json")

			if _, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("plus"), "a lighthouse", tc.references, tc.size, tc.quality); err != nil {
				t.Fatalf("GenerateCodexImage: %v", err)
			}

			reqs := fake.requestsFor(http.MethodPost, codexImageURL)
			if len(reqs) != 1 {
				t.Fatalf("codex calls = %d, want exactly 1", len(reqs))
			}
			golden, err := osReadFixture(tc.golden)
			if err != nil {
				t.Fatalf("read golden body %s: %v", tc.golden, err)
			}
			if string(reqs[0].Body) != string(golden) {
				t.Fatalf("request body drifted from the oracle pin:\ngot:  %s\nwant: %s", reqs[0].Body, golden)
			}
			assertCodexHeaders(t, reqs[0])
		})
	}
}

// ---------------------------------------------------------------------------
// TestCodexSSEExtraction — SSE data frames (or a single JSON body) are parsed
// and the base64 data URLs extracted recursively, in document order, the
// data:image/ prefix stripped and the payloads decoded to raw bytes.
// ---------------------------------------------------------------------------

func TestCodexSSEExtraction(t *testing.T) {
	fake := newDrawFake(t)
	fake.fixture("POST "+codexImageURL, "codex-image-sse.sse", 200)

	result, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("plus"), "a lighthouse", nil, "1024x1024", "high")
	if err != nil {
		t.Fatalf("GenerateCodexImage: %v", err)
	}

	// The fixture carries two DISTINCT images on purpose: the walker must
	// return every image_generation_call.result it finds (the oracle does
	// not dedupe), in stream order — the output_item.done event's item
	// first, then the deeper-nested response.completed output[] entry.
	if len(result.Images) != 2 {
		t.Fatalf("images = %d, want the two fixture results in stream order", len(result.Images))
	}
	if string(result.Images[0].Data) != string(pngBytes) {
		t.Fatalf("first image bytes drifted (want the full fixture PNG)")
	}
	if string(result.Images[1].Data) != string(codexSecondImage) {
		t.Fatalf("second image bytes drifted (want the decoded AAEC payload)")
	}
	if result.RevisedPrompt != "a lighthouse" {
		t.Fatalf("RevisedPrompt = %q, want the prompt echoed (oracle stream_codex_image_outputs)", result.RevisedPrompt)
	}
	if result.ConversationID != "" {
		t.Fatalf("ConversationID = %q, the codex route establishes none", result.ConversationID)
	}

	reqs := fake.requestsFor(http.MethodPost, codexImageURL)
	if len(reqs) != 1 {
		t.Fatalf("codex calls = %d, want exactly 1", len(reqs))
	}
	assertCodexHeaders(t, reqs[0])
}

// TestCodexJSONBody covers the non-stream response shape: a content-type
// containing application/json selects the single-JSON parse branch, and the
// "team" plan passes the gate too.
func TestCodexJSONBody(t *testing.T) {
	fake := newDrawFake(t)
	codexJSONFixture(fake, "codex-image-json.json")

	result, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("team"), "a lighthouse", nil, "", "")
	if err != nil {
		t.Fatalf("GenerateCodexImage: %v", err)
	}
	if len(result.Images) != 1 || string(result.Images[0].Data) != string(pngBytes) {
		t.Fatalf("images = %d, want the one fixture PNG", len(result.Images))
	}
}

// ---------------------------------------------------------------------------
// TestCodexPlanGate — free refuses BEFORE any HTTP call; an unknown or empty
// plan attempts once and maps 403/404 to typed codex_image_unavailable (never
// a silent success); plus/pro/team succeed; known plans keep the generic
// status mapping (a plus 403 is upstream_status, not unavailable).
// ---------------------------------------------------------------------------

func TestCodexPlanGate(t *testing.T) {
	t.Run("free refuses before the call", func(t *testing.T) {
		fake := newDrawFake(t) // nothing scripted: any call would fail typed
		result, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("free"), "a lighthouse", nil, "1024x1024", "high")
		assertCodexImageError(t, err, ImageCodeCodexUnavailable)
		if result.Err == nil {
			t.Fatalf("result.Err = nil, want the typed refusal")
		}
		if calls := len(fake.requestsForMethod(http.MethodPost)); calls != 0 {
			t.Fatalf("free plan made %d upstream call(s), want none", calls)
		}
	})

	for _, tc := range []struct {
		plan   string
		status int
	}{
		{plan: "", status: http.StatusForbidden},
		{plan: "enterprise", status: http.StatusNotFound},
	} {
		t.Run(fmt.Sprintf("unknown plan %q maps HTTP %d", tc.plan, tc.status), func(t *testing.T) {
			fake := newDrawFake(t)
			fake.status("POST "+codexImageURL, tc.status, []byte(`{"error": {"message": "route not available"}}`))

			_, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile(tc.plan), "a lighthouse", nil, "1024x1024", "high")
			imgErr := assertCodexImageError(t, err, ImageCodeCodexUnavailable)
			if imgErr.HTTPStatus != tc.status {
				t.Fatalf("error HTTPStatus = %d, want %d", imgErr.HTTPStatus, tc.status)
			}
			if imgErr.Retryable {
				t.Fatalf("codex_image_unavailable must not be retryable")
			}
			if calls := len(fake.requestsForMethod(http.MethodPost)); calls != 1 {
				t.Fatalf("unknown plan made %d upstream call(s), want exactly 1 attempt", calls)
			}
		})
	}

	t.Run("plus succeeds", func(t *testing.T) {
		fake := newDrawFake(t)
		codexJSONFixture(fake, "codex-image-json.json")

		result, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("plus"), "a lighthouse", nil, "1024x1024", "high")
		if err != nil {
			t.Fatalf("GenerateCodexImage: %v", err)
		}
		if len(result.Images) != 1 {
			t.Fatalf("images = %d, want the one fixture PNG", len(result.Images))
		}
	})

	t.Run("the gate is case-insensitive", func(t *testing.T) {
		for _, plan := range []string{"PRO", "Team"} {
			fake := newDrawFake(t)
			codexJSONFixture(fake, "codex-image-json.json")

			if _, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile(plan), "a lighthouse", nil, "1024x1024", "high"); err != nil {
				t.Fatalf("plan %q: %v", plan, err)
			}
		}
	})

	t.Run("a known plan keeps the generic status mapping", func(t *testing.T) {
		fake := newDrawFake(t)
		fake.status("POST "+codexImageURL, http.StatusForbidden, nil)

		_, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("plus"), "a lighthouse", nil, "1024x1024", "high")
		assertCodexImageError(t, err, ImageCodeUpstreamStatus) // not codex_image_unavailable
	})
}

// ---------------------------------------------------------------------------
// TestCodexRetryAfter — a 429 carries Retry-After into the typed retryable
// rate_limited outcome (oracle UpstreamHTTPError retry_after=...).
// ---------------------------------------------------------------------------

func TestCodexRetryAfter(t *testing.T) {
	fake := newDrawFake(t)
	fake.on("POST "+codexImageURL, func() Resp {
		headers := http.Header{}
		headers.Set("Retry-After", "7")
		return Resp{StatusCode: http.StatusTooManyRequests, Headers: headers, Body: nil}
	})

	_, err := GenerateCodexImage(context.Background(), fake, fixtureCredential(), codexProfile("plus"), "a lighthouse", nil, "1024x1024", "high")
	imgErr := assertCodexImageError(t, err, ImageCodeRateLimited)
	if !imgErr.Retryable {
		t.Fatalf("the 429 must be retryable")
	}
	if imgErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error HTTPStatus = %d, want 429", imgErr.HTTPStatus)
	}
	if imgErr.RetryAfter != 7 {
		t.Fatalf("error RetryAfter = %d, want the header value 7", imgErr.RetryAfter)
	}
}

// ---------------------------------------------------------------------------
// TestCodexRequestDeadline — the oracle's urlopen timeout=1200 is wired as a
// named constant deadline on the request context (the signature carries no
// config, so the brief's "else a named constant" branch).
// ---------------------------------------------------------------------------

// deadlineProbe records the context deadline one Do call sees.
type deadlineProbe struct {
	deadlineOk bool
	deadline   time.Time
}

func (p *deadlineProbe) Do(ctx context.Context, req Req) (Resp, error) {
	if deadline, ok := ctx.Deadline(); ok {
		p.deadline, p.deadlineOk = deadline, true
	}
	headers := http.Header{}
	// application/json selects the single-JSON parse branch (the oracle
	// branches purely on the content-type).
	headers.Set("Content-Type", "application/json")
	return Resp{StatusCode: 200, Headers: headers, Body: []byte(`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"data:image/png;base64,AAEC"}]}}`)}, nil
}

func (p *deadlineProbe) DoStream(ctx context.Context, req Req) (*Stream, error) {
	return nil, errors.New("deadlineProbe: the codex route never streams")
}

func TestCodexRequestDeadline(t *testing.T) {
	probe := &deadlineProbe{}

	if _, err := GenerateCodexImage(context.Background(), probe, fixtureCredential(), codexProfile("plus"), "a lighthouse", nil, "1024x1024", "high"); err != nil {
		t.Fatalf("GenerateCodexImage: %v", err)
	}
	if !probe.deadlineOk {
		t.Fatalf("the request context carries no deadline, want the 1200s oracle timeout")
	}
	remaining := time.Until(probe.deadline)
	if remaining <= 0 || remaining > 1200*time.Second {
		t.Fatalf("deadline is %v away, want ~1200s", remaining)
	}
}
