package upstream

// conversation.go ports the reference's conversation SSE client and patch
// protocol:
//
//   - openai_backend_api.py stream_conversation + _conversation_headers +
//     _chat_target (the POST, the sentinel token headers, authed/anon target)
//   - utils/helper.py iter_sse_payloads (only "data:" lines yield payloads)
//   - openai_backend_api.py _iter_sse_payloads_capped (the hard wall-clock
//     watchdog that closes the stream past the cap and raises
//     ImageStreamHardTimeoutError — here the typed upstream_stream_timeout)
//   - services/protocol/conversation.py iter_conversation_payloads +
//     update_conversation_state + assistantRawText/apply_text_patch/
//     apply_patch_op + is_visible_assistant_message + sanitize_output_text
//     (the {"p","o","v"} patch protocol, assistant-message filtering and the
//     private-use rich-text annotation sanitizer)
//
// The consumer of the event channel is deliberately neutral: events carry
// assembled state plus typed errors, and the executor (Task 12) maps them to
// OpenAI chunks. Review Focus #2 is pinned here: an unknown event type must
// surface as a "raw" event without interrupting the stream, and a stalled
// stream must hit the hard cap and close with a typed timeout, never hang.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// ConvEvent Kind values (neutral wire for the executor).
const (
	// ConvEventMessageID: the conversation_id and/or the current assistant
	// message id changed — the turn's assistant message has (re)started.
	ConvEventMessageID = "message_id"
	// ConvEventAssistantDelta: sanitized assistant text grew; Delta is the
	// new suffix, Text the accumulated sanitized text.
	ConvEventAssistantDelta = "assistant_delta"
	// ConvEventComplete: the stream ended normally ([DONE] or a clean EOF).
	// Text is the final assembled answer — empty when the turn produced no
	// visible assistant message (the caller maps that to its own failure).
	ConvEventComplete = "complete"
	// ConvEventError: Err carries the typed ConvError.
	ConvEventError = "error"
	// ConvEventRaw: a payload the patch protocol did not turn into text or
	// ids (unknown event types, markers, metadata, non-JSON frames). Raw
	// carries the parsed event (or the raw payload string when unparseable).
	ConvEventRaw = "raw"
)

// ConvEvent is one neutral stream event. Every event carries the current
// state snapshot so consumers never miss an id or flag transition.
type ConvEvent struct {
	Kind           string
	Delta          string
	Text           string
	ConversationID string
	MessageID      string
	Blocked        bool
	ToolInvoked    *bool
	TurnUseCase    string
	FileIDs        []string
	SedimentIDs    []string
	Raw            any
	Err            error
}

// ConvError is the typed failure contract for the conversation stream and
// the conversation poll. Code is stable (Task 12 maps it to RPC error codes
// and HTTP statuses); HTTPStatus carries the upstream status when the error
// came from one; Retryable drives host-side retry decisions. Message text
// never carries upstream body bytes (secret rule).
type ConvError struct {
	Code       string
	HTTPStatus int
	Retryable  bool
	// RetryAfter is the Retry-After header value in seconds when known
	// (rate_limited only).
	RetryAfter int
	message    string
}

func (e *ConvError) Error() string {
	return e.Code + ": " + e.message
}

