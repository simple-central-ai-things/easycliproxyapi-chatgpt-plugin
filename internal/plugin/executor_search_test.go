package plugin

// executor_search_test.go pins the Feature F3 web-search executor contract:
// a request that asks for search (a web_search(_preview…) tool with no other
// tools, a web_search tool_choice, a web_search_options block, or a search
// model prefix) routes through the upstream SEARCH flow — conduit prepare
// (system_hints ["search"]), the f/conversation SSE with force_use_search,
// then the conversation-document poll — instead of the plain conversation
// pipeline, and returns the cited answer (Sources block + url_citation
// annotations) in the normal envelope for the format. A normal request and a
// mixed tools array (a search tool alongside a function tool) must NOT route
// through search.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// The pinned upstream search URLs (same routes as the drawing flow, but the
// executor must issue them, not the plain conversation route).
const (
	searchPrepareURL = "https://chatgpt.com/backend-api/f/conversation/prepare"
	searchSSEURL     = "https://chatgpt.com/backend-api/f/conversation"
	searchPollURL    = "https://chatgpt.com/backend-api/conversation/" + execSearchConversation
)

const execSearchConversation = "c-search-1"

// execSearchSSE is the fixture stream that carries the conversation id and a
// single visible assistant add, then [DONE] (search start leg).
const execSearchSSE = `data: {"p":"","o":"add","v":{"message":{"id":"m-search-1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]},"conversation_id":"c-search-1"}}

data: [DONE]
`

// execSearchPollDoc is one settled conversation document: the assistant
// answer with an inline URL plus a metadata sources block and the finished
// status (search wait leg).
const execSearchPollDoc = `{
  "mapping": {
    "msg-b": {
      "message": {
        "id": "msg-b",
        "author": {"role": "assistant"},
        "create_time": 200.5,
        "content": {"content_type": "text", "parts": ["The answer cites https://example.org/b."]},
        "metadata": {
          "finish_details": {"type": "finished_successfully"},
          "sources": [
            {"title": "Example A", "url": "https://example.org/a", "snippet": "Snippet A", "source_type": "web"}
          ]
        }
      }
    }
  }
}`

// scriptSearchFlow wires one full search run against the fake caller for the
// given bearer: the sentinel handshake, the conduit prepare, the SSE chunk
// queue and the settled poll document.
func scriptSearchFlow(t *testing.T, fake *execFakeCaller, bearer string) {
	t.Helper()
	fake.doScripts = happySentinelScripts(bearer)
	fake.doScripts[fake.scriptKey(searchPrepareURL, bearer)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"conduit_token": "conduit-search-1"}`),
	}
	fake.setChunks(chunkedSSE(execSearchSSE, 7))
	fake.doScripts[fake.scriptKey(searchPollURL, bearer)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(execSearchPollDoc),
	}
}

// searchToolPayload is one chat payload carrying a web_search tool.
func searchToolPayload(toolType string) map[string]any {
	return map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "what is this?"}},
		"tools":    []any{map[string]any{"type": toolType}},
	}
}

// assertSearchUpstreamIssued pins that the search flow — and ONLY the search
// flow — touched the wire: the conduit prepare and the f/conversation SSE
// were each issued once, the poll document fetched, and the plain
// conversation route (POST /backend-api/conversation) never started.
func assertSearchUpstreamIssued(t *testing.T, fake *execFakeCaller) {
	t.Helper()
	fake.mu.Lock()
	prepares, sse, polls, plainConversation := 0, 0, 0, 0
	for _, record := range fake.calls {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(record.request, &req)
		switch {
		case record.method == pluginabi.MethodHostHTTPDo && req.URL == searchPrepareURL:
			prepares++
		case record.method == pluginabi.MethodHostHTTPDoStream && req.URL == searchSSEURL:
			sse++
		case record.method == pluginabi.MethodHostHTTPDo && req.URL == searchPollURL:
			polls++
		case req.URL == execConversationURL:
			plainConversation++
		}
	}
	fake.mu.Unlock()
	if prepares != 1 {
		t.Fatalf("search prepare issued %d times, want 1", prepares)
	}
	if sse != 1 {
		t.Fatalf("search SSE issued %d times, want 1", sse)
	}
	if polls != 1 {
		t.Fatalf("search poll issued %d times, want 1", polls)
	}
	if plainConversation != 0 {
		t.Fatalf("the plain conversation route was touched %d times, want 0 (search must not reuse it)", plainConversation)
	}
}

// searchCompletion mirrors the chat.completion envelope with the cited
// annotations the search path attaches to the assistant message.
type searchCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role        string `json:"role"`
			Content     string `json:"content"`
			Annotations []struct {
				Type        string `json:"type"`
				URLCitation struct {
					StartIndex int    `json:"start_index"`
					EndIndex   int    `json:"end_index"`
					URL        string `json:"url"`
					Title      string `json:"title"`
				} `json:"url_citation"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeSearchCompletion(t *testing.T, resp []byte) searchCompletion {
	t.Helper()
	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var completion searchCompletion
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload is not a chat.completion: %v (%s)", err, out.Payload)
	}
	return completion
}

