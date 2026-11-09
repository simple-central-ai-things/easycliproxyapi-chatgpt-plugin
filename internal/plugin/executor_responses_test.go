package plugin

// executor_responses_test.go pins the OpenAI Responses API format
// (openai-response, Phase 2 F1): the request decode (instructions → system,
// input string/item-list → conversation messages, function_call /
// function_call_output agent-loop folds, reasoning skip), the non-stream
// response.completed envelope, the stream event sequence (created →
// added → delta* → done → completed, no [DONE] sentinel), the zero-content
// typed 502, the image_generation tool routing to the image pipeline, and
// the function-tools emulation reusing the chat format's XML contract.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

var (
	respIDRe = regexp.MustCompile(`^resp_[0-9a-f]{32}$`)
	msgIDRe  = regexp.MustCompile(`^msg_[0-9a-f]{32}$`)
)

// execResponseRequest builds one SDK-typed executor request tagged the
// openai-response format (both Format and SourceFormat — the host passes
// the client's protocol untranslated).
func execResponseRequest(t *testing.T, cred Credential, model string, payload any, stream bool) pluginapi.ExecutorRequest {
	t.Helper()
	req := execRequest(t, cred, model, payload, stream)
	req.Format = "openai-response"
	req.SourceFormat = "openai-response"
	return req
}

// responsesJSON renders one map as a JSON payload for the shape-level tests.
func responsesJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

// asString reads one decoded-JSON field as a string (non-strings read as "").
func asString(value any) string {
	s, _ := value.(string)
	return s
}

// decodeResponseFrame parses one emitted SSE frame ("data: <json>\n\n") into
// its event object.
func decodeResponseFrame(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	s := string(payload)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("emitted payload is not a data:-framed SSE line: %q", payload)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(s[len("data: "):len(s)-2]), &event); err != nil {
		t.Fatalf("decode response frame: %v (%q)", err, payload)
	}
	return event
}

func responseFrameType(t *testing.T, payload []byte) string {
	t.Helper()
	return asString(decodeResponseFrame(t, payload)["type"])
}

// ---------------------------------------------------------------------------
// TestResponsesInputMapping — the Responses request decode at the shape
// level (no upstream call): instructions → leading system, input string →
// one user message, item lists per the reference's messages_from_input,
// the agent-loop folds, reasoning skip, effort extraction and the tools
// contract.
// ---------------------------------------------------------------------------

