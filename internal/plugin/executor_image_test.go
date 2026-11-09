package plugin

// executor_image_test.go pins the Task 13 contract: the openai-image format
// (the host's /v1/images/generations and /v1/images/edits arriving
// untranslated) routed by model base name to the two reviewed pipelines
// (web-drawing GenerateImage, codex GenerateCodexImage), with the n>1
// sequential loop, the generations-vs-edits payload parse, the message_as_error
// mapping (Review Focus #5), the pinned images response envelope and the
// single-chunk stream.
//
// The image pipelines themselves (Task 9/10) carry an upstream polling clock
// that is unexported, so the executor tests stub the pipeline ENTRY POINTS
// (imageAcquireSentinel / imageGenerate / imageGenerateCodex) with canned
// ImageResults rather than driving a full pipeline run. That keeps these tests
// focused on the executor's own logic — route selection, payload parsing,
// response shaping, the n-loop, message_as_error, the stream — which is what
// Task 13 owns. The pipeline internals are fully covered by the upstream
// package's own fixture-clock tests.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

// solidPNG is a 2x2 solid-color PNG — a valid decodable image the executor can
// hand to the pipeline and read back from the response.
func solidPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetRGBA(x, y, color.RGBA{200, 30, 30, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// maskPNG is a 2x2 RGBA PNG with a single transparent pixel — a decodable edit
// mask whose alpha carves the reference when composited.
func maskPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	img.SetRGBA(1, 0, color.RGBA{0, 255, 0, 255})
	img.SetRGBA(0, 1, color.RGBA{0, 0, 255, 255})
	img.SetRGBA(1, 1, color.RGBA{0, 0, 0, 0}) // transparent edit hole
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// The pipeline-entry-point stub.
// ---------------------------------------------------------------------------

// imgStub records every pipeline entry point the executor invokes and hands
// back canned results. lastRefs is the references the drawing/codex route
// received (the parsed edit input); lastProfile is the codex plan; lastCred
// is the credential the route ran with (the workbench pins the credential
// acquisition through it).
type imgStub struct {
	acquireCalls  int
	generateCalls int
	codexCalls    int
	lastRefs      []upstream.ReferenceImage
	lastPrompt    string
	lastSize      string
	lastQuality   string
	lastProfile   upstream.Profile
	lastCred      Credential
	gotTokens     upstream.SentinelTokens

	tokens   upstream.SentinelTokens
	tokenErr error

	genResult upstream.ImageResult
	genErr    error

	codexResult upstream.ImageResult
	codexErr    error
}

// stubImageSeams swaps the three pipeline entry points for scripted fakes and
// restores the originals on cleanup (the sseHardCapFor test-seam pattern).
func stubImageSeams(t *testing.T, s *imgStub) {
	t.Helper()
	origAcquire := imageAcquireSentinel
	origGen := imageGenerate
	origCodex := imageGenerateCodex
	t.Cleanup(func() {
		imageAcquireSentinel = origAcquire
		imageGenerate = origGen
		imageGenerateCodex = origCodex
	})

	imageAcquireSentinel = func(_ context.Context, _ upstream.Upstream, _ Credential, _ bool) (upstream.SentinelTokens, error) {
		s.acquireCalls++
		return s.tokens, s.tokenErr
	}
	imageGenerate = func(_ context.Context, _ upstream.Upstream, cred Credential, tokens upstream.SentinelTokens, _ Config, prompt string, references []upstream.ReferenceImage, size, quality string, _ func(step string)) (upstream.ImageResult, error) {
		s.generateCalls++
		s.lastCred = cred
		s.gotTokens = tokens
		s.lastRefs = references
		s.lastPrompt = prompt
		s.lastSize = size
		s.lastQuality = quality
		return s.genResult, s.genErr
	}
	imageGenerateCodex = func(_ context.Context, _ upstream.Upstream, cred Credential, profile upstream.Profile, prompt string, references []upstream.ReferenceImage, size, quality string) (upstream.ImageResult, error) {
		s.codexCalls++
		s.lastCred = cred
		s.lastProfile = profile
		s.lastRefs = references
		s.lastPrompt = prompt
		s.lastSize = size
		s.lastQuality = quality
		return s.codexResult, s.codexErr
	}
}

// ---------------------------------------------------------------------------
// Request/response helpers.
// ---------------------------------------------------------------------------

// imgRequest builds one SDK-typed executor request in the openai-image format.
// headers carries the request Content-Type (the multipart boundary for edits).
func imgRequest(t *testing.T, cred Credential, model string, payload []byte, headers http.Header) pluginapi.ExecutorRequest {
	t.Helper()
	return pluginapi.ExecutorRequest{
		AuthID:       execAuthFile,
		AuthProvider: providerKey,
		Model:        model,
		Format:       "openai-image",
		SourceFormat: "openai-image",
		Payload:      payload,
		Headers:      headers,
		StorageJSON:  cred.StorageJSON(),
	}
}

func imgJSONPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal image payload: %v", err)
	}
	return raw
}

