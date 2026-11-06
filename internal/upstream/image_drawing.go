package upstream

// image_drawing.go ports the reference's web-drawing image pipeline
// (openai_backend_api.py _stream_picture_conversation /
// _prepare_image_conversation / _start_image_generation /
// _poll_image_results / resolve_conversation_image_urls /
// download_image_bytes, and services/protocol/conversation.py
// _resolve_image_output / _get_detailed_error_from_tasks /
// find_conversation_by_prompt + is_model_text_reply_instead_of_image +
// _find_content_policy_error_in_conversation). One GenerateImage run is one
// attempt against ONE credential: the account-pool retries (token refresh,
// TLS, connection timeout, account swap) are the executor's job (Task 13).
//
// The pipeline never hides conversations: the reference's
// image_remove_conversation_after_result flag defaults OFF, and the plugin
// config carries no such flag — HideConversation (poll.go) stays available
// for a future P2 config flag but is not invoked here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// The drawing route paths (oracle _prepare_image_conversation /
// _start_image_generation).
const (
	drawingPreparePath = "/backend-api/f/conversation/prepare"
	drawingSSEPath     = "/backend-api/f/conversation"
)

// ImageError is the typed failure contract for the drawing pipeline. Code is
// stable (Task 13 maps it to OpenAI images errors and HTTP statuses);
// Message never carries upstream body bytes or token material (secret rule).
type ImageError struct {
	Code       string
	HTTPStatus int
	Retryable  bool
	// RetryAfter carries the Retry-After header in seconds when known
	// (rate_limited only).
	RetryAfter int
	Message    string
	// ConversationID scopes the failure to the drawing turn when one was
	// established.
	ConversationID string
}

func (e *ImageError) Error() string {
	return e.Code + ": " + e.Message
}

// ImageError codes (stable; the executor matches on these).
const (
	ImageCodeInvalidToken       = "invalid_token"
	ImageCodeRateLimited        = "rate_limited"
	ImageCodeUpstreamStatus     = "upstream_status"
	ImageCodeUpstreamMalformed  = "image_response_malformed"
	ImageCodeTextReply          = "text_reply"
	ImageCodeContentPolicy      = "content_policy_violation"
	ImageCodeUpstreamImageError = "upstream_image_error"
	ImageCodeStreamTimeout      = "upstream_stream_timeout"
	ImageCodeStreamError        = "upstream_stream_error"
	ImageCodePollTimeout        = "conversation_poll_timeout"
	ImageCodeNoImage            = "no_image_generated"
)

// Drawing clock seams: production sleeps use the poll package's real clock
// primitives; tests swap them for recorders (see fakeDrawingClock).
var (
	drawingSleep  = ctxSleep
	drawingJitter = randDuration
	drawingNow    = time.Now
)

// ImageOutput is one generated image: the downloaded bytes (already deduped).
type ImageOutput struct {
	Data []byte
}

// ImageResult is the outcome of one drawing run. On success Images holds the
// deduped PNGs; on every typed outcome Err carries the *ImageError and the
// returned error is the same value. RevisedPrompt echoes the caller's prompt
// (oracle format_image_result falls back to request.prompt).
type ImageResult struct {
	Images         []ImageOutput
	ConversationID string
	RevisedPrompt  string
	Err            error
}