// assertCitedAnswer pins the reference rendering: the answer text carries the
// Sources block and the url_citation annotations span the listed urls.
func assertCitedAnswer(t *testing.T, completion searchCompletion) {
	t.Helper()
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly one", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	content := choice.Message.Content
	for _, want := range []string{
		"The answer cites https://example.org/b.",
		"Sources:",
		"1. Example A - https://example.org/a",
		"https://example.org/b", // the inline url also surfaces as a listed source
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q:\n%s", want, content)
		}
	}
	annotations := choice.Message.Annotations
	if len(annotations) == 0 {
		t.Fatalf("the search answer carries no url_citation annotations")
	}
	for _, ann := range annotations {
		if ann.Type != "url_citation" {
			t.Fatalf("annotation type = %q, want url_citation", ann.Type)
		}
		if ann.URLCitation.URL == "" {
			t.Fatalf("annotation %q has no url", ann.URLCitation.URL)
		}
		// The cited span must actually point at the url in the content.
		start, end := ann.URLCitation.StartIndex, ann.URLCitation.EndIndex
		if start < 0 || end > len(content) || start > end {
			t.Fatalf("annotation range [%d,%d] out of bounds for content of length %d", start, end, len(content))
		}
		if content[start:end] != ann.URLCitation.URL {
			t.Fatalf("annotation span %q != url %q", content[start:end], ann.URLCitation.URL)
		}
	}
}

// ---------------------------------------------------------------------------
// TestDetectSearchRequest — the detection table: a web_search tool (no other
// tools), a web_search tool_choice, a web_search_options block, and each
// search model prefix all detect; a plain request and a mixed search+function
// tools array do not.
// ---------------------------------------------------------------------------

func TestDetectSearchRequest(t *testing.T) {
	marshal := func(body map[string]any) []byte {
		raw, _ := json.Marshal(body)
		return raw
	}
	base := func(extra map[string]any) map[string]any {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "q"}}}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	cases := []struct {
		name string
		body map[string]any
		slug string
		want bool
	}{
		{"web_search tool", base(map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}), "gpt-5", true},
		{"web_search_preview tool", base(map[string]any{"tools": []any{map[string]any{"type": "web_search_preview"}}}), "gpt-5", true},
		{"web_search_preview dated tool", base(map[string]any{"tools": []any{map[string]any{"type": "web_search_preview_2025_03_11"}}}), "gpt-5", true},
		{"web_search tool_choice", base(map[string]any{"tool_choice": map[string]any{"type": "web_search"}}), "gpt-5", true},
		{"web_search_options block", base(map[string]any{"web_search_options": map[string]any{"search_context_size": "medium"}}), "gpt-5", true},
		{"search model prefix exact", base(nil), "gpt-4o-search-preview", true},
		{"search model prefix mini", base(nil), "gpt-4o-mini-search-preview", true},
		{"search model prefix api dash", base(nil), "gpt-5-search-api-2025-03-11", true},
		{"plain request", base(nil), "gpt-5", false},
		{"search prefix substring not match", base(nil), "gpt-4o-search-previewx", false},
		{"function tool only", base(map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}}), "gpt-5", false},
		{"mixed search and function tool", base(map[string]any{"tools": []any{
			map[string]any{"type": "web_search_preview"},
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		}}), "gpt-5", false},
		{"empty tools array", base(map[string]any{"tools": []any{}}), "gpt-5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWebSearchRequest(marshal(tc.body), tc.slug); got != tc.want {
				t.Fatalf("isWebSearchRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSearchToolRoutesThroughSearch — a web_search tool routes through the
// search flow and returns the cited answer in the chat envelope.
// ---------------------------------------------------------------------------

func TestSearchToolRoutesThroughSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	scriptSearchFlow(t, fake, bearer)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", searchToolPayload("web_search_preview"), false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	completion := decodeSearchCompletion(t, resp)
	assertSearchUpstreamIssued(t, fake)
	assertCitedAnswer(t, completion)
	if completion.Model != "chatgpt-proxy/openai/gpt-5" {
		t.Fatalf("model = %q, want the requested published id echoed", completion.Model)
	}
}

// TestSearchModelPrefixRoutesThroughSearch — a search model prefix (no tools)
// routes through the search flow likewise.
func TestSearchModelPrefixRoutesThroughSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	scriptSearchFlow(t, fake, bearer)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5-search-api", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "what is this?"}},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	completion := decodeSearchCompletion(t, resp)
	assertSearchUpstreamIssued(t, fake)
	assertCitedAnswer(t, completion)
}

