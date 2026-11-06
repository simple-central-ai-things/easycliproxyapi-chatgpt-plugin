package upstream

// image_drawing_test.go pins the Task 9 contract: the full web-drawing flow
// (files trio → conduit prepare → image SSE → conversation poll → download
// URL resolve → PNG download + dedupe), the text-reply fallback with
// conversation recovery (Review Focus #5), content-policy detection, the
// mid-stream hard timeout, the tasks error doc, and the pinned progress step
// order (uploading → preparing → generating → receiving_image).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	outputFileID    = "file_00000000fedcba9876543210fedcba98"
	referenceFileID = "file_00000000a1b2c3d4e5f60718293a4b"
	blobUploadURL   = "https://upload.blob.example.com/containers/refs/blob-abc"

	conversationsListURL = chatGPTBaseURL + "/backend-api/conversations?offset=0&limit=10&order=updated&conversation_filter=all"
)

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func asImageError(err error, target **ImageError) bool {
	return errors.As(err, target)
}

// scriptUploadTrio scripts the reference-upload trio (create → blob PUT →
// confirm) for one image landing at referenceFileID.
func scriptUploadTrio(fake *drawFake, uploadURL, fileID string) {
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files", "files-create.json", 200)
	fake.status("PUT "+uploadURL, http.StatusCreated, nil)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files/"+fileID+"/uploaded", "files-uploaded.json", 200)
}

// scriptPollSuccess scripts the post-SSE settle: an empty tasks list, a
// pending doc then a complete doc carrying the output file id, and a download
// URL resolve (primary or fallback variant) ending in one PNG download.
func scriptPollSuccess(t *testing.T, fake *drawFake, conversationID, downloadVariant string) {
	t.Helper()
	fake.fixture("GET "+chatGPTBaseURL+"/backend-api/tasks", "tasks-ok.json", 200)
	pending, err := osReadFixture("conversation-doc-pending.json")
	if err != nil {
		t.Fatalf("read pending doc fixture: %v", err)
	}
	complete, err := osReadFixture("conversation-doc-complete.json")
	if err != nil {
		t.Fatalf("read complete doc fixture: %v", err)
	}
	calls := 0
	fake.on("GET "+chatGPTBaseURL+"/backend-api/conversation/"+conversationID, func() Resp {
		body := pending
		if calls > 0 {
			body = complete // the document settles between polls
		}
		calls++
		return Resp{StatusCode: 200, Headers: http.Header{}, Body: body}
	})
	switch downloadVariant {
	case "primary":
		fake.fixture("GET "+chatGPTBaseURL+"/backend-api/files/"+outputFileID+"/download", "files-download-primary.json", 200)
		fake.status("GET https://cdn.example.com/img/primary.png", 200, pngBytes)
	default: // fallback: the primary lookup 404s, the alternate route resolves
		fake.status("GET "+chatGPTBaseURL+"/backend-api/files/"+outputFileID+"/download", 404, nil)
		fake.fixture("GET "+chatGPTBaseURL+"/backend-api/files/download/"+outputFileID, "files-download-fallback.json", 200)
		fake.status("GET https://cdn.example.com/img/fallback.png", 200, pngBytes)
	}
}

// ---------------------------------------------------------------------------
// TestGenerateHappyPath — SSE carries file-service ids → settle → resolve
// (fallback variant) → one PNG, pinned progress order, typed wire shapes.
// ---------------------------------------------------------------------------

