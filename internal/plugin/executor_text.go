package plugin

// executor_text.go implements the OpenAI text conversation format: the
// request shaping (model resolution, OpenAI function-tools emulation —
// see tools.go — with the nameless-tools refusal kept, thinking-effort
// mapping), the non-stream chat.completion assembly (including the
// zero-content → typed 502 contract, Review Focus #2) and the stream pump
// (ConvEvent → chat.completion.chunk frames over host.stream.emit, with
// host.stream.close issued exactly once on every exit path).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// sseHardCapFor derives the conversation stream's hard wall-clock cap from
// the live config: the plan reuses image-poll-timeout-secs as the SSE hard
// cap (upstream defaultSSEHardCap precedent). A var only so tests can
// shrink it to milliseconds — the config expresses seconds.
var sseHardCapFor = func(cfg Config) time.Duration {
	return time.Duration(cfg.ImagePollTimeoutSecs) * time.Second
}

// textTurn is one prepared OpenAI text turn: everything the start path
// (sentinel + conversation SSE), the non-stream assembly and the stream
// pump share. text is assembled incrementally by the pump for the
// terminal usage estimate; it never crosses the ABI.
type textTurn struct {
	cfg      Config
	cred     Credential
	upstream upstream.Upstream
	payload  map[string]any // the built upstream conversation body
	messages []upstream.ConversationMessage
	hardCap  time.Duration
	modelID  string // the requested published id, echoed in every frame
	turnID   string // one turn id per turn, shared by every frame:
	// chatcmpl-<uuid> (openai) or resp_<uuid hex> (openai-response)
	created      int64  // the response envelope's created_at (openai-response only)
	promptTokens int    // documented estimate over the request messages
	text         string // assembled assistant text (pump only)
	// authFileName is the record's auth file name (host-selected AuthID or
	// the source path base) a forced refresh persists under.
	authFileName string
	// toolMode is true when the request carried an emulatable tools array:
	// the reply text is parsed for the reference's tool-call XML and the
	// stream is buffered (tool requests are not token-streamed).
	toolMode bool
	// searchMode is true when the request asked for web search (a
	// web_search tool with no other tools, a web_search tool_choice, a
	// web_search_options block, or a search model prefix): the turn runs
	// upstream.Search instead of the conversation pipeline, and searchQuery
	// is the last user message text it posts.
	searchMode  bool
	searchQuery string
	// slug and effort mirror the values baked into turn.payload so the
	// nudge retry can rebuild it with extra messages.
	slug   string
	effort string
	// nudged marks the empty-args retry as spent; nudgeRetry, when set by
	// the openai handler, re-runs the turn once and returns its text.
	nudged     bool
	nudgeRetry func() (string, bool)
}

// prepareTextTurn decodes the credential, resolves the model, shapes the
// OpenAI payload and builds the upstream conversation body. Every failure
// here is a typed request-level error BEFORE any upstream call (the tools
// refusal and the unknown-model refusal must cost zero network).
func (m *Manager) prepareTextTurn(req executorRequest) (*textTurn, *pluginabi.Error) {
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	// Model resolution (controller ruling): text turns publish as
	// <prefix>/openai/<slug>; anything else — the image models (Task 13)
	// or a foreign id — is refused cleanly with typed model_unknown.
	slug, found := trimTextModelPrefix(req.Model, cfg.ModelPrefix)
	if !found {
		return nil, typedErr("model_unknown",
			fmt.Sprintf("model %q is not a published text model (%s/openai/<slug>)", req.Model, cfg.ModelPrefix), 400)
	}

	turn := &textTurn{
		cfg:          cfg,
		cred:         cred,
		modelID:      req.Model,
		turnID:       turnID(),
		hardCap:      sseHardCapFor(cfg),
		authFileName: execAuthFileName(req),
	}
	turn.slug = slug

	// Request shaping from the host-passed OpenAI chat payload
	// (SourceFormat == output format → the host passes it untranslated).
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}

	// Web-search detection runs BEFORE the tools refusal: a search request
	// carries nameless web_search tools that the ordinary pipeline would
	// refuse typed (oracle is_web_search_chat_request gate).
	searchMode := isWebSearchRequest(body, slug)

	messages, effort, toolMode, shapeErr := shapeChatRequest(body, searchMode)
	if shapeErr != nil {
		return nil, shapeErr
	}
	turn.messages = messages
	turn.toolMode = toolMode

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return nil, typedErr("upstream_error", "upstream transport unavailable: "+err.Error(), 500)
	}
	turn.upstream = u

	if searchMode {
		// The search flow posts its own bodies (oracle search prepare +
		// f/conversation with force_use_search); no conversation payload,
		// no thinking-effort mapping. The query is the last user message.
		if apiErr := prepareSearchTurn(turn, messages); apiErr != nil {
			return nil, apiErr
		}
		return turn, nil
	}

	if effort == "" {
		// The config default applies when the request carries no
		// thinking_effort / reasoning_effort (auto → omitted upstream).
		effort = cfg.ThinkingEffort
	}
	turn.effort = effort
	turn.promptTokens = estimatePromptTokens(messages)

	convReq := upstream.ConversationRequest{
		Messages:       messages,
		Model:          slug,
		ThinkingEffort: effort,
	}
	payload, err := upstream.BuildConversationPayload(convReq)
	if err != nil {
		return nil, typedErr("invalid_request", "malformed chat messages: "+err.Error(), 400)
	}
	turn.payload = payload

	return turn, nil
}

