package plugin

// executor_claude_test.go pins the Anthropic Messages format (claude, Phase
// 2 F2): the request decode (system + messages with tool_use / tool_result
// block flattening, the Claude Code XML-rule swap), the non-stream message
// envelope (content blocks, stop_reason, the documented usage estimate), the
// tool round-trip (the reply's <tool_calls> XML parsed back into tool_use
// blocks), the stream event sequence (message_start ... message_stop with
// the reference's event:/data: framing and NO [DONE] sentinel), and the
// typed error envelope the host renders as the Anthropic error shape.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	// The claude reference emits dashed UUIDv4s (f"msg_{uuid.uuid4()}" /
	// f"toolu_{uuid.uuid4()}") — unlike the Responses format's hex form.
	claudeMsgIDRe   = regexp.MustCompile(`^msg_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	claudeToolUIDRe = regexp.MustCompile(`^toolu_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// execClaudeRequest builds one SDK-typed executor request tagged the claude
// format (both Format and SourceFormat — the host passes the client's
// protocol untranslated).
func execClaudeRequest(t *testing.T, cred Credential, model string, payload any, stream bool) pluginapi.ExecutorRequest {
	t.Helper()
	req := execRequest(t, cred, model, payload, stream)
	req.Format = claudeFormat
	req.SourceFormat = claudeFormat
	return req
}

// decodeClaudeFrame parses one emitted Anthropic SSE frame:
// "event: <type>\ndata: <json>\n\n" (the reference's anthropic_sse_stream).
func decodeClaudeFrame(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	s := string(payload)
	if !strings.HasPrefix(s, "event: ") || !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("emitted payload is not an event:/data:-framed SSE frame: %q", payload)
	}
	idx := strings.Index(s, "\ndata: ")
	if idx < 0 {
		t.Fatalf("emitted payload lacks the data: line: %q", payload)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(s[idx+len("\ndata: "):len(s)-2]), &event); err != nil {
		t.Fatalf("decode claude frame: %v (%q)", err, payload)
	}
	return event
}

func claudeFrameType(t *testing.T, payload []byte) string {
	t.Helper()
	event := decodeClaudeFrame(t, payload)
	if asString(event["type"]) != strings.TrimPrefix(strings.SplitN(string(payload), "\n", 2)[0], "event: ") {
		t.Fatalf("the frame's event: line and its JSON type disagree: %s", payload)
	}
	return asString(event["type"])
}

// claudeToolXML renders one single tool-call block with the reference's
// exact XML (assembled from the tools.go tokens so the literal tag never
// appears contiguously in this source).
func claudeToolXML(name, params string) string {
	return "<tool_calls>\n" + toolCallToken + "\n" +
		"<tool_name>" + name + "</tool_name>\n" +
		"<parameters>" + params + "</parameters>\n" +
		toolCallCloseToken + "\n</tool_calls>"
}

// countExecHostCalls returns the recorded host calls under one method name.
func countExecHostCalls(t *testing.T, fake *execFakeCaller, method string) int {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	count := 0
	for _, call := range fake.calls {
		if call.method == method {
			count++
		}
	}
	return count
}

// claudeReplySSE wraps one assembled assistant reply text as the fixture
// conversation stream (message add + parts append + [DONE]).
func claudeReplySSE(t *testing.T, reply string) string {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"p": "/message/content/parts/0", "o": "append", "v": reply,
	})
	if err != nil {
		t.Fatalf("marshal reply frame: %v", err)
	}
	return `data: v1

data: {"p":"","o":"add","v":{"message":{"id":"m-1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]},"status":"in_progress"},"conversation_id":"c-1"}}

data: ` + string(frame) + `

data: [DONE]
`
}

// ---------------------------------------------------------------------------
// TestClaudeMessagesMapping — the Anthropic request decode at the shape
// level (no upstream call): system (string and list) → the leading system
// message, the block flattening (text / tool_use / tool_result), the
// Claude Code XML-rule swap, effort absence and the tools contract.
// ---------------------------------------------------------------------------

