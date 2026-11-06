package plugin

// executor_image.go implements the openai-image executor format: the host's
// /v1/images/generations and /v1/images/edits arrive untranslated as
// SourceFormat "openai-image". The executor routes by model base name to the
// two reviewed image pipelines — web-drawing (upstream.GenerateImage, with a
// sentinel bundle acquired once per request) and codex
// (upstream.GenerateCodexImage, no sentinel, the stored plan) — runs n>1 as a
// sequential loop, parses generations JSON vs edits multipart/JSON into
// references (with mask compositing), maps text_reply via message_as_error
// (Review Focus #5), and answers with the pinned OpenAI images envelope
// (created / data[] / usage). A 401 mid-run forces one refresh-then-retry,
// reusing the Task 12 refresh+persist seams.
//
// Streaming image requests execute non-streaming and emit a SINGLE final
// payload chunk then close once: the executor emits the framed SSE itself
// (`data: <payload>\n\n` followed by `data: [DONE]\n\n`, see
// emitImageSingleChunk), because the registry host's image path has no SSE
// synthesis — so chunk fidelity only needs the final payload — only the last
// envelope is emitted, never per-image deltas.
//
// The pipeline entry points are lifted to package vars (imageAcquireSentinel /
// imageGenerate / imageGenerateCodex) so the offline tests swap in scripted
// fakes: the upstream package's polling clock is unexported, so the executor
// tests drive canned ImageResults rather than a full pipeline run (the
// pipelines themselves are fully covered by the upstream fixture-clock tests).
// Production runs the real upstream implementations.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// The OpenAI/reference limits the edits parse enforces.
const (
	imageMaxReferences = 10
	imageMaxPartBytes  = 50 << 20 // 50MB per image part
)

// imageRoute selects one of the two reviewed pipelines.
type imageRoute int

const (
	imageRouteDrawing imageRoute = iota
	imageRouteCodex
)

// imageAcquireSentinel / imageGenerate / imageGenerateCodex are the three
// pipeline entry points the executor calls; see the file doc for why they are
// package vars (the offline test seam).
var (
	imageAcquireSentinel = upstream.AcquireSentinel
	imageGenerate        = upstream.GenerateImage
	imageGenerateCodex   = upstream.GenerateCodexImage
)

// imageTurn is one prepared openai-image request: everything the run loop, the
// refresh-retry and the response assembly share.
type imageTurn struct {
	cfg          Config
	cred         Credential
	upstream     upstream.Upstream
	route        imageRoute
	prompt       string
	size         string
	quality      string
	n            int
	references   []upstream.ReferenceImage
	authFileName string
}

// errImageRefreshFailed marks a forced image refresh that failed or could not
// be persisted; finalizeImageTurn maps it to typed 401 invalid_token.
var errImageRefreshFailed = errors.New("the forced image credential refresh failed or could not be persisted")

// isImageRequest reports whether the executor request is the openai-image
// format (the host tags both /v1/images/generations and /v1/images/edits).
func isImageRequest(req executorRequest) bool {
	return req.SourceFormat == "openai-image" || req.Format == "openai-image"
}

// ---------------------------------------------------------------------------
// Dispatch entry points (wired from executor.go).
// ---------------------------------------------------------------------------

// executeImage is the non-streaming openai-image turn.
func (m *Manager) executeImage(req executorRequest) ([]byte, error) {
	turn, apiErr := m.prepareImageTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	defer cancel()
	results, runErr := m.runImageTurn(ctx, turn, req.HostCallbackID)
	payload, apiErr := finalizeImageTurn(turn, results, runErr)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}
	return ok(pluginapi.ExecutorResponse{Payload: payload}), nil
}

// executeImageStream is the streaming openai-image turn: it executes the whole
// request non-streaming, then emits the final payload as a single chunk and
// closes the downstream stream exactly once.
func (m *Manager) executeImageStream(req executorRequest) ([]byte, error) {
	turn, apiErr := m.prepareImageTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	results, runErr := m.runImageTurn(ctx, turn, req.HostCallbackID)
	payload, apiErr := finalizeImageTurn(turn, results, runErr)
	if apiErr != nil {
		m.closeImageStreamOnce(req.StreamID, envelopeText(apiErr), cancel)
		return ok(map[string]any{}), nil
	}
	m.emitImageSingleChunk(req.StreamID, payload, cancel)
	return ok(map[string]any{}), nil
}