func TestGenerateHappyPath(t *testing.T) {
	cfg := testDrawingCfg()
	fake := newDrawFake(t)
	fake.chunks = sseFixture(t, "draw-success.sse")
	scriptUploadTrio(fake, blobUploadURL, referenceFileID)
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))
	scriptPollSuccess(t, fake, "c-draw-1", "fallback")

	var steps []string
	var sleeps []time.Duration
	fakeDrawingClock(t, &sleeps)
	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req", Proof: "fixture-pow"}, cfg,
		"a lighthouse", []ReferenceImage{{Data: pngBytes, Width: 4, Height: 3}},
		"1024x1024", "high", func(step string) { steps = append(steps, step) })
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	assertProgress(t, steps, "uploading", "preparing", "generating", "receiving_image")
	if len(result.Images) != 1 || string(result.Images[0].Data) != string(pngBytes) {
		t.Fatalf("images = %d, want the one downloaded PNG", len(result.Images))
	}
	if result.ConversationID != "c-draw-1" {
		t.Fatalf("ConversationID = %q", result.ConversationID)
	}
	if result.RevisedPrompt != "a lighthouse" {
		t.Fatalf("RevisedPrompt = %q, want the original prompt echoed (oracle format_image_result)", result.RevisedPrompt)
	}
	wantDrawingSleeps(t, sleeps, 10) // seeded SSE ids hit on the first fetch after the 10s initial wait (oracle check-before-hit fast return)
	assertNoHideConversation(t, fake)

	// The prepare body: byte-pinned picture_v2 orchestration.
	prepares := fake.requestsFor(http.MethodPost, chatGPTBaseURL+drawingPreparePath)
	if len(prepares) != 1 {
		t.Fatalf("prepare calls = %d, want 1", len(prepares))
	}
	var prepareBody map[string]any
	if err := json.Unmarshal(prepares[0].Body, &prepareBody); err != nil {
		t.Fatalf("prepare body is not JSON: %v", err)
	}
	assertPrepareBody(t, prepareBody)

	// The SSE request: conduit token, fresh trace id, image headers, payload.
	streams := fake.requestsFor(http.MethodPost, chatGPTBaseURL+drawingSSEPath)
	if len(streams) != 1 {
		t.Fatalf("SSE calls = %d, want 1", len(streams))
	}
	req := streams[0]
	if got := req.Headers.Get("X-Conduit-Token"); got != "conduit-1" {
		t.Fatalf("X-Conduit-Token = %q", got)
	}
	if got := req.Headers.Get("X-Oai-Turn-Trace-Id"); !uuidRE.MatchString(got) {
		t.Fatalf("X-Oai-Turn-Trace-Id = %q, want a fresh uuid4", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "fixture-req" {
		t.Fatalf("requirements token header = %q", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Proof-Token"); got != "fixture-pow" {
		t.Fatalf("proof token header = %q", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Turnstile-Token"); got != "" {
		t.Fatalf("the image route carries no turnstile header (oracle _image_headers)")
	}
	if got := req.Headers.Get("X-OpenAI-Target-Path"); got != drawingSSEPath {
		t.Fatalf("X-OpenAI-Target-Path = %q", got)
	}
	assertImageSSEBody(t, req.Body, referenceFileID)
}

func assertPrepareBody(t *testing.T, body map[string]any) {
	t.Helper()
	if body["action"] != "next" || body["client_prepare_state"] != "success" {
		t.Fatalf("prepare body action/client_prepare_state = %v/%v", body["action"], body["client_prepare_state"])
	}
	if body["model"] != "gpt-5-5" {
		t.Fatalf("prepare model = %v, want the drawing upstream model", body["model"])
	}
	if _, ok := body["thinking_effort"]; ok {
		t.Fatalf("thinking_effort must be omitted for the default \"auto\" effort")
	}
	hints, _ := body["system_hints"].([]any)
	if len(hints) != 1 || hints[0] != "picture_v2" {
		t.Fatalf("system_hints = %v, want [picture_v2]", hints)
	}
	pq, _ := body["partial_query"].(map[string]any)
	content, _ := pq["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	wantBuilt := "a lighthouse" + "\n\n" + fmt.Sprintf(imageSizeHintFormat, "1024x1024") + fmt.Sprintf(imageQualityHintFormat, "high")
	if len(parts) != 1 || parts[0] != wantBuilt {
		t.Fatalf("partial_query parts = %v, want the size/quality-decorated prompt", parts)
	}
	ci, _ := body["client_contextual_info"].(map[string]any)
	if ci["app_name"] != "chatgpt.com" {
		t.Fatalf("client_contextual_info = %v", ci)
	}
	if body["supports_buffering"] != true || !reflectStringSlice(body["supported_encodings"], "v1") {
		t.Fatalf("supports_buffering/supported_encodings = %v/%v", body["supports_buffering"], body["supported_encodings"])
	}
}

func reflectStringSlice(value any, item string) bool {
	items, _ := value.([]any)
	return len(items) == 1 && items[0] == item
}

func assertImageSSEBody(t *testing.T, body []byte, referenceID string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("SSE body is not JSON: %v", err)
	}
	if doc["action"] != "next" || doc["client_prepare_state"] != "sent" || doc["model"] != "gpt-5-5" {
		t.Fatalf("SSE body core fields = %v/%v/%v", doc["action"], doc["client_prepare_state"], doc["model"])
	}
	if doc["enable_message_followups"] != true || doc["supports_buffering"] != true {
		t.Fatalf("SSE body flags = %v/%v", doc["enable_message_followups"], doc["supports_buffering"])
	}
	if doc["paragen_cot_summary_display_override"] != "allow" || doc["force_parallel_switch"] != "auto" {
		t.Fatalf("SSE body overrides = %v/%v", doc["paragen_cot_summary_display_override"], doc["force_parallel_switch"])
	}
	ci, _ := doc["client_contextual_info"].(map[string]any)
	if ci["time_since_loaded"] != float64(1200) || ci["pixel_ratio"] != 1.2 || ci["page_height"] != float64(1072) {
		t.Fatalf("client_contextual_info = %v, want the oracle's drawing geometry", ci)
	}
	messages, _ := doc["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	msg, _ := messages[0].(map[string]any)
	if author, _ := msg["author"].(map[string]any); author["role"] != "user" {
		t.Fatalf("message author = %v", msg["author"])
	}
	created, _ := msg["create_time"].(float64)
	if created <= 0 {
		t.Fatalf("message create_time = %v, want an epoch float", msg["create_time"])
	}
	content, _ := msg["content"].(map[string]any)
	if content["content_type"] != "multimodal_text" {
		t.Fatalf("content_type = %v, want multimodal_text", content["content_type"])
	}
	parts, _ := content["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want the image_asset_pointer + prompt", len(parts))
	}
	pointer, _ := parts[0].(map[string]any)
	if pointer["content_type"] != "image_asset_pointer" || pointer["asset_pointer"] != "file-service://"+referenceID {
		t.Fatalf("part 0 = %v, want the file-service pointer", parts[0])
	}
	if pointer["width"] != float64(4) || pointer["height"] != float64(3) || pointer["size_bytes"] != float64(len(pngBytes)) {
		t.Fatalf("pointer dims = %v/%v/%v", pointer["width"], pointer["height"], pointer["size_bytes"])
	}
	wantBuilt := "a lighthouse" + "\n\n" + fmt.Sprintf(imageSizeHintFormat, "1024x1024") + fmt.Sprintf(imageQualityHintFormat, "high")
	if parts[1] != wantBuilt {
		t.Fatalf("part 1 = %v, want the decorated prompt", parts[1])
	}
	metadata, _ := msg["metadata"].(map[string]any)
	if !reflectStringSlice(metadata["system_hints"], "picture_v2") {
		t.Fatalf("metadata.system_hints = %v", metadata["system_hints"])
	}
	attachments, _ := metadata["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("metadata.attachments = %v, want one entry", metadata["attachments"])
	}
	att, _ := attachments[0].(map[string]any)
	if att["id"] != referenceID || att["mimeType"] != "image/png" || att["name"] != "image_1.png" || att["size"] != float64(len(pngBytes)) {
		t.Fatalf("attachment = %v", att)
	}
}

// TestGenerateMultiReference — two references → two image_asset_pointer parts
// with their file-service ids, two attachment entries, two upload trios.

func TestGenerateMultiReference(t *testing.T) {
	cfg := testDrawingCfg()
	fake := newDrawFake(t)
	fake.chunks = sseFixture(t, "draw-success.sse")

	creates := 0
	fake.on("POST "+chatGPTBaseURL+"/backend-api/files", func() Resp {
		creates++
		if creates == 1 {
			body, _ := osReadFixture("files-create.json")
			return Resp{StatusCode: 200, Body: body}
		}
		return Resp{StatusCode: 200, Body: []byte(`{"upload_url": "https://upload.blob.example.com/containers/refs/blob-def", "file_id": "file_00000000b2c3d4e5f60718293a4b5c"}`)}
	})
	fake.status("PUT "+blobUploadURL, http.StatusCreated, nil)
	fake.status("PUT https://upload.blob.example.com/containers/refs/blob-def", http.StatusCreated, nil)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files/"+referenceFileID+"/uploaded", "files-uploaded.json", 200)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files/file_00000000b2c3d4e5f60718293a4b5c/uploaded", "files-uploaded.json", 200)
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))
	scriptPollSuccess(t, fake, "c-draw-1", "primary")

	secondPNG := append([]byte{}, pngBytes...)
	secondPNG[len(secondPNG)-1]++
	fake.handlers["GET https://cdn.example.com/img/primary.png"] = func() Resp {
		return Resp{StatusCode: 200, Body: secondPNG}
	}

	var sleeps []time.Duration
	fakeDrawingClock(t, &sleeps)

	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg,
		"a cat and a dog", []ReferenceImage{
			{Data: pngBytes, Width: 4, Height: 3},
			{Data: secondPNG, FileName: "cat.jpg", MimeType: "image/jpeg", Width: 8, Height: 6},
		}, "", "", nil)
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(result.Images))
	}

	puts := fake.requestsForMethod(http.MethodPut)
	if len(puts) != 2 {
		t.Fatalf("blob PUTs = %d, want 2", len(puts))
	}
	if string(puts[1].Body) != string(secondPNG) {
		t.Fatalf("second PUT body is not the second image")
	}
	if got := puts[1].Headers.Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("second PUT Content-Type = %q, want the per-image mime", got)
	}

	streams := fake.requestsFor(http.MethodPost, chatGPTBaseURL+drawingSSEPath)
	var doc map[string]any
	_ = json.Unmarshal(streams[0].Body, &doc)
	messages, _ := doc["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 2 pointers + prompt", len(parts))
	}
	p0, _ := parts[0].(map[string]any)
	p1, _ := parts[1].(map[string]any)
	if p0["asset_pointer"] != "file-service://"+referenceFileID || p1["asset_pointer"] != "file-service://file_00000000b2c3d4e5f60718293a4b5c" {
		t.Fatalf("asset pointers = %v / %v", p0["asset_pointer"], p1["asset_pointer"])
	}
	if parts[2] != "a cat and a dog" {
		t.Fatalf("prompt part = %v", parts[2])
	}
	metadata, _ := msg["metadata"].(map[string]any)
	attachments, _ := metadata["attachments"].([]any)
	if len(attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(attachments))
	}
	att1, _ := attachments[1].(map[string]any)
	if att1["id"] != "file_00000000b2c3d4e5f60718293a4b5c" || att1["mimeType"] != "image/jpeg" || att1["name"] != "cat.jpg" {
		t.Fatalf("second attachment = %v", att1)
	}
	if content["content_type"] != "multimodal_text" {
		t.Fatalf("content_type = %v", content["content_type"])
	}
	assertNoHideConversation(t, fake)
}