func TestClaudeMessagesMapping(t *testing.T) {
	t.Run("string system with tools merges the schema prompt", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"system": "You are helpful.",
			"messages": []any{
				map[string]any{"role": "user", "content": "Say hello"},
			},
			"tools": []any{
				map[string]any{"name": "get_weather", "description": "Weather", "input_schema": map[string]any{"type": "object"}},
			},
		})
		messages, toolMode, apiErr := shapeClaudeRequest(body, false)
		if apiErr != nil {
			t.Fatalf("shapeClaudeRequest: %+v", apiErr)
		}
		if !toolMode {
			t.Fatal("toolMode = false, want the emulated tool mode")
		}
		if len(messages) != 2 {
			t.Fatalf("messages = %+v, want system + user", messages)
		}
		if messages[0].Role != "system" || !strings.HasPrefix(messages[0].Content, "You are helpful.\n\n") ||
			!strings.Contains(messages[0].Content, "Tool: get_weather") ||
			!strings.Contains(messages[0].Content, `Parameters: {"type":"object"}`) ||
			!strings.Contains(messages[0].Content, "Tool use rules:") {
			t.Fatalf("messages[0] = %+v, want the system with the tool schema prompt merged", messages[0])
		}
		if messages[1].Role != "user" || messages[1].Content != "Say hello" {
			t.Fatalf("messages[1] = %+v, want the user message", messages[1])
		}
	})

	t.Run("tool_use and tool_result blocks flatten to text", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"messages": []any{
				map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": "Let me check."},
						map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "Izmir"}},
					},
				},
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "25C"},
					},
				},
			},
		})
		messages, toolMode, apiErr := shapeClaudeRequest(body, false)
		if apiErr != nil {
			t.Fatalf("shapeClaudeRequest: %+v", apiErr)
		}
		if toolMode {
			t.Fatal("no tools array, toolMode must stay false")
		}
		if len(messages) != 2 {
			t.Fatalf("messages = %+v, want the two flattened messages", messages)
		}
		assistant := messages[0]
		if assistant.Role != "assistant" || !strings.HasPrefix(assistant.Content, "Let me check.") ||
			!strings.Contains(assistant.Content, "<tool_calls>") ||
			!strings.Contains(assistant.Content, "<tool_name>get_weather</tool_name>") ||
			!strings.Contains(assistant.Content, `{"city":"Izmir"}`) ||
			!strings.Contains(assistant.Content, "</tool_calls>") {
			t.Fatalf("messages[0] = %+v, want the prose plus the reference's tool_use XML text block", assistant)
		}
		if messages[1].Role != "user" || messages[1].Content != "Tool result toolu_1: 25C" {
			t.Fatalf("messages[1] = %+v, want the reference's tool_result text", messages[1])
		}
	})

	t.Run("list system joins its text blocks and image parts contribute nothing", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"system": []any{
				map[string]any{"type": "text", "text": "First."},
				map[string]any{"type": "text", "text": "Second."},
			},
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
						map[string]any{"type": "text", "text": "Look."},
					},
				},
			},
		})
		messages, _, apiErr := shapeClaudeRequest(body, false)
		if apiErr != nil {
			t.Fatalf("shapeClaudeRequest: %+v", apiErr)
		}
		if len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != "First.Second." {
			t.Fatalf("messages[:1] = %+v, want the list system joined without separators", messages)
		}
		if messages[1].Content != "Look." {
			t.Fatalf("messages[1] = %+v, want only the text part (the image has no text-path surface)", messages[1])
		}
	})

	t.Run("Claude Code system swaps the schema prompt for the XML rule", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"system":   "You are Claude Code, Anthropic's official CLI.",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools": []any{
				map[string]any{"name": "get_weather", "description": "Weather", "input_schema": map[string]any{"type": "object"}},
			},
		})
		messages, toolMode, apiErr := shapeClaudeRequest(body, false)
		if apiErr != nil || !toolMode {
			t.Fatalf("apiErr = %+v toolMode = %v, want the emulated tool mode", apiErr, toolMode)
		}
		if !strings.Contains(messages[0].Content, "Tool output adapter: when calling tools") {
			t.Fatalf("messages[0] = %+v, want the single-line XML rule for Claude Code clients", messages[0])
		}
		if strings.Contains(messages[0].Content, "Available tools:") {
			t.Fatalf("messages[0] = %+v, the full schema prompt must be replaced for Claude Code", messages[0])
		}
	})

	t.Run("Claude Code system without tools still gets the XML rule", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"system":   "You are Claude Code.",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		messages, toolMode, apiErr := shapeClaudeRequest(body, false)
		if apiErr != nil {
			t.Fatalf("shapeClaudeRequest: %+v", apiErr)
		}
		if toolMode {
			t.Fatal("no tools array, toolMode must stay false")
		}
		if !strings.Contains(messages[0].Content, "Tool output adapter: when calling tools") {
			t.Fatalf("messages[0] = %+v, want the XML rule appended for Claude Code clients", messages[0])
		}
	})

	t.Run("nameless tools are refused, empty messages refused", func(t *testing.T) {
		_, _, apiErr := shapeClaudeRequest(responsesJSON(t, map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":    []any{map[string]any{"type": "web_search_20250305"}},
		}), false)
		if apiErr == nil || apiErr.Code != "tools_unsupported" || apiErr.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("apiErr = %+v, want typed 400 tools_unsupported", apiErr)
		}
		_, _, apiErr = shapeClaudeRequest(responsesJSON(t, map[string]any{
			"system": "hi",
		}), false)
		if apiErr == nil || apiErr.Code != "invalid_request" || apiErr.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("apiErr = %+v, want typed 400 invalid_request for the missing messages", apiErr)
		}
	})
}

