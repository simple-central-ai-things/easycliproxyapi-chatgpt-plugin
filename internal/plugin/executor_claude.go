package plugin

// executor_claude.go implements the Anthropic Messages format (claude,
// Phase 2 F2): the host passes /v1/messages requests untranslated
// (SourceFormat "claude"), so this file owns only the INBOUND decode
// (system + messages → conversation messages, per the reference's
// preprocess_payload / normalize_messages) and the OUTBOUND shaping (the
// message envelope and the Anthropic SSE event sequence instead of
// chat.completion). The upstream turn is the SAME conversation pipeline as
// the openai format — sentinel handshake, conversation SSE, the 401
// refresh-retry + persist loop and the zero-content typed 502 — all reused
// from executor.go / executor_text.go, exactly as F1 did for
// openai-response.
//
// Inbound: the top-level `system` (a string or a list of text blocks)
// becomes the leading system message; each message's content flattens —
// text parts in order, a tool_use part rewritten to the reference's exact
// single-<tool_calls> XML text block, a tool_result part to the reference's
// "Tool result {tool_use_id}: {content}" text, image and any other part
// contributing nothing (the text path has no image surface).
//
// Tools: Anthropic tools (name / description / input_schema) reuse the
// chat format's XML emulation (tools.go). The reference's merge_system
// special case is kept: when the system prompt carries "You are Claude
// Code", the per-tool schema prompt is REPLACED by the single-line
// XML_TOOL_RULE — and the rule is appended even without a tools array.
// The reply's <tool_calls> XML parses back into tool_use content blocks
// with fresh toolu_<uuid4()> ids.
//
// Outbound (non-stream): the reference's message_response envelope —
// id msg_<uuid4()> (dashed UUID, unlike the Responses format's hex form),
// type "message", role assistant, the requested model echoed, content[] of
// text and/or tool_use blocks, stop_reason "end_turn" / "tool_use",
// stop_sequence null, usage in the reference's {input_tokens,
// output_tokens} shape filled with the documented bytes/4 estimate.
//
// Streaming: the reference's Anthropic SSE sequence — message_start →
// content_block_start → content_block_delta* → content_block_stop (per
// block) → message_delta (stop_reason + output usage) → message_stop —
// each event framed "event: <type>\ndata: <json>\n\n" the way the
// reference's anthropic_sse_stream does. Every claude turn buffers the
// whole upstream conversation first and emits the event sequence at once
// (the chat format's ruling #3, extended to all claude turns; the event
// sequence equals the reference's incremental emission at completion).
// The host passes same-format plugin streams through verbatim (no
// claude→claude translator is registered), so the framing rides each
// emitted payload — and there is NO data: [DONE] sentinel: message_stop
// is the Anthropic terminator.
//
// Errors: every failure answers the same typed pluginabi.Error envelope
// the other formats use. The host's Claude handler (the /v1/messages
// route's WriteErrorResponse override, CLIProxyAPI sdk/api/handlers/claude)
// renders it as the Anthropic {"type":"error","error":{type,message}} body,
// the error type derived from the HTTP status — the host-side equivalent
// of the reference's anthropic_error_response (error_response.py
// lines 107-124). The plugin therefore carries NO per-format error bytes;
// mid-stream failures close the stream with the typed text and the host
// emits the reference's "event: error" frame.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// claudeFormat is the executor format name the registration advertises and
// the dispatcher branches on (the host's sdktranslator "claude" wire name).
const claudeFormat = "claude"

// isClaudeRequest reports whether the executor request is the claude
// format (either tag — the host passes the client's protocol untranslated).
func isClaudeRequest(req executorRequest) bool {
	return req.SourceFormat == claudeFormat || req.Format == claudeFormat
}

// ---------------------------------------------------------------------------
// Ids.
// ---------------------------------------------------------------------------