func TestResponsesInputMapping(t *testing.T) {
	t.Run("string input with instructions", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"instructions": "Be brief.",
			"input":        "Say hello",
		})
		messages, effort, toolMode, apiErr := shapeResponseRequest(body)
		if apiErr != nil {
			t.Fatalf("shapeResponseRequest: %+v", apiErr)
		}
		if effort != "" || toolMode {
			t.Fatalf("effort = %q toolMode = %v, want plain defaults", effort, toolMode)
		}
		if len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != "Be brief." ||
			messages[1].Role != "user" || messages[1].Content != "Say hello" {
			t.Fatalf("messages = %+v, want [system Be brief.][user Say hello]", messages)
		}
	})

	t.Run("item list with message, function_call and function_call_output", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "What is the weather?"},
				map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "thinking"}}},
				map[string]any{"type": "function_call", "name": "get_weather", "arguments": `{"city":"Izmir"}`, "call_id": "call-1"},
				map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "25C"},
			},
		})
		messages, _, toolMode, apiErr := shapeResponseRequest(body)
		if apiErr != nil {
			t.Fatalf("shapeResponseRequest: %+v", apiErr)
		}
		if toolMode {
			t.Fatal("no tools array, toolMode must stay false")
		}
		if len(messages) != 3 {
			t.Fatalf("messages = %+v, want user question, folded call, folded result (reasoning skipped)", messages)
		}
		if messages[0].Role != "user" || messages[0].Content != "What is the weather?" {
			t.Fatalf("messages[0] = %+v, want the user question", messages[0])
		}
		if messages[1].Role != "assistant" || !strings.Contains(messages[1].Content, "<tool_name>get_weather</tool_name>") ||
			!strings.Contains(messages[1].Content, `{"city":"Izmir"}`) {
			t.Fatalf("messages[1] = %+v, want the assistant message with the folded tool-call XML", messages[1])
		}
		if messages[2].Role != "user" || messages[2].Content != "Tool result call-1: 25C" {
			t.Fatalf("messages[2] = %+v, want the reference's tool_result text", messages[2])
		}
	})

	t.Run("content parts accumulate into one user message", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"input": []any{
				map[string]any{"type": "input_text", "text": "Hello"},
				map[string]any{"type": "output_text", "text": " world"},
			},
		})
		messages, _, _, apiErr := shapeResponseRequest(body)
		if apiErr != nil {
			t.Fatalf("shapeResponseRequest: %+v", apiErr)
		}
		if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != "Hello world" {
			t.Fatalf("messages = %+v, want one user message with the parts joined", messages)
		}
	})

	t.Run("thinking effort from reasoning block and flat spellings", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"reasoning block":  {"input": "hi", "reasoning": map[string]any{"effort": "high"}},
			"thinking_effort":  {"input": "hi", "thinking_effort": "low"},
			"reasoning_effort": {"input": "hi", "reasoning_effort": "medium"},
		} {
			t.Run(name, func(t *testing.T) {
				want := effortFromAny(t, body)
				_, effort, _, apiErr := shapeResponseRequest(responsesJSON(t, body))
				if apiErr != nil {
					t.Fatalf("shapeResponseRequest: %+v", apiErr)
				}
				if effort != want {
					t.Fatalf("effort = %q, want %q (the reference's effort precedence)", effort, want)
				}
			})
		}
	})

	t.Run("empty input is a typed request error", func(t *testing.T) {
		for name, input := range map[string]any{"absent": nil, "blank string": "   "} {
			t.Run(name, func(t *testing.T) {
				_, _, _, apiErr := shapeResponseRequest(responsesJSON(t, map[string]any{"input": input}))
				if apiErr == nil || apiErr.Code != "invalid_request" || apiErr.HTTPStatus != http.StatusBadRequest {
					t.Fatalf("apiErr = %+v, want typed 400 invalid_request", apiErr)
				}
			})
		}
	})

	t.Run("image parts on the text path are refused", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_image", "image_url": map[string]any{"url": "https://example.com/a.png"}},
				}},
			},
		})
		_, _, _, apiErr := shapeResponseRequest(body)
		if apiErr == nil || apiErr.Code != "invalid_request" {
			t.Fatalf("apiErr = %+v, want typed invalid_request (image inputs need the image_generation path)", apiErr)
		}
	})

	t.Run("function tools are emulated, nameless tools refused", func(t *testing.T) {
		body := responsesJSON(t, map[string]any{
			"input": "hi",
			"tools": []any{
				map[string]any{"type": "function", "name": "get_weather", "description": "Weather", "parameters": map[string]any{"type": "object"}},
			},
		})
		messages, _, toolMode, apiErr := shapeResponseRequest(body)
		if apiErr != nil || !toolMode {
			t.Fatalf("apiErr = %+v toolMode = %v, want the emulated tool mode", apiErr, toolMode)
		}
		if messages[0].Role != "system" || !strings.Contains(messages[0].Content, "Tool: get_weather") {
			t.Fatalf("messages[0] = %+v, want the tool prompt merged into the system message", messages[0])
		}

		webSearch := responsesJSON(t, map[string]any{
			"input": "hi",
			"tools": []any{map[string]any{"type": "web_search_preview"}},
		})
		_, _, _, apiErr = shapeResponseRequest(webSearch)
		if apiErr == nil || apiErr.Code != "tools_unsupported" || apiErr.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("apiErr = %+v, want typed 400 tools_unsupported", apiErr)
		}
	})
}

// effortFromAny reads the test body's expected effort value (the map keys
// differ per spelling, so the expected value is read positionally in the
// test table).
func effortFromAny(t *testing.T, body map[string]any) string {
	t.Helper()
	if block, ok := body["reasoning"].(map[string]any); ok {
		return asString(block["effort"])
	}
	if s, ok := body["thinking_effort"].(string); ok {
		return s
	}
	return asString(body["reasoning_effort"])
}