// GenerateImage runs the full web-drawing flow for one credential: upload the
// reference images (files trio) → prepare the conduit token → stream the
// picture SSE → settle the conversation document (tasks check, poll, content
// policy scan) → resolve file ids to download URLs → download and dedupe the
// PNGs. Each phase reports through onProgress with the reference step names
// (uploading → preparing → generating → receiving_image; the last only when
// an image is actually received).
//
// Typed outcomes (returned both as result.Err and as the error): the SSE
// ending in assistant prose is detected (referenced_image_ids marker or a
// {"size":"WxH","n":1} fragment), the conversation id is recovered from the
// recent-conversations list when the stream was too short to carry it, and
// the settle uses the extended poll (3 retries × up to 300s) — still prose
// yields typed text_reply; assistant-text content-policy rejections yield
// typed content_policy_violation; a tasks doc with metadata.is_error yields
// typed upstream_image_error; a stalled SSE past the hard cap yields typed
// upstream_stream_timeout with the partial result discarded.
func GenerateImage(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, cfg model.Config, prompt string, references []ReferenceImage, size, quality string, onProgress func(step string)) (ImageResult, error) {
	result := ImageResult{RevisedPrompt: prompt}
	progress := func(step string) {
		if onProgress != nil {
			onProgress(step)
		}
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return imageFailure(result, &ImageError{
			Code:    ImageCodeInvalidToken,
			Message: "the drawing route requires a credential with an access token",
		})
	}
	builtPrompt := buildImagePrompt(prompt, size, quality)
	startedAt := drawingNow()

	progress("uploading")
	var assets []UploadedAsset
	if len(references) > 0 {
		var err error
		assets, err = UploadReferenceImages(ctx, u, cred, references)
		if err != nil {
			return imageFailure(result, err)
		}
	}

	progress("preparing")
	conduitToken, err := prepareImageConversation(ctx, u, cred, tokens, cfg, builtPrompt)
	if err != nil {
		return imageFailure(result, err)
	}

	progress("generating")
	state, err := runImageSSE(ctx, u, cred, tokens, conduitToken, cfg, builtPrompt, assets)
	if err != nil {
		return imageFailure(result, err)
	}

	message := strings.TrimSpace(state.Text)
	conversationID := state.ConversationID
	fileIDs := append([]string(nil), state.FileIDs...)
	sedimentIDs := append([]string(nil), state.SedimentIDs...)
	shouldPoll := len(references) > 0 || state.TurnUseCase == "image gen"
	hasIDs := len(fileIDs)+len(sedimentIDs) > 0
	result.ConversationID = conversationID

	// Moderation-blocked prose: the typed content policy rejection, the
	// detailed tasks error consulted first (oracle blocked branch,
	// tasksDetail default wait_secs=2.0).
	if !hasIDs && message != "" && state.Blocked {
		detail := tasksDetail(ctx, u, cred, conversationID, 2*time.Second)
		return imageFailure(result, newImageError(ImageCodeContentPolicy, policyMessageOr(detail, message), conversationID))
	}

	// Text-reply detection runs BEFORE the plain-prose early exit: the model
	// may have kicked off an asynchronous image generation.
	isTextReply := message != "" && isModelTextReply(message)

	// Plain prose with no reason to poll: tasks detail, then the assistant
	// text keyword scan, then the typed text_reply.
	if !hasIDs && message != "" && !shouldPoll && !isTextReply {
		if detail := tasksDetail(ctx, u, cred, conversationID, time.Second); detail != "" {
			return imageFailure(result, newImageError(ImageCodeContentPolicy, detail, conversationID))
		}
		if isContentPolicyError(message) {
			return imageFailure(result, newImageError(ImageCodeContentPolicy, policyMessageOr("", message), conversationID))
		}
		return imageFailure(result, newImageError(ImageCodeTextReply, message, conversationID))
	}

	// When the SSE was too short to carry the conversation id, recover it
	// from the recent-conversations list (oracle find_conversation_by_prompt).
	if isTextReply && conversationID == "" {
		conversationID = findConversationByPrompt(ctx, u, cred, prompt, startedAt)
		result.ConversationID = conversationID
	}

	if hasIDs || (conversationID != "" && (isTextReply || shouldPoll)) {
		pollTimeout := configPollTimeout(cfg)
		if isTextReply {
			pollTimeout = extendedPollTimeout(cfg)
		}
		urls, settledFiles, settledSediments, err := settleImageTurn(ctx, u, cred, cfg, conversationID, fileIDs, sedimentIDs, pollTimeout)
		if err != nil {
			// On a text reply the upstream may still be generating
			// asynchronously: a content-policy or task-error signal from the
			// first settle is ignored and the retry poll decides.
			if !(isTextReply && (isImageCode(err, ImageCodeContentPolicy) || isImageCode(err, ImageCodeUpstreamImageError))) {
				return imageFailure(result, err)
			}
		} else {
			fileIDs, sedimentIDs = settledFiles, settledSediments
		}
		if len(urls) > 0 {
			return finishImageDownload(ctx, u, cred, result, urls, progress)
		}

		// Extended retry poll (oracle MAX_POLL_RETRIES loop): up to 3
		// attempts × up to 300s, 30s × attempt backoff between transient
		// failures; a poll timeout or policy/task error stops the retries.
		if (message != "" && isTextReply || shouldPoll) && conversationID != "" {
			retryTimeout := extendedPollTimeout(cfg)
			for attempt := 1; attempt <= textReplyPollRetries; attempt++ {
				ids, sedIDs, pollErr := pollForImageIDs(ctx, u, cred, cfg, conversationID, fileIDs, sedimentIDs, retryTimeout)
				if pollErr == nil {
					fileIDs, sedimentIDs = ids, sedIDs
					break
				}
				if isImageCode(pollErr, ImageCodePollTimeout) ||
					isImageCode(pollErr, ImageCodeContentPolicy) ||
					isImageCode(pollErr, ImageCodeUpstreamImageError) {
					break
				}
				if attempt < textReplyPollRetries {
					if sleepErr := drawingSleep(ctx, textReplyRetryBackoff*time.Duration(attempt)); sleepErr != nil {
						return imageFailure(result, sleepErr)
					}
				} else {
					break
				}
			}
			if len(fileIDs)+len(sedimentIDs) > 0 {
				downloadURLs := resolveImageURLs(ctx, u, cred, conversationID, fileIDs, sedimentIDs)
				if len(downloadURLs) > 0 {
					return finishImageDownload(ctx, u, cred, result, downloadURLs, progress)
				}
			}
		}

		if message != "" {
			return imageFailure(result, newImageError(ImageCodeTextReply, message, conversationID))
		}
		return imageFailure(result, &ImageError{
			Code:           ImageCodePollTimeout,
			Retryable:      true,
			Message:        "Image generation completed upstream but the result could not be retrieved. The image may still be processing. Please try again in a moment.",
			ConversationID: conversationID,
		})
	}

	if message != "" {
		return imageFailure(result, newImageError(ImageCodeTextReply, message, conversationID))
	}
	return imageFailure(result, &ImageError{
		Code:           ImageCodeNoImage,
		Message:        "Image generation started upstream but the response was incomplete. Please try again.",
		ConversationID: conversationID,
	})
}