// TestGenerateTextReplyFallback — Review Focus #5: SSE ends with prose (the
// referenced_image_ids marker), the conversation id is recovered from the
// recent-conversations list, the extended poll settles, and the image comes
// back. No references, no size/quality decoration.

func TestGenerateTextReplyFallback(t *testing.T) {
	cfg := testDrawingCfg()
	fake := newDrawFake(t)
	fake.chunks = sseFixture(t, "draw-text-reply.sse")
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))
	fake.fixture("GET "+conversationsListURL, "draw-recent-conversations.json", 200)
	scriptPollSuccess(t, fake, "c-rec-1", "primary")

	var sleeps []time.Duration
	var steps []string
	fakeDrawingClock(t, &sleeps)
	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg,
		"a lighthouse painting", nil, "", "", func(step string) { steps = append(steps, step) })
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if result.ConversationID != "c-rec-1" {
		t.Fatalf("ConversationID = %q, want the recovered id", result.ConversationID)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(result.Images))
	}
	assertProgress(t, steps, "uploading", "preparing", "generating", "receiving_image")
	wantDrawingSleeps(t, sleeps, 10, 10)

	lists := fake.requestsFor(http.MethodGet, conversationsListURL)
	if len(lists) != 1 {
		t.Fatalf("conversations-list calls = %d, want exactly one recovery attempt", len(lists))
	}
	if got := lists[0].Headers.Get("Accept"); got != "application/json" {
		t.Fatalf("recovery Accept = %q", got)
	}
	if got := lists[0].Headers.Get("X-OpenAI-Target-Path"); got != "/backend-api/conversations?offset=0&limit=10&order=updated&conversation_filter=all" {
		t.Fatalf("recovery X-OpenAI-Target-Path = %q (oracle passes the full query path)", got)
	}
	assertNoHideConversation(t, fake)
}