// trimTextModelPrefix resolves the published text model id to its upstream
// slug. Image models and foreign ids do not carry the text prefix.
func trimTextModelPrefix(model, prefix string) (string, bool) {
	slug, found := strings.CutPrefix(model, prefix+"/openai/")
	return slug, found && slug != ""
}

func hasAccessToken(cred Credential) bool {
	return cred.AccessToken != ""
}

// shapeChatPayload mirrors the OpenAI chat request fields this executor
// consumes. tools is a RawMessage so a present-but-null value reads as
// absent; both effort spellings are accepted (controller ruling). A message
// carries the tool surfaces the agent loop replays: a role:"tool" result's
// tool_call_id and an assistant message's tool_calls.
type shapeChatPayload struct {
	Messages []struct {
		Role       string                   `json:"role"`
		Content    any                      `json:"content"`
		ToolCallID string                   `json:"tool_call_id"`
		ToolCalls  []shapeAssistantToolCall `json:"tool_calls"`
	} `json:"messages"`
	Tools           json.RawMessage `json:"tools"`
	ThinkingEffort  string          `json:"thinking_effort"`
	ReasoningEffort string          `json:"reasoning_effort"`
}

// shapeChatRequest turns the host-passed OpenAI chat JSON into the upstream
// conversation messages, the resolved thinking effort and the tool mode.
//
// tools: a non-empty array of OpenAI function tools is emulated (tools.go) —
// the tool prompt is merged into the conversation's system message(s) and
// the reply's tool-call XML is parsed back into OpenAI tool_calls by the
// turn. A tools array where no tool carries a usable function name keeps
// the honest typed 400 tools_unsupported refusal BEFORE any upstream call.
//
// Tool mode additionally appends the compact userBridge to the last user
// message (PROVENANCE.md 2026-11-11 recency evidence).

// Agent-loop round-trip: a role:"tool" result folds in as user text the way
// the reference formats a tool_result block ("Tool result {id}:
// {content}"); a prior assistant tool_calls message folds back into the
// reference's tool_use XML text blocks so the upstream sees the calls it
// (we) already made.
func shapeChatRequest(body []byte, searchMode bool) ([]upstream.ConversationMessage, string, bool, *pluginabi.Error) {
	if len(body) == 0 {
		return nil, "", false, typedErr("invalid_request", "empty chat payload", 400)
	}
	var payload shapeChatPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false, typedErr("invalid_request", "the chat payload is not valid JSON", 400)
	}

	// The tools refusal (no usable name / non-array) precedes every other
	// work and costs zero network. In search mode the tools are the search
	// request itself (the gate already verified no function tool is mixed
	// in), so they are skipped — the search flow posts none upstream.
	var prompt string
	var toolMode bool
	if !searchMode {
		var err *pluginabi.Error
		prompt, toolMode, err = shapeTools(payload.Tools)
		if err != nil {
			return nil, "", false, err
		}
	}

	if len(payload.Messages) == 0 {
		return nil, "", false, typedErr("invalid_request", "the chat payload needs at least one message", 400)
	}

	messages := make([]upstream.ConversationMessage, 0, len(payload.Messages))
	for _, item := range payload.Messages {
		content, err := messageText(item.Content)
		if err != nil {
			return nil, "", false, err
		}
		switch item.Role {
		case "tool":
			// The reference's tool_result text block; the upstream has no
			// tool author role, so the result folds into a user message.
			messages = append(messages, upstream.ConversationMessage{
				Role:    "user",
				Content: "Tool result " + item.ToolCallID + ": " + content,
			})
		case "assistant":
			if len(item.ToolCalls) > 0 {
				content = foldAssistantToolCalls(content, item.ToolCalls)
			}
			messages = append(messages, upstream.ConversationMessage{Role: "assistant", Content: content})
		default:
			messages = append(messages, upstream.ConversationMessage{Role: item.Role, Content: content})
		}
	}

	// The tool prompt joins the conversation's system message(s) (the
	// reference's merge_system).
	messages = mergeSystemPrompt(messages, prompt)

	if toolMode {
		messages = appendUserBridge(messages)
	}

	effort := payload.ThinkingEffort
	if effort == "" {
		effort = payload.ReasoningEffort
	}
	return messages, effort, toolMode, nil
}