var (
	// ErrInvalidToken is a 401 on any conversation/poll call: the caller
	// forces an auth refresh and exactly one retry (Review Focus #3).
	ErrInvalidToken = &ConvError{
		Code:       "invalid_token",
		HTTPStatus: http.StatusUnauthorized,
		message:    "the upstream rejected the bearer token",
	}
	// ErrStreamTimeout is the hard-cap watchdog firing (the oracle's
	// ImageStreamHardTimeoutError analog). The turn may still be running
	// upstream; the caller surfaces a typed 504, never a hang.
	ErrStreamTimeout = &ConvError{
		Code:       "upstream_stream_timeout",
		HTTPStatus: http.StatusGatewayTimeout,
		Retryable:  true,
		message:    "the conversation stream exceeded its hard cap and was closed",
	}
	// ErrPollTimeout: the poll budget ran out.
	ErrPollTimeout = &ConvError{
		Code:      "conversation_poll_timeout",
		Retryable: true,
		message:   "the conversation poll exhausted its budget",
	}
	// ErrStreamReadFailed: a mid-stream read failure that is not the
	// watchdog and not a caller cancellation.
	ErrStreamReadFailed = &ConvError{
		Code:      "upstream_stream_error",
		Retryable: true,
		message:   "the conversation stream read failed",
	}
	// ErrMalformedConversationDoc: a 200 conversation body that is not a
	// JSON object.
	ErrMalformedConversationDoc = &ConvError{
		Code:      "conversation_doc_malformed",
		Retryable: false,
		message:   "the upstream returned a malformed conversation document",
	}
	// ErrPayloadTooLarge: the upstream edge refused the conversation body
	// outright (HTTP 413, before the model saw it). The whole OpenAI
	// message array rides on one stateless request, so a long conversation
	// can exceed the backend's accepted payload size even well inside the
	// advertised token limit. Never retryable: the same body fails again —
	// the caller must trim or compact the context or use a larger model.
	ErrPayloadTooLarge = &ConvError{
		Code:       "payload_too_large",
		HTTPStatus: http.StatusRequestEntityTooLarge,
		Retryable:  false,
		message: "the ChatGPT backend rejected the request body as too large (HTTP 413) — " +
			"the conversation exceeds this model's accepted payload size; " +
			"trim or compact the context, or use a model with a larger limit",
	}
)

// statusError maps one upstream status the way the oracle's ensure_ok + the
// typed retry rules do: 2xx passes, 401 is the typed invalid_token, 429 is
// retryable rate_limited (carrying Retry-After when parseable), 413 is the
// typed non-retryable payload_too_large (the body exceeded the backend's
// accepted size — trimming, not retrying, is the fix), 5xx is retryable
// upstream_status, everything else non-retryable.
func statusError(status int, retryAfter *int) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusUnauthorized {
		return ErrInvalidToken
	}
	if status == http.StatusTooManyRequests {
		err := &ConvError{
			Code:       "rate_limited",
			HTTPStatus: status,
			Retryable:  true,
			message:    "the upstream rate-limited the request",
		}
		if retryAfter != nil {
			err.RetryAfter = *retryAfter
		}
		return err
	}
	if status == http.StatusRequestEntityTooLarge {
		return ErrPayloadTooLarge
	}
	retryable := status >= http.StatusInternalServerError
	return &ConvError{
		Code:       "upstream_status",
		HTTPStatus: status,
		Retryable:  retryable,
		message:    fmt.Sprintf("the upstream returned HTTP %d", status),
	}
}

// parseRetryAfter honors the oracle's rule: only a pure integer header value
// counts (HTTP-date forms are treated as absent); retry_after=0 still counts
// as "retry immediately".
var retryAfterIntRe = regexp.MustCompile(`^[0-9]+$`)