// ---------------------------------------------------------------------------
// TestClaudeNonStream — scripted upstream reply → the message envelope:
// id msg_<uuid4()>, type "message", role assistant, the model echoed, the
// text content block, stop_reason end_turn, stop_sequence null, and the
// documented bytes/4 usage estimate.
// ---------------------------------------------------------------------------

func TestClaudeNonStream(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"system":   "Be brief.",
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Payload, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	if !claudeMsgIDRe.MatchString(asString(body["id"])) {
		t.Fatalf("id = %q, want msg_<uuid4()>", body["id"])
	}
	if body["type"] != "message" || body["role"] != "assistant" {
		t.Fatalf("type/role = %v/%v, want message/assistant", body["type"], body["role"])
	}
	if body["model"] != "chatgpt-proxy/openai/gpt-5" {
		t.Fatalf("model = %v, want the requested published id echoed", body["model"])
	}
	if body["stop_reason"] != "end_turn" || body["stop_sequence"] != nil {
		t.Fatalf("stop_reason/stop_sequence = %v/%v, want end_turn/null", body["stop_reason"], body["stop_sequence"])
	}

	content := body["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content has %d blocks, want one text block", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello world!" {
		t.Fatalf("content[0] = %+v, want the text block with the assembled reply", block)
	}

	// Documented bytes/4 estimate: system "Be brief."=9→2,
	// "Say hello"=9→2 → input 4; "Hello world!"=12→3.
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %+v, want {input_tokens:4 output_tokens:3}", usage)
	}

	// The upstream conversation body carries the system message.
	record := fake.conversationStart()
	if record.method == "" {
		t.Fatalf("the conversation stream never started")
	}
	var wire struct {
		Body []byte `json:"body"`
	}
	if err := json.Unmarshal(record.request, &wire); err != nil {
		t.Fatalf("decode do_stream request: %s", record.request)
	}
	if !strings.Contains(string(wire.Body), `"role": "system"`) {
		t.Fatalf("conversation body missing the system message:\n%s", wire.Body)
	}
}

// ---------------------------------------------------------------------------
// TestClaudeToolRoundTrip — a tools request whose reply carries the
// reference's <tool_calls> XML parses back into tool_use content blocks
// with fresh toolu_<uuid4()> ids and stop_reason tool_use (the prose keeps
// the markup-stripped remainder); an all-markup reply has no text block.
// ---------------------------------------------------------------------------