func imageFailure(result ImageResult, err error) (ImageResult, error) {
	if imgErr, ok := err.(*ImageError); ok {
		result.Err = imgErr
	}
	return result, err
}

func newImageError(code, message, conversationID string) *ImageError {
	return &ImageError{Code: code, Message: message, ConversationID: conversationID}
}

func isImageCode(err error, code string) bool {
	var imgErr *ImageError
	return errors.As(err, &imgErr) && imgErr.Code == code
}

// transportError types one upstream transport failure (no response at all):
// retryable upstream_status — except caller cancellations, which pass through
// raw so the executor unwinds them instead of retrying a dead request.
func transportError(message string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ImageError{Code: ImageCodeUpstreamStatus, Retryable: true, Message: message}
}

// toImageError folds the conversation-seam ConvError (and transport failures)
// into the drawing typed error, keeping the codes identical. Caller
// cancellations pass through untouched.
func toImageError(err error, conversationID string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var convErr *ConvError
	if errors.As(err, &convErr) {
		return &ImageError{
			Code:           convErr.Code,
			HTTPStatus:     convErr.HTTPStatus,
			Retryable:      convErr.Retryable,
			RetryAfter:     convErr.RetryAfter,
			Message:        convErr.message,
			ConversationID: conversationID,
		}
	}
	return &ImageError{
		Code:           ImageCodeUpstreamStatus,
		Retryable:      true,
		Message:        "the upstream call failed",
		ConversationID: conversationID,
	}
}

// ---------------------------------------------------------------------------
// Prompt decoration and upstream model resolution.
// ---------------------------------------------------------------------------

// The oracle's build_image_prompt hint fragments. These are WIRE payload
// text — byte-identical to the reference's zh-CN decoration (the request
// persona is OAI-Language zh-CN), not user-facing copy, so they stay Chinese
// like every other pinned wire constant in this package.
const (
	imageSizeHintFormat    = "输出图片尺寸为 %s。"
	imageQualityHintFormat = "输出图片质量为 %s。"
)

// buildImagePrompt ports build_image_prompt: size/quality hints are appended
// to the trimmed prompt; without hints the prompt passes through unchanged.
func buildImagePrompt(prompt, size, quality string) string {
	var hints []string
	if size != "" {
		hints = append(hints, fmt.Sprintf(imageSizeHintFormat, size))
	}
	if quality != "" {
		hints = append(hints, fmt.Sprintf(imageQualityHintFormat, quality))
	}
	if len(hints) == 0 {
		return prompt
	}
	return strings.TrimSpace(prompt) + "\n\n" + strings.Join(hints, "")
}

// imageModelSettings ports _image_model_settings for the web-drawing route:
// the config's drawing upstream model, with a trailing -standard/-extended/
// -max suffix read as the thinking effort; otherwise the config default
// effort, normalized.
func imageModelSettings(cfg model.Config) (string, string) {
	upstream := cfg.DrawingUpstreamModel
	if index := strings.LastIndex(upstream, "-"); index >= 0 {
		suffix := strings.ToLower(upstream[index+1:])
		switch suffix {
		case "standard", "extended", "max":
			return upstream[:index], suffix
		}
	}
	return upstream, NormalizeThinkingEffort(cfg.ThinkingEffort)
}

// ---------------------------------------------------------------------------
// The picture_v2 prepare + SSE request builders (oracle
// _prepare_image_conversation / _start_image_generation), rendered in the
// oracle's dict insertion order with the reference's json.dumps default
// dialect (appendPyJSONDefault).
// ---------------------------------------------------------------------------

func prepareImageConversation(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, cfg model.Config, builtPrompt string) (string, error) {
	upstreamModel, effort := imageModelSettings(cfg)
	payload := pyObj{pairs: []pyPair{
		{key: "action", val: "next"},
		{key: "fork_from_shared_post", val: false},
		{key: "parent_message_id", val: newUUID()},
		{key: "model", val: upstreamModel},
		{key: "client_prepare_state", val: "success"},
		{key: "timezone_offset_min", val: -480},
		{key: "timezone", val: "Asia/Shanghai"},
		{key: "conversation_mode", val: pyObj{pairs: []pyPair{{key: "kind", val: "primary_assistant"}}}},
		{key: "system_hints", val: []any{"picture_v2"}},
		{key: "partial_query", val: pyObj{pairs: []pyPair{
			{key: "id", val: newUUID()},
			{key: "author", val: pyObj{pairs: []pyPair{{key: "role", val: "user"}}}},
			{key: "content", val: pyObj{pairs: []pyPair{
				{key: "content_type", val: "text"},
				{key: "parts", val: []any{builtPrompt}},
			}}},
		}}},
		{key: "supports_buffering", val: true},
		{key: "supported_encodings", val: []any{"v1"}},
		{key: "client_contextual_info", val: pyObj{pairs: []pyPair{{key: "app_name", val: "chatgpt.com"}}}},
	}}
	if effort != "" {
		payload.pairs = append(payload.pairs, pyPair{key: "thinking_effort", val: effort})
	}
	body, err := appendPyJSONDefault(nil, payload)
	if err != nil {
		return "", fmt.Errorf("image prepare body: %w", err)
	}

	// Oracle _image_headers: session fingerprint + bearer, then the
	// requirements + proof tokens ONLY (no turnstile/SO on the image route),
	// no conduit token yet, Accept */* (not SSE).
	headers := SessionHeaders(&cred, drawingPreparePath)
	headers = Authed(&cred, headers)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", tokens.ChatRequirements)
	if tokens.Proof != "" {
		headers.Set("OpenAI-Sentinel-Proof-Token", tokens.Proof)
	}

	resp, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + drawingPreparePath,
		Headers:     headers,
		Body:        body,
		ContentType: "application/json",
	})
	if err != nil {
		return "", transportError("the upstream prepare call failed", err)
	}
	if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
		return "", imgErr
	}
	var prepared struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(resp.Body, &prepared); err != nil {
		return "", &ImageError{Code: ImageCodeUpstreamMalformed, Message: "the upstream prepare response is malformed"}
	}
	// An empty conduit token is tolerated: the oracle simply omits the
	// X-Conduit-Token header for it.
	return prepared.ConduitToken, nil
}