func parseRetryAfter(h http.Header) *int {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if !retryAfterIntRe.MatchString(value) {
		return nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	return &seconds
}

// ---------------------------------------------------------------------------
// StreamConversation — the SSE client.
// ---------------------------------------------------------------------------

// defaultSSEHardCap mirrors the oracle reusing image_poll_timeout_secs
// (default 120) as the stream hard cap; the executor overrides it with the
// live config value via WithSSEHardCap.
const defaultSSEHardCap = 120 * time.Second

// StreamOption tunes one StreamConversation run.
type StreamOption func(*streamOptions)

type streamOptions struct {
	hardCap         time.Duration
	historyText     string
	historyMessages []string
}

// WithSSEHardCap sets the hard wall-clock cap for the whole stream. The
// watchdog cancels the stream context and closes the upstream stream at the
// cap, and the reader emits the typed upstream_stream_timeout error event.
func WithSSEHardCap(d time.Duration) StreamOption {
	return func(o *streamOptions) { o.hardCap = d }
}

// WithRequestMessages supplies the full request message history so replayed
// assistant messages are matched against it and skipped (the oracle's
// iter_conversation_payloads history_index protocol). The executor passes
// the same messages it used to build the payload.
func WithRequestMessages(messages []ConversationMessage) StreamOption {
	return func(o *streamOptions) {
		for _, m := range messages {
			if m.Role == "assistant" {
				o.historyText += m.Content
				if m.Content != "" {
					o.historyMessages = append(o.historyMessages, m.Content)
				}
			}
		}
	}
}

// StreamConversation POSTs the built payload to the upstream conversation
// route and returns the neutral event channel. It never blocks the caller
// past DoStream; the reader and the hard-cap watchdog run as goroutines, and
// the channel is closed exactly once on every exit path (success, error,
// caller cancellation, watchdog timeout).
//
// cred empty (no access token) switches the path family to /backend-anon/
// and drops Authorization, mirroring the oracle's access-token branch.
func StreamConversation(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, payload map[string]any, opts ...StreamOption) (<-chan ConvEvent, error) {
	if len(payload) == 0 {
		return nil, errors.New("StreamConversation: a conversation payload is required")
	}
	o := streamOptions{hardCap: defaultSSEHardCap}
	for _, opt := range opts {
		opt(&o)
	}

	body, err := serializeConversationPayload(payload)
	if err != nil {
		return nil, err
	}

	authed := strings.TrimSpace(cred.AccessToken) != ""
	path := conversationPath
	if !authed {
		path = AnonPath(path)
	}

	// The oracle's _conversation_headers: the requirements token is always
	// sent; the proof/turnstile/so tokens only when acquired.
	headers := SessionHeaders(&cred, path)
	if authed {
		headers = Authed(&cred, headers)
	}
	headers.Set("Accept", "text/event-stream")
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", tokens.ChatRequirements)
	if tokens.Proof != "" {
		headers.Set("OpenAI-Sentinel-Proof-Token", tokens.Proof)
	}
	if tokens.Turnstile != "" {
		headers.Set("OpenAI-Sentinel-Turnstile-Token", tokens.Turnstile)
	}
	if tokens.SO != "" {
		headers.Set("OpenAI-Sentinel-SO-Token", tokens.SO)
	}

	return runConversationStream(ctx, u, path, headers, body, o.hardCap,
		o.historyText, o.historyMessages)
}

// runConversationStream opens one upstream SSE with an already-built body and
// header set, runs the hard-cap watchdog and the reader goroutine, and
// returns the neutral event channel. StreamConversation builds the text-turn
// request and calls this; the drawing route (image_drawing.go) builds the
// image-turn request (oracle _image_headers: the sentinel set there carries
// only the requirements + proof tokens, plus the conduit token and a fresh
// X-Oai-Turn-Trace-Id) and calls the same seam.
func runConversationStream(ctx context.Context, u Upstream, path string, headers http.Header, body []byte, hardCap time.Duration, historyText string, historyMessages []string) (<-chan ConvEvent, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := u.DoStream(streamCtx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + path,
		Headers:     headers,
		Body:        body,
		ContentType: "application/json",
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("conversation stream start: %w", err)
	}
	if err := statusError(stream.StatusCode, parseRetryAfter(stream.Headers)); err != nil {
		cancel()
		_ = stream.Close()
		return nil, fmt.Errorf("conversation stream start: %w", err)
	}

	ch := make(chan ConvEvent, 16)
	var timedOut atomic.Bool
	timer := time.AfterFunc(hardCap, func() {
		timedOut.Store(true)
		cancel()
		// Closing the stream unblocks a reader stuck in a synchronous
		// read (both transports unblock on Close).
		_ = stream.Close()
	})
	readerOptions := streamOptions{hardCap: hardCap, historyText: historyText, historyMessages: historyMessages}
	go func() {
		defer close(ch)
		defer timer.Stop()
		defer cancel()
		defer func() { _ = stream.Close() }()
		readConversationStream(ctx, ch, stream, readerOptions, timedOut.Load)
	}()
	return ch, nil
}

// readConversationStream is the oracle's iter_sse_payloads +
// iter_conversation_payloads loop over one Stream.
func readConversationStream(ctx context.Context, ch chan<- ConvEvent, stream *Stream, o streamOptions, timedOut func() bool) {
	state := newConvState()
	decoder := &sseFrameDecoder{}
	for {
		chunk, err := stream.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean end without [DONE]: the oracle's generator simply
				// stops; surface the same terminal state explicitly so a
				// consumer never has to infer completion from a closed
				// channel.
				ch <- state.snapshot(ConvEventComplete, nil)
				return
			}
			if timedOut() {
				ch <- state.snapshot(ConvEventError, ErrStreamTimeout)
				return
			}
			// A caller cancellation has already unwound the consumer;
			// closing silently (the deferred close/Cancel) is correct —
			// emitting an error event nobody reads would block the buffer.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return
			}
			ch <- state.snapshot(ConvEventError, fmt.Errorf("%w: %v", ErrStreamReadFailed, err))
			return
		}
		for _, payload := range decoder.push(chunk) {
			finished := emitPayload(ch, state, o, payload)
			if finished {
				return
			}
		}
	}
}

