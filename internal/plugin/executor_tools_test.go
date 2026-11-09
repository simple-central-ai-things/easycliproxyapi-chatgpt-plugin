package plugin

// executor_tools_test.go pins the OpenAI function-tools emulation on the
// text executor: a well-formed tools array is emulated via the reference's
// XML tool contract (prompt injected into the system message, the reply's
// <tool_calls> XML parsed back into OpenAI tool_calls), role:"tool" results
// and prior assistant tool_calls round-trip into the conversation payload,
// and a tools array with no usable function name keeps the typed 400
// tools_unsupported refusal. Same scripted fake host caller as
// executor_test.go.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// execWeatherTool is one well-formed OpenAI function tool.
var execWeatherTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "Get the weather for a city",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		},
	},
}

// execToolsSSE is the fixture stream whose assistant reply is prose plus the
// reference's tool-call XML (the exact <tool_calls>/<tool_call>/
// <tool_name>/<parameters> contract the tool prompt instructs).
const execToolsSSE = `data: v1

data: {"p":"","o":"add","v":{"message":{"id":"m-9","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]},"status":"in_progress"},"conversation_id":"c-9"}}

data: {"p":"/message/content/parts/0","o":"append","v":"I will check.\n<tool_calls>\n<tool_call>\n<tool_name>get_weather</tool_name>\n<parameters>{\"city\":\"Paris\"}</parameters>\n</tool_call>\n</tool_calls>"}

data: [DONE]
`

// execToolsAssembled is the text the fixture assembles (usage estimates are
// pinned against it, as the README documents the bytes/4 heuristic). The
// tool-call tags ride in via the tools.go constants so the exact tokens
// never appear in this source.
const execToolsAssembled = "I will check.\n<tool_calls>\n" + toolCallToken +
	"\n<tool_name>get_weather</tool_name>\n<parameters>{\"city\":\"Paris\"}</parameters>\n" +
	toolCallCloseToken + "\n</tool_calls>"

// toolsCompletion mirrors the chat.completion envelope when the message
// carries tool_calls.
type toolsCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// decodeToolsCompletion decodes the executor result's Payload into the
// tool-aware completion envelope.
func decodeToolsCompletion(t *testing.T, resp []byte) toolsCompletion {
	t.Helper()
	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var completion toolsCompletion
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload is not a chat.completion: %v (%s)", err, out.Payload)
	}
	return completion
}

// conversationBodyText returns the recorded upstream conversation POST body.
func conversationBodyText(t *testing.T, fake *execFakeCaller) string {
	t.Helper()
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
	return string(wire.Body)
}

// assertZeroUpstreamCalls pins the refusal-before-network contract.
func assertZeroUpstreamCalls(t *testing.T, fake *execFakeCaller) {
	t.Helper()
	fake.mu.Lock()
	upstreamCalls := 0
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostHTTPDo || record.method == pluginabi.MethodHostHTTPDoStream {
			upstreamCalls++
		}
	}
	fake.mu.Unlock()
	if upstreamCalls != 0 {
		t.Fatalf("the refusal made %d upstream calls, want zero (refuse before any network)", upstreamCalls)
	}
}

// ---------------------------------------------------------------------------
// TestToolsEmulatedNonStream — a function tool + an XML reply yields
// OpenAI tool_calls, finish_reason "tool_calls" and the stripped prose.
// ---------------------------------------------------------------------------

