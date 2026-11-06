package upstream

// image_codex.go ports the reference's codex image route (image route B):
// openai_backend_api.py iter_codex_image_response_events +
// _iter_codex_response_events + _codex_responses_headers +
// _codex_image_input, and services/protocol/conversation.py
// _codex_response_images + stream_codex_image_outputs.
//
// One POST /backend-api/codex/responses call (plain bearer, NO sentinel
// handshake, NO session fingerprint headers — the oracle's urllib transport)
// returns the image as base64 data URLs inside the response.output[] items,
// streamed as SSE data frames or delivered as a single JSON body depending
// on the content-type. The route is plan-gated: the reference's account
// lookup filters source_type="codex" accounts to plan_types
// ("plus","team","pro") before it may use this endpoint; free accounts are
// refused here BEFORE the call, and unknown plans attempt once, mapping
// 403/404 to the typed codex_image_unavailable (never a silent success).
//
// GenerateCodexImage is one attempt against ONE credential — account-pool
// retries (refresh, account swap) are the executor's job (Task 13), like on
// the drawing route.

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// The codex route pins (oracle lines 72, 108-111, 785-801 and the urlopen
// timeout on line 850).
const (
	codexImagePath = "/backend-api/codex/responses"

	// codexImageTimeout ports the oracle's urlopen(timeout=1200). The
	// signature carries no config, so the brief's "else a named constant"
	// branch applies: it rides the request context deadline.
	codexImageTimeout = 1200 * time.Second

	// WIRE payload constants — byte-identical to the reference.
	codexResponsesModel        = "gpt-5.5"
	codexResponsesInstructions = "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result."
	codexImageToolModel        = "gpt-image-2"

	// Oracle `str(size or "1024x1024")` / `str(quality or "auto")` fallbacks.
	codexDefaultImageSize    = "1024x1024"
	codexDefaultImageQuality = "auto"
)

// ImageCodeCodexUnavailable marks an account whose plan does not (or cannot
// be verified to) use the codex image route: the free-plan pre-refusal and
// the 403/404 of one unknown-plan attempt. Stable code — Task 13 maps it to
// an OpenAI images error.
const ImageCodeCodexUnavailable = "codex_image_unavailable"

// GenerateCodexImage runs the codex image route for one credential: build
// the byte-pinned body, POST it with the minimal bearer + content-type
// header set, parse the SSE frames or single JSON body, extract every
// image_generation_call result recursively (base64 data URLs, prefix
// stripped), decode them to raw bytes, and return them in the drawing
// route's ImageResult shape so Task 13 treats both routes uniformly.
func GenerateCodexImage(ctx context.Context, u Upstream, cred model.Credential, profile Profile, prompt string, references []ReferenceImage, size, quality string) (ImageResult, error) {
	result := ImageResult{RevisedPrompt: prompt}

	// Plan gate (reference _generate_image_account_lookup): plus/pro/team
	// (case-insensitive) are known-good; free refuses BEFORE the call.
	planKnown := false
	switch strings.ToLower(strings.TrimSpace(profile.Plan)) {
	case "plus", "pro", "team":
		planKnown = true
	case "free":
		return imageFailure(result, &ImageError{
			Code:    ImageCodeCodexUnavailable,
			Message: "the codex image route requires a Plus, Pro or Team plan",
		})
	}
	// ""/unknown: attempt once; a 403/404 maps to codex_image_unavailable
	// below — never a silent success.

	if strings.TrimSpace(cred.AccessToken) == "" {
		return imageFailure(result, &ImageError{
			Code:    ImageCodeInvalidToken,
			Message: "the codex route requires a credential with an access token",
		})
	}

	body, err := codexImageBody(prompt, references, size, quality)
	if err != nil {
		return imageFailure(result, &ImageError{
			Code:    ImageCodeUpstreamMalformed,
			Message: "the codex request body could not be built",
		})
	}

	callCtx, cancel := context.WithTimeout(ctx, codexImageTimeout)
	defer cancel()

	// Oracle _codex_responses_headers: bearer + content-type ONLY — the
	// codex route deliberately skips the sentinel handshake and the whole
	// session fingerprint block (no SessionHeaders call here).
	headers := Authed(&cred, make(http.Header))
	headers.Set("Content-Type", "application/json")

	resp, err := u.Do(callCtx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + codexImagePath,
		Headers:     headers,
		Body:        body,
		ContentType: "application/json",
	})
	if err != nil {
		return imageFailure(result, transportError("the codex image call failed", err))
	}

	if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
		if !planKnown && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			return imageFailure(result, &ImageError{
				Code:       ImageCodeCodexUnavailable,
				HTTPStatus: resp.StatusCode,
				Message:    "the codex image route is not available for this account",
			})
		}
		return imageFailure(result, imgErr)
	}

	payloads := []string{}
	for _, event := range codexEvents(resp) {
		payloads = append(payloads, codexResponseImages(event)...)
	}
	if len(payloads) == 0 {
		return imageFailure(result, &ImageError{
			Code:    ImageCodeNoImage,
			Message: "no image result was found in the codex response",
		})
	}

	images := make([]ImageOutput, 0, len(payloads))
	for _, payload := range payloads {
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return imageFailure(result, &ImageError{
				Code:    ImageCodeUpstreamMalformed,
				Message: "the codex image result is not valid base64",
			})
		}
		images = append(images, ImageOutput{Data: data})
	}
	result.Images = images
	return result, nil
}