// ---------------------------------------------------------------------------
// Request preparation.
// ---------------------------------------------------------------------------

// prepareImageTurn decodes the credential, resolves the route by model base
// name, parses the generations/edits payload and builds the upstream transport.
// Every failure is a typed request-level error BEFORE any upstream call (route
// refusal and payload errors cost zero network).
func (m *Manager) prepareImageTurn(req executorRequest) (*imageTurn, *pluginabi.Error) {
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	// Route by model base name (controller ruling): the published image ids
	// are <prefix>/<ImageModelName> (drawing) and <prefix>/<CodexImageModelName>
	// (codex); anything else is a typed model_unknown.
	base, found := strings.CutPrefix(req.Model, cfg.ModelPrefix+"/")
	route, ok := imageRouteFor(base, cfg)
	if !found || !ok {
		return nil, typedErr("model_unknown", fmt.Sprintf(
			"model %q is not a published image model (%s/%s or %s/%s)",
			req.Model, cfg.ModelPrefix, cfg.ImageModelName, cfg.ModelPrefix, cfg.CodexImageModelName), 400)
	}

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return nil, typedErr("upstream_error", "upstream transport unavailable: "+err.Error(), 500)
	}

	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	// The payload parse may fetch http(s) image sources through the upstream
	// transport; it is bounded separately from the turn budget.
	parseCtx, parseCancel := context.WithTimeout(context.Background(), 60*time.Second)
	parsed, shapeErr := parseImagePayload(body, req.Headers.Get("Content-Type"), u, parseCtx)
	parseCancel()
	if shapeErr != nil {
		return nil, shapeErr
	}

	return &imageTurn{
		cfg:          cfg,
		cred:         cred,
		upstream:     u,
		route:        route,
		prompt:       parsed.prompt,
		size:         parsed.size,
		quality:      parsed.quality,
		n:            parsed.n,
		references:   parsed.references,
		authFileName: execAuthFileName(req),
	}, nil
}

// imageRouteFor maps one model base name to its pipeline route.
func imageRouteFor(base string, cfg Config) (imageRoute, bool) {
	switch base {
	case cfg.ImageModelName:
		return imageRouteDrawing, true
	case cfg.CodexImageModelName:
		return imageRouteCodex, true
	}
	return imageRouteDrawing, false
}

// ---------------------------------------------------------------------------
// The run loop: n>1 sequential + the 401 refresh-retry (Task 12 seams).
// ---------------------------------------------------------------------------

// runImageTurn runs the request with the refresh-retry loop: at most two
// attempts, the second only after a forced refresh succeeded AND was
// persisted (the 401 contract is identical to the text executor, Review Focus
// #3). It returns the raw pipeline error for finalizeImageTurn to classify, or
// errImageRefreshFailed when the refresh/persist could not complete.
func (m *Manager) runImageTurn(ctx context.Context, turn *imageTurn, callbackID string) ([]upstream.ImageResult, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		results, err := m.attemptImageTurn(ctx, turn)
		if err == nil {
			return results, nil
		}
		lastErr = err
		if attempt == 0 && turn.cfg.Retry401Refresh && isImageInvalidToken(err) {
			refreshed, refreshErr := m.refreshToken(ctx, callbackID, turn.cred)
			if refreshErr != nil {
				return nil, errImageRefreshFailed
			}
			if saveErr := m.saveCredential(turn.authFileName, refreshed); saveErr != nil {
				return nil, errImageRefreshFailed
			}
			turn.cred = refreshed
			continue
		}
		break
	}
	return nil, lastErr
}