func TestClaudeToolRoundTrip(t *testing.T) {
	cred := execCredential()
	tools := []any{
		map[string]any{"name": "get_weather", "description": "Weather", "input_schema": map[string]any{"type": "object"}},
	}

	t.Run("prose plus a tool call", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(claudeReplySSE(t, "Checking.\n"+claudeToolXML("get_weather", `{"city":"Izmir"}`)), 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "Weather in Izmir?"}},
				"tools":    tools,
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		var out struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
			t.Fatalf("decode executor response: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Payload, &body); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		if body["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v, want tool_use", body["stop_reason"])
		}
		content := body["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("content has %d blocks, want the prose text plus the tool_use block", len(content))
		}
		if block := content[0].(map[string]any); block["type"] != "text" || block["text"] != "Checking." {
			t.Fatalf("content[0] = %+v, want the markup-stripped prose", block)
		}
		use := content[1].(map[string]any)
		if use["type"] != "tool_use" || use["name"] != "get_weather" {
			t.Fatalf("content[1] = %+v, want the get_weather tool_use block", use)
		}
		if !claudeToolUIDRe.MatchString(asString(use["id"])) {
			t.Fatalf("tool_use id = %q, want toolu_<uuid4()>", use["id"])
		}
		input := use["input"].(map[string]any)
		if input["city"] != "Izmir" {
			t.Fatalf("input = %+v, want the parsed parameters", input)
		}
	})

	t.Run("all-markup reply has no text block", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(claudeReplySSE(t, claudeToolXML("get_weather", `{"city":"Izmir"}`)), 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "Weather?"}},
				"tools":    tools,
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		var out struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
			t.Fatalf("decode executor response: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Payload, &body); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		content := body["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "tool_use" {
			t.Fatalf("content = %+v, want exactly the tool_use block", content)
		}
	})

	t.Run("tool mode parses no calls: end_turn with stripped prose", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(claudeReplySSE(t, "No call needed."), 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools":    tools,
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		var out struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
			t.Fatalf("decode executor response: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Payload, &body); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		if body["stop_reason"] != "end_turn" || len(body["content"].([]any)) != 1 {
			t.Fatalf("stop_reason/content = %v/%+v, want end_turn with the text block", body["stop_reason"], body["content"])
		}
	})
}

// ---------------------------------------------------------------------------
// TestClaudeStreamSequence — buffered-then-emit: the whole upstream turn
// drains, then the reference's Anthropic SSE sequence goes out —
// message_start → content_block_start → content_block_delta* →
// content_block_stop → message_delta (stop_reason + usage) → message_stop —
// each framed "event: <type>\ndata: <json>\n\n", the downstream stream
// closed exactly once, and NO data: [DONE] sentinel (message_stop is the
// Anthropic terminator).
// ---------------------------------------------------------------------------