func serializeImagePayload(cfg model.Config, builtPrompt string, assets []UploadedAsset) ([]byte, error) {
	upstreamModel, effort := imageModelSettings(cfg)

	var parts []any
	for _, asset := range assets {
		parts = append(parts, pyObj{pairs: []pyPair{
			{key: "content_type", val: "image_asset_pointer"},
			{key: "asset_pointer", val: "file-service://" + asset.FileID},
			{key: "width", val: asset.Width},
			{key: "height", val: asset.Height},
			{key: "size_bytes", val: asset.Size},
		}})
	}
	parts = append(parts, builtPrompt)
	content := pyObj{pairs: []pyPair{
		{key: "content_type", val: "multimodal_text"},
		{key: "parts", val: parts},
	}}
	if len(assets) == 0 {
		content = pyObj{pairs: []pyPair{
			{key: "content_type", val: "text"},
			{key: "parts", val: []any{builtPrompt}},
		}}
	}

	metadata := pyObj{pairs: []pyPair{
		{key: "developer_mode_connector_ids", val: []any{}},
		{key: "selected_github_repos", val: []any{}},
		{key: "selected_all_github_repos", val: false},
		{key: "system_hints", val: []any{"picture_v2"}},
		{key: "serialization_metadata", val: pyObj{pairs: []pyPair{{key: "custom_symbol_offsets", val: []any{}}}}},
	}}
	if len(assets) > 0 {
		attachments := make([]any, 0, len(assets))
		for _, asset := range assets {
			attachments = append(attachments, pyObj{pairs: []pyPair{
				{key: "id", val: asset.FileID},
				{key: "mimeType", val: asset.MimeType},
				{key: "name", val: asset.FileName},
				{key: "size", val: asset.Size},
				{key: "width", val: asset.Width},
				{key: "height", val: asset.Height},
			}})
		}
		metadata.pairs = append(metadata.pairs, pyPair{key: "attachments", val: attachments})
	}

	// Oracle create_time = time.time() (an epoch-seconds float).
	now := drawingNow()
	createTime := float64(now.Unix()) + float64(now.Nanosecond())/1e9
	message := pyObj{pairs: []pyPair{
		{key: "id", val: newUUID()},
		{key: "author", val: pyObj{pairs: []pyPair{{key: "role", val: "user"}}}},
		{key: "create_time", val: createTime},
		{key: "content", val: content},
		{key: "metadata", val: metadata},
	}}

	payload := pyObj{pairs: []pyPair{
		{key: "action", val: "next"},
		{key: "messages", val: []any{message}},
		{key: "parent_message_id", val: newUUID()},
		{key: "model", val: upstreamModel},
		{key: "client_prepare_state", val: "sent"},
		{key: "timezone_offset_min", val: -480},
		{key: "timezone", val: "Asia/Shanghai"},
		{key: "conversation_mode", val: pyObj{pairs: []pyPair{{key: "kind", val: "primary_assistant"}}}},
		{key: "enable_message_followups", val: true},
		{key: "system_hints", val: []any{"picture_v2"}},
		{key: "supports_buffering", val: true},
		{key: "supported_encodings", val: []any{"v1"}},
		{key: "client_contextual_info", val: pyObj{pairs: []pyPair{
			{key: "is_dark_mode", val: false},
			{key: "time_since_loaded", val: 1200},
			{key: "page_height", val: 1072},
			{key: "page_width", val: 1724},
			{key: "pixel_ratio", val: 1.2},
			{key: "screen_height", val: 1440},
			{key: "screen_width", val: 2560},
			{key: "app_name", val: "chatgpt.com"},
		}}},
		{key: "paragen_cot_summary_display_override", val: "allow"},
		{key: "force_parallel_switch", val: "auto"},
	}}
	if effort != "" {
		payload.pairs = append(payload.pairs, pyPair{key: "thinking_effort", val: effort})
	}

	return appendPyJSONDefault(nil, payload)
}