// ---------------------------------------------------------------------------
// TestResponsesNonStream — scripted upstream reply → the response.completed
// envelope object (what the reference's collect_response returns): id
// resp_<uuid hex>, object "response", status "completed", the assistant text
// in an output message item, the documented usage estimate in the
// reference's token_usage shape, and the instructions riding upstream as
// the system message.
// ---------------------------------------------------------------------------

func TestResponsesNonStream(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"instructions": "Be brief.",
			"input":        "Say hello",
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
	if !respIDRe.MatchString(asString(body["id"])) {
		t.Fatalf("id = %q, want resp_<uuid hex>", body["id"])
	}
	if body["object"] != "response" || body["status"] != "completed" {
		t.Fatalf("object/status = %v/%v, want response/completed", body["object"], body["status"])
	}
	if body["error"] != nil || body["incomplete_details"] != nil {
		t.Fatalf("error/incomplete_details must be null: %v/%v", body["error"], body["incomplete_details"])
	}
	if body["model"] != "chatgpt-proxy/openai/gpt-5" {
		t.Fatalf("model = %v, want the requested published id echoed", body["model"])
	}
	if createdAt, _ := body["created_at"].(float64); createdAt <= 0 {
		t.Fatalf("created_at = %v, want a unix timestamp", body["created_at"])
	}

	output := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output has %d items, want one message item", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" || item["status"] != "completed" {
		t.Fatalf("output[0] = %+v, want the completed assistant message item", item)
	}
	if !msgIDRe.MatchString(asString(item["id"])) {
		t.Fatalf("item id = %v, want msg_<uuid hex>", item["id"])
	}
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello world!" {
		t.Fatalf("content[0] = %+v, want the output_text part with the assembled text", part)
	}

	// Documented estimate in the reference's token_usage shape: instructions
	// "Be brief."=9→2, input "Say hello"=9→2, output "Hello world!"=12→3.
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(3) || usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %+v, want {4 3 7} in the token_usage shape", usage)
	}
	details := usage["output_tokens_details"].(map[string]any)
	if details["text_tokens"] != float64(3) {
		t.Fatalf("output_tokens_details = %+v, want text_tokens 3", details)
	}

	// The upstream conversation body carries the instructions as the system
	// message.
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
// TestResponsesStreamSequence — the SSE event sequence in the reference's
// order: response.created → output_item.added (in_progress) →
// output_text.delta* → output_text.done → output_item.done →
// response.completed (with usage), the downstream stream closed exactly
// once and cleanly, and NO data: [DONE] sentinel (response.completed is the
// Responses API terminator).
// ---------------------------------------------------------------------------