// TestNormalRequestDoesNotRouteSearch — a plain text request (no search
// signal) must run the ordinary conversation pipeline and never touch the
// search route.
func TestNormalRequestDoesNotRouteSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts(bearer)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	decodeExecResult(t, resp)

	// The plain conversation stream started; the search route was never
	// issued.
	record := fake.conversationStart()
	if record.method == "" {
		t.Fatalf("the plain conversation stream never started")
	}
	fake.mu.Lock()
	searchCalls := 0
	for _, call := range fake.calls {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(call.request, &req)
		if req.URL == searchPrepareURL || req.URL == searchPollURL {
			searchCalls++
		}
	}
	fake.mu.Unlock()
	if searchCalls != 0 {
		t.Fatalf("the search route was touched %d times, want 0 for a plain request", searchCalls)
	}
}

// TestMixedToolsDoNotRouteSearch — a web_search tool alongside a function
// tool is a mixed request: the search gate fails (a non-search tool is
// present), so it falls back to the ordinary emulated-tools pipeline and must
// not issue the search route.
func TestMixedToolsDoNotRouteSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts(bearer)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "q"}},
			"tools": []any{
				map[string]any{"type": "web_search_preview"},
				map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{}}},
			},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	decodeExecResult(t, resp)

	if fake.conversationStart().method == "" {
		t.Fatalf("the mixed request must run the plain conversation pipeline")
	}
	fake.mu.Lock()
	searchCalls := 0
	for _, call := range fake.calls {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(call.request, &req)
		if req.URL == searchPrepareURL || req.URL == searchPollURL {
			searchCalls++
		}
	}
	fake.mu.Unlock()
	if searchCalls != 0 {
		t.Fatalf("the search route was touched %d times for a mixed tools array, want 0", searchCalls)
	}
}

// ---------------------------------------------------------------------------
// TestSearchStreamEmitsCitedAnswer — the streaming search turn buffers the
// whole result and emits it as chat.completion.chunk frames (role/content,
// terminal, [DONE]) and closes exactly once.
// ---------------------------------------------------------------------------

func TestSearchStreamEmitsCitedAnswer(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	scriptSearchFlow(t, fake, bearer)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", searchToolPayload("web_search"), true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	decodeExecResult(t, resp)
	assertSearchUpstreamIssued(t, fake)

	// role/content + terminal + [DONE]
	waitForStreamEnd(t, fake, 3)
	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	contentFrame := decodeChunkFrame(t, emits[0].payload)
	if contentFrame.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first frame role = %q, want assistant", contentFrame.Choices[0].Delta.Role)
	}
	if !strings.Contains(contentFrame.Choices[0].Delta.Content, "Sources:") {
		t.Fatalf("first frame content = %q, want the cited answer with the Sources block", contentFrame.Choices[0].Delta.Content)
	}
	terminal := decodeChunkFrame(t, emits[1].payload)
	if terminal.Choices[0].FinishReason == nil || *terminal.Choices[0].FinishReason != "stop" {
		t.Fatalf("terminal finish_reason = %v, want stop", terminal.Choices[0].FinishReason)
	}
	if string(emits[2].payload) != "data: [DONE]\n\n" {
		t.Fatalf("last emit = %q, want the [DONE] sentinel", emits[2].payload)
	}
	if len(closes) != 1 || closes[0].errMsg != "" {
		t.Fatalf("closes = %+v, want exactly one clean close", closes)
	}
}

// ---------------------------------------------------------------------------
// TestSearchRefreshRetryOn401 — a 401 on the sentinel leg forces the internal
// refresh + persist, then the search flow retries once and completes (the
// Review Focus #3 contract applied to the search route).
// ---------------------------------------------------------------------------