// runImageSSE starts the picture SSE (oracle _start_image_generation +
// _iter_sse_payloads_capped) and drains it to the final patch-protocol state.
// The hard cap reuses the config's image poll timeout, exactly like the
// oracle; a tripped cap ends the run typed with the partial state discarded.
func runImageSSE(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, conduitToken string, cfg model.Config, builtPrompt string, assets []UploadedAsset) (ConvEvent, error) {
	body, err := serializeImagePayload(cfg, builtPrompt, assets)
	if err != nil {
		return ConvEvent{}, fmt.Errorf("image SSE body: %w", err)
	}

	// Oracle _image_headers for the SSE leg: requirements + proof tokens
	// only, the conduit token when present, and a fresh X-Oai-Turn-Trace-Id
	// per run.
	headers := SessionHeaders(&cred, drawingSSEPath)
	headers = Authed(&cred, headers)
	headers.Set("Accept", "text/event-stream")
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", tokens.ChatRequirements)
	if tokens.Proof != "" {
		headers.Set("OpenAI-Sentinel-Proof-Token", tokens.Proof)
	}
	if conduitToken != "" {
		headers.Set("X-Conduit-Token", conduitToken)
	}
	headers.Set("X-Oai-Turn-Trace-Id", newUUID())

	hardCap := time.Duration(cfg.ImagePollTimeoutSecs) * time.Second
	ch, err := runConversationStream(ctx, u, drawingSSEPath, headers, body, hardCap, "", nil)
	if err != nil {
		return ConvEvent{}, toImageError(err, "")
	}
	var last ConvEvent
	for ev := range ch {
		last = ev
		if ev.Kind == ConvEventError {
			return ConvEvent{}, toImageError(ev.Err, ev.ConversationID)
		}
	}
	return last, nil
}

// ---------------------------------------------------------------------------
// The settle phase: tasks check, conversation document poll, content-policy
// scan (oracle _poll_image_results machinery driven through PollOptions).
// ---------------------------------------------------------------------------

const (
	// Extended poll budget for text-reply / image turns (oracle
	// max(config.image_poll_timeout_secs, 300)).
	extendedPollBudget = 300 * time.Second
	// Oracle MAX_POLL_RETRIES / retry backoff (30s × attempt).
	textReplyPollRetries  = 3
	textReplyRetryBackoff = 30 * time.Second
)

func configPollTimeout(cfg model.Config) time.Duration {
	return time.Duration(cfg.ImagePollTimeoutSecs) * time.Second
}

func extendedPollTimeout(cfg model.Config) time.Duration {
	if base := configPollTimeout(cfg); base >= extendedPollBudget {
		return base
	}
	return extendedPollBudget
}

// settleImageTurn ports resolve_conversation_image_urls(poll=True): poll the
// document until ids appear (or a policy rejection is seen), then resolve
// every id to a download URL. Without a conversation id the poll is skipped
// and ids resolve directly (oracle's empty-conversation branch).
func settleImageTurn(ctx context.Context, u Upstream, cred model.Credential, cfg model.Config, conversationID string, fileIDs, sedimentIDs []string, pollTimeout time.Duration) ([]string, []string, []string, error) {
	if strings.TrimSpace(conversationID) == "" {
		return resolveImageURLs(ctx, u, cred, "", fileIDs, sedimentIDs), fileIDs, sedimentIDs, nil
	}
	mergedFiles, mergedSediments, err := pollForImageIDs(ctx, u, cred, cfg, conversationID, fileIDs, sedimentIDs, pollTimeout)
	if err != nil {
		return nil, mergedFiles, mergedSediments, err
	}
	urls := resolveImageURLs(ctx, u, cred, conversationID, mergedFiles, mergedSediments)
	return urls, mergedFiles, mergedSediments, nil
}

// pollForImageIDs ports _poll_image_results: the /backend-api/tasks check
// first (a structured is_error task ends the turn typed — the reference's
// per-iteration check, applied here at poll start), then the conversation
// document poll whose hit predicate (PollOptions.Done) merges the
// image-tool records or trips on a content-policy text scan. Seeded ids from
// the SSE make the first successful fetch a hit, like the oracle's
// check-before-hit fast return.
func pollForImageIDs(ctx context.Context, u Upstream, cred model.Credential, cfg model.Config, conversationID string, seedFileIDs, seedSedimentIDs []string, pollTimeout time.Duration) ([]string, []string, error) {
	if detail := tasksDetail(ctx, u, cred, conversationID, 0); detail != "" {
		return nil, nil, newImageError(ImageCodeUpstreamImageError, detail, conversationID)
	}
	fileIDs := addUnique(nil, seedFileIDs)
	sedimentIDs := addUnique(nil, seedSedimentIDs)
	var policyMsg string

	opts := NewPollOptions(cfg)
	opts.Timeout = pollTimeout
	opts.sleep = drawingSleep
	opts.jitter = drawingJitter
	opts.Done = func(doc ConversationDoc) bool {
		for _, record := range extractImageToolRecords(doc.Mapping) {
			fileIDs = addUnique(fileIDs, record.FileIDs)
			sedimentIDs = addUnique(sedimentIDs, record.SedimentIDs)
		}
		if len(fileIDs)+len(sedimentIDs) > 0 {
			return true
		}
		if msg := findContentPolicyError(doc.Mapping); msg != "" {
			policyMsg = msg
			return true
		}
		return false
	}

	if _, err := PollConversation(ctx, u, cred, conversationID, opts); err != nil {
		return nil, nil, toImageError(err, conversationID)
	}
	if policyMsg != "" {
		return nil, nil, newImageError(ImageCodeContentPolicy, policyMsg, conversationID)
	}
	return fileIDs, sedimentIDs, nil
}