func TestResponsesStreamSequence(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"input": "Say hello",
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	result := decodeExecResult(t, resp)
	if strings.TrimSpace(string(result)) != "{}" {
		t.Fatalf("async stream response = %s, want an empty result (the pump emits downstream)", result)
	}

	// created, added, "Hello", " world", "!", done, item done, completed
	waitForStreamEnd(t, fake, 8)

	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	var respID, itemID string
	for i, emit := range emits {
		if emit.streamID != execDownstreamID {
			t.Fatalf("emit[%d] stream_id = %q, want the host-allocated downstream id", i, emit.streamID)
		}
		event := decodeResponseFrame(t, emit.payload)
		want := wantTypes[i]
		if asString(event["type"]) != want {
			t.Fatalf("emit[%d] type = %v, want %s", i, event["type"], want)
		}
		switch want {
		case "response.created":
			r := event["response"].(map[string]any)
			if !respIDRe.MatchString(asString(r["id"])) || r["status"] != "in_progress" {
				t.Fatalf("created response = %+v, want resp_<hex> in_progress", r)
			}
			respID = asString(r["id"])
		case "response.output_item.added":
			r := event["response"]
			if r != nil {
				t.Fatalf("output_item.added carries an unexpected response field: %+v", event)
			}
			if event["output_index"] != float64(0) {
				t.Fatalf("output_index = %v, want 0", event["output_index"])
			}
			rItem := event["item"].(map[string]any)
			if rItem["status"] != "in_progress" || rItem["type"] != "message" {
				t.Fatalf("added item = %+v, want the in_progress message item", rItem)
			}
			itemID = asString(rItem["id"])
			if !msgIDRe.MatchString(itemID) {
				t.Fatalf("added item id = %q, want msg_<uuid hex>", itemID)
			}
		case "response.output_text.delta":
			if event["item_id"] != itemID || event["output_index"] != float64(0) || event["content_index"] != float64(0) {
				t.Fatalf("delta addressing = %+v, want item %q at 0/0", event, itemID)
			}
		case "response.output_text.done":
			if event["item_id"] != itemID || event["text"] != "Hello world!" {
				t.Fatalf("done = %+v, want the full text under the item id", event)
			}
		case "response.output_item.done":
			doneItem := event["item"].(map[string]any)
			if doneItem["status"] != "completed" {
				t.Fatalf("done item = %+v, want status completed", doneItem)
			}
		case "response.completed":
			r := event["response"].(map[string]any)
			if r["id"] != respID || r["status"] != "completed" {
				t.Fatalf("completed response = %+v, want the created turn completed", r)
			}
			if r["usage"] == nil {
				t.Fatalf("completed response lacks usage: %+v", r)
			}
			rOutput := r["output"].([]any)
			if len(rOutput) != 1 {
				t.Fatalf("completed output has %d items, want one", len(rOutput))
			}
		}
	}
	// The deltas carry the assembled text in order.
	wantDeltas := []string{"Hello", " world", "!"}
	for i, want := range wantDeltas {
		if event := decodeResponseFrame(t, emits[2+i].payload); event["delta"] != want {
			t.Fatalf("delta[%d] = %v, want %q", i, event["delta"], want)
		}
	}

	// The stream closes exactly once, cleanly — and no frame may be the
	// chat format's [DONE] sentinel.
	if len(closes) != 1 || closes[0].errMsg != "" || closes[0].streamID != execDownstreamID {
		t.Fatalf("closes = %+v, want exactly one clean close", closes)
	}
	for _, emit := range emits {
		if strings.Contains(string(emit.payload), "[DONE]") {
			t.Fatalf("a [DONE] sentinel leaked into the responses stream: %s", emit.payload)
		}
	}
}

// ---------------------------------------------------------------------------
// TestResponsesZeroContent — a completed turn with no visible assistant
// content is a typed 502 no_assistant_content (never an empty success),
// both stream and non-stream (matching the openai format's contract).
// ---------------------------------------------------------------------------

func TestResponsesZeroContent(t *testing.T) {
	cred := execCredential()

	t.Run("non-stream", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"input": "hello",
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "no_assistant_content" || got.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("error = %+v, want typed 502 no_assistant_content", got)
		}
	})

	t.Run("stream", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"input": "hello",
			}, true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle(executor.execute_stream): %v", err)
		}
		decodeExecResult(t, resp)

		// The pump ends with exactly one typed close; no completed event.
		waitForClose(t, fake)
		fake.mu.Lock()
		emits := append([]execStreamEmit(nil), fake.emits...)
		closes := append([]execStreamClose(nil), fake.closes...)
		fake.mu.Unlock()
		for _, emit := range emits {
			if type_ := responseFrameType(t, emit.payload); type_ == "response.completed" || type_ == "response.output_item.done" {
				t.Fatalf("a %s was emitted on a zero-content stream: %s", type_, emit.payload)
			}
		}
		if len(closes) != 1 || !strings.Contains(closes[0].errMsg, "no_assistant_content") {
			t.Fatalf("closes = %+v, want one close naming no_assistant_content", closes)
		}
	})
}

// ---------------------------------------------------------------------------
// TestResponsesImageTool — an image_generation tool routes the request
// through the image pipeline (the prompt extracted per the reference, the
// tool's size/quality riding through, the text model defaulting to the
// drawing route) and answers with image_generation_call output items
// carrying the base64 result; streaming emits the buffered event sequence.
// ---------------------------------------------------------------------------

// stubImagePipeline swaps the pipeline entry points with canned results —
// the executor_image_test.go seam.
type stubImagePipeline struct {
	prompt        string
	size          string
	quality       string
	refs          []upstream.ReferenceImage
	generateCalls int
	codexCalls    int
}