// sseFrameDecoder assembles SSE payloads from raw chunk reads: the oracle's
// iter_lines + iter_sse_payloads — only lines starting with "data:" yield a
// payload (the "data:" prefix stripped and whitespace-trimmed); "event:",
// comment (":"), and blank lines are skipped. Frames are emitted in order.
type sseFrameDecoder struct {
	pending []byte
}

func (d *sseFrameDecoder) push(chunk []byte) []string {
	d.pending = append(d.pending, chunk...)
	var payloads []string
	for {
		idx := bytes.IndexByte(d.pending, '\n')
		if idx < 0 {
			return payloads
		}
		line := bytes.TrimSuffix(d.pending[:idx], []byte{'\r'})
		d.pending = d.pending[idx+1:]
		s := string(line)
		if !strings.HasPrefix(s, "data:") {
			continue
		}
		payload := strings.TrimSpace(s[len("data:"):])
		if payload != "" {
			payloads = append(payloads, payload)
		}
	}
}

// emitPayload processes one SSE payload through the oracle's
// iter_conversation_payloads dispatch. It reports whether the stream is
// finished ([DONE]).
func emitPayload(ch chan<- ConvEvent, st *convState, o streamOptions, payload string) bool {
	if payload == "" {
		return false
	}
	if payload == "[DONE]" {
		ch <- st.snapshot(ConvEventComplete, nil)
		return true
	}

	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		// Non-JSON content: keep it as a raw event without interrupting
		// the stream (oracle conversation.raw).
		st.update(payload, nil)
		ch <- st.snapshot(ConvEventRaw, payload)
		return false
	}
	event, isDict := parsed.(map[string]any)
	if !isDict {
		// A JSON scalar/array: the oracle yields it as an event with the
		// raw value and no state update.
		ch <- st.snapshot(ConvEventRaw, parsed)
		return false
	}

	prevConversationID, prevMessageID := st.conversationID, st.messageID
	st.update(payload, event)

	// History replay skip: a full assistant add matching the expected
	// history text clears the accumulator and produces no event (and its
	// id is not tracked — the replayed message is not part of this turn).
	if st.historyIndex < len(o.historyMessages) &&
		eventAssistantText(event, o.historyText) == o.historyMessages[st.historyIndex] {
		st.historyIndex++
		st.rawText = ""
		st.text = ""
		return false
	}

	// Track before the delta branch so a full message add that also changes
	// the text carries the new message id in its snapshot.
	st.trackMessageID(event)

	nextRaw := assistantRawText(event, st.rawText, o.historyText)
	nextText := sanitizeOutputText(nextRaw)
	st.rawText = nextRaw
	if nextText != st.text {
		delta := nextText
		if strings.HasPrefix(nextText, st.text) {
			delta = nextText[len(st.text):]
		}
		st.text = nextText
		ch <- st.snapshotDelta(delta)
		return false
	}

	if st.conversationID != prevConversationID || st.messageID != prevMessageID {
		ch <- st.snapshot(ConvEventMessageID, redactEvent(event))
		return false
	}
	ch <- st.snapshot(ConvEventRaw, redactEvent(event))
	return false
}

// redactEvent drops the resume token before any event is surfaced: the
// oracle keeps it internal and it must never cross to downstream users.
func redactEvent(event map[string]any) map[string]any {
	if event["type"] != "resume_conversation_token" {
		return event
	}
	sanitized := make(map[string]any, len(event))
	for key, value := range event {
		if key == "token" {
			continue
		}
		sanitized[key] = value
	}
	return sanitized
}

// ---------------------------------------------------------------------------
// Conversation state (the oracle's ConversationState + update_conversation_state).
// ---------------------------------------------------------------------------