// codexImageBody renders the oracle's payload dict in its insertion order
// with json.dumps default-dialect encoding (", " / ": " separators,
// ensure_ascii) — pinned byte-for-byte by TestCodexBodyGolden.
func codexImageBody(prompt string, references []ReferenceImage, size, quality string) ([]byte, error) {
	// Oracle _codex_image_input: the input_text part first, then one
	// input_image part per reference. Our references carry raw bytes; the
	// data URL they become always matches the oracle's `data:image/...`
	// prefix check, so no extra wrapping is needed.
	content := []any{pyObj{pairs: []pyPair{
		{key: "type", val: "input_text"},
		{key: "text", val: prompt},
	}}}
	action := "generate"
	if len(references) > 0 {
		action = "edit"
		for _, ref := range references {
			content = append(content, pyObj{pairs: []pyPair{
				{key: "type", val: "input_image"},
				{key: "image_url", val: codexReferenceDataURL(ref)},
			}})
		}
	}
	if size == "" {
		size = codexDefaultImageSize
	}
	if quality == "" {
		quality = codexDefaultImageQuality
	}

	payload := pyObj{pairs: []pyPair{
		{key: "model", val: codexResponsesModel},
		{key: "instructions", val: codexResponsesInstructions},
		{key: "store", val: false},
		{key: "input", val: []any{pyObj{pairs: []pyPair{
			{key: "role", val: "user"},
			{key: "content", val: content},
		}}}},
		{key: "tools", val: []any{pyObj{pairs: []pyPair{
			{key: "type", val: "image_generation"},
			{key: "model", val: codexImageToolModel},
			{key: "action", val: action},
			{key: "size", val: size},
			{key: "quality", val: quality},
			{key: "output_format", val: "png"},
		}}}},
		{key: "tool_choice", val: pyObj{pairs: []pyPair{{key: "type", val: "image_generation"}}}},
		{key: "stream", val: true},
	}}
	return appendPyJSONDefault(nil, payload)
}

// codexReferenceDataURL renders one reference as the data URL the oracle's
// _codex_image_input puts into input_image.image_url (the mime defaults to
// image/png, like the upload trio).
func codexReferenceDataURL(ref ReferenceImage) string {
	mime := strings.TrimSpace(ref.MimeType)
	if mime == "" {
		mime = defaultReferenceMimeType
	}
	// Intentional divergence: the oracle hard-codes a data:image/png label
	// for non-data-URL inputs; honoring the reference's real mime (falling
	// back to image/png) is strictly-more-correct and fixture-invisible.
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(ref.Data)
}

// codexEvents ports _iter_codex_response_events: the content-type decides —
// application/json is one JSON document, everything else is SSE "data:"
// frames. Only object events are kept, in document order.
func codexEvents(resp Resp) []any {
	contentType := strings.ToLower(strings.TrimSpace(resp.Headers.Get("Content-Type")))
	if strings.Contains(contentType, "application/json") {
		event, err := decodePyJSON(resp.Body)
		if err != nil {
			return nil
		}
		if _, isObj := event.(pyObj); isObj {
			return []any{event}
		}
		return nil
	}
	var events []any
	var lines []string
	flush := func() {
		if len(lines) == 0 {
			return
		}
		payloadText := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = nil
		// Oracle: skip blank payloads and the [DONE] sentinel; json.loads
		// failures are logged upstream and skipped here.
		if payloadText == "" || payloadText == "[DONE]" {
			return
		}
		event, err := decodePyJSON([]byte(payloadText))
		if err != nil {
			return
		}
		if _, isObj := event.(pyObj); isObj {
			events = append(events, event)
		}
	}
	for _, line := range strings.Split(string(resp.Body), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// Oracle line[5:].lstrip(): strip ALL leading whitespace.
			lines = append(lines, strings.TrimLeft(line[len("data:"):], " \t\r\v\f"))
		}
	}
	flush() // the oracle appends one trailing "" to flush the last frame
	return events
}

// codexResponseImages ports _codex_response_images: a recursive walk that
// returns every image_generation_call result as a base64 payload — the
// data:image/ prefix (up to the first comma) stripped, like the oracle.
// Objects are traversed in document (insertion) order via pyObj, so the
// image order is deterministic and matches the oracle's dict.values() walk.
func codexResponseImages(value any) []string {
	switch v := value.(type) {
	case pyObj:
		if typ, _ := v.find("type"); typ == "image_generation_call" {
			if result, ok := v.find("result"); ok {
				if s, isStr := result.(string); isStr {
					if payload := strings.TrimSpace(s); payload != "" {
						return []string{codexBase64Payload(payload)}
					}
				}
			}
		}
		var images []string
		for _, pair := range v.pairs {
			images = append(images, codexResponseImages(pair.val)...)
		}
		return images
	case []any:
		var images []string
		for _, item := range v {
			images = append(images, codexResponseImages(item)...)
		}
		return images
	default:
		return nil
	}
}

// codexBase64Payload is the oracle's `result.split(",", 1)[1] if
// result.startswith("data:image/") else result`: the base64 payload behind
// one data URL.
func codexBase64Payload(result string) string {
	if strings.HasPrefix(result, "data:image/") {
		if comma := strings.IndexByte(result, ','); comma >= 0 {
			return result[comma+1:]
		}
	}
	return result
}