// attemptImageTurn runs one full request: for the drawing route it acquires a
// single sentinel bundle for the whole request (then runs the pipeline n
// times — parallel is phase 2); the codex route runs no sentinel handshake and
// passes the credential's STORED plan (persisted at login/refresh by Task 11 —
// never a fresh ProbeProfile here).
func (m *Manager) attemptImageTurn(ctx context.Context, turn *imageTurn) ([]upstream.ImageResult, error) {
	var tokens upstream.SentinelTokens
	if turn.route == imageRouteDrawing {
		acquired, err := imageAcquireSentinel(ctx, turn.upstream, turn.cred, false)
		if err != nil {
			return nil, err
		}
		tokens = acquired
	}
	profile := upstream.Profile{Plan: turn.cred.Plan}

	results := make([]upstream.ImageResult, 0, turn.n)
	for i := 0; i < turn.n; i++ {
		var (
			result upstream.ImageResult
			err    error
		)
		switch turn.route {
		case imageRouteDrawing:
			result, err = imageGenerate(ctx, turn.upstream, turn.cred, tokens, turn.cfg,
				turn.prompt, turn.references, turn.size, turn.quality, nil)
		case imageRouteCodex:
			result, err = imageGenerateCodex(ctx, turn.upstream, turn.cred, profile,
				turn.prompt, turn.references, turn.size, turn.quality)
		}
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// isImageInvalidToken classifies a typed 401 the image run can surface: the
// pipeline's *ImageError invalid_token, or the shared sentinel/missing-token
// detection (the seam error string is never parsed — controller ruling).
func isImageInvalidToken(err error) bool {
	var imgErr *upstream.ImageError
	if errors.As(err, &imgErr) {
		return imgErr.Code == upstream.ImageCodeInvalidToken
	}
	return isInvalidTokenError(err)
}

// ---------------------------------------------------------------------------
// Response finalization.
// ---------------------------------------------------------------------------

// finalizeImageTurn turns one run outcome into either the images payload or a
// typed envelope. The text_reply/content_policy outcomes honor message_as_error
// (Review Focus #5): on, the prose is a typed 400 content_policy_violation;
// off, the prose is returned as a revised_prompt-only data entry with a warning
// message (mirrors the reference `message` key).
func finalizeImageTurn(turn *imageTurn, results []upstream.ImageResult, runErr error) ([]byte, *pluginabi.Error) {
	if runErr != nil {
		if errors.Is(runErr, errImageRefreshFailed) {
			return nil, typedErr("invalid_token",
				"the upstream rejected the bearer token and the forced refresh failed", http.StatusUnauthorized)
		}
		var imgErr *upstream.ImageError
		if errors.As(runErr, &imgErr) {
			switch imgErr.Code {
			case upstream.ImageCodeContentPolicy:
				return nil, typedErr("content_policy_violation", imgErr.Message, http.StatusBadRequest)
			case upstream.ImageCodeTextReply:
				if turn.cfg.MessageAsError {
					return nil, typedErr("content_policy_violation", imgErr.Message, http.StatusBadRequest)
				}
				// message-as-error off: emit the prose as a byte-less data entry
				// (revised_prompt + warning message). This is the executor's
				// CONTRACT (mirrors the reference `message` key) and is pinned by
				// the offline test. KNOWN HOST-RENDER DEGRADATION (documented, not
				// hidden): behind the registry host, extractXAIImagesResponse skips
				// a data entry carrying neither b64_json nor url and then fails
				// with "upstream did not return image output", so the client sees a
				// 502 rather than the prose. A byte-less entry cannot survive the
				// host re-render; surfacing the prose to the client would require a
				// host-side change. See the README message-as-error contract.
				envelope := map[string]any{
					"created": time.Now().Unix(),
					"data": []map[string]any{{
						"revised_prompt": imgErr.Message,
						"message":        "the model answered with text instead of an image; no image bytes were returned",
					}},
					"usage": imageUsage(turn, 0),
				}
				raw, _ := json.Marshal(envelope)
				return raw, nil
			}
		}
		return nil, mapImageError(runErr)
	}
	return assembleImageResponse(turn, results), nil
}

// assembleImageResponse renders the pinned OpenAI images envelope: created, the
// data[] entries and the DOCUMENTED usage estimate (never exact).
//
// Every entry is served as inline b64_json — NEVER a synthesized url. The image
// pipelines (Task 9/10) return only raw bytes and discard the upstream
// download URLs (bearer-gated chatgpt.com backend-api paths, not public), and a
// c-shared library has no HTTP surface to serve bytes from. The registry host
// re-renders image payloads via buildImagesAPIResponseFromXAI, which passes any
// non-empty url VERBATIM (suppressing the host's own functional fallback), so a
// synthesized url would hand the client a dead link. A b64_json-only entry is
// instead rendered by the host as a functional url:"data:image/png;base64,..."
// for the client — so url-mode requests are served as inline base64 (a real
// resolvable url would require a future pipeline that surfaces one).
func assembleImageResponse(turn *imageTurn, results []upstream.ImageResult) []byte {
	data := make([]map[string]any, 0)
	completionTokens := 0
	for _, result := range results {
		revised := result.RevisedPrompt
		for _, img := range result.Images {
			completionTokens += len(img.Data) / 4
			data = append(data, map[string]any{
				"revised_prompt": revised,
				"b64_json":       base64.StdEncoding.EncodeToString(img.Data),
			})
		}
	}
	envelope := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   imageUsage(turn, completionTokens),
	}
	raw, _ := json.Marshal(envelope)
	return raw
}

// imageUsage is the documented estimate over the request and output bytes
// (bytes/4, the text executor's documented convention) — never an exact count.
func imageUsage(turn *imageTurn, completionTokens int) map[string]any {
	promptTokens := len(turn.prompt) / 4
	for _, ref := range turn.references {
		promptTokens += len(ref.Data) / 4
	}
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
}

// mapImageError maps a pipeline failure onto the typed envelope table. The
// text_reply / content_policy outcomes are decided by finalizeImageTurn first;
// this is the fallback classifier (and the refresh-loop's source of truth).
func mapImageError(err error) *pluginabi.Error {
	var imgErr *upstream.ImageError
	if errors.As(err, &imgErr) {
		switch imgErr.Code {
		case upstream.ImageCodeInvalidToken:
			return typedErr("invalid_token", imgErr.Message, http.StatusUnauthorized)
		case upstream.ImageCodeRateLimited:
			e := typedErr("rate_limited", imgErr.Message, http.StatusTooManyRequests)
			e.Retryable = true
			if imgErr.RetryAfter > 0 {
				e.Message += fmt.Sprintf("; retry after %ds", imgErr.RetryAfter)
			}
			return e
		case upstream.ImageCodeStreamTimeout:
			e := typedErr("upstream_stream_timeout", imgErr.Message, http.StatusGatewayTimeout)
			e.Retryable = true
			return e
		case upstream.ImageCodePollTimeout:
			e := typedErr(imgErr.Code, imgErr.Message, http.StatusGatewayTimeout)
			e.Retryable = true
			return e
		case upstream.ImageCodeContentPolicy, upstream.ImageCodeTextReply:
			return typedErr("content_policy_violation", imgErr.Message, http.StatusBadRequest)
		default:
			status := imgErr.HTTPStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			e := typedErr("upstream_error", imgErr.Message, status)
			e.Retryable = imgErr.Retryable
			return e
		}
	}
	// Sentinel-handshake and generic transport failures reuse the shared
	// classifier (Review Focus #3 table).
	return mapUpstreamError(err)
}

// ---------------------------------------------------------------------------
// The single-chunk stream.
// ---------------------------------------------------------------------------

// sseFrame wraps one payload as a data: SSE event ("data: <payload>\n\n"). The
// host writes every emitted chunk VERBATIM into the text/event-stream body, so
// image frames are framed exactly like the text executor's chat.completion.chunk
// frames — a bare JSON blob would not parse as SSE.
func sseFrame(payload []byte) []byte {
	out := make([]byte, 0, len("data: ")+len(payload)+2)
	out = append(out, "data: "...)
	out = append(out, payload...)
	return append(out, '\n', '\n')
}

// sseDoneSentinel is the OpenAI stream terminator, emitted as its own frame.
var sseDoneSentinel = []byte("data: [DONE]\n\n")

// emitImageSingleChunk executes the whole request non-streaming (by the caller)
// and emits the final images payload as exactly ONE framed SSE event followed by
// the [DONE] sentinel, closing the downstream stream exactly once on every exit
// path (success or an emit failure). The registry host's image path
// (streamOpenAICompatImages) has no SSE synthesis — the "host synthesizes SSE"
// claim only holds for the built-in xAI route — so the framing is the plugin's
// job, matching the text executor's documented chunk contract.
func (m *Manager) emitImageSingleChunk(downstreamID string, payload []byte, cancelBudget context.CancelFunc) {
	var closeOnce sync.Once
	closeStream := func(msg string) {
		closeOnce.Do(func() {
			cancelBudget()
			_, _ = m.host.Call(pluginabi.MethodHostStreamClose, streamCloseWire(downstreamID, msg))
		})
	}
	defer closeStream("")
	emit := func(chunk []byte) bool {
		if _, err := m.host.Call(pluginabi.MethodHostStreamEmit, streamEmitWire(downstreamID, chunk)); err != nil {
			// The downstream bridge is gone (host aborted); stop emitting and
			// close once with a static label — no payload material.
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}
	if !emit(sseFrame(payload)) {
		return
	}
	emit(sseDoneSentinel)
}

// closeImageStreamOnce closes the downstream stream exactly once carrying the
// typed error text (the stream close is a plain string, so the code rides in
// the text — the text executor's closeStream convention).
func (m *Manager) closeImageStreamOnce(downstreamID, msg string, cancelBudget context.CancelFunc) {
	var closeOnce sync.Once
	closeOnce.Do(func() {
		cancelBudget()
		_, _ = m.host.Call(pluginabi.MethodHostStreamClose, streamCloseWire(downstreamID, msg))
	})
}

// ---------------------------------------------------------------------------
// Payload parsing: generations JSON vs edits multipart / edits JSON fallback.
// ---------------------------------------------------------------------------

// imageParsed is the decoded image request independent of its wire shape.
type imageParsed struct {
	prompt     string
	size       string
	quality    string
	n          int
	references []upstream.ReferenceImage
}

// parseImagePayload dispatches by Content-Type: a multipart body is the host
// re-encoded /v1/images/edits form; everything else is JSON (generations, or
// the edits JSON fallback carrying image_url[]).
func parseImagePayload(body []byte, contentType string, u upstream.Upstream, ctx context.Context) (imageParsed, *pluginabi.Error) {
	if strings.Contains(contentType, "multipart/form-data") {
		return parseMultipartEdits(body, contentType, u, ctx)
	}
	return parseJSONImage(body, u, ctx)
}

// imageJSONPayload mirrors the OpenAI images request fields this executor
// consumes. The image/mask sources are RawMessage so a present-but-null value
// reads as absent and either a string or a string array is accepted.
// response_format is deliberately NOT consumed: url mode is served as inline
// b64_json (see assembleImageResponse), so the field is ignored.
type imageJSONPayload struct {
	Prompt   string          `json:"prompt"`
	Size     string          `json:"size"`
	Quality  string          `json:"quality"`
	N        *int            `json:"n"`
	ImageURL json.RawMessage `json:"image_url"`
	Image    json.RawMessage `json:"image"`
	MaskURL  json.RawMessage `json:"mask_url"`
	Mask     json.RawMessage `json:"mask"`
}

func parseJSONImage(body []byte, u upstream.Upstream, ctx context.Context) (imageParsed, *pluginabi.Error) {
	if len(body) == 0 {
		return imageParsed{}, typedErr("invalid_request", "empty image payload", 400)
	}
	var payload imageJSONPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return imageParsed{}, typedErr("invalid_request", "the image payload is not valid JSON", 400)
	}
	if strings.TrimSpace(payload.Prompt) == "" {
		return imageParsed{}, typedErr("invalid_request", "the image payload needs a prompt", 400)
	}
	n, apiErr := imageCountPtr(payload.N)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}

	refSources, apiErr := collectImageSources(payload.ImageURL, payload.Image)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	maskSources, apiErr := collectImageSources(payload.MaskURL, payload.Mask)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	references, apiErr := decodeReferenceImages(refSources, u, ctx)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	masks, apiErr := decodeMaskImages(maskSources, u, ctx)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	references, apiErr = applyMasks(references, masks)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	for i := range references {
		applyImageDims(&references[i])
	}

	return imageParsed{
		prompt:     payload.Prompt,
		size:       payload.Size,
		quality:    payload.Quality,
		n:          n,
		references: references,
	}, nil
}

