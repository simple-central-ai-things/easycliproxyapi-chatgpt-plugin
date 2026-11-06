package plugin

// executor_text.go implements the OpenAI text conversation format: the
// request shaping (model resolution, tools refusal, thinking-effort
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
	cfg          Config
	cred         Credential
	upstream     upstream.Upstream
	payload      map[string]any // the built upstream conversation body
	messages     []upstream.ConversationMessage
	hardCap      time.Duration
	modelID      string // the requested published id, echoed in every frame
	turnID       string // one chatcmpl-<uuid> per turn (all frames share it)
	promptTokens int    // documented estimate over the request messages
	text         string // assembled assistant text (pump only)
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
		cfg:     cfg,
		cred:    cred,
		modelID: req.Model,
		turnID:  turnID(),
		hardCap: sseHardCapFor(cfg),
	}

	// Request shaping from the host-passed OpenAI chat payload
	// (SourceFormat == output format → the host passes it untranslated).
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	messages, effort, shapeErr := shapeChatRequest(body)
	if shapeErr != nil {
		return nil, shapeErr
	}
	turn.messages = messages
	if effort == "" {
		// The config default applies when the request carries no
		// thinking_effort / reasoning_effort (auto → omitted upstream).
		effort = cfg.ThinkingEffort
	}
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

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return nil, typedErr("upstream_error", "upstream transport unavailable: "+err.Error(), 500)
	}
	turn.upstream = u
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
// absent; both effort spellings are accepted (controller ruling).
type shapeChatPayload struct {
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Tools           json.RawMessage `json:"tools"`
	ThinkingEffort  string          `json:"thinking_effort"`
	ReasoningEffort string          `json:"reasoning_effort"`
}

// shapeChatRequest turns the host-passed OpenAI chat JSON into the
// upstream conversation messages plus the resolved thinking effort.
// tools → typed 400 tools_unsupported BEFORE any upstream call (honest
// refusal; SCPM routes tool-calling clients elsewhere).
func shapeChatRequest(body []byte) ([]upstream.ConversationMessage, string, *pluginabi.Error) {
	if len(body) == 0 {
		return nil, "", typedErr("invalid_request", "empty chat payload", 400)
	}
	var payload shapeChatPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", typedErr("invalid_request", "the chat payload is not valid JSON", 400)
	}

	// The tools refusal is unconditional and precedes every other work.
	var tools []any
	if len(payload.Tools) > 0 && json.Unmarshal(payload.Tools, &tools) == nil && len(tools) > 0 {
		return nil, "", typedErr("tools_unsupported",
			"this provider does not support tools; send the request without them", 400)
	}

	if len(payload.Messages) == 0 {
		return nil, "", typedErr("invalid_request", "the chat payload needs at least one message", 400)
	}

	messages := make([]upstream.ConversationMessage, 0, len(payload.Messages))
	for _, item := range payload.Messages {
		content, err := messageText(item.Content)
		if err != nil {
			return nil, "", err
		}
		messages = append(messages, upstream.ConversationMessage{Role: item.Role, Content: content})
	}

	effort := payload.ThinkingEffort
	if effort == "" {
		effort = payload.ReasoningEffort
	}
	return messages, effort, nil
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
	for ev := range events {
		switch ev.Kind {
		case upstream.ConvEventError:
			return nil, mapUpstreamError(ev.Err)
		case upstream.ConvEventComplete:
			if ev.Blocked {
				return nil, contentPolicyViolation()
			}
			if ev.Text == "" {
				return nil, noAssistantContent()
			}
			return completionPayload(turn, ev.Text), nil
		}
	}
	// StreamConversation's reader emits complete or error before closing;
	// reaching the closed channel means the turn budget expired mid-drain.
	return nil, streamTimeoutEnvelope()
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

// completionPayload renders the pinned chat.completion envelope: the id is
// the turn's chatcmpl-<uuid>, the model echoes the requested published id,
// and the usage carries the documented estimate.
func completionPayload(turn *textTurn, content string) []byte {
	output := estimateTokens(content)
	envelope := map[string]any{
		"id":      turn.turnID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   turn.modelID,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
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