// messageText resolves one OpenAI message content: a plain string passes
// through; a parts array contributes its text parts joined in order;
// anything else is a typed request error (image/tool parts never travel
// the text path).
func messageText(content any) (string, *pluginabi.Error) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var text string
		for _, part := range value {
			obj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if partType, _ := obj["type"].(string); partType != "" && partType != "text" {
				return "", typedErr("invalid_request",
					"non-text message parts are not supported by this provider", 400)
			}
			if s, ok := obj["text"].(string); ok {
				text += s
			}
		}
		return text, nil
	case nil:
		return "", nil
	default:
		return "", typedErr("invalid_request", "the message content must be a string or a parts array", 400)
	}
}

// estimateTokens is the DOCUMENTED estimate for token counts: bytes/4,
// pinned in the README. It is never presented as an exact count.
func estimateTokens(text string) int {
	return len(text) / 4
}

func estimatePromptTokens(messages []upstream.ConversationMessage) int {
	total := 0
	for _, message := range messages {
		total += estimateTokens(message.Content)
	}
	return total
}

// ---------------------------------------------------------------------------
// Non-stream assembly.
// ---------------------------------------------------------------------------

// assembleTextCompletion drains the event channel and answers either the
// chat.completion envelope or a typed error. Zero assembled content is a
// typed 502 no_assistant_content — never an empty 200 (Review Focus #2).
// A channel that closes without a terminal event (the turn budget expired
// mid-drain) maps to the typed 504: the turn may still be running upstream.
func assembleTextCompletion(events <-chan upstream.ConvEvent, turn *textTurn) ([]byte, *pluginabi.Error) {
	text, err := drainTextCompletion(events, turn)
	if err != nil {
		return nil, err
	}
	return completionPayload(turn, text), nil
}

// drainTextCompletion consumes the event channel to a terminal state and
// returns the assembled raw assistant text (markup included) or a typed
// error. Both the non-stream assembly and the buffered tool stream reuse it
// (controller ruling #3). Zero content is the typed 502; a closed channel
// with no terminal event is the typed 504.
func drainTextCompletion(events <-chan upstream.ConvEvent, turn *textTurn) (string, *pluginabi.Error) {
	for ev := range events {
		switch ev.Kind {
		case upstream.ConvEventError:
			return "", mapUpstreamError(ev.Err)
		case upstream.ConvEventComplete:
			if ev.Blocked {
				return "", contentPolicyViolation()
			}
			if ev.Text == "" {
				return "", noAssistantContent()
			}
			text := ev.Text
			if turn.nudgeRetry != nil && turnNeedsNudge(turn, text) {
				applyNudge(turn, text)
				if retryText, ok := turn.nudgeRetry(); ok {
					text = retryText
				}
			}
			return text, nil
		}
	}
	// StreamConversation's reader emits complete or error before closing;
	// reaching the closed channel means the turn budget expired mid-drain.
	return "", streamTimeoutEnvelope()
}

func contentPolicyViolation() *pluginabi.Error {
	return typedErr("content_policy_violation",
		"the upstream blocked the response as a content policy violation", 400)
}

func streamTimeoutEnvelope() *pluginabi.Error {
	e := typedErr("upstream_stream_timeout", upstream.ErrStreamTimeout.Error(), 504)
	e.Retryable = true
	return e
}