// TestGenerateContentPolicy — moderation-blocked prose: the typed
// content_policy_violation carries the tasks detail, never the bearer token,
// and no conversation document fetch happens.

func TestGenerateContentPolicy(t *testing.T) {
	cfg := testDrawingCfg()
	fake := newDrawFake(t)
	fake.chunks = sseFixture(t, "draw-content-policy.sse")
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))
	fake.fixture("GET "+chatGPTBaseURL+"/backend-api/tasks", "tasks-error-policy.json", 200)

	var sleeps []time.Duration
	var steps []string
	fakeDrawingClock(t, &sleeps)
	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg,
		"make a disturbing picture", nil, "", "", func(step string) { steps = append(steps, step) })
	assertImageError(t, err, ImageCodeContentPolicy)
	var imgErr *ImageError
	if !asImageError(err, &imgErr) {
		t.Fatalf("error = %v", err)
	}
	if imgErr.Message != "This image request was blocked by upstream moderation." {
		t.Fatalf("Message = %q, want the tasks detail text", imgErr.Message)
	}
	if strings.Contains(imgErr.Error(), "fixture-access-token") {
		t.Fatalf("error text must never echo the bearer token: %s", imgErr.Error())
	}
	assertProgress(t, steps, "uploading", "preparing", "generating")
	wantDrawingSleeps(t, sleeps, 2) // the oracle's blocked-branch tasksDetail wait_secs=2.0
	if len(result.Images) != 0 {
		t.Fatalf("images = %d, want none", len(result.Images))
	}
	if result.ConversationID != "c-policy-1" {
		t.Fatalf("ConversationID = %q", result.ConversationID)
	}
	if got := len(fake.requestsFor(http.MethodGet, chatGPTBaseURL+"/backend-api/conversation/c-policy-1")); got != 0 {
		t.Fatalf("blocked prose must not fetch the conversation document (got %d calls)", got)
	}
}