type convState struct {
	text           string // sanitized accumulated assistant text
	rawText        string // unsanitized accumulated assistant text
	conversationID string
	messageID      string // current visible assistant message id
	fileIDs        []string
	sedimentIDs    []string
	blocked        bool
	toolInvoked    *bool
	turnUseCase    string
	historyIndex   int
}

func newConvState() *convState {
	return &convState{}
}

// snapshot renders the neutral event for one Kind.
func (st *convState) snapshot(kind string, extra any) ConvEvent {
	ev := ConvEvent{
		Kind:           kind,
		Text:           st.text,
		ConversationID: st.conversationID,
		MessageID:      st.messageID,
		Blocked:        st.blocked,
		ToolInvoked:    st.toolInvoked,
		TurnUseCase:    st.turnUseCase,
		FileIDs:        append([]string(nil), st.fileIDs...),
		SedimentIDs:    append([]string(nil), st.sedimentIDs...),
	}
	switch v := extra.(type) {
	case error:
		ev.Err = v
	default:
		ev.Raw = v
	}
	return ev
}

func (st *convState) snapshotDelta(delta string) ConvEvent {
	ev := st.snapshot(ConvEventAssistantDelta, nil)
	ev.Delta = delta
	return ev
}

// update ports update_conversation_state: id extraction from the raw payload
// (regex, so it works even on malformed frames), the image-context gate for
// file/sediment ids, and the typed state fields.
func (st *convState) update(payload string, event map[string]any) {
	conversationID, fileIDs, sedimentIDs := extractConversationIDs(payload)
	if conversationID != "" && st.conversationID == "" {
		st.conversationID = conversationID
	}

	if event != nil {
		op, _ := event["o"].(string)
		isPatchEvent := op == "patch"
		isUserMessage := isUserMessageEvent(event)
		// Accept file/sediment ids only in an image context: a complete
		// image_gen tool message, a prior server_ste_metadata flip of
		// tool_invoked (never on user messages — their ids are uploads),
		// or a patch payload referencing asset pointers.
		imageContext := isImageToolEvent(event) ||
			(st.toolInvoked != nil && *st.toolInvoked && !isUserMessage) ||
			(isPatchEvent && !isUserMessage &&
				(strings.Contains(payload, "asset_pointer") || strings.Contains(payload, "file-service://")))
		if imageContext {
			st.fileIDs = addUnique(st.fileIDs, fileIDs)
			st.sedimentIDs = addUnique(st.sedimentIDs, sedimentIDs)
		}
	}

	if event == nil {
		return
	}
	if cid, ok := event["conversation_id"].(string); ok && cid != "" {
		st.conversationID = cid
	}
	if v, ok := event["v"].(map[string]any); ok {
		if cid, ok := v["conversation_id"].(string); ok && cid != "" {
			st.conversationID = cid
		}
	}
	if event["type"] == "moderation" {
		if moderation, ok := event["moderation_response"].(map[string]any); ok && moderation["blocked"] == true {
			st.blocked = true
		}
	}
	if event["type"] == "server_ste_metadata" {
		if metadata, ok := event["metadata"].(map[string]any); ok {
			if invoked, ok := metadata["tool_invoked"].(bool); ok {
				st.toolInvoked = &invoked
			}
			if useCase, ok := metadata["turn_use_case"].(string); ok && useCase != "" {
				st.turnUseCase = useCase
			}
		}
	}
}

// trackMessageID records the current visible assistant message id.
func (st *convState) trackMessageID(event map[string]any) {
	message, ok := eventMessage(event)
	if !ok || !isVisibleAssistantMessage(message) {
		return
	}
	if id, ok := message["id"].(string); ok && id != "" {
		st.messageID = id
	}
}