func TestClaudeStreamSequence(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	result := decodeExecResult(t, resp)
	if strings.TrimSpace(string(result)) != "{}" {
		t.Fatalf("stream response = %s, want an empty result (the turn drains synchronously)", result)
	}

	// message_start, block start, the single buffered text delta, block
	// stop, message_delta, message_stop.
	waitForStreamEnd(t, fake, 6)

	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	wantTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	for i, emit := range emits {
		if emit.streamID != execDownstreamID {
			t.Fatalf("emit[%d] stream_id = %q, want the host-allocated downstream id", i, emit.streamID)
		}
		if got := claudeFrameType(t, emit.payload); got != wantTypes[i] {
			t.Fatalf("emit[%d] type = %s, want %s", i, got, wantTypes[i])
		}
	}

	start := decodeClaudeFrame(t, emits[0].payload)
	message := start["message"].(map[string]any)
	if !claudeMsgIDRe.MatchString(asString(message["id"])) || message["type"] != "message" ||
		message["role"] != "assistant" || message["model"] != "chatgpt-proxy/openai/gpt-5" {
		t.Fatalf("message_start.message = %+v, want the msg_<uuid> assistant envelope", message)
	}
	if usage := message["usage"].(map[string]any); usage["output_tokens"] != float64(0) {
		t.Fatalf("message_start usage = %+v, want output_tokens 0", usage)
	}

	blockStart := decodeClaudeFrame(t, emits[1].payload)
	if blockStart["index"] != float64(0) {
		t.Fatalf("content_block_start index = %v, want 0", blockStart["index"])
	}
	if block := blockStart["content_block"].(map[string]any); block["type"] != "text" || block["text"] != "" {
		t.Fatalf("content_block_start block = %+v, want the empty text block", block)
	}

	// The buffered turn streams the WHOLE assembled text as one delta —
	// the reference's event sequence at completion, not per-chunk deltas.
	delta := decodeClaudeFrame(t, emits[2].payload)
	if delta["index"] != float64(0) {
		t.Fatalf("delta index = %v, want 0", delta["index"])
	}
	if d := delta["delta"].(map[string]any); d["type"] != "text_delta" || d["text"] != "Hello world!" {
		t.Fatalf("delta = %+v, want one text_delta carrying the full assembled text", d)
	}

	msgDelta := decodeClaudeFrame(t, emits[4].payload)
	if d := msgDelta["delta"].(map[string]any); d["stop_reason"] != "end_turn" || d["stop_sequence"] != nil {
		t.Fatalf("message_delta.delta = %+v, want stop_reason end_turn and stop_sequence null", d)
	}
	if usage := msgDelta["usage"].(map[string]any); usage["output_tokens"] != float64(3) {
		t.Fatalf("message_delta usage = %+v, want output_tokens 3 (the documented estimate)", usage)
	}

	if msg := decodeClaudeFrame(t, emits[5].payload)["created"]; msg == nil {
		t.Fatal("message_stop lacks the created field")
	}

	// The stream closes exactly once, cleanly — and no frame may be the
	// chat format's [DONE] sentinel.
	if len(closes) != 1 || closes[0].errMsg != "" || closes[0].streamID != execDownstreamID {
		t.Fatalf("closes = %+v, want exactly one clean close", closes)
	}
	for _, emit := range emits {
		if strings.Contains(string(emit.payload), "[DONE]") {
			t.Fatalf("a [DONE] sentinel leaked into the claude stream: %s", emit.payload)
		}
	}
}