// TestGenerateTimeoutMidStream — a stalled SSE hits the hard cap (cfg
// image-poll-timeout re-used as the cap, oracle _iter_sse_payloads_capped)
// and fails typed with the partial result discarded.

func TestGenerateTimeoutMidStream(t *testing.T) {
	cfg := testDrawingCfg()
	cfg.ImagePollTimeoutSecs = 0 // hard cap fires immediately against the stall
	fake := newDrawFake(t)
	fake.stall = true
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))

	var steps []string
	before := time.Now()
	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg,
		"a lighthouse", nil, "", "", func(step string) { steps = append(steps, step) })
	if time.Since(before) > 5*time.Second {
		t.Fatalf("the hard cap must fail fast, took %v", time.Since(before))
	}
	assertImageError(t, err, ImageCodeStreamTimeout)
	var imgErr *ImageError
	if !asImageError(err, &imgErr) || !imgErr.Retryable {
		t.Fatalf("error = %+v, want retryable typed timeout", err)
	}
	assertProgress(t, steps, "uploading", "preparing", "generating")
	if len(result.Images) != 0 {
		t.Fatalf("the partial result must be discarded (got %d images)", len(result.Images))
	}
	if !fake.wasClosed() {
		t.Fatalf("the watchdog did not close the stalled stream")
	}
}