// resolveAnswer parses the assembled assistant text into the OpenAI answer
// shape: the (markup-stripped) content, the tool_calls entries (each with a
// fresh call_<uuid> id and the JSON-string arguments) and the finish_reason.
// toolMode gates the parse: without a tools array the text is prose and the
// answer is a normal stop completion — incidental XML never becomes calls.
func resolveAnswer(turn *textTurn, text string) (content string, toolCalls []map[string]any, finish string) {
	if !turn.toolMode {
		return text, nil, "stop"
	}
	calls := parseToolCalls(text)
	content = stripToolMarkup(text)
	finish = "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
		for _, call := range calls {
			args, _ := json.Marshal(call.args)
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID(),
				"type": "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": string(args),
				},
			})
		}
	}
	return content, toolCalls, finish
}

// completionPayload renders the pinned chat.completion envelope: the id is
// the turn's chatcmpl-<uuid>, the model echoes the requested published id,
// and the usage carries the documented estimate over the RAW assembled text
// (markup included — the model really generated it).
func completionPayload(turn *textTurn, text string) []byte {
	content, toolCalls, finish := resolveAnswer(turn, text)
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	output := estimateTokens(text)
	envelope := map[string]any{
		"id":      turn.turnID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   turn.modelID,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     turn.promptTokens,
			"completion_tokens": output,
			"total_tokens":      turn.promptTokens + output,
		},
	}
	raw, _ := json.Marshal(envelope)
	return raw
}

// ---------------------------------------------------------------------------
// The stream pump.
// ---------------------------------------------------------------------------