// parseMultipartEdits parses one host re-encoded edits multipart body: image[]
// / images[] / image_url[] file parts become references, mask[] / mask[] parts
// become masks (alpha-composited onto the matching reference by index), and
// prompt / size / quality / n are plain form fields. Parts are capped at 50MB
// and at most 10 images are accepted.
func parseMultipartEdits(body []byte, contentType string, u upstream.Upstream, ctx context.Context) (imageParsed, *pluginabi.Error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return imageParsed{}, typedErr("invalid_request", "malformed multipart content-type", 400)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return imageParsed{}, typedErr("invalid_request", "the multipart body has no boundary", 400)
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)

	var (
		references []upstream.ReferenceImage
		masks      [][]byte
		prompt     string
		size       string
		quality    string
		nStr       string
	)

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return imageParsed{}, typedErr("invalid_request", "malformed multipart edits body", 400)
		}
		// Read one byte past the cap so an over-size part is detected, not truncated.
		data, err := io.ReadAll(io.LimitReader(part, imageMaxPartBytes+1))
		if err != nil {
			return imageParsed{}, typedErr("invalid_request", "could not read a multipart part", 400)
		}
		if len(data) > imageMaxPartBytes {
			return imageParsed{}, typedErr("invalid_request", "an image part exceeds the 50MB cap", 400)
		}
		switch part.FormName() {
		case "prompt":
			prompt = string(data)
		case "size":
			size = string(data)
		case "quality":
			quality = string(data)
		case "n":
			nStr = string(data)
		case "image", "image[]", "images", "images[]", "image_url", "image_url[]":
			imgData, mimeHint, derr := decodeImageSource(string(data), u, ctx)
			if derr != nil {
				return imageParsed{}, typedErr("invalid_request", "could not decode an image part", 400)
			}
			references = append(references, upstream.ReferenceImage{Data: imgData, MimeType: mimeHint})
		case "mask", "mask[]":
			maskData, _, derr := decodeImageSource(string(data), u, ctx)
			if derr != nil {
				return imageParsed{}, typedErr("invalid_request", "could not decode a mask part", 400)
			}
			masks = append(masks, maskData)
		}
	}

	if len(references) == 0 {
		return imageParsed{}, typedErr("invalid_request", "the edits body needs at least one image", 400)
	}
	if len(references) > imageMaxReferences {
		return imageParsed{}, typedErr("invalid_request", "at most 10 images are supported", 400)
	}
	if strings.TrimSpace(prompt) == "" {
		return imageParsed{}, typedErr("invalid_request", "the edits body needs a prompt", 400)
	}

	n, apiErr := imageCountFrom(nStr)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	references, apiErr = applyMasks(references, masks)
	if apiErr != nil {
		return imageParsed{}, apiErr
	}
	for i := range references {
		applyImageDims(&references[i])
	}

	return imageParsed{
		prompt:     prompt,
		size:       size,
		quality:    quality,
		n:          n,
		references: references,
	}, nil
}