// TestGenerateTasksErrorDoc — metadata.is_error in the tasks doc, detected
// during the settle phase, ends the turn typed before any document fetch.

func TestGenerateTasksErrorDoc(t *testing.T) {
	cfg := testDrawingCfg()
	fake := newDrawFake(t)
	fake.chunks = sseFixture(t, "draw-success.sse")
	fake.status("POST "+chatGPTBaseURL+drawingPreparePath, 200, []byte(`{"conduit_token": "conduit-1"}`))
	fake.fixture("GET "+chatGPTBaseURL+"/backend-api/tasks", "tasks-error.json", 200)

	result, err := GenerateImage(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg,
		"a lighthouse", nil, "", "", nil)
	assertImageError(t, err, ImageCodeUpstreamImageError)
	var imgErr *ImageError
	if !asImageError(err, &imgErr) {
		t.Fatalf("error = %v", err)
	}
	if imgErr.Message != "Image generation failed upstream: provider error" {
		t.Fatalf("Message = %q, want the matching conversation's task error", imgErr.Message)
	}
	if strings.Contains(imgErr.Message, "unrelated error") {
		t.Fatalf("the client-side conversation filter leaked another conversation's task")
	}
	if result.ConversationID != "c-draw-1" {
		t.Fatalf("ConversationID = %q", result.ConversationID)
	}
	if got := len(fake.requestsFor(http.MethodGet, chatGPTBaseURL+"/backend-api/conversation/c-draw-1")); got != 0 {
		t.Fatalf("a detected tasks error must abort before the conversation document poll (got %d calls)", got)
	}
}

func assertNoHideConversation(t *testing.T, fake *drawFake) {
	t.Helper()
	for _, req := range fake.requests {
		if req.Method == http.MethodPatch && strings.Contains(req.URL, "/conversation/") {
			t.Fatalf("the drawing pipeline must not hide conversations (got PATCH %s)", req.URL)
		}
	}
}

// ---------------------------------------------------------------------------
// Unit pins for the oracle logic the flow tests exercise indirectly.
// ---------------------------------------------------------------------------

func TestBuildImagePrompt(t *testing.T) {
	if got := buildImagePrompt("a cat", "", ""); got != "a cat" {
		t.Fatalf("no hints: got %q, want the prompt unchanged", got)
	}
	want := "a cat" + "\n\n" + fmt.Sprintf(imageSizeHintFormat, "1024x1024")
	if got := buildImagePrompt("a cat", "1024x1024", ""); got != want {
		t.Fatalf("size hint: got %q, want %q", got, want)
	}
	wantBoth := "a cat" + "\n\n" + fmt.Sprintf(imageSizeHintFormat, "1024x1024") + fmt.Sprintf(imageQualityHintFormat, "low")
	if got := buildImagePrompt("  a cat  ", "1024x1024", "low"); got != wantBoth {
		t.Fatalf("both hints: got %q, want %q (trimmed prompt)", got, wantBoth)
	}
}