// TestClaudeToolStreamSequence — the tool round-trip in stream form: the
// buffered blocks emit a text block then the tool_use block (start with
// empty input, one input_json_delta, stop), and message_delta carries
// stop_reason tool_use.
func TestClaudeToolStreamSequence(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(claudeReplySSE(t, claudeToolXML("get_weather", `{"city":"Izmir"}`)), 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Weather?"}},
			"tools": []any{
				map[string]any{"name": "get_weather", "description": "Weather", "input_schema": map[string]any{"type": "object"}},
			},
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	decodeExecResult(t, resp)

	// The reply is all markup, so there is exactly one content block — the
	// tool_use: message_start, block start, the input_json delta, block
	// stop, message_delta, message_stop.
	waitForStreamEnd(t, fake, 6)

	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	wantTypes := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	for i, emit := range emits {
		if got := claudeFrameType(t, emit.payload); got != wantTypes[i] {
			t.Fatalf("emit[%d] type = %s, want %s", i, got, wantTypes[i])
		}
	}

	// The reply is all markup, so the single block must already be the
	// tool_use (no prose block).
	start := decodeClaudeFrame(t, emits[1].payload)
	if start["index"] != float64(0) {
		t.Fatalf("tool block start index = %v, want 0 (no prose block)", start["index"])
	}
	block := start["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "get_weather" {
		t.Fatalf("tool_use start block = %+v, want the get_weather tool_use block", block)
	}
	if input, _ := block["input"].(map[string]any); len(input) != 0 {
		t.Fatalf("tool_use start input = %+v, want the reference's empty input (filled by the delta)", input)
	}
	if !claudeToolUIDRe.MatchString(asString(block["id"])) {
		t.Fatalf("tool_use id = %q, want toolu_<uuid4()>", block["id"])
	}
	delta := decodeClaudeFrame(t, emits[2].payload)
	if d := delta["delta"].(map[string]any); d["type"] != "input_json_delta" {
		t.Fatalf("tool delta = %+v, want input_json_delta", d)
	}
	partial := asString(delta["delta"].(map[string]any)["partial_json"])
	var params map[string]any
	if err := json.Unmarshal([]byte(partial), &params); err != nil {
		t.Fatalf("partial_json = %q is not JSON: %v", partial, err)
	}
	if params["city"] != "Izmir" {
		t.Fatalf("partial_json params = %+v, want the parsed input", params)
	}

	msgDelta := decodeClaudeFrame(t, emits[4].payload)
	if d := msgDelta["delta"].(map[string]any); d["stop_reason"] != "tool_use" {
		t.Fatalf("message_delta stop_reason = %v, want tool_use", d)
	}
	if len(closes) != 1 || closes[0].errMsg != "" {
		t.Fatalf("closes = %+v, want exactly one clean close", closes)
	}
}

// ---------------------------------------------------------------------------
// TestClaudeErrorEnvelope — the plugin answers every typed failure with the
// same typed pluginabi.Error envelope the other formats use: the host's
// Claude handler (the /v1/messages route's WriteErrorResponse override)
// renders it as the Anthropic {"type":"error","error":{...}} body, its
// error type derived from the HTTP status exactly like the reference's
// anthropic_error_response. The test pins the plugin side of that contract.
// ---------------------------------------------------------------------------

func TestClaudeErrorEnvelope(t *testing.T) {
	cred := execCredential()

	t.Run("shape-level rejection is typed before any upstream call", func(t *testing.T) {
		fake := newExecFakeCaller()
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools":    []any{map[string]any{"type": "web_search_20250305"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "tools_unsupported" || got.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("error = %+v, want typed 400 tools_unsupported", got)
		}
		if hits := countExecHostCalls(t, fake, pluginabi.MethodHostHTTPDo); hits != 0 {
			t.Fatalf("host.http.do was called %d times, want zero (the refusal costs no network)", hits)
		}
	})

	t.Run("upstream failure maps to the typed envelope the host renders Anthropic", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.withStreamStart(http.StatusTooManyRequests, map[string][]string{"Retry-After": {"12"}})
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "rate_limited" || got.HTTPStatus != http.StatusTooManyRequests || !got.Retryable {
			t.Fatalf("error = %+v, want retryable 429 rate_limited (renders as the Anthropic rate_limit_error)", got)
		}
	})

	t.Run("zero content is typed 502, never an empty message envelope", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "no_assistant_content" || got.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("error = %+v, want typed 502 no_assistant_content", got)
		}
	})

	t.Run("stream failure closes once with the typed text", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle(executor.execute_stream): %v", err)
		}
		decodeExecResult(t, resp)

		waitForClose(t, fake)
		fake.mu.Lock()
		emits := append([]execStreamEmit(nil), fake.emits...)
		closes := append([]execStreamClose(nil), fake.closes...)
		fake.mu.Unlock()
		for _, emit := range emits {
			if claudeFrameType(t, emit.payload) == "message_delta" || claudeFrameType(t, emit.payload) == "message_stop" {
				t.Fatalf("a terminal frame was emitted on a zero-content stream: %s", emit.payload)
			}
		}
		if len(closes) != 1 || !strings.Contains(closes[0].errMsg, "no_assistant_content") {
			t.Fatalf("closes = %+v, want one close naming no_assistant_content", closes)
		}
	})
}

// ---------------------------------------------------------------------------
// TestClaudeDispatchWiring — the claude tag routes to the claude executor
// and a foreign format still refuses typed unsupported_protocol.
// ---------------------------------------------------------------------------

func TestClaudeDispatchWiring(t *testing.T) {
	cred := execCredential()

	t.Run("claude format is accepted by the dispatcher", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execHappySSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execClaudeRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		var out struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
			t.Fatalf("decode executor response: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Payload, &body); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		if body["type"] != "message" {
			t.Fatalf("payload type = %v, want the claude message envelope", body["type"])
		}
	})

	t.Run("foreign formats still refuse typed", func(t *testing.T) {
		m := execManager(t, newExecFakeCaller(), "")
		for _, format := range []string{"gemini", "codex"} {
			req := execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, false)
			req.Format = format
			req.SourceFormat = format
			resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t, req, ""))
			if err != nil {
				t.Fatalf("Handle(executor.execute): %v", err)
			}
			got := decodeExecError(t, resp)
			if got.Code != "unsupported_protocol" || got.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("format %s answered %+v, want typed 400 unsupported_protocol", format, got)
			}
		}
	})
}