// claudeUUID renders one fresh UUIDv4 with dashes — the reference's
// f"msg_{uuid.uuid4()}" / f"toolu_{uuid.uuid4()}" convention (unlike the
// Responses format's hex form).
func claudeUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("uuid generation: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func claudeMsgID() string     { return "msg_" + claudeUUID() }
func claudeToolUseID() string { return "toolu_" + claudeUUID() }

// ---------------------------------------------------------------------------
// The request decode: Anthropic system/messages/tools → conversation
// messages.
// ---------------------------------------------------------------------------

// claudeShapePayload mirrors the Anthropic Messages request fields this
// executor consumes. system and every content value are raw `any`: both
// arrive as strings or as block lists.
type claudeShapePayload struct {
	System   any `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Tools json.RawMessage `json:"tools"`
}

// claudeMessageText ports the reference's message_text (conversation.py
// lines 159-166): a string passes through; a list contributes its
// text / input_text / output_text blocks' content in order, joined with
// NO separator (the reference's "".join); anything else contributes
// nothing.
func claudeMessageText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if partType := jsonStringOr(item["type"]); partType == "text" || partType == "input_text" || partType == "output_text" {
				builder.WriteString(jsonStringOr(item["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

// claudeCodeSystemMarker is the reference's Claude Code detector
// (_has_claude_code_system, anthropic_v1_messages.py lines 72-77).
const claudeCodeSystemMarker = "You are Claude Code"

// claudeXMLToolRule is the reference's XML_TOOL_RULE (lines 17-18): the
// single-line tool-output contract Claude Code clients get instead of the
// full per-tool schema prompt (assembled from the tools.go tokens so the
// literal tag never appears contiguously in this source).
const claudeXMLToolRule = "Tool output adapter: when calling tools, output ONLY this XML and no prose/markdown:\n" +
	"<tool_calls>\n" + toolCallToken + "<tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters>\n" +
	toolCallCloseToken + "</tool_calls>"

// hasClaudeCodeSystem ports the reference's _has_claude_code_system: a
// string system carrying the marker, or any list item's text carrying it.
func hasClaudeCodeSystem(system any) bool {
	switch value := system.(type) {
	case string:
		return strings.Contains(value, claudeCodeSystemMarker)
	case []any:
		for _, raw := range value {
			item, ok := raw.(map[string]any)
			if ok && strings.Contains(jsonStringOr(item["text"]), claudeCodeSystemMarker) {
				return true
			}
		}
	}
	return false
}

// mergeClaudeSystem ports the reference's merge_system (lines 59-69): the
// tool prompt — or the single-line XML rule for Claude Code clients,
// which REPLACES the schema prompt and is added even without tools —
// joins the request's system text. A string system is trimmed and gets a
// blank-line separator; a list system concatenates its text blocks (the
// reference's message_text join) with no separator; an absent system
// yields the extra alone.
func mergeClaudeSystem(system any, prompt string) string {
	extra := prompt
	if hasClaudeCodeSystem(system) {
		extra = claudeXMLToolRule
	}
	systemText := claudeMessageText(system)
	if extra == "" {
		return systemText
	}
	if systemText == "" {
		return extra
	}
	if _, isString := system.(string); isString {
		return strings.TrimSpace(systemText) + "\n\n" + extra
	}
	return systemText + extra
}

// claudeToolUseXML is the reference's tool_use rewrite (_preprocess_block,
// line 148): the call rendered as the exact single-line <tool_calls> XML
// text block — the parser and the stream round-trip both key on it.
func claudeToolUseXML(name string, input any) string {
	return "<tool_calls>\n" + toolCallToken +
		"<tool_name>" + name + "</tool_name>" +
		"<parameters>" + jsonText(input) + "</parameters>" +
		toolCallCloseToken + "</tool_calls>"
}

// flattenClaudeContent resolves one message's content (string or block
// list) to the text the upstream posts: the reference's
// preprocess_messages / _preprocess_block (lines 121-151) followed by the
// normalize_messages message_text join — text parts in order, a tool_use
// part rewritten to the reference's tool_use XML text block, a
// tool_result part to the reference's "Tool result {id}: {content}" text,
// image and any other part contributing nothing to the text path.
func flattenClaudeContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch blockType := jsonStringOr(block["type"]); blockType {
			case "tool_use":
				builder.WriteString(claudeToolUseXML(jsonStringOr(block["name"]), firstJSONTruthy(block["input"])))
			case "tool_result":
				builder.WriteString("Tool result " + jsonStringOr(block["tool_use_id"]) + ": " + jsonStringOr(block["content"]))
			case "text", "input_text", "output_text":
				builder.WriteString(jsonStringOr(block["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

// shapeClaudeRequest decodes one Anthropic Messages request body into the
// upstream conversation messages and the tool mode. Per the reference's
// message_request: the tools refusal (no usable name / non-array) precedes
// every other work and costs zero network; the system text (with the
// Claude Code rule swap) becomes the leading system message when
// non-empty; each message flattens per its role — a blank role defaults
// to user, the reference's message.get("role", "user"). The claude
// request carries no thinking-effort spelling (the reference reads none),
// so the config default applies at the turn.
func shapeClaudeRequest(body []byte) ([]upstream.ConversationMessage, bool, *pluginabi.Error) {
	if len(body) == 0 {
		return nil, false, typedErr("invalid_request", "empty claude payload", 400)
	}
	var payload claudeShapePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, typedErr("invalid_request", "the claude payload is not valid JSON", 400)
	}

	prompt, toolMode, err := shapeTools(payload.Tools)
	if err != nil {
		return nil, false, err
	}

	if len(payload.Messages) == 0 {
		return nil, false, typedErr("invalid_request",
			"the claude payload needs at least one message", 400)
	}

	messages := make([]upstream.ConversationMessage, 0, len(payload.Messages)+1)
	if system := mergeClaudeSystem(payload.System, prompt); system != "" {
		messages = append(messages, upstream.ConversationMessage{Role: "system", Content: system})
	}
	for _, item := range payload.Messages {
		role := item.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, upstream.ConversationMessage{
			Role:    role,
			Content: flattenClaudeContent(item.Content),
		})
	}
	return messages, toolMode, nil
}

// ---------------------------------------------------------------------------
// Dispatch entry points (wired from executor.go).
// ---------------------------------------------------------------------------

// prepareClaudeTextTurn shapes one claude text turn: the chat format's
// textTurn with the claude decode and the msg_<uuid4()> turn id standing
// in for the chatcmpl- id — it is the message envelope's id, shared by
// every frame of the turn. created carries message_stop's field.
func (m *Manager) prepareClaudeTextTurn(req executorRequest) (*textTurn, *pluginabi.Error) {
	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	// Model resolution mirrors the chat format: text turns publish as
	// <prefix>/openai/<slug>; anything else refuses typed model_unknown.
	slug, found := trimTextModelPrefix(req.Model, cfg.ModelPrefix)
	if !found {
		return nil, typedErr("model_unknown",
			fmt.Sprintf("model %q is not a published text model (%s/openai/<slug>)", req.Model, cfg.ModelPrefix), 400)
	}

	turn := &textTurn{
		cfg:          cfg,
		cred:         cred,
		modelID:      req.Model,
		turnID:       claudeMsgID(),
		created:      time.Now().Unix(),
		hardCap:      sseHardCapFor(cfg),
		authFileName: execAuthFileName(req),
	}

	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	messages, toolMode, shapeErr := shapeClaudeRequest(body)
	if shapeErr != nil {
		return nil, shapeErr
	}
	turn.messages = messages
	turn.toolMode = toolMode
	// The claude request carries no thinking-effort spelling: the config
	// default applies at the turn (auto → omitted upstream).
	effort := cfg.ThinkingEffort
	turn.promptTokens = estimatePromptTokens(messages)

	convReq := upstream.ConversationRequest{
		Messages:       messages,
		Model:          slug,
		ThinkingEffort: effort,
	}
	payload, err := upstream.BuildConversationPayload(convReq)
	if err != nil {
		return nil, typedErr("invalid_request", "malformed claude messages: "+err.Error(), 400)
	}
	turn.payload = payload

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return nil, typedErr("upstream_error", "upstream transport unavailable: "+err.Error(), 500)
	}
	turn.upstream = u
	return turn, nil
}

// executeClaude is the non-stream claude turn: the turn start (sentinel +
// conversation SSE + the 401 refresh-retry loop) reuses the shared text
// pipeline, the drain reuses the zero-content typed 502 contract, and the
// answer is the message envelope.
func (m *Manager) executeClaude(req executorRequest) ([]byte, error) {
	turn, apiErr := m.prepareClaudeTextTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	defer cancel()

	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		return errEnvelope(err), nil
	}
	text, err := drainTextCompletion(ch, turn)
	if err != nil {
		return errEnvelope(err), nil
	}
	return ok(pluginapi.ExecutorResponse{Payload: claudeMessagePayload(turn, text)}), nil
}

// executeClaudeStream is the stream claude turn: the whole upstream turn
// drains inside Handle, then the Anthropic event sequence goes out at
// once (buffered-then-emit — see the file header). Every path answers an
// empty result immediately and closes the downstream stream exactly once.
func (m *Manager) executeClaudeStream(req executorRequest) ([]byte, error) {
	turn, apiErr := m.prepareClaudeTextTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		cancel()
		return errEnvelope(err), nil
	}

	m.emitClaudeStream(req.StreamID, ch, turn, cancel)
	return ok(map[string]any{}), nil
}

// ---------------------------------------------------------------------------
// Non-stream assembly: the content blocks and the message envelope.
// ---------------------------------------------------------------------------

// claudeContentBlocks ports the reference's content_blocks (lines
// 168-174): in tool mode the reply's parsed <tool_calls> XML become
// tool_use blocks with fresh toolu_<uuid4()> ids (a leading text block
// keeps the markup-stripped prose when non-empty) and stop_reason
// "tool_use"; everything else — including an all-markup reply that parses
// no calls — answers one text block with the stripped prose and
// "end_turn".
func claudeContentBlocks(turn *textTurn, text string) ([]map[string]any, string) {
	prose := stripToolMarkup(text)
	if turn.toolMode {
		calls := parseToolCalls(text)
		if len(calls) > 0 {
			blocks := make([]map[string]any, 0, 1+len(calls))
			if prose != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": prose})
			}
			for _, call := range calls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    claudeToolUseID(),
					"name":  call.name,
					"input": call.args,
				})
			}
			return blocks, "tool_use"
		}
	}
	return []map[string]any{{"type": "text", "text": prose}}, "end_turn"
}

// claudeMessagePayload renders the reference's message_response envelope
// (lines 154-165): id msg_<uuid4()>, type "message", role assistant, the
// requested published model echoed, the content blocks, stop_reason,
// stop_sequence null and the documented bytes/4 usage estimate over the
// RAW assembled text (markup included — the model really generated it).
func claudeMessagePayload(turn *textTurn, text string) []byte {
	content, stopReason := claudeContentBlocks(turn, text)
	envelope := map[string]any{
		"id":            turn.turnID,
		"type":          "message",
		"role":          "assistant",
		"model":         turn.modelID,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  turn.promptTokens,
			"output_tokens": estimateTokens(text),
		},
	}
	raw, _ := json.Marshal(envelope)
	return raw
}

// ---------------------------------------------------------------------------
// The stream emitter.
// ---------------------------------------------------------------------------

// claudeEventFrame renders one Anthropic SSE event the way the reference's
// anthropic_sse_stream does (utils/helper.py): "event: <type>\ndata:
// <json>\n\n". The host passes same-format plugin streams through
// verbatim (no claude→claude translator is registered), so the full
// framing rides each emitted payload; there is NO data: [DONE] sentinel —
// message_stop is the Anthropic terminator.
func claudeEventFrame(event map[string]any) []byte {
	eventType, _ := event["type"].(string)
	raw, _ := json.Marshal(event)
	return []byte("event: " + eventType + "\ndata: " + string(raw) + "\n\n")
}

// emitClaudeStream drains the upstream turn and emits the reference's
// Anthropic event sequence (stream_events, lines 225-287): message_start
// → per content block content_block_start / content_block_delta /
// content_block_stop (text blocks stream one text_delta; tool_use blocks
// announce empty input then one input_json_delta carrying the full JSON —
// the reference's _stream_buffered_blocks) → message_delta (stop_reason +
// output usage) → message_stop (with created). host.stream.close is issued
// exactly once on every exit path; failures close with the typed text the
// host renders as the reference's "event: error" frame.
func (m *Manager) emitClaudeStream(downstreamID string, events <-chan upstream.ConvEvent, turn *textTurn, cancelBudget context.CancelFunc) {
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
	send := func(event map[string]any) bool {
		return emit(claudeEventFrame(event))
	}

	text, err := drainTextCompletion(events, turn)
	if err != nil {
		closeStream(fmt.Sprintf("%s (http %d)", envelopeText(err), err.HTTPStatus))
		return
	}
	content, stopReason := claudeContentBlocks(turn, text)

	// message_start: the reference's empty-content assistant message with
	// the input usage and output_tokens 0 (line 233).
	if !send(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            turn.turnID,
			"type":          "message",
			"role":          "assistant",
			"model":         turn.modelID,
			"content":       []map[string]any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  turn.promptTokens,
				"output_tokens": 0,
			},
		},
	}) {
		return
	}
	for index, block := range content {
		if block["type"] == "tool_use" {
			if !send(map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    block["id"],
					"name":  block["name"],
					"input": map[string]any{},
				},
			}) {
				return
			}
			partial, _ := json.Marshal(block["input"])
			if !send(map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(partial),
				},
			}) {
				return
			}
		} else {
			if !send(map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}) {
				return
			}
			if !send(map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{
					"type": "text_delta",
					"text": block["text"],
				},
			}) {
				return
			}
		}
		if !send(map[string]any{"type": "content_block_stop", "index": index}) {
			return
		}
	}
	// message_delta: the reference's terminal stop_reason + output usage
	// (line 271).
	if !send(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateTokens(text)},
	}) {
		return
	}
	// message_stop terminates the Anthropic stream (line 273); the host's
	// Claude handler treats its absence as a typed stream error.
	send(map[string]any{"type": "message_stop", "created": turn.created})
}