func TestToolsEmulatedNonStream(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execToolsSSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "Be brief."},
				map[string]any{"role": "user", "content": "What is the weather in Paris?"},
			},
			"tools": []any{execWeatherTool},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	completion := decodeToolsCompletion(t, resp)

	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly one", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if choice.Message.Role != "assistant" {
		t.Fatalf("message.role = %q, want assistant", choice.Message.Role)
	}
	if choice.Message.Content != "I will check." {
		t.Fatalf("content = %q, want the stripped prose (tool markup removed)", choice.Message.Content)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want exactly one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if !callIDRe.MatchString(call.ID) {
		t.Fatalf("tool call id = %q, want call_<uuid>", call.ID)
	}
	if call.Type != "function" {
		t.Fatalf("tool call type = %q, want function", call.Type)
	}
	if call.Function.Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", call.Function.Name)
	}
	if call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the JSON string of the parsed parameters", call.Function.Arguments)
	}
	// The usage estimate covers the RAW assembled text (markup included) —
	// the model really generated it.
	if completion.Usage.CompletionTokens != estimateTokens(execToolsAssembled) {
		t.Fatalf("completion_tokens = %d, want the raw-text estimate %d",
			completion.Usage.CompletionTokens, estimateTokens(execToolsAssembled))
	}

	// The tool prompt was merged into the system message of the upstream
	// conversation body.
	body := conversationBodyText(t, fake)
	for _, want := range []string{
		`"role": "system"`, "Be brief.", "Available tools:", "Tool: get_weather",
		"Description: Get the weather for a city", "Tool use rules:",
		"<tool_calls>", "<tool_name>TOOL_NAME</tool_name>", "<![CDATA[value]]>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("conversation body missing %s:\n%s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// TestToolsNoCallNormal — tools present but the reply is plain prose: a
// normal stop completion, no tool_calls, and the tool prompt was injected
// as a NEW system message when the request had none.
// ---------------------------------------------------------------------------

func TestToolsNoCallNormal(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
			"tools":    []any{execWeatherTool},
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
	if strings.Contains(string(out.Payload), "tool_calls") {
		t.Fatalf("a prose reply must not carry tool_calls: %s", out.Payload)
	}
	var completion chatCompletion
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload is not a chat.completion: %v (%s)", err, out.Payload)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", completion.Choices[0].FinishReason)
	}
	if completion.Choices[0].Message.Content != "Hello world!" {
		t.Fatalf("content = %q, want the plain prose", completion.Choices[0].Message.Content)
	}

	body := conversationBodyText(t, fake)
	if !strings.Contains(body, `"role": "system"`) || !strings.Contains(body, "Available tools:") {
		t.Fatalf("the tool prompt must be injected as a new system message:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// TestToolsStreamEmitsToolCalls — streaming with tools is buffered: one
// role + stripped-prose frame, one finish frame carrying the tool_calls and
// the usage, then [DONE]. Not token-streamed (documented tradeoff).
// ---------------------------------------------------------------------------

func TestToolsStreamEmitsToolCalls(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execToolsSSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "What is the weather in Paris?"}},
			"tools":    []any{execWeatherTool},
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	result := decodeExecResult(t, resp)
	if strings.TrimSpace(string(result)) != "{}" {
		t.Fatalf("async stream response = %s, want an empty result (the pump emits downstream)", result)
	}

	// role + prose, tool_calls finish, [DONE]
	waitForStreamEnd(t, fake, 3)

	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	if string(emits[2].payload) != "data: [DONE]\n\n" {
		t.Fatalf("last emit = %q, want the [DONE] sentinel", emits[2].payload)
	}

	first := decodeToolsFrame(t, emits[0].payload)
	if first.Choices[0].FinishReason != nil {
		t.Fatalf("first frame finish_reason = %v, want null", first.Choices[0].FinishReason)
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first frame delta.role = %q, want assistant", first.Choices[0].Delta.Role)
	}
	if first.Choices[0].Delta.Content != "I will check." {
		t.Fatalf("first frame delta.content = %q, want the stripped prose", first.Choices[0].Delta.Content)
	}

	finishFrame := decodeToolsFrame(t, emits[1].payload)
	if finishFrame.Choices[0].FinishReason == nil || *finishFrame.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish frame finish_reason = %v, want tool_calls", finishFrame.Choices[0].FinishReason)
	}
	toolCalls := finishFrame.Choices[0].Delta.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("finish frame tool_calls = %+v, want exactly one", toolCalls)
	}
	if !callIDRe.MatchString(toolCalls[0].ID) {
		t.Fatalf("finish frame tool call id = %q, want call_<uuid>", toolCalls[0].ID)
	}
	if toolCalls[0].Type != "function" || toolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("finish frame tool call = %+v, want function/get_weather", toolCalls[0])
	}
	if toolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("finish frame arguments = %q, want the JSON string", toolCalls[0].Function.Arguments)
	}
	if finishFrame.Usage == nil || finishFrame.Usage.CompletionTokens != estimateTokens(execToolsAssembled) {
		t.Fatalf("finish frame usage = %+v, want the raw-text estimate", finishFrame.Usage)
	}

	if len(closes) != 1 || closes[0].streamID != execDownstreamID || closes[0].errMsg != "" {
		t.Fatalf("closes = %+v, want exactly one clean close of %s", closes, execDownstreamID)
	}

	// Tool requests are not token-streamed: the whole buffered answer went
	// out as one content frame, never as the fixture's incremental deltas.
	body := conversationBodyText(t, fake)
	if !strings.Contains(body, "Available tools:") {
		t.Fatalf("the tool prompt must reach the upstream even in stream mode:\n%s", body)
	}
}