// extractConversationIDs ports the oracle's payload scan: conversation_id
// (regex), file-service ids, real image file ids (file_00000000 + 24 hex),
// the generic file id scan (Python `file[-_](?!service\b)` — RE2 has no
// lookaround, so the "service" exclusion is applied post-match, which is
// exact: both engines take the maximal word run, and the lookahead only
// rejects the bare name "service"), and sediment ids.
var (
	conversationIDRe  = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)
	fileServiceIDRe   = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	realImageFileIDRe = regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`)
	fileIDScanRe      = regexp.MustCompile(`\bfile[-_][A-Za-z0-9_-]+\b`)
	sedimentIDRe      = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
)

func extractConversationIDs(payload string) (string, []string, []string) {
	var conversationID string
	if match := conversationIDRe.FindStringSubmatch(payload); match != nil {
		conversationID = match[1]
	}

	fileIDs := []string{}
	for _, match := range fileServiceIDRe.FindAllStringSubmatch(payload, -1) {
		fileIDs = addUnique(fileIDs, []string{match[1]})
	}
	// No capture group: findall semantics (full match), like the oracle's
	// REAL_IMAGE_FILE_ID_RE.findall — FindAllStringSubmatch would panic on
	// match[1].
	for _, match := range realImageFileIDRe.FindAllString(payload, -1) {
		fileIDs = addUnique(fileIDs, []string{match})
	}
	for _, match := range fileIDScanRe.FindAllString(payload, -1) {
		name := match[len("file")+1:] // strip "file" + the separator
		if name == "service" {
			continue
		}
		fileIDs = addUnique(fileIDs, []string{match})
	}

	sedimentIDs := []string{}
	for _, match := range sedimentIDRe.FindAllStringSubmatch(payload, -1) {
		sedimentIDs = addUnique(sedimentIDs, []string{match[1]})
	}
	return conversationID, fileIDs, sedimentIDs
}

// addUnique is the oracle's order-preserving dedupe (append candidates, drop
// empties, keep first occurrence).
func addUnique(values, candidates []string) []string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		values = append(values, candidate)
	}
	out := values[:0]
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// eventMessage resolves the oracle's `event.get("message") or
// (value.get("message") if isinstance(value, dict))` (Python falsy `or` —
// an empty message object falls through to the v-embedded one).
func eventMessage(event map[string]any) (map[string]any, bool) {
	if message, ok := event["message"].(map[string]any); ok && len(message) > 0 {
		return message, true
	}
	if v, ok := event["v"].(map[string]any); ok {
		if message, ok := v["message"].(map[string]any); ok && len(message) > 0 {
			return message, true
		}
	}
	return nil, false
}

// isImageToolEvent ports the oracle's image-tool detector (exact role and
// task-type comparisons — no case folding, like the reference).
func isImageToolEvent(event map[string]any) bool {
	message, ok := eventMessage(event)
	if !ok {
		return false
	}
	author, _ := message["author"].(map[string]any)
	if role, _ := author["role"].(string); role != "tool" {
		return false
	}
	metadata, _ := message["metadata"].(map[string]any)
	if taskType, _ := metadata["async_task_type"].(string); taskType == "image_gen" {
		return true
	}
	content, _ := message["content"].(map[string]any)
	if contentType, _ := content["content_type"].(string); contentType != "multimodal_text" {
		return false
	}
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		obj, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if contentType, _ := obj["content_type"].(string); contentType == "image_asset_pointer" {
			return true
		}
		if pointer, _ := obj["asset_pointer"].(string); strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
	}
	return false
}

// isUserMessageEvent ports _is_user_message_event (strip+lower, unlike
// isImageToolEvent's exact compare — the reference does both styles).
func isUserMessageEvent(event map[string]any) bool {
	message, ok := eventMessage(event)
	if !ok {
		return false
	}
	author, _ := message["author"].(map[string]any)
	role, _ := author["role"].(string)
	return strings.ToLower(strings.TrimSpace(role)) == "user"
}

// ---------------------------------------------------------------------------
// The patch protocol (the oracle's apply_text_patch / apply_patch_op /
// assistantRawText / assistantMessageText / strip_history).
// ---------------------------------------------------------------------------

// assistantRawText ports assistant_raw_text: a full visible assistant message
// returns its own text (replacing the accumulator); otherwise the {"p","o",
// "v"} patch semantics fold onto the accumulator.
func assistantRawText(event map[string]any, currentText, historyText string) string {
	candidates := []map[string]any{event}
	if v, ok := event["v"].(map[string]any); ok {
		candidates = append(candidates, v)
	}
	for _, candidate := range candidates {
		message, ok := candidate["message"].(map[string]any)
		if !ok || !isVisibleAssistantMessage(message) {
			continue
		}
		if text := assistantMessageText(message); text != "" {
			return stripHistory(text, historyText)
		}
	}
	return applyTextPatch(event, currentText, historyText)
}