func TestSearchRefreshRetryOn401(t *testing.T) {
	cred := execCredential()
	old := "Bearer " + cred.AccessToken
	fresh := "Bearer fresh-access-token"
	fake := newExecFakeCaller()
	for key, resp := range happySentinelScripts(fresh) {
		fake.doScripts[key] = resp
	}
	fake.doScripts[fake.scriptKey(execPrepareURL, old)] = hostHTTPResponse{StatusCode: http.StatusUnauthorized}
	fake.doScripts[fake.scriptKey(execPlanCheckURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`),
	}
	fake.doScripts[fake.scriptKey(execOAuthTokenURL, "")] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"access_token":"fresh-access-token","refresh_token":"fresh-refresh-token","id_token":"fresh-id-token"}`),
	}
	// The search legs run under the refreshed bearer only.
	fake.doScripts[fake.scriptKey(searchPrepareURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"conduit_token": "conduit-search-1"}`),
	}
	fake.setChunks(chunkedSSE(execSearchSSE, 7))
	fake.doScripts[fake.scriptKey(searchPollURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(execSearchPollDoc),
	}
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", searchToolPayload("web_search_preview"), false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	completion := decodeSearchCompletion(t, resp)
	assertCitedAnswer(t, completion)

	if hits := fake.doHit(fake.scriptKey(execOAuthTokenURL, "")); hits != 1 {
		t.Fatalf("the token endpoint was hit %d times, want exactly one refresh", hits)
	}
	// The persisted rotation happened before the search retry leg.
	fake.mu.Lock()
	saveIdx, searchPrepareIdx := -1, -1
	for i, record := range fake.calls {
		switch {
		case record.method == pluginabi.MethodHostAuthSave:
			saveIdx = i
		case record.method == pluginabi.MethodHostHTTPDo:
			var req struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(record.request, &req)
			if req.URL == searchPrepareURL {
				searchPrepareIdx = i
			}
		}
	}
	fake.mu.Unlock()
	if saveIdx < 0 || searchPrepareIdx < 0 || saveIdx > searchPrepareIdx {
		t.Fatalf("auth.save / search-prepare order wrong: save=%d prepare=%d, want save first", saveIdx, searchPrepareIdx)
	}
}

// TestSearchRefreshRetryOnSearchLeg401 — a 401 on the search legs (the
// conduit prepare, reached with a valid sentinel bundle) also forces the
// refresh + persist + one retry, not only a sentinel-leg 401.
func TestSearchRefreshRetryOnSearchLeg401(t *testing.T) {
	cred := execCredential()
	old := "Bearer " + cred.AccessToken
	fresh := "Bearer fresh-access-token"
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts(old)
	for key, resp := range happySentinelScripts(fresh) {
		fake.doScripts[key] = resp
	}
	// First attempt: sentinel OK (old bearer), search prepare 401.
	fake.doScripts[fake.scriptKey(searchPrepareURL, old)] = hostHTTPResponse{StatusCode: http.StatusUnauthorized}
	// Refresh scripts.
	fake.doScripts[fake.scriptKey(execPlanCheckURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`),
	}
	fake.doScripts[fake.scriptKey(execOAuthTokenURL, "")] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"access_token":"fresh-access-token","refresh_token":"fresh-refresh-token","id_token":"fresh-id-token"}`),
	}
	// Retry attempt: the search legs under the refreshed bearer.
	fake.doScripts[fake.scriptKey(searchPrepareURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"conduit_token": "conduit-search-1"}`),
	}
	fake.setChunks(chunkedSSE(execSearchSSE, 7))
	fake.doScripts[fake.scriptKey(searchPollURL, fresh)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(execSearchPollDoc),
	}
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", searchToolPayload("web_search_preview"), false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	completion := decodeSearchCompletion(t, resp)
	assertCitedAnswer(t, completion)

	if hits := fake.doHit(fake.scriptKey(execOAuthTokenURL, "")); hits != 1 {
		t.Fatalf("the token endpoint was hit %d times, want exactly one refresh", hits)
	}
	fake.mu.Lock()
	prepareHits := fake.doHits[fake.scriptKey(searchPrepareURL, old)] + fake.doHits[fake.scriptKey(searchPrepareURL, fresh)]
	fake.mu.Unlock()
	if prepareHits != 2 {
		t.Fatalf("search prepare issued %d times, want 2 (one failed, one retry)", prepareHits)
	}
}

// ---------------------------------------------------------------------------
// TestSearchResponsesFormatRoutesSearch — the openai-response format's
// web_search tool routes through the search flow and answers with the cited
// text inside the Responses envelope (output message item).
// ---------------------------------------------------------------------------

func TestSearchResponsesFormatRoutesSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	scriptSearchFlow(t, fake, bearer)
	m := execManager(t, fake, "")

	req := execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
		"input": "what is this?",
		"tools": []any{map[string]any{"type": "web_search_preview"}},
	}, false)
	req.Format = responseFormat
	req.SourceFormat = responseFormat

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t, req, ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out.Payload, &envelope); err != nil {
		t.Fatalf("payload is not the Responses envelope: %v", err)
	}
	output, _ := envelope["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output items = %d, want one message item", len(output))
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("message content parts = %d, want one", len(content))
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	if !strings.Contains(text, "Sources:") || !strings.Contains(text, "The answer cites https://example.org/b.") {
		t.Fatalf("responses output text = %q, want the cited search answer", text)
	}
}

// ---------------------------------------------------------------------------
// TestSearchClaudeFormatRoutesSearch — the claude format's search model
// prefix routes through the search flow and answers with the cited text as a
// single text block in the Anthropic message envelope.
// ---------------------------------------------------------------------------

func TestSearchClaudeFormatRoutesSearch(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	scriptSearchFlow(t, fake, bearer)
	m := execManager(t, fake, "")

	req := execRequest(t, cred, "chatgpt-proxy/openai/gpt-5-search-api", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "what is this?"}},
	}, false)
	req.Format = claudeFormat
	req.SourceFormat = claudeFormat

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t, req, ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out.Payload, &envelope); err != nil {
		t.Fatalf("payload is not the claude envelope: %v", err)
	}
	content, _ := envelope["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("claude content blocks = %d, want one text block", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("claude content block type = %v, want text", block["type"])
	}
	text, _ := block["text"].(string)
	if !strings.Contains(text, "Sources:") || !strings.Contains(text, "The answer cites https://example.org/b.") {
		t.Fatalf("claude text block = %q, want the cited search answer", text)
	}
}

// ---------------------------------------------------------------------------
// TestSearchZeroContentIs502 — a search run that settles with no assistant
// answer is the typed 502 no_assistant_content (never an empty 200). The
// poll document carries no assistant message, so the wait times out to a
// partial empty result under a tiny configured budget.
// ---------------------------------------------------------------------------

func TestSearchZeroContentIs502(t *testing.T) {
	cred := execCredential()
	bearer := "Bearer " + cred.AccessToken
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts(bearer)
	fake.doScripts[fake.scriptKey(searchPrepareURL, bearer)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"conduit_token": "conduit-search-1"}`),
	}
	fake.setChunks(chunkedSSE(execSearchSSE, 7))
	// A document with no assistant message: the stable/done predicate never
	// fires, so the (tiny) search budget exhausts to an empty result.
	fake.doScripts[fake.scriptKey(searchPollURL, bearer)] = hostHTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"mapping": {}}`),
	}
	m := execManager(t, fake, "search-timeout-secs: 2\nsearch-poll-interval-secs: 1\n")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", searchToolPayload("web_search_preview"), false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	got := decodeExecError(t, resp)
	if got.Code != "no_assistant_content" || got.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("error = %+v, want typed 502 no_assistant_content", got)
	}
}

// ---------------------------------------------------------------------------
// TestSearchErrorClassesPinned — the search typed errors map onto the README
// error matrix: search_poll_timeout is a retryable 504, and a malformed
// search response (missing conduit token) is a non-retryable 502. A folded
// invalid_token still maps to the shared 401 class.
// ---------------------------------------------------------------------------

func TestSearchErrorClassesPinned(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
		wantRetry  bool
	}{
		{
			"poll timeout",
			&upstream.SearchError{Code: upstream.SearchCodePollTimeout, HTTPStatus: http.StatusGatewayTimeout, Retryable: true, Message: "the search poll exhausted its budget"},
			"search_poll_timeout", http.StatusGatewayTimeout, true,
		},
		{
			"malformed response",
			&upstream.SearchError{Code: upstream.SearchCodeMalformed, HTTPStatus: http.StatusBadGateway, Message: "the upstream search response is malformed"},
			"search_response_malformed", http.StatusBadGateway, false,
		},
		{
			"folded invalid token",
			&upstream.SearchError{Code: "invalid_token", HTTPStatus: http.StatusUnauthorized, Message: "the upstream rejected the bearer token"},
			"invalid_token", http.StatusUnauthorized, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mapSearchError(tc.err)
			if e.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", e.Code, tc.wantCode)
			}
			if e.HTTPStatus != tc.wantStatus {
				t.Fatalf("status = %d, want %d", e.HTTPStatus, tc.wantStatus)
			}
			if e.Retryable != tc.wantRetry {
				t.Fatalf("retryable = %v, want %v", e.Retryable, tc.wantRetry)
			}
		})
	}
}