func (s *stubImagePipeline) install(t *testing.T) {
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
		return upstream.SentinelTokens{}, nil
	}
	imageGenerate = func(_ context.Context, _ upstream.Upstream, _ Credential, _ upstream.SentinelTokens, _ Config,
		prompt string, references []upstream.ReferenceImage, size, quality string, _ func(step string)) (upstream.ImageResult, error) {
		s.generateCalls++
		s.prompt = prompt
		s.refs = references
		s.size = size
		s.quality = quality
		return upstream.ImageResult{
			Images:        []upstream.ImageOutput{{Data: solidPNG()}},
			RevisedPrompt: "a red square",
		}, nil
	}
	imageGenerateCodex = func(_ context.Context, _ upstream.Upstream, _ Credential, _ upstream.Profile,
		prompt string, references []upstream.ReferenceImage, size, quality string) (upstream.ImageResult, error) {
		s.codexCalls++
		return upstream.ImageResult{}, errors.New("codex route must not run for the responses image test")
	}
}

// waitForClose waits until the downstream stream has been closed exactly
// once (the pump/emitter is asynchronous; emits are intentionally not
// constrained here, the zero-content path may carry header frames).
func waitForClose(t *testing.T, fake *execFakeCaller) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, closes := fake.downstreamCounts()
		if closes == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream did not end: closes=%d want=1", closes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResponsesImageToolNonStream(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	stub := &stubImagePipeline{}
	stub.install(t)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "Draw a red square"},
					map[string]any{"type": "input_image", "image_url": map[string]any{
						"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(solidPNG()),
					}},
				}},
			},
			"tools": []any{
				map[string]any{"type": "image_generation", "size": "1024x1024", "quality": "high"},
			},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	if stub.generateCalls != 1 || stub.codexCalls != 0 {
		t.Fatalf("generate = %d codex = %d, want the drawing route exactly once", stub.generateCalls, stub.codexCalls)
	}
	if stub.prompt != "Draw a red square" {
		t.Fatalf("prompt = %q, want the reference's extract_response_prompt text", stub.prompt)
	}
	if stub.size != "1024x1024" || stub.quality != "high" {
		t.Fatalf("size/quality = %q/%q, want the tool definition values", stub.size, stub.quality)
	}
	if len(stub.refs) != 1 || stub.refs[0].Width != 2 || stub.refs[0].Height != 2 || stub.refs[0].MimeType != "image/png" {
		t.Fatalf("refs = %+v, want the input image decoded with dims", stub.refs)
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
	if body["object"] != "response" || body["status"] != "completed" || !respIDRe.MatchString(asString(body["id"])) {
		t.Fatalf("envelope = %+v, want the completed response envelope", body)
	}
	output := body["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output has %d items, want one image_generation_call", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "image_generation_call" || item["status"] != "completed" || item["id"] != "ig_1" {
		t.Fatalf("output[0] = %+v, want the completed ig_1 image_generation_call", item)
	}
	if item["result"] != base64.StdEncoding.EncodeToString(solidPNG()) {
		t.Fatalf("result = %v, want the base64 image bytes", item["result"])
	}
	if item["revised_prompt"] != "a red square" {
		t.Fatalf("revised_prompt = %v, want the pipeline's revision", item["revised_prompt"])
	}
	if body["usage"] == nil {
		t.Fatalf("the image response carries no usage")
	}
}

func TestResponsesImageToolStream(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	stub := &stubImagePipeline{}
	stub.install(t)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"input": "Draw a red square",
			"tools": []any{map[string]any{"type": "image_generation"}},
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	decodeExecResult(t, resp)

	// created, added, done, completed — the buffered image sequence.
	waitForClose(t, fake)
	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()
	if len(emits) != 4 {
		t.Fatalf("emits = %d, want the 4-event buffered sequence", len(emits))
	}
	wantTypes := []string{"response.created", "response.output_item.added", "response.output_item.done", "response.completed"}
	for i, emit := range emits {
		if got := responseFrameType(t, emit.payload); got != wantTypes[i] {
			t.Fatalf("emit[%d] = %s, want %s", i, got, wantTypes[i])
		}
	}
	if len(closes) != 1 || closes[0].errMsg != "" {
		t.Fatalf("closes = %+v, want exactly one clean close", closes)
	}
}

func TestResponsesImageToolMissingPrompt(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	stub := &stubImagePipeline{}
	stub.install(t)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"input": []any{map[string]any{"type": "message", "role": "system", "content": "draw something"}},
			"tools": []any{map[string]any{"type": "image_generation"}},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	got := decodeExecError(t, resp)
	if got.Code != "invalid_request" || got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %+v, want typed 400 invalid_request (the reference's 'input text is required')", got)
	}
	if stub.generateCalls != 0 {
		t.Fatalf("the pipeline ran %d times, want zero before a prompt exists", stub.generateCalls)
	}
}

// ---------------------------------------------------------------------------
// TestResponsesFunctionTools — function tools reuse the chat format's XML
// emulation: a tool-call reply parses into function_call output items
// (fresh call_<uuid> ids, JSON-string arguments) and the stream buffers
// the whole turn before emitting the event sequence.
// ---------------------------------------------------------------------------

func TestResponsesFunctionTools(t *testing.T) {
	cred := execCredential()
	tool := map[string]any{
		"type":        "function",
		"name":        "get_weather",
		"description": "Get the weather",
		"parameters":  map[string]any{"type": "object"},
	}

	t.Run("non-stream reply carries a function_call item", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execToolsSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"input": "What is the weather in Paris?",
				"tools": []any{tool},
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
		output := body["output"].([]any)
		var messageItem, callItem map[string]any
		for _, raw := range output {
			item := raw.(map[string]any)
			if item["type"] == "message" {
				messageItem = item
			}
			if item["type"] == "function_call" {
				callItem = item
			}
		}
		if callItem == nil {
			t.Fatalf("output = %+v, want a function_call item", output)
		}
		if callItem["name"] != "get_weather" || callItem["arguments"] != `{"city":"Paris"}` {
			t.Fatalf("function_call = %+v, want the parsed name and JSON-string arguments", callItem)
		}
		if id := asString(callItem["call_id"]); !strings.HasPrefix(id, "call_") {
			t.Fatalf("call_id = %q, want a fresh call_<uuid>", id)
		}
		if callItem["status"] != "completed" {
			t.Fatalf("function_call status = %v, want completed", callItem["status"])
		}
		if messageItem == nil {
			t.Fatalf("output = %+v, want the stripped prose kept as the message item", output)
		}
		content := messageItem["content"].([]any)
		if got := asString(content[0].(map[string]any)["text"]); got != "I will check." {
			t.Fatalf("message text = %q, want the markup-stripped prose", got)
		}
	})

	t.Run("stream buffers then emits the item sequence", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execToolsSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execResponseRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"input": "What is the weather in Paris?",
				"tools": []any{tool},
			}, true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle(executor.execute_stream): %v", err)
		}
		decodeExecResult(t, resp)

		// created, message added+done, call added+done, completed.
		waitForClose(t, fake)
		fake.mu.Lock()
		emits := append([]execStreamEmit(nil), fake.emits...)
		closes := append([]execStreamClose(nil), fake.closes...)
		fake.mu.Unlock()

		wantTypes := []string{
			"response.created",
			"response.output_item.added",
			"response.output_item.done",
			"response.output_item.added",
			"response.output_item.done",
			"response.completed",
		}
		if len(emits) != len(wantTypes) {
			t.Fatalf("emits = %d, want %d buffered frames", len(emits), len(wantTypes))
		}
		for i, emit := range emits {
			event := decodeResponseFrame(t, emit.payload)
			if got := asString(event["type"]); got != wantTypes[i] {
				t.Fatalf("emit[%d] = %s, want %s", i, got, wantTypes[i])
			}
			if i == 1 || i == 3 {
				item := event["item"].(map[string]any)
				if item["type"] != "function_call" && i == 3 {
					t.Fatalf("added item[%d] = %+v, want the function_call item", i, item)
				}
			}
			if i == len(wantTypes)-1 {
				r := event["response"].(map[string]any)
				if r["status"] != "completed" || r["usage"] == nil {
					t.Fatalf("completed = %+v, want status completed with usage", r)
				}
			}
		}
		if len(closes) != 1 || closes[0].errMsg != "" {
			t.Fatalf("closes = %+v, want exactly one clean close", closes)
		}
	})
}