// eventAssistantText is the oracle's history-match probe: full visible
// assistant message text only, else "".
func eventAssistantText(event map[string]any, historyText string) string {
	candidates := []map[string]any{event}
	if v, ok := event["v"].(map[string]any); ok {
		candidates = append(candidates, v)
	}
	for _, candidate := range candidates {
		message, ok := candidate["message"].(map[string]any)
		if !ok || !isVisibleAssistantMessage(message) {
			continue
		}
		return stripHistory(assistantMessageText(message), historyText)
	}
	return ""
}

// applyTextPatch ports apply_text_patch, quirks included:
//
//   - p == "/message/content/parts/0" routes straight to the op handler
//     (an "add" there is a text no-op)
//   - a bare string v appends ONLY when the accumulator is non-empty and
//     the frame carries neither p nor o (Python falsy: an empty-string p/o
//     counts as absent)
//   - o:"patch" with a list v folds item by item, in order; a plain v list
//     (no o) folds the same way
func applyTextPatch(event map[string]any, currentText, historyText string) string {
	if p, _ := event["p"].(string); p == "/message/content/parts/0" {
		return applyPatchOp(event, currentText, historyText)
	}

	operations := event["v"]
	if ops, ok := operations.(string); ok {
		if currentText != "" && pyFalsy(event["p"]) && pyFalsy(event["o"]) {
			return currentText + ops
		}
	}

	if o, _ := event["o"].(string); o == "patch" {
		if list, ok := operations.([]any); ok {
			text := currentText
			for _, item := range list {
				if patch, ok := item.(map[string]any); ok {
					text = applyTextPatch(patch, text, historyText)
				}
			}
			return text
		}
	}

	list, ok := operations.([]any)
	if !ok {
		return currentText
	}
	text := currentText
	for _, item := range list {
		if patch, ok := item.(map[string]any); ok {
			text = applyTextPatch(patch, text, historyText)
		}
	}
	return text
}

// applyPatchOp ports apply_patch_op: append/replace on the text accumulator;
// every other op is a no-op.
func applyPatchOp(operation map[string]any, currentText, historyText string) string {
	op, _ := operation["o"].(string)
	value := patchStringValue(operation["v"])
	switch op {
	case "append":
		return currentText + value
	case "replace":
		return stripHistory(value, historyText)
	}
	return currentText
}

// patchStringValue is the oracle's str(operation.get("v") or ""): falsy
// values collapse to "", strings pass through, everything else renders via
// the Python str() emulation (pythonStr).
func patchStringValue(v any) string {
	if pyFalsy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pythonStr(v)
}

// pyFalsy is Python truthiness over the JSON value set (the oracle's `not
// event.get("p")` checks).
func pyFalsy(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		return value == ""
	case int:
		return value == 0
	case int64:
		return value == 0
	case float64:
		return value == 0
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

// assistantMessageText ports assistant_message_text: string parts joined,
// with the content_type "code" text-field fallback.
func assistantMessageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	if parts, ok := content["parts"].([]any); ok {
		var text strings.Builder
		for _, part := range parts {
			if s, ok := part.(string); ok {
				text.WriteString(s)
			}
		}
		if joined := text.String(); joined != "" {
			return joined
		}
	}
	if text, _ := content["text"].(string); text != "" {
		return text
	}
	return ""
}

// isVisibleAssistantMessage ports is_visible_assistant_message: role must be
// assistant (strip+lower), the message must not be visually hidden, a set
// recipient must be "all", and a set channel must be "final". Empty
// recipient/channel pass (the oracle only rejects NON-empty wrong values).
func isVisibleAssistantMessage(message map[string]any) bool {
	author, ok := message["author"].(map[string]any)
	if !ok {
		return false
	}
	role, _ := author["role"].(string)
	if strings.ToLower(strings.TrimSpace(role)) != "assistant" {
		return false
	}
	if metadata, ok := message["metadata"].(map[string]any); ok {
		if metadata["is_visually_hidden_from_conversation"] == true {
			return false
		}
	}
	recipient, _ := message["recipient"].(string)
	recipient = strings.ToLower(strings.TrimSpace(recipient))
	if recipient != "" && recipient != "all" {
		return false
	}
	channel, _ := message["channel"].(string)
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "" && channel != "final" {
		return false
	}
	return true
}