// toolsFrame mirrors the chat.completion.chunk shape with a tool_calls delta.
type toolsFrame struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeToolsFrame(t *testing.T, payload []byte) toolsFrame {
	t.Helper()
	if !strings.HasPrefix(string(payload), "data: ") || !strings.HasSuffix(string(payload), "\n\n") {
		t.Fatalf("emitted payload is not a data:-framed SSE line: %q", payload)
	}
	var frame toolsFrame
	if err := json.Unmarshal(payload[len("data: "):len(payload)-2], &frame); err != nil {
		t.Fatalf("decode chunk frame: %v (%q)", err, payload)
	}
	return frame
}

// ---------------------------------------------------------------------------
// TestToolResultRoundTrip — a follow-up request carrying the prior assistant
// tool_call message and the role:"tool" result must fold both into the
// upstream conversation (the agent loop continues from there).
// ---------------------------------------------------------------------------

func TestToolResultRoundTrip(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "What is the weather in Paris?"},
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": "{}",
							},
						},
					},
				},
				map[string]any{
					"role":         "tool",
					"tool_call_id": "call_1",
					"content":      "Sunny, 20C",
				},
			},
			"tools": []any{execWeatherTool},
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
	var completion chatCompletion
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload is not a chat.completion: %v (%s)", err, out.Payload)
	}
	if completion.Choices[0].FinishReason != "stop" || completion.Choices[0].Message.Content != "Hello world!" {
		t.Fatalf("choice[0] = %+v, want a normal stop completion of the plain reply", completion.Choices[0])
	}
	if strings.Contains(string(out.Payload), "tool_calls") {
		t.Fatalf("the follow-up prose reply must not carry tool_calls: %s", out.Payload)
	}

	// The upstream conversation body carries both the folded assistant call
	// and the tool result, so the model sees the loop context.
	body := conversationBodyText(t, fake)
	for _, want := range []string{
		"Tool result call_1: Sunny, 20C",
		"<tool_calls>",
		"<tool_name>get_weather</tool_name>",
		"<parameters>{}</parameters>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("conversation body missing the round-tripped %s:\n%s", want, body)
		}
	}
	// The role:"tool" message folds into a user-author message.
	if !strings.Contains(body, `"role": "user"`) {
		t.Fatalf("the tool result must fold into a user author role:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// TestToolsRefusedWhenNoName — a tools array where no tool carries a usable
// function name keeps the honest typed 400 tools_unsupported refusal BEFORE
// any upstream call. A web_search(_preview…) tools array is NOT refused here:
// it routes through the search flow (executor_search_test.go).
// ---------------------------------------------------------------------------

func TestToolsRefusedWhenNoName(t *testing.T) {
	for name, tools := range map[string]any{
		"function without name": []any{map[string]any{"type": "function"}},
		"no tool has a name":    []any{map[string]any{"type": "function"}, map[string]any{"name": ""}},
		"tools is not an array": map[string]any{"name": "get_weather"},
	} {
		t.Run(name, func(t *testing.T) {
			cred := execCredential()
			fake := newExecFakeCaller()
			m := execManager(t, fake, "")

			resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
				execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
					"messages": []any{map[string]any{"role": "user", "content": "hello"}},
					"tools":    tools,
				}, false), ""))
			if err != nil {
				t.Fatalf("Handle(executor.execute): %v", err)
			}
			got := decodeExecError(t, resp)
			if got.Code != "tools_unsupported" || got.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("error = %+v, want typed 400 tools_unsupported", got)
			}
			assertZeroUpstreamCalls(t, fake)
		})
	}
}

// An empty tools array is the absence of tools: the turn proceeds normally.
func TestToolsEmptyArrayProceeds(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
			"tools":    []any{},
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
	if !strings.Contains(string(out.Payload), "Hello world!") {
		t.Fatalf("completion = %s, want the normal answer", out.Payload)
	}
}