func TestIsModelTextReply(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{`Preparing the image. Parameters: {"referenced_image_ids": ["ref-1"], "n": 1}`, true},
		{`{"size":"1920x1088","n":1}`, true},
		{`{"size" : "800x600" , "n" : 2}`, true},
		{`Just a nice painting of a lighthouse.`, false},
		{`{"referenced_image_ids": []}`, false}, // empty list does not match the oracle regex
		{"", false},
	}
	for _, tc := range cases {
		if got := isModelTextReply(tc.message); got != tc.want {
			t.Fatalf("isModelTextReply(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}

func TestExtractImageToolRecordsAndPolicyScan(t *testing.T) {
	var doc struct {
		Mapping map[string]any
	}
	body := `{
		"mapping": {
			"u": {"message": {"author": {"role": "user"}, "create_time": 1,
				"content": {"content_type": "multimodal_text", "parts": [
					{"content_type": "image_asset_pointer", "asset_pointer": "file-service://user-upload-id"}]}}},
			"a-plain": {"message": {"author": {"role": "assistant"}, "create_time": 3,
				"content": {"content_type": "text", "parts": ["just prose"]}}},
			"a-task": {"message": {"author": {"role": "assistant"}, "create_time": 2,
				"metadata": {"async_task_type": "image_gen"},
				"content": {"content_type": "text", "parts": []}}},
			"t-late": {"message": {"author": {"role": "tool"}, "create_time": 4,
				"content": {"content_type": "multimodal_text", "parts": [
					{"asset_pointer": "sediment://sed-1", "content_type": "image_asset_pointer"}]}}},
			"t-early": {"message": {"author": {"role": "tool"}, "create_time": 1,
				"content": {"content_type": "multimodal_text", "parts": [
					{"asset_pointer": "file-service://out-1"}]}}}
		}
	}`
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	records := extractImageToolRecords(doc.Mapping)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3 (user uploads and plain assistant prose are not records)", len(records))
	}
	var gotIDs []string
	for _, rec := range records {
		gotIDs = append(gotIDs, rec.MessageID)
	}
	if strings.Join(gotIDs, ",") != "t-early,a-task,t-late" {
		t.Fatalf("records sorted by create_time = %v, want [t-early a-task t-late]", gotIDs)
	}
	if records[0].FileIDs[0] != "out-1" || len(records[0].SedimentIDs) != 0 {
		t.Fatalf("t-early ids = %v/%v", records[0].FileIDs, records[0].SedimentIDs)
	}
	if len(records[1].FileIDs) != 0 && len(records[1].SedimentIDs) != 0 {
		t.Fatalf("a-task is a record with no ids (async_task_type image_gen)")
	}
	if records[2].SedimentIDs[0] != "sed-1" {
		t.Fatalf("t-late sediment = %v", records[2].SedimentIDs)
	}

	if got := findContentPolicyError(doc.Mapping); got != "" {
		t.Fatalf("policy scan = %q, want empty for this document", got)
	}

	// A policy scan hit returns the assistant text truncated to 500 chars
	// (the oracle truncates AFTER the keyword match, so the keyword must
	// sit inside the first 500).
	policyDoc := struct {
		Mapping map[string]any
	}{}
	longText := "This violates the content policy. " + strings.Repeat("x", 550)
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"mapping":{
		"user": {"message": {"author": {"role": "user"}, "content": {"content_type": "text", "parts": ["blocked text here"]}}},
		"asst": {"message": {"author": {"role": "assistant"}, "content": {"content_type": "text", "parts": [%q]}}}}}`, longText)), &policyDoc)
	got := findContentPolicyError(policyDoc.Mapping)
	if len(got) != 500 || !strings.Contains(got, "policy") {
		t.Fatalf("policy scan = %d chars, want 500 incl. the keyword", len(got))
	}
	if strings.Contains(got, "blocked text here") {
		t.Fatalf("user messages are never scanned for policy text")
	}
}

func TestResolveImageURLs(t *testing.T) {
	fake := newDrawFake(t)
	idPrimary := outputFileID
	idFallback := "file_00000000c3d4e5f60718293a4bb2c3"
	idBroken := "file_00000000e5f60718293a4bb2c3d4e5"
	fake.fixture("GET "+chatGPTBaseURL+"/backend-api/files/"+idPrimary+"/download", "files-download-primary.json", 200)
	fake.status("GET "+chatGPTBaseURL+"/backend-api/files/"+idFallback+"/download", 404, nil)
	fake.fixture("GET "+chatGPTBaseURL+"/backend-api/files/download/"+idFallback, "files-download-fallback.json", 200)
	fake.status("GET "+chatGPTBaseURL+"/backend-api/files/"+idBroken+"/download", 500, nil)
	fake.status("GET "+chatGPTBaseURL+"/backend-api/files/download/"+idBroken, 500, nil)
	// The sediment attachment resolves to the same URL as the primary file:
	// dedupe must collapse it.
	fake.status("GET "+chatGPTBaseURL+"/backend-api/conversation/c-1/attachment/sed-1/download", 200,
		[]byte(`{"download_url": "https://cdn.example.com/img/primary.png"}`))

	cred := fixtureCredential()
	urls := resolveImageURLs(context.Background(), fake, cred, "c-1",
		[]string{"file_upload", idPrimary, idFallback, idBroken}, []string{"sed-1"})
	want := []string{"https://cdn.example.com/img/primary.png", "https://cdn.example.com/img/fallback.png"}
	if len(urls) != len(want) || urls[0] != want[0] || urls[1] != want[1] {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
	if got := len(fake.requestsFor(http.MethodGet, chatGPTBaseURL+"/backend-api/files/file_upload/download")); got != 0 {
		t.Fatalf("the file_upload skip pattern must never hit the wire (got %d calls)", got)
	}

	// Without a conversation id, sediment ids resolve to nothing.
	fake2 := newDrawFake(t)
	if urls2 := resolveImageURLs(context.Background(), fake2, cred, "", nil, []string{"sed-1"}); urls2 != nil {
		t.Fatalf("no conversation: got %v — want empty", urls2)
	}
}

func TestFindConversationByPrompt(t *testing.T) {
	fake := newDrawFake(t)
	fake.fixture("GET "+conversationsListURL, "draw-recent-conversations.json", 200)
	cred := fixtureCredential()

	if got := findConversationByPrompt(context.Background(), fake, cred, "a lighthouse painting", fixedEpoch); got != "c-rec-1" {
		t.Fatalf("keyword scoring = %q, want c-rec-1", got)
	}
	if got := findConversationByPrompt(context.Background(), fake, cred, "zzz qqq", fixedEpoch); got != "c-rec-1" {
		t.Fatalf("no-keyword fallback = %q, want the latest in-window conversation", got)
	}

	// An upstream failure swallows to empty (recovery failed), no error.
	fake2 := newDrawFake(t)
	fake2.status("GET "+conversationsListURL, 500, nil)
	if got := findConversationByPrompt(context.Background(), fake2, cred, "a lighthouse painting", fixedEpoch); got != "" {
		t.Fatalf("failed list = %q, want empty", got)
	}

	// An empty list returns empty.
	fake3 := newDrawFake(t)
	fake3.status("GET "+conversationsListURL, 200, []byte(`{"items": []}`))
	if got := findConversationByPrompt(context.Background(), fake3, cred, "a lighthouse painting", fixedEpoch); got != "" {
		t.Fatalf("empty list = %q, want empty", got)
	}
}

// wantDrawingSleeps asserts the recorded fake-clock sleeps by seconds.
func wantDrawingSleeps(t *testing.T, sleeps []time.Duration, seconds ...float64) {
	t.Helper()
	if len(sleeps) != len(seconds) {
		t.Fatalf("sleeps = %v, want %v seconds", sleeps, seconds)
	}
	for i, want := range seconds {
		if sleeps[i].Seconds() != want {
			t.Fatalf("sleep[%d] = %v, want %v (all: %v)", i, sleeps[i], want, sleeps)
		}
	}
}