// ---------------------------------------------------------------------------
// Image source decoding, masks, and dimensions.
// ---------------------------------------------------------------------------

// collectImageSources flattens one or more RawMessage image/mask fields into a
// string list, accepting either a single string or an array of strings.
func collectImageSources(raws ...json.RawMessage) ([]string, *pluginabi.Error) {
	var sources []string
	for _, raw := range raws {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var single string
		if err := json.Unmarshal(raw, &single); err == nil {
			if strings.TrimSpace(single) != "" {
				sources = append(sources, single)
			}
			continue
		}
		var many []string
		if err := json.Unmarshal(raw, &many); err != nil {
			return nil, typedErr("invalid_request", "image sources must be strings or an array of strings", 400)
		}
		sources = append(sources, many...)
	}
	return sources, nil
}

// decodeReferenceImages resolves every reference source to raw bytes with its
// mime hint (dimensions filled later by applyImageDims after mask compositing).
func decodeReferenceImages(sources []string, u upstream.Upstream, ctx context.Context) ([]upstream.ReferenceImage, *pluginabi.Error) {
	if len(sources) > imageMaxReferences {
		return nil, typedErr("invalid_request", "at most 10 images are supported", 400)
	}
	refs := make([]upstream.ReferenceImage, 0, len(sources))
	for _, src := range sources {
		data, mime, err := decodeImageSource(src, u, ctx)
		if err != nil {
			return nil, typedErr("invalid_request", "could not decode an image source", 400)
		}
		refs = append(refs, upstream.ReferenceImage{Data: data, MimeType: mime})
	}
	return refs, nil
}