// buildMultipartEdits renders one host re-encoded edits body: one image[] file
// part (raw bytes) plus a mask[] file part and the prompt/size/quality form
// fields. It returns the body and the Content-Type (with boundary).
func buildMultipartEdits(t *testing.T, img, mask []byte, prompt, size, quality string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image[]", "ref.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := fw.Write(img); err != nil {
		t.Fatalf("write image part: %v", err)
	}
	mfw, err := w.CreateFormFile("mask[]", "mask.png")
	if err != nil {
		t.Fatalf("create mask part: %v", err)
	}
	if _, err := mfw.Write(mask); err != nil {
		t.Fatalf("write mask part: %v", err)
	}
	_ = w.WriteField("prompt", prompt)
	_ = w.WriteField("size", size)
	_ = w.WriteField("quality", quality)
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// imageResponse mirrors the pinned OpenAI images envelope.
type imageResponse struct {
	Created int64 `json:"created"`
	Data    []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
		Message       string `json:"message"`
	} `json:"data"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// decodeImageExecResult asserts a success envelope and returns the images
// payload decoded into its pinned shape.
func decodeImageExecResult(t *testing.T, resp []byte) imageResponse {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("executor envelope not OK: %s", resp)
	}
	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var ir imageResponse
	if err := json.Unmarshal(out.Payload, &ir); err != nil {
		t.Fatalf("payload is not an images response: %v (%s)", err, out.Payload)
	}
	return ir
}

func assertPNGInEntry(t *testing.T, b64 string, want []byte) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("b64_json is not valid base64: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("b64_json does not decode to the fixture PNG")
	}
}

// ---------------------------------------------------------------------------
// TestGenerationsHappyPath — drawing fixture → created + data[0].b64_json
// decodes to the fixture PNG, revised_prompt present, sentinel acquired once,
// the drawing route called, the codex route NOT called.
// ---------------------------------------------------------------------------

func TestGenerationsHappyPath(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a lighthouse"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	payload := imgJSONPayload(t, map[string]any{
		"model":   "chatgpt-proxy/gpt-image-2-proxy",
		"prompt":  "a lighthouse",
		"size":    "1024x1024",
		"quality": "high",
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if ir.Created <= 0 {
		t.Fatalf("created = %d, want a unix timestamp", ir.Created)
	}
	if len(ir.Data) != 1 {
		t.Fatalf("data = %d, want exactly one image", len(ir.Data))
	}
	assertPNGInEntry(t, ir.Data[0].B64JSON, png)
	if ir.Data[0].RevisedPrompt != "a lighthouse" {
		t.Fatalf("revised_prompt = %q, want the prompt echoed", ir.Data[0].RevisedPrompt)
	}
	if ir.Usage.TotalTokens <= 0 {
		t.Fatalf("usage = %+v, want a documented estimate", ir.Usage)
	}

	// Route selection: the drawing route acquired one sentinel bundle and ran
	// the pipeline once; the codex route never ran.
	if s.acquireCalls != 1 {
		t.Fatalf("acquireCalls = %d, want 1 (sentinel per run)", s.acquireCalls)
	}
	if s.generateCalls != 1 {
		t.Fatalf("generateCalls = %d, want 1", s.generateCalls)
	}
	if s.codexCalls != 0 {
		t.Fatalf("codexCalls = %d, want 0 (drawing model must not use the codex route)", s.codexCalls)
	}
	if s.lastPrompt != "a lighthouse" || s.lastSize != "1024x1024" || s.lastQuality != "high" {
		t.Fatalf("pipeline args = {prompt:%q size:%q quality:%q}", s.lastPrompt, s.lastSize, s.lastQuality)
	}
	if len(s.lastRefs) != 0 {
		t.Fatalf("generations carries no references, want none: %v", s.lastRefs)
	}
}

// ---------------------------------------------------------------------------
// TestCodexRouteSelected — model base codex-gpt-image-2 → the codex route is
// called with the stored plan (no fresh ProbeProfile) and no sentinel; the
// drawing route is NOT called.
// ---------------------------------------------------------------------------

func TestCodexRouteSelected(t *testing.T) {
	cred := execCredential()
	cred.Plan = "pro"
	png := solidPNG()
	s := &imgStub{
		codexResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a codex cat"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	payload := imgJSONPayload(t, map[string]any{
		"model":  "chatgpt-proxy/codex-gpt-image-2",
		"prompt": "a cat",
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/codex-gpt-image-2", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if len(ir.Data) != 1 || ir.Data[0].RevisedPrompt != "a codex cat" {
		t.Fatalf("data = %+v, want the codex result", ir.Data)
	}
	if s.codexCalls != 1 {
		t.Fatalf("codexCalls = %d, want 1 (codex model must use the codex route)", s.codexCalls)
	}
	if s.generateCalls != 0 {
		t.Fatalf("generateCalls = %d, want 0 (drawing route must NOT be used)", s.generateCalls)
	}
	if s.acquireCalls != 0 {
		t.Fatalf("acquireCalls = %d, want 0 (the codex route runs no sentinel handshake)", s.acquireCalls)
	}
	if s.lastProfile.Plan != "pro" {
		t.Fatalf("codex profile plan = %q, want the stored credential plan (no fresh ProbeProfile)", s.lastProfile.Plan)
	}
}

// ---------------------------------------------------------------------------
// TestEditsMultipart — a real multipart edits body (1 reference + a mask) is
// parsed into references carrying dimensions and mime, the mask is
// alpha-composited (the pipeline receives bytes that differ from the original),
// and the drawing route runs once.
// ---------------------------------------------------------------------------

func TestEditsMultipart(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "edit it"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	body, contentType := buildMultipartEdits(t, png, maskPNG(), "edit it", "1024x1024", "high")
	headers := http.Header{"Content-Type": {contentType}}
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", body, headers), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if len(ir.Data) != 1 {
		t.Fatalf("data = %d, want 1", len(ir.Data))
	}
	if s.generateCalls != 1 {
		t.Fatalf("generateCalls = %d, want 1", s.generateCalls)
	}
	if len(s.lastRefs) != 1 {
		t.Fatalf("references = %d, want 1 parsed from the multipart body", len(s.lastRefs))
	}
	ref := s.lastRefs[0]
	if ref.Width != 2 || ref.Height != 2 {
		t.Fatalf("reference dims = %dx%d, want the decoded 2x2", ref.Width, ref.Height)
	}
	if ref.MimeType != "image/png" {
		t.Fatalf("reference mime = %q, want image/png", ref.MimeType)
	}
	if bytes.Equal(ref.Data, png) {
		t.Fatalf("the mask was not composited: the reference bytes equal the original")
	}
}

// ---------------------------------------------------------------------------
// TestEditsJSONFallback — a JSON edits body with image_url[] is accepted: the
// data-URL decodes to the reference bytes with dimensions and mime.
// ---------------------------------------------------------------------------

func TestEditsJSONFallback(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "edit it"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	payload := imgJSONPayload(t, map[string]any{
		"model":     "chatgpt-proxy/gpt-image-2-proxy",
		"prompt":    "edit it",
		"image_url": []string{dataURL},
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	decodeImageExecResult(t, resp)
	if s.generateCalls != 1 {
		t.Fatalf("generateCalls = %d, want 1", s.generateCalls)
	}
	if len(s.lastRefs) != 1 {
		t.Fatalf("references = %d, want 1 parsed from image_url[]", len(s.lastRefs))
	}
	ref := s.lastRefs[0]
	if !bytes.Equal(ref.Data, png) {
		t.Fatalf("the image_url data-URL was not decoded to the reference bytes")
	}
	if ref.Width != 2 || ref.Height != 2 {
		t.Fatalf("reference dims = %dx%d, want 2x2", ref.Width, ref.Height)
	}
	if ref.MimeType != "image/png" {
		t.Fatalf("reference mime = %q, want image/png", ref.MimeType)
	}
}

// ---------------------------------------------------------------------------
// TestMessageAsError — a text_reply from the pipeline is a typed 400
// content_policy_violation when message_as_error is on (the default) —
// Review Focus #5.
// ---------------------------------------------------------------------------

func TestMessageAsError(t *testing.T) {
	cred := execCredential()
	s := &imgStub{
		tokens: upstream.SentinelTokens{ChatRequirements: "req-1"},
		genErr: &upstream.ImageError{Code: upstream.ImageCodeTextReply,
			Message: "I answered with prose instead of drawing."},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "") // default config → message-as-error true

	payload := imgJSONPayload(t, map[string]any{
		"model":  "chatgpt-proxy/gpt-image-2-proxy",
		"prompt": "draw me a weapon",
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	got := decodeExecError(t, resp)
	if got.Code != "content_policy_violation" || got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %+v, want typed 400 content_policy_violation", got)
	}
}

// ---------------------------------------------------------------------------
// TestMessageAsErrorOff — with message-as-error false the prose is returned as
// a revised_prompt-only data entry carrying a warning message field.
// ---------------------------------------------------------------------------

func TestMessageAsErrorOff(t *testing.T) {
	cred := execCredential()
	prose := "Sure, here is a description instead of an image."
	s := &imgStub{
		tokens: upstream.SentinelTokens{ChatRequirements: "req-1"},
		genErr: &upstream.ImageError{Code: upstream.ImageCodeTextReply, Message: prose},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "message-as-error: false")

	payload := imgJSONPayload(t, map[string]any{
		"model":  "chatgpt-proxy/gpt-image-2-proxy",
		"prompt": "draw me a weapon",
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if len(ir.Data) != 1 {
		t.Fatalf("data = %d, want one prose entry", len(ir.Data))
	}
	if ir.Data[0].RevisedPrompt != prose {
		t.Fatalf("revised_prompt = %q, want the prose", ir.Data[0].RevisedPrompt)
	}
	if ir.Data[0].Message == "" {
		t.Fatalf("want a warning message field on the prose entry")
	}
	if ir.Data[0].B64JSON != "" || ir.Data[0].URL != "" {
		t.Fatalf("the prose entry must carry no image bytes: %+v", ir.Data[0])
	}
}

// ---------------------------------------------------------------------------
// TestNSequential — n:2 → two data[] entries, the pipeline called twice, the
// sentinel acquired once for the whole request (the fresh per-call trace id is
// a pipeline-internal guarantee pinned by the Task 9 fixture tests).
// ---------------------------------------------------------------------------

func TestNSequential(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "two of them"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	payload := imgJSONPayload(t, map[string]any{
		"model":  "chatgpt-proxy/gpt-image-2-proxy",
		"prompt": "draw two lighthouses",
		"n":      2,
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if len(ir.Data) != 2 {
		t.Fatalf("data = %d, want 2 (n is a sequential loop)", len(ir.Data))
	}
	for _, entry := range ir.Data {
		assertPNGInEntry(t, entry.B64JSON, png)
	}
	if s.generateCalls != 2 {
		t.Fatalf("generateCalls = %d, want 2 sequential pipeline runs", s.generateCalls)
	}
	if s.acquireCalls != 1 {
		t.Fatalf("acquireCalls = %d, want 1 sentinel bundle for the whole request", s.acquireCalls)
	}
}

// ---------------------------------------------------------------------------
// TestStreamIsSingleChunk — a streaming image request executes non-streaming
// and emits exactly ONE payload chunk (the final images response) then closes
// exactly once; the handler answers an empty result immediately.
// ---------------------------------------------------------------------------

func TestStreamIsSingleChunk(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a lighthouse"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "")

	req := imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy",
		imgJSONPayload(t, map[string]any{"model": "chatgpt-proxy/gpt-image-2-proxy", "prompt": "a lighthouse"}),
		nil)
	req.Stream = true

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t, req, execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	result := decodeExecResult(t, resp)
	if strings.TrimSpace(string(result)) != "{}" {
		t.Fatalf("async stream response = %s, want an empty result (the payload is emitted downstream)", result)
	}

	// The registry host writes each emitted chunk VERBATIM into the
	// text/event-stream body (no SSE synthesis on this path), so the plugin
	// must emit framed SSE: one data: event carrying the final payload, then
	// the [DONE] sentinel — matching the text executor's chunk contract.
	emits, closes := fake.downstreamCounts()
	if emits != 2 {
		t.Fatalf("emits = %d, want the framed payload event + the [DONE] sentinel", emits)
	}
	if closes != 1 {
		t.Fatalf("closes = %d, want exactly one", closes)
	}

	fake.mu.Lock()
	emitsRec := append([]execStreamEmit(nil), fake.emits...)
	closeRec := fake.closes[0]
	fake.mu.Unlock()

	first := emitsRec[0]
	if first.streamID != execDownstreamID {
		t.Fatalf("emit stream_id = %q, want the downstream id", first.streamID)
	}
	if !strings.HasPrefix(string(first.payload), "data: ") || !strings.HasSuffix(string(first.payload), "\n\n") {
		t.Fatalf("first emit is not a framed SSE event: %q", first.payload)
	}
	var ir imageResponse
	if err := json.Unmarshal(first.payload[len("data: "):len(first.payload)-2], &ir); err != nil {
		t.Fatalf("the framed chunk is not the final images payload: %v (%s)", err, first.payload)
	}
	if len(ir.Data) != 1 || ir.Data[0].B64JSON == "" {
		t.Fatalf("chunk data = %+v, want one b64_json image entry", ir.Data)
	}

	if string(emitsRec[1].payload) != "data: [DONE]\n\n" {
		t.Fatalf("last emit = %q, want the [DONE] sentinel", emitsRec[1].payload)
	}
	if closeRec.streamID != execDownstreamID || closeRec.errMsg != "" {
		t.Fatalf("close = %+v, want one clean close of the downstream id", closeRec)
	}
}

// ---------------------------------------------------------------------------
// TestURLModeServedAsInlineBase64 — response_format "url" with a configured
// base-url is still served as inline b64_json, NEVER a synthesized url: the
// pipeline returns only raw bytes and the registry host would pass a non-empty
// url through verbatim (suppressing its own functional data-URL fallback), so a
// synthesized url would hand the client a dead link.
// ---------------------------------------------------------------------------

func TestURLModeServedAsInlineBase64(t *testing.T) {
	cred := execCredential()
	png := solidPNG()
	s := &imgStub{
		tokens:    upstream.SentinelTokens{ChatRequirements: "req-1"},
		genResult: upstream.ImageResult{Images: []upstream.ImageOutput{{Data: png}}, RevisedPrompt: "a lighthouse"},
	}
	fake := newExecFakeCaller()
	stubImageSeams(t, s)
	m := execManager(t, fake, "base-url: https://images.example.org")

	payload := imgJSONPayload(t, map[string]any{
		"model":           "chatgpt-proxy/gpt-image-2-proxy",
		"prompt":          "a lighthouse",
		"response_format": "url",
	})
	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		imgRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", payload, nil), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	ir := decodeImageExecResult(t, resp)
	if len(ir.Data) != 1 {
		t.Fatalf("data = %d, want 1", len(ir.Data))
	}
	if ir.Data[0].URL != "" {
		t.Fatalf("url = %q, want empty (never emit a synthesized url)", ir.Data[0].URL)
	}
	assertPNGInEntry(t, ir.Data[0].B64JSON, png)
	if ir.Data[0].RevisedPrompt != "a lighthouse" {
		t.Fatalf("revised_prompt = %q, want the prompt echoed", ir.Data[0].RevisedPrompt)
	}
}