// pumpTextStream converts the ConvEvent channel into OpenAI chunk frames
// downstream: one chatcmpl turn id shared by every frame, the role chunk
// on the first assistant signal, a content frame per assistant_delta, the
// terminal finish_reason:"stop" chunk (carrying the usage estimate), then
// the data: [DONE] sentinel (the host writes each emitted payload to the
// client SSE body verbatim — the mistral/commandcode/opencode contract).
// host.stream.close is issued exactly once on every exit path (success,
// error, zero content, content policy, emit failure, budget cancel); the
// turn budget cancel is the last bound once the watchdog has run out of
// hard cap.
func (m *Manager) pumpTextStream(downstreamID string, events <-chan upstream.ConvEvent, turn *textTurn, cancelBudget context.CancelFunc) {
	var closeOnce sync.Once
	closeStream := func(message string) {
		closeOnce.Do(func() {
			cancelBudget()
			_, _ = m.host.Call(pluginabi.MethodHostStreamClose, streamCloseWire(downstreamID, message))
		})
	}
	emit := func(payload []byte) bool {
		_, err := m.host.Call(pluginabi.MethodHostStreamEmit, streamEmitWire(downstreamID, payload))
		if err != nil {
			// The downstream bridge is gone (host aborted); stop pumping
			// and close once with a static label — no payload material.
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}
	defer closeStream("")

	roleSent := false
	terminalSent := false
	sendRole := func() {
		if roleSent {
			return
		}
		roleSent = true
		emit(chunkFrame(turn, map[string]any{"role": "assistant"}, nil))
	}
	sendDelta := func(content string) {
		sendRole()
		emit(chunkFrame(turn, map[string]any{"role": "assistant", "content": content}, nil))
	}
	sendTerminal := func() {
		if terminalSent {
			return
		}
		terminalSent = true
		finish := "stop"
		usage := map[string]any{
			"prompt_tokens":     turn.promptTokens,
			"completion_tokens": estimateTokens(turn.text),
			"total_tokens":      turn.promptTokens + estimateTokens(turn.text),
		}
		if !emit(chunkFrame(turn, map[string]any{}, &finish, usage)) {
			return
		}
		// [DONE] semantics: the OpenAI client stops on the first
		// sentinel; emitting it here matches every host built-in
		// executor (devin/openai_compat) and the opencode plugin.
		emit([]byte("data: [DONE]\n\n"))
	}

	for ev := range events {
		switch ev.Kind {
		case upstream.ConvEventMessageID:
			sendRole()
		case upstream.ConvEventAssistantDelta:
			turn.text += ev.Delta
			sendDelta(ev.Delta)
		case upstream.ConvEventComplete:
			turn.text = ev.Text
			if ev.Blocked {
				closeStream(envelopeText(contentPolicyViolation()))
				return
			}
			if ev.Text == "" {
				// Review Focus #2 in stream form: never a terminal
				// chunk for an empty answer — one typed close instead.
				closeStream(envelopeText(noAssistantContent()))
				return
			}
			sendTerminal()
			return // the deferred closeStream("") runs once, cleanly
		case upstream.ConvEventError:
			mapped := mapUpstreamError(ev.Err)
			closeStream(fmt.Sprintf("%s (http %d)", envelopeText(mapped), mapped.HTTPStatus))
			return
		}
		// ConvEventRaw: the patch protocol already filtered these;
		// nothing downstream.
	}
	// Channel closed without a terminal event: the turn budget expired
	// mid-stream. The turn may still be running upstream — typed 504.
	closeStream(envelopeText(streamTimeoutEnvelope()))
}

// emitBufferedToolStream is the streaming-with-tools path (controller
// ruling #3, accepted v1 tradeoff): tool requests are NOT token-streamed.
// The full upstream turn is drained first (reusing the non-stream
// assembly), the tool calls are parsed out of the buffered text, and only
// then are the frames emitted: a role + stripped-prose frame, a finish
// frame carrying the tool_calls (when any) with finish_reason
// "tool_calls"/"stop" and the usage estimate, then [DONE]. host.stream.close
// is issued exactly once on every exit path, as in the pump.
func (m *Manager) emitBufferedToolStream(downstreamID string, events <-chan upstream.ConvEvent, turn *textTurn, cancelBudget context.CancelFunc) {
	var closeOnce sync.Once
	closeStream := func(message string) {
		closeOnce.Do(func() {
			cancelBudget()
			_, _ = m.host.Call(pluginabi.MethodHostStreamClose, streamCloseWire(downstreamID, message))
		})
	}
	defer closeStream("")
	emit := func(payload []byte) bool {
		_, err := m.host.Call(pluginabi.MethodHostStreamEmit, streamEmitWire(downstreamID, payload))
		if err != nil {
			// The downstream bridge is gone (host aborted); stop emitting
			// and close once with a static label — no payload material.
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}

	text, err := drainTextCompletion(events, turn)
	if err != nil {
		closeStream(fmt.Sprintf("%s (http %d)", envelopeText(err), err.HTTPStatus))
		return
	}
	content, toolCalls, finish := resolveAnswer(turn, text)

	// Role chunk first; the stripped prose rides in the same frame when
	// non-empty (the reference's stream_text_chat_completion pattern of a
	// role+content first chunk), else a bare role chunk.
	if content != "" {
		if !emit(chunkFrame(turn, map[string]any{"role": "assistant", "content": content}, nil)) {
			return
		}
	} else {
		if !emit(chunkFrame(turn, map[string]any{"role": "assistant"}, nil)) {
			return
		}
	}

	finishReason := finish
	delta := map[string]any{}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	usage := map[string]any{
		"prompt_tokens":     turn.promptTokens,
		"completion_tokens": estimateTokens(text),
		"total_tokens":      turn.promptTokens + estimateTokens(text),
	}
	if !emit(chunkFrame(turn, delta, &finishReason, usage)) {
		return
	}
	emit([]byte("data: [DONE]\n\n"))
}

// envelopeText renders a typed envelope error as "code: message" — the
// host's stream close carries a plain string, so the code must ride in the
// text for the close to stay typed. (pluginabi.Error.Error() is message-only.)
func envelopeText(e *pluginabi.Error) string {
	return e.Code + ": " + e.Message
}

func noAssistantContent() *pluginabi.Error {
	return typedErr("no_assistant_content",
		"the turn completed with no visible assistant content", 502)
}

// chunkFrame renders one chat.completion.chunk SSE line: "data: <json>\n\n".
// finishReason nil = the frame carries no finish_reason yet (null), usage
// appears only on the terminal frame.
func chunkFrame(turn *textTurn, delta map[string]any, finishReason *string, usage ...map[string]any) []byte {
	frame := map[string]any{
		"id":      turn.turnID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   turn.modelID,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	if len(usage) > 0 {
		frame["usage"] = usage[0]
	}
	raw, _ := json.Marshal(frame)
	out := make([]byte, 0, len("data: ")+len(raw)+2)
	out = append(out, "data: "...)
	out = append(out, raw...)
	return append(out, '\n', '\n')
}

// streamEmitWire / streamCloseWire are the host stream-bridge request
// shapes (snake_case, payload base64 by encoding/json).
func streamEmitWire(streamID string, payload []byte) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	return raw
}

func streamCloseWire(streamID, message string) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": message})
	return raw
}