func decodeMaskImages(sources []string, u upstream.Upstream, ctx context.Context) ([][]byte, *pluginabi.Error) {
	masks := make([][]byte, 0, len(sources))
	for _, src := range sources {
		data, _, err := decodeImageSource(src, u, ctx)
		if err != nil {
			return nil, typedErr("invalid_request", "could not decode a mask source", 400)
		}
		masks = append(masks, data)
	}
	return masks, nil
}

// applyMasks alpha-composites mask[i] over reference[i] (index pairing, like
// the reference). A mask with no matching image is a typed request error; an
// undecodable mask/reference is skipped (the original reference bytes are
// kept) — a documented gap for exotic formats.
func applyMasks(refs []upstream.ReferenceImage, masks [][]byte) ([]upstream.ReferenceImage, *pluginabi.Error) {
	if len(masks) == 0 {
		return refs, nil
	}
	if len(masks) > len(refs) {
		return nil, typedErr("invalid_request", "a mask has no matching image", 400)
	}
	for i, mask := range masks {
		composited, err := compositeMask(refs[i].Data, mask)
		if err != nil {
			continue
		}
		refs[i].Data = composited
	}
	return refs, nil
}

// decodeImageSource resolves one image source to raw bytes plus a mime hint.
// It accepts, in order: a raw binary blob (PNG/JPEG/GIF magic), a data-URL, a
// bare base64 string, and an http(s) URL fetched through the upstream
// transport — the reference accepts all of these for image parts.
func decodeImageSource(src string, u upstream.Upstream, ctx context.Context) ([]byte, string, error) {
	if isImageBinary([]byte(src)) {
		return []byte(src), "", nil
	}
	s := strings.TrimSpace(src)
	if strings.HasPrefix(s, "data:") {
		return decodeDataURL(s)
	}
	if isBase64(s) {
		data, err := base64.StdEncoding.DecodeString(s)
		return data, "", err
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		resp, err := u.Do(fetchCtx, upstream.Req{Method: http.MethodGet, URL: s})
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, "", errors.New("the image url fetch failed")
		}
		return resp.Body, "", nil
	}
	return nil, "", errors.New("unrecognized image source")
}