// tasksDetail ports _get_detailed_error_from_tasks: wait, fetch the tasks
// list filtered by conversation id, return the first structured error text.
// Any failure swallows to "" like the oracle.
func tasksDetail(ctx context.Context, u Upstream, cred model.Credential, conversationID string, wait time.Duration) string {
	if strings.TrimSpace(conversationID) == "" {
		return ""
	}
	if wait > 0 {
		if err := drawingSleep(ctx, wait); err != nil {
			return ""
		}
	}
	tasks, err := FetchTasks(ctx, u, cred, TaskFilter{ConversationID: conversationID})
	if err != nil {
		return ""
	}
	for _, task := range tasks {
		isErr, message := task.Error()
		if isErr && message != "" {
			return message
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Download URL resolution and byte download (oracle _resolve_image_urls /
// _get_file_download_url / _get_attachment_download_url / download_image_bytes).
// ---------------------------------------------------------------------------

// resolveImageURLs resolves each id to a download URL, order-preserving and
// deduped: the "file_upload" pattern is skipped without hitting the wire;
// file ids try the primary lookup then the brief's alternate route
// (/backend-api/files/download/{id}); sediment ids resolve through the
// conversation attachment route (only with a conversation id). Per-id
// failures skip that id, like the oracle's except-and-continue.
func resolveImageURLs(ctx context.Context, u Upstream, cred model.Credential, conversationID string, fileIDs, sedimentIDs []string) []string {
	var urls []string
	for _, fileID := range fileIDs {
		if fileID == "file_upload" {
			continue
		}
		url, err := getFileDownloadURL(ctx, u, cred, fileID)
		if err != nil || url == "" {
			url, err = getFilesDownloadURL(ctx, u, cred, fileID)
		}
		if err != nil || url == "" {
			continue
		}
		urls = addUnique(urls, []string{url})
	}
	if strings.TrimSpace(conversationID) != "" {
		for _, sedimentID := range sedimentIDs {
			url, err := getAttachmentDownloadURL(ctx, u, cred, conversationID, sedimentID)
			if err != nil || url == "" {
				continue
			}
			urls = addUnique(urls, []string{url})
		}
	}
	return urls
}

func getFileDownloadURL(ctx context.Context, u Upstream, cred model.Credential, fileID string) (string, error) {
	return downloadURLFromGET(ctx, u, cred, "/backend-api/files/"+fileID+"/download")
}

func getFilesDownloadURL(ctx context.Context, u Upstream, cred model.Credential, fileID string) (string, error) {
	return downloadURLFromGET(ctx, u, cred, "/backend-api/files/download/"+fileID)
}

func getAttachmentDownloadURL(ctx context.Context, u Upstream, cred model.Credential, conversationID, attachmentID string) (string, error) {
	return downloadURLFromGET(ctx, u, cred,
		"/backend-api/conversation/"+conversationID+"/attachment/"+attachmentID+"/download")
}

// downloadURLFromGET is the oracle's _get_file_download_url: GET with
// session headers + bearer, read download_url or url (Python falsy `or`).
func downloadURLFromGET(ctx context.Context, u Upstream, cred model.Credential, path string) (string, error) {
	headers := SessionHeaders(&cred, path)
	headers = Authed(&cred, headers)
	headers.Set("Accept", "application/json")

	resp, err := u.Do(ctx, Req{
		Method:  http.MethodGet,
		URL:     chatGPTBaseURL + path,
		Headers: headers,
	})
	if err != nil {
		return "", transportError("the download URL lookup failed", err)
	}
	if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
		return "", imgErr
	}
	var doc struct {
		DownloadURL string `json:"download_url"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return "", nil
	}
	if doc.DownloadURL != "" {
		return doc.DownloadURL, nil
	}
	return doc.URL, nil
}

// downloadImageBytes ports download_image_bytes: GET every URL with the
// session's base fingerprint (no X-OpenAI-Target-* pair — those only route
// chatgpt.com backend-api calls), failing typed on any non-2xx (the oracle
// ensure_ok's here, no per-url continue), deduping identical payloads.
func downloadImageBytes(ctx context.Context, u Upstream, cred model.Credential, urls []string) ([]ImageOutput, error) {
	images := []ImageOutput{}
	for _, url := range urls {
		resp, err := u.Do(ctx, Req{
			Method:  http.MethodGet,
			URL:     url,
			Headers: downloadHeaders(&cred),
		})
		if err != nil {
			return nil, transportError("the image download failed", err)
		}
		if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
			return nil, imgErr
		}
		if imageBytesPresent(images, resp.Body) {
			continue
		}
		images = append(images, ImageOutput{Data: append([]byte(nil), resp.Body...)})
	}
	return images, nil
}

func imageBytesPresent(images []ImageOutput, data []byte) bool {
	for _, image := range images {
		if bytes.Equal(image.Data, data) {
			return true
		}
	}
	return false
}

// downloadHeaders is the reference's requests.Session base: every GET
// inherits the session fingerprint block and bearer, but NOT the per-call
// X-OpenAI-Target-Path/Route pair (the oracle's _headers extras).
func downloadHeaders(cred *model.Credential) http.Header {
	headers := SessionHeaders(cred, "")
	headers.Del("X-OpenAI-Target-Path")
	headers.Del("X-OpenAI-Target-Route")
	return Authed(cred, headers)
}

func finishImageDownload(ctx context.Context, u Upstream, cred model.Credential, result ImageResult, urls []string, progress func(string)) (ImageResult, error) {
	progress("receiving_image")
	images, err := downloadImageBytes(ctx, u, cred, urls)
	if err != nil {
		return imageFailure(result, err)
	}
	result.Images = images
	return result, nil
}

// ---------------------------------------------------------------------------
// Text-reply detection, content-policy scan, and conversation recovery.
// ---------------------------------------------------------------------------

// The oracle's text-reply detectors (conversation.py
// REFERENCED_IMAGE_IDS_RE / TOOL_PARAMS_JSON_RE): a full tool-call JSON with
// referenced_image_ids, or a partial tool-params fragment like
// {"size":"1920x1088","n":1}.
var (
	referencedImageIDsRe = regexp.MustCompile(`"referenced_image_ids"\s*:\s*\[([^\]]+)\]`)
	toolParamsJSONRe     = regexp.MustCompile(`\{\s*"size"\s*:\s*"\d+x\d+"\s*,\s*"n"\s*:\s*\d+\s*\}`)
)

// isModelTextReply ports is_model_text_reply_instead_of_image.
func isModelTextReply(message string) bool {
	if message == "" {
		return false
	}
	return referencedImageIDsRe.MatchString(message) || toolParamsJSONRe.MatchString(message)
}

// The oracle's _CONTENT_POLICY_KEYWORDS, byte-identical: the refusal wording
// scans assistant text the reference matches in Chinese and English.
var contentPolicyKeywords = []string{
	"内容政策", "防护限制", "违反", "moderation", "policy", "blocked",
	"不能生成", "无法生成", "不能帮助", "无法帮助",
	"裸体", "裸露", "色情", "性内容", "未成年",
	"抱歉，我不能",
}

// isContentPolicyError ports _is_content_policy_error (lowercased scan).
func isContentPolicyError(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	for _, keyword := range contentPolicyKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// policyMessageOr is the oracle's error-text precedence: the detailed tasks
// error, else the assistant text truncated to 500 chars (runes, like Python
// slicing), else the generic rejection.
func policyMessageOr(detail, message string) string {
	if detail != "" {
		return detail
	}
	if message != "" {
		return truncate500(message)
	}
	return "Image generation was rejected by upstream policy."
}

func truncate500(text string) string {
	runes := []rune(text)
	if len(runes) > 500 {
		runes = runes[:500]
	}
	return string(runes)
}

// imageToolRecord is one image-tool message in a conversation document
// (oracle _extract_image_tool_records entry).
type imageToolRecord struct {
	MessageID   string
	CreateTime  float64
	FileIDs     []string
	SedimentIDs []string
}

// extractImageToolRecords ports _extract_image_tool_records: the message_id
// → node walk over the mapping, role/async_task_type/asset-pointer gates,
// records sorted by create_time.
func extractImageToolRecords(mapping map[string]any) []imageToolRecord {
	var records []imageToolRecord
	for messageID, node := range mapping {
		nodeMap, _ := node.(map[string]any)
		message, _ := nodeMap["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		metadata, _ := message["metadata"].(map[string]any)
		content, _ := message["content"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringOf(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}
		isImageGen := stringOf(metadata["async_task_type"]) == "image_gen"
		hasAssetPointer := hasImageAssetPointer(content) || hasImageAssetPointer(metadata)
		if role == "assistant" && !isImageGen && !hasAssetPointer {
			continue
		}
		fileIDs, sedimentIDs := extractImageReferenceIDs(map[string]any{
			"content":  content,
			"metadata": metadata,
		})
		if !isImageGen && !hasAssetPointer && len(fileIDs)+len(sedimentIDs) == 0 {
			continue
		}
		records = append(records, imageToolRecord{
			MessageID:   messageID,
			CreateTime:  jsonFloatValue(message["create_time"]),
			FileIDs:     fileIDs,
			SedimentIDs: sedimentIDs,
		})
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].CreateTime < records[j].CreateTime
	})
	return records
}

// extractImageReferenceIDs ports _extract_image_reference_ids: a recursive
// string walk collecting file-service URIs, real image file ids and sediment
// URIs (the reference's FILE_SERVICE_ID_RE / REAL_IMAGE_FILE_ID_RE /
// SEDIMENT_ID_RE — shared with the SSE state scanner).
func extractImageReferenceIDs(value any) ([]string, []string) {
	var fileIDs, sedimentIDs []string
	switch v := value.(type) {
	case string:
		for _, match := range fileServiceIDRe.FindAllStringSubmatch(v, -1) {
			fileIDs = addUnique(fileIDs, []string{match[1]})
		}
		for _, match := range realImageFileIDRe.FindAllString(v, -1) {
			fileIDs = addUnique(fileIDs, []string{match})
		}
		for _, match := range sedimentIDRe.FindAllStringSubmatch(v, -1) {
			sedimentIDs = addUnique(sedimentIDs, []string{match[1]})
		}
	case map[string]any:
		for _, item := range v {
			f, s := extractImageReferenceIDs(item)
			fileIDs = addUnique(fileIDs, f)
			sedimentIDs = addUnique(sedimentIDs, s)
		}
	case []any:
		for _, item := range v {
			f, s := extractImageReferenceIDs(item)
			fileIDs = addUnique(fileIDs, f)
			sedimentIDs = addUnique(sedimentIDs, s)
		}
	}
	return fileIDs, sedimentIDs
}

// hasImageAssetPointer ports _has_image_asset_pointer (recursive dict/list
// walk for the image_asset_pointer content type or a file-service/sediment
// asset_pointer).
func hasImageAssetPointer(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if stringOf(v["content_type"]) == "image_asset_pointer" {
			return true
		}
		if pointer := stringOf(v["asset_pointer"]); strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// findContentPolicyError ports _find_content_policy_error_in_conversation:
// the assistant/tool message text scan returning the matching text truncated
// to 500 chars, or "" when nothing matches.
func findContentPolicyError(mapping map[string]any) string {
	for _, node := range mapping {
		nodeMap, _ := node.(map[string]any)
		message, _ := nodeMap["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringOf(author["role"])))
		if role != "assistant" && role != "tool" {
			continue
		}
		text := messageDocumentText(message["content"])
		if text != "" && isContentPolicyError(text) {
			return truncate500(text)
		}
	}
	return ""
}

// messageDocumentText is the oracle's text extraction for one document
// message: stripped string parts plus the content.text field, joined by
// newlines (or the bare string content).
func messageDocumentText(content any) string {
	var parts []string
	switch c := content.(type) {
	case map[string]any:
		if rawParts, ok := c["parts"].([]any); ok {
			for _, part := range rawParts {
				if s, ok := part.(string); ok && strings.TrimSpace(s) != "" {
					parts = append(parts, strings.TrimSpace(s))
				}
			}
		}
		if text := strings.TrimSpace(stringOf(c["text"])); text != "" {
			parts = append(parts, text)
		}
	case string:
		if strings.TrimSpace(c) != "" {
			parts = append(parts, strings.TrimSpace(c))
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// Conversation recovery (oracle _list_recent_conversations +
// find_conversation_by_prompt).
// ---------------------------------------------------------------------------

// listRecentConversations GETs the recent-conversations list; items come
// back as {"items": ...} or {"conversations": ...} (Python falsy `or`).
func listRecentConversations(ctx context.Context, u Upstream, cred model.Credential, limit int) ([]map[string]any, error) {
	path := fmt.Sprintf("/backend-api/conversations?offset=0&limit=%d&order=updated&conversation_filter=all", limit)
	headers := SessionHeaders(&cred, path)
	headers = Authed(&cred, headers)
	headers.Set("Accept", "application/json")

	resp, err := u.Do(ctx, Req{
		Method:  http.MethodGet,
		URL:     chatGPTBaseURL + path,
		Headers: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("list recent conversations: %w", err)
	}
	if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
		return nil, imgErr
	}
	var doc struct {
		Items         []any `json:"items"`
		Conversations []any `json:"conversations"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("list recent conversations: %w", err)
	}
	list := doc.Items
	if len(list) == 0 {
		list = doc.Conversations
	}
	items := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if itemMap, ok := item.(map[string]any); ok {
			items = append(items, itemMap)
		}
	}
	return items, nil
}

// findConversationByPrompt ports find_conversation_by_prompt: the
// started_at ± window filter, the prompt-title keyword score (+0.3 for
// "image" titles, best > 0.1 wins), then the latest in-window conversation
// fallback. Any list failure swallows to "" (the oracle logs and returns
// empty).
func findConversationByPrompt(ctx context.Context, u Upstream, cred model.Credential, prompt string, startedAt time.Time) string {
	items, err := listRecentConversations(ctx, u, cred, 10)
	if err != nil {
		return ""
	}
	started := float64(startedAt.Unix())
	promptWords := wordSet(strings.Fields(strings.ToLower(strings.TrimSpace(prompt))))

	bestMatch, bestScore := "", 0.0
	for _, item := range items {
		convID := firstString(item, "id", "conversation_id")
		if convID == "" {
			continue
		}
		updatedAt := jsonFloatAny(item, "update_time", "updated_at")
		if updatedAt != 0 && started != 0 && (updatedAt < started-30 || updatedAt > started+600) {
			continue
		}
		title := strings.ToLower(firstString(item, "title"))
		score := 0.0
		if len(promptWords) > 0 && title != "" {
			titleWords := wordSet(strings.Fields(title))
			common := 0
			for word := range promptWords {
				if titleWords[word] {
					common++
				}
			}
			if common > 0 {
				score = float64(common) / float64(len(promptWords))
			}
		}
		if strings.HasPrefix(title, "image") {
			score += 0.3
		}
		if score > bestScore {
			bestScore, bestMatch = score, convID
		}
	}
	if bestMatch != "" && bestScore > 0.1 {
		return bestMatch
	}
	for _, item := range items {
		convID := firstString(item, "id", "conversation_id")
		updatedAt := jsonFloatAny(item, "update_time", "updated_at")
		if convID != "" && updatedAt != 0 && started != 0 && updatedAt >= started-30 {
			return convID
		}
	}
	return ""
}

func wordSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := item[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func stringOf(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func jsonFloatAny(item map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value, ok := item[key]; ok && value != nil {
			return jsonFloatValue(value)
		}
	}
	return 0
}

func jsonFloatValue(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