// stripHistory is the oracle's strip_history loop: peel the history prefix
// while it still leads the text.
func stripHistory(text, historyText string) string {
	for historyText != "" && strings.HasPrefix(text, historyText) {
		text = text[len(historyText):]
	}
	return text
}

// ---------------------------------------------------------------------------
// The rich-text annotation sanitizer (the oracle's sanitize_output_text).
// ---------------------------------------------------------------------------

// ChatGPT web sometimes returns rich annotation markers using private-use
// characters (\uE200 opens, \uE201 closes, \uE202 separates fields). API
// clients cannot render those. The sanitizer preserves readable labels from
// entity/link annotations and removes internal citation pointers, ported
// regex-for-regex from the reference (order matters: the before-punctuation
// variant runs first so its leading space survives).
// The private-use marker runes (Go string escapes — RE2 has no pattern-level
// \u escape).
const (
	markerOpen  = "\uE200"
	markerClose = "\uE201"
	markerField = "\uE202"
)

// The oracle's before-punctuation regex needs a lookahead, which RE2 does
// not support; sanitizeOutputText scans left to right and applies the exact
// same three rules: (1) annotation immediately before [.,;:!?] — keep the
// leading whitespace run and the replacement, or swallow both when the
// replacement is empty; (2) any other annotation — replace the marker only,
// whitespace untouched; (3) trailing unclosed annotation — strip to the end.
var (
	internalTurnRe    = regexp.MustCompile(`turn\d+[a-z]*\d*`)
	internalTurnAnyRe = regexp.MustCompile(`turn\d+\w*`)
)

func sanitizeOutputText(text string) string {
	isInternal := func(part string) bool {
		value := strings.TrimSpace(part)
		if value == "" {
			return true
		}
		lower := strings.ToLower(value)
		return internalTurnRe.MatchString(lower) ||
			internalTurnAnyRe.MatchString(lower) ||
			strings.HasPrefix(lower, "turn") ||
			strings.HasPrefix(lower, "source") ||
			strings.HasPrefix(lower, "sources")
	}

	readablePart := func(parts []string) string {
		for _, part := range parts {
			value := strings.TrimSpace(part)
			if value != "" && !isInternal(value) {
				return value
			}
		}
		return ""
	}

	annotationText := func(payload string) string {
		parts := strings.Split(payload, markerField)
		kind := strings.ToLower(parts[0])
		data := parts[1:]
		if kind == "url" {
			label := ""
			if len(data) > 0 {
				label = data[0]
			}
			url := ""
			if len(data) > 1 {
				url = data[1]
			}
			if label != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
				return label + " (" + url + ")"
			}
			if label != "" {
				return label
			}
			return url // the oracle's `label or url` fallback
		}
		if kind == "cite" {
			return readablePart(data)
		}
		return readablePart(data)
	}

	var out strings.Builder
	rest := text
	for {
		openIdx := strings.Index(rest, markerOpen)
		if openIdx < 0 {
			out.WriteString(rest)
			break
		}
		afterOpen := rest[openIdx:]
		closeIdx := strings.Index(afterOpen, markerClose)
		if closeIdx < 0 {
			// Rule 3: a trailing unclosed annotation strips from the
			// marker to the end (the oracle's \uE200[^\uE201]*$); the text
			// before the marker survives.
			out.WriteString(rest[:openIdx])
			break
		}
		payload := afterOpen[len(markerOpen):closeIdx]
		// closeIdx is measured from the start of afterOpen (the open marker
		// is included), so end must not add len(markerOpen) again.
		end := openIdx + closeIdx + len(markerClose)
		replacement := annotationText(payload)

		before := rest[:openIdx]
		if end < len(rest) && strings.ContainsRune(",.;:!", rune(rest[end])) {
			// Rule 1: annotation immediately before punctuation — the
			// replacement keeps its leading whitespace run, or both are
			// swallowed when the replacement is empty.
			if replacement == "" {
				out.WriteString(strings.TrimRight(before, " \t\n\r\f\v"))
			} else {
				out.WriteString(before)
				out.WriteString(replacement)
			}
		} else {
			// Rule 2: the marker itself is replaced, whitespace untouched.
			out.WriteString(before)
			out.WriteString(replacement)
		}
		rest = rest[end:]
	}
	return out.String()
}