// decodeDataURL resolves one "data:<mime>;base64,<payload>" URL to its bytes
// and mime. Non-base64 data URLs are refused (rare, and not needed offline).
func decodeDataURL(s string) ([]byte, string, error) {
	header, payload, found := strings.Cut(s, ",")
	if !found {
		return nil, "", errors.New("malformed data url")
	}
	mime := header[len("data:"):]
	if strings.HasSuffix(mime, ";base64") {
		mime = strings.TrimSuffix(mime, ";base64")
		data, err := base64.StdEncoding.DecodeString(payload)
		return data, mime, err
	}
	return nil, "", errors.New("unsupported data url encoding")
}

func isImageBinary(raw []byte) bool {
	switch {
	case len(raw) >= 4 && raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G':
		return true // PNG
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return true // JPEG
	case len(raw) >= 3 && raw[0] == 'G' && raw[1] == 'I' && raw[2] == 'F':
		return true // GIF
	}
	return false
}

var base64Re = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

func isBase64(s string) bool {
	return len(s) > 0 && len(s)%4 == 0 && base64Re.MatchString(s)
}

// compositeMask alpha-composites one mask over its reference using the stdlib
// (image/draw Over) and re-encodes the result as PNG. The stdlib decoders cover
// PNG/JPEG/GIF/BMP; an exotic or undecodable format fails and the caller keeps
// the original reference bytes — a documented gap (the reference uses Pillow).
func compositeMask(reference, mask []byte) ([]byte, error) {
	refImg, _, err := image.Decode(bytes.NewReader(reference))
	if err != nil {
		return nil, err
	}
	maskImg, _, err := image.Decode(bytes.NewReader(mask))
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(refImg.Bounds())
	draw.Draw(dst, dst.Bounds(), refImg, refImg.Bounds().Min, draw.Src)
	draw.Draw(dst, dst.Bounds(), maskImg, maskImg.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// applyImageDims fills the reference's dimensions and mime from a stdlib
// DecodeConfig when the bytes are decodable. Undecodable bytes keep their
// dims/mime at zero — the pipeline defaults the mime to image/png — a
// documented gap for exotic formats.
func applyImageDims(ref *upstream.ReferenceImage) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(ref.Data))
	if err != nil {
		return
	}
	ref.Width = cfg.Width
	ref.Height = cfg.Height
	if strings.TrimSpace(ref.MimeType) == "" {
		ref.MimeType = imageFormatToMime(format)
	}
}

func imageFormatToMime(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	}
	return ""
}

// ---------------------------------------------------------------------------
// n validation.
// ---------------------------------------------------------------------------

func imageCountPtr(n *int) (int, *pluginabi.Error) {
	count := 1
	if n != nil {
		count = *n
	}
	return validateImageCount(count)
}

func imageCountFrom(s string) (int, *pluginabi.Error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, nil
	}
	count, err := strconv.Atoi(s)
	if err != nil {
		return 0, typedErr("invalid_request", "n must be an integer between 1 and 4", 400)
	}
	return validateImageCount(count)
}

func validateImageCount(count int) (int, *pluginabi.Error) {
	if count < 1 || count > 4 {
		return 0, typedErr("invalid_request", "n must be between 1 and 4", 400)
	}
	return count, nil
}
