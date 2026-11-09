package upstream

// search_test.go pins the Task F3 web-search flow: the reference's
// openai_backend_api.py search() port (prepare with system_hints ["search"]
// and the "no-token" conduit placeholder → f/conversation SSE with
// force_use_search → conversation-document poll with the finished-status /
// stable-text stop condition), the source extraction (metadata walk + inline
// URL regex), and the typed SearchError contract the executor maps.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

const (
	searchConduitToken = "conduit-search-1"
	searchConversation = "c-search-1"

	execSearchPrepareURL = chatGPTBaseURL + drawingPreparePath
	execSearchSSEURL     = chatGPTBaseURL + drawingSSEPath
	execSearchPollURL    = chatGPTBaseURL + "/backend-api/conversation/" + searchConversation
)

func testSearchCfg() model.Config {
	return model.Config{
		SearchUpstreamModel:    "gpt-5-5",
		SearchTimeoutSecs:      300,
		SearchPollIntervalSecs: 3,
	}
}

// searchFakeClock swaps the search clock seams: every sleep is recorded
// (never elapsed), jitter is zero.
func searchFakeClock(t *testing.T, sleeps *[]time.Duration) {
	t.Helper()
	oldSleep, oldJitter := searchSleep, searchJitter
	searchSleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	searchJitter = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(func() { searchSleep, searchJitter = oldSleep, oldJitter })
}

// scriptSearchPrepareSSE scripts the search start legs: the conduit prepare
// and the f/conversation SSE whose frames carry the conversation id and a
// visible assistant message, ending in [DONE].
func scriptSearchPrepareSSE(t *testing.T, fake *drawFake, conduitToken string) {
	t.Helper()
	fake.status("POST "+execSearchPrepareURL, http.StatusOK,
		[]byte(`{"conduit_token":`+jsonString(conduitToken)+`}`))
	fake.chunks = chunkBytes([]byte(`data: {"p":"","o":"add","v":{"message":{"id":"m-search-1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]},"conversation_id":"`+searchConversation+`"}}

data: [DONE]
`), 7)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// scriptSearchPollDone scripts one conversation-document fetch carrying the
// assistant answer, sources metadata, finish_details and the assistant id.
func scriptSearchPollDone(t *testing.T, fake *drawFake) {
	t.Helper()
	fake.status("GET "+execSearchPollURL, http.StatusOK, []byte(searchDoneDoc))
}

// searchDoneDoc is one settled conversation document: two assistant messages
// (the later create_time wins), the answer with an inline URL, a metadata
// sources block and finish_details finished_successfully.
const searchDoneDoc = `{
  "mapping": {
    "msg-a": {
      "message": {
        "id": "msg-a",
        "author": {"role": "assistant"},
        "create_time": 100.0,
        "content": {"content_type": "text", "parts": ["earlier draft"]},
        "metadata": {}
      }
    },
    "msg-b": {
      "message": {
        "id": "msg-b",
        "author": {"role": "assistant"},
        "create_time": 200.5,
        "content": {"content_type": "text", "parts": ["The answer cites https://example.org/b. See also https://example.org/a"]},
        "metadata": {
          "finish_details": {"type": "finished_successfully"},
          "sources": [
            {"title": "Example A", "url": "https://example.org/a", "snippet": "Snippet A", "source_type": "web"},
            {"name": "Example C", "link": "https://example.org/c", "text": "Snippet C", "type": "api"}
          ]
        }
      }
    },
    "user-1": {
      "message": {
        "id": "user-1",
        "author": {"role": "user"},
        "create_time": 90.0,
        "content": {"content_type": "text", "parts": ["what is this?"]}
      }
    }
  }
}`

func assertSearchError(t *testing.T, err error, code string) *SearchError {
	t.Helper()
	var searchErr *SearchError
	if !errors.As(err, &searchErr) {
		t.Fatalf("error = %v, want typed *SearchError", err)
	}
	if searchErr.Code != code {
		t.Fatalf("SearchError code = %q, want %q", searchErr.Code, code)
	}
	return searchErr
}

// ---------------------------------------------------------------------------
// TestSearchHappyPath — prepare → SSE → one settled document (finished
// status) → answer + sources + inline URL merge; wire shapes asserted.
// ---------------------------------------------------------------------------

func TestSearchHappyPath(t *testing.T) {
	cfg := testSearchCfg()
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	scriptSearchPollDone(t, fake)
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	result, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req", Proof: "fixture-pow"}, cfg,
		"what is this?")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none (the finished status settles on the first fetch)", sleeps)
	}
	if result.ConversationID != searchConversation {
		t.Fatalf("ConversationID = %q", result.ConversationID)
	}
	if result.Status != "finished_successfully" {
		t.Fatalf("Status = %q", result.Status)
	}
	if result.AssistantMessageID != "msg-b" {
		t.Fatalf("AssistantMessageID = %q, want the latest assistant message (create_time 200.5)", result.AssistantMessageID)
	}
	if result.CreateTime != 200.5 {
		t.Fatalf("CreateTime = %v, want 200.5", result.CreateTime)
	}
	wantAnswer := "The answer cites https://example.org/b. See also https://example.org/a"
	if result.Answer != wantAnswer {
		t.Fatalf("Answer = %q, want %q", result.Answer, wantAnswer)
	}
	wantSources := []SearchSource{
		{Title: "Example A", URL: "https://example.org/a", Snippet: "Snippet A", SourceType: "web"},
		{Title: "Example C", URL: "https://example.org/c", Snippet: "Snippet C", SourceType: "api"},
		{Title: "", URL: "https://example.org/b", Snippet: "", SourceType: ""},
	}
	if len(result.Sources) != len(wantSources) {
		t.Fatalf("sources = %+v, want %+v", result.Sources, wantSources)
	}
	for i, want := range wantSources {
		if result.Sources[i] != want {
			t.Fatalf("source[%d] = %+v, want %+v", i, result.Sources[i], want)
		}
	}

	// Prepare wire: search hint, the literal parent id, no sentinel headers,
	// the "no-token" conduit placeholder.
	prepares := fake.requestsFor(http.MethodPost, execSearchPrepareURL)
	if len(prepares) != 1 {
		t.Fatalf("prepare calls = %d, want 1", len(prepares))
	}
	assertSearchPrepareBody(t, prepares[0].Body)
	prepareHeaders := prepares[0].Headers
	if prepareHeaders.Get("X-Conduit-Token") != "no-token" {
		t.Fatalf("prepare X-Conduit-Token = %q, want the \"no-token\" placeholder (oracle)", prepareHeaders.Get("X-Conduit-Token"))
	}
	if prepareHeaders.Get("Accept") != "*/*" {
		t.Fatalf("prepare Accept = %q, want */*", prepareHeaders.Get("Accept"))
	}
	if got := prepareHeaders.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "" {
		t.Fatalf("prepare carries sentinel header %q, the oracle's search prepare does not", got)
	}

	// SSE wire: conduit token, fresh trace id, requirements+proof only,
	// force_use_search and the search source marker.
	streams := fake.requestsFor(http.MethodPost, execSearchSSEURL)
	if len(streams) != 1 {
		t.Fatalf("SSE calls = %d, want 1", len(streams))
	}
	req := streams[0]
	if got := req.Headers.Get("X-Conduit-Token"); got != searchConduitToken {
		t.Fatalf("SSE X-Conduit-Token = %q", got)
	}
	if got := req.Headers.Get("X-Oai-Turn-Trace-Id"); !uuidRE.MatchString(got) {
		t.Fatalf("SSE X-Oai-Turn-Trace-Id = %q, want a fresh uuid4", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "fixture-req" {
		t.Fatalf("SSE requirements token header = %q", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Proof-Token"); got != "fixture-pow" {
		t.Fatalf("SSE proof token header = %q", got)
	}
	if got := req.Headers.Get("OpenAI-Sentinel-Turnstile-Token"); got != "" {
		t.Fatalf("the search route carries no turnstile header (oracle _image_headers)")
	}
	assertSearchSSEBody(t, req.Body)
}

func assertSearchPrepareBody(t *testing.T, body []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("prepare body is not JSON: %v", err)
	}
	if doc["action"] != "next" || doc["client_prepare_state"] != "success" || doc["model"] != "gpt-5-5" {
		t.Fatalf("prepare body core fields = %v/%v/%v", doc["action"], doc["client_prepare_state"], doc["model"])
	}
	if doc["parent_message_id"] != "client-created-root" {
		t.Fatalf("prepare parent_message_id = %v, want the literal \"client-created-root\" (oracle)", doc["parent_message_id"])
	}
	if _, ok := doc["thinking_effort"]; ok {
		t.Fatalf("the search prepare carries no thinking_effort")
	}
	if !reflectStringSlice(doc["system_hints"], "search") {
		t.Fatalf("prepare system_hints = %v, want [search]", doc["system_hints"])
	}
	pq, _ := doc["partial_query"].(map[string]any)
	content, _ := pq["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0] != "what is this?" {
		t.Fatalf("partial_query parts = %v, want the query", parts)
	}
	ci, _ := doc["client_contextual_info"].(map[string]any)
	if ci["app_name"] != "chatgpt.com" {
		t.Fatalf("client_contextual_info = %v", ci)
	}
}

func assertSearchSSEBody(t *testing.T, body []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("SSE body is not JSON: %v", err)
	}
	if doc["action"] != "next" || doc["client_prepare_state"] != "success" || doc["model"] != "gpt-5-5" {
		t.Fatalf("SSE body core fields = %v/%v/%v", doc["action"], doc["client_prepare_state"], doc["model"])
	}
	if doc["force_use_search"] != true {
		t.Fatalf("force_use_search = %v, want true (oracle)", doc["force_use_search"])
	}
	if doc["client_reported_search_source"] != "conversation_composer_web_icon" {
		t.Fatalf("client_reported_search_source = %v (oracle)", doc["client_reported_search_source"])
	}
	if doc["parent_message_id"] != "client-created-root" {
		t.Fatalf("SSE parent_message_id = %v, want the literal (oracle)", doc["parent_message_id"])
	}
	if _, ok := doc["system_hints"].([]any); !ok {
		t.Fatalf("SSE system_hints = %v, want the empty array (oracle)", doc["system_hints"])
	}
	ci, _ := doc["client_contextual_info"].(map[string]any)
	if ci["time_since_loaded"] != float64(36) || ci["page_height"] != float64(925) ||
		ci["page_width"] != float64(886) || ci["pixel_ratio"] != float64(2) {
		t.Fatalf("client_contextual_info = %v, want the oracle's search geometry", ci)
	}
	messages, _ := doc["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	msg, _ := messages[0].(map[string]any)
	if author, _ := msg["author"].(map[string]any); author["role"] != "user" {
		t.Fatalf("message author = %v", msg["author"])
	}
	created, _ := msg["create_time"].(float64)
	if created <= 0 {
		t.Fatalf("message create_time = %v, want an epoch float", msg["create_time"])
	}
	content, _ := msg["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0] != "what is this?" {
		t.Fatalf("message parts = %v, want the query", parts)
	}
	metadata, _ := msg["metadata"].(map[string]any)
	if !reflectStringSlice(metadata["system_hints"], "search") {
		t.Fatalf("message metadata.system_hints = %v, want [search] (oracle)", metadata["system_hints"])
	}
}

// ---------------------------------------------------------------------------
// TestSearchStableTextStop — no finished status: the answer must be stable
// for two consecutive polls (three fetches) before the wait returns.
// ---------------------------------------------------------------------------

func TestSearchStableTextStop(t *testing.T) {
	cfg := testSearchCfg()
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	calls := 0
	fake.on("GET "+execSearchPollURL, func() Resp {
		calls++
		return Resp{StatusCode: http.StatusOK, Headers: http.Header{}, Body: []byte(searchStableDoc)}
	})
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	result, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if calls != 3 {
		t.Fatalf("document fetches = %d, want 3 (stable for 2 polls)", calls)
	}
	if len(sleeps) != 2 || sleeps[0] != 3*time.Second || sleeps[1] != 3*time.Second {
		t.Fatalf("sleeps = %v, want two steady 3s intervals (no initial wait)", sleeps)
	}
	if result.Answer != "same answer" {
		t.Fatalf("Answer = %q", result.Answer)
	}
	if result.Status != "" {
		t.Fatalf("Status = %q, want empty (no finished status reached)", result.Status)
	}
}

// searchStableDoc carries an assistant answer but no finish_details: the
// stable-text stop is the only exit.
const searchStableDoc = `{
  "mapping": {
    "msg-1": {
      "message": {
        "id": "msg-1",
        "author": {"role": "assistant"},
        "create_time": 10.0,
        "content": {"content_type": "text", "parts": ["same answer"]},
        "metadata": {}
      }
    }
  }
}`

// ---------------------------------------------------------------------------
// TestSearchPollTimeoutPartial — the budget runs out with an answer that
// never finishes or stabilizes (the text keeps growing): the oracle returns
// the last extraction instead of raising.
// ---------------------------------------------------------------------------

func TestSearchPollTimeoutPartial(t *testing.T) {
	cfg := testSearchCfg()
	cfg.SearchTimeoutSecs = 5 // budget: one fetch + one interval sleep
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	calls := 0
	fake.on("GET "+execSearchPollURL, func() Resp {
		calls++
		body := strings.ReplaceAll(searchStableDoc, "same answer", "answer "+strconv.Itoa(calls))
		return Resp{StatusCode: http.StatusOK, Headers: http.Header{}, Body: []byte(body)}
	})
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	result, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want 2 (the budget allows one interval sleep)", calls)
	}
	if result.Answer != "answer 2" {
		t.Fatalf("Answer = %q, want the last partial extraction", result.Answer)
	}
}

// ---------------------------------------------------------------------------
// TestSearchPollTimeoutNoResult — every fetch 409s (the swallow set) until
// the budget runs out with no extraction: the typed search_poll_timeout.
// ---------------------------------------------------------------------------

func TestSearchPollTimeoutNoResult(t *testing.T) {
	cfg := testSearchCfg()
	cfg.SearchTimeoutSecs = 6
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	fake.status("GET "+execSearchPollURL, http.StatusConflict, nil)
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	_, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	searchErr := assertSearchError(t, err, SearchCodePollTimeout)
	if searchErr.HTTPStatus != http.StatusGatewayTimeout || !searchErr.Retryable {
		t.Fatalf("search_poll_timeout = status %d retryable %v, want 504 retryable", searchErr.HTTPStatus, searchErr.Retryable)
	}
	// Backoffs 2s + 4s (jitter zero) fill the 6s budget.
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second {
		t.Fatalf("sleeps = %v, want the 2s/4s backoffs", sleeps)
	}
}

// ---------------------------------------------------------------------------
// TestSearchSwallowsNotReadyStatuses — 404/409/423 are "not ready yet"
// (the oracle's swallow set): the poll backs off through all three and
// returns the first settled document. 401 still fails immediately.
// ---------------------------------------------------------------------------

func TestSearchSwallowsNotReadyStatuses(t *testing.T) {
	cfg := testSearchCfg()
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	calls := 0
	fake.on("GET "+execSearchPollURL, func() Resp {
		calls++
		switch calls {
		case 1:
			return Resp{StatusCode: http.StatusNotFound, Headers: http.Header{}}
		case 2:
			return Resp{StatusCode: http.StatusLocked, Headers: http.Header{}}
		case 3:
			return Resp{StatusCode: http.StatusConflict, Headers: http.Header{}}
		}
		return Resp{StatusCode: http.StatusOK, Headers: http.Header{}, Body: []byte(searchDoneDoc)}
	})
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	result, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.AssistantMessageID != "msg-b" {
		t.Fatalf("AssistantMessageID = %q", result.AssistantMessageID)
	}
	if len(sleeps) != 3 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second || sleeps[2] != 8*time.Second {
		t.Fatalf("sleeps = %v, want the 2/4/8s backoffs across the swallowed statuses", sleeps)
	}
}

func TestSearchPoll401FailsImmediately(t *testing.T) {
	cfg := testSearchCfg()
	fake := newDrawFake(t)
	scriptSearchPrepareSSE(t, fake, searchConduitToken)
	fake.status("GET "+execSearchPollURL, http.StatusUnauthorized, nil)
	var sleeps []time.Duration
	searchFakeClock(t, &sleeps)

	_, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	searchErr := assertSearchError(t, err, "invalid_token")
	if searchErr.HTTPStatus != http.StatusUnauthorized || searchErr.Retryable {
		t.Fatalf("invalid_token = status %d retryable %v, want 401 non-retryable", searchErr.HTTPStatus, searchErr.Retryable)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v, a 401 poll must not back off", sleeps)
	}
}

// ---------------------------------------------------------------------------
// TestSearchPrepareFailures — a missing conduit token (oracle RuntimeError)
// and a 401 prepare fold to typed SearchErrors.
// ---------------------------------------------------------------------------

func TestSearchPrepareFailures(t *testing.T) {
	t.Run("missing conduit token", func(t *testing.T) {
		fake := newDrawFake(t)
		fake.status("POST "+execSearchPrepareURL, http.StatusOK, []byte(`{"conduit_token": ""}`))
		_, err := Search(context.Background(), fake, fixtureCredential(),
			SentinelTokens{ChatRequirements: "fixture-req"}, testSearchCfg(), "q")
		searchErr := assertSearchError(t, err, SearchCodeMalformed)
		if searchErr.HTTPStatus != http.StatusBadGateway || searchErr.Retryable {
			t.Fatalf("malformed = status %d retryable %v, want 502 non-retryable", searchErr.HTTPStatus, searchErr.Retryable)
		}
		if len(fake.requestsFor(http.MethodPost, execSearchSSEURL)) != 0 {
			t.Fatalf("the SSE leg must not start after a failed prepare")
		}
	})

	t.Run("prepare 401", func(t *testing.T) {
		fake := newDrawFake(t)
		fake.status("POST "+execSearchPrepareURL, http.StatusUnauthorized, nil)
		_, err := Search(context.Background(), fake, fixtureCredential(),
			SentinelTokens{ChatRequirements: "fixture-req"}, testSearchCfg(), "q")
		searchErr := assertSearchError(t, err, "invalid_token")
		if searchErr.HTTPStatus != http.StatusUnauthorized || searchErr.Retryable {
			t.Fatalf("invalid_token = status %d retryable %v", searchErr.HTTPStatus, searchErr.Retryable)
		}
	})
}

// TestSearchSSEWithoutConversationID — the stream ends without ever carrying
// a conversation id (oracle RuntimeError "conversation_id not found").
func TestSearchSSEWithoutConversationID(t *testing.T) {
	cfg := testSearchCfg()
	fake := newDrawFake(t)
	fake.status("POST "+execSearchPrepareURL, http.StatusOK, []byte(`{"conduit_token": "c-1"}`))
	fake.chunks = chunkBytes([]byte(`data: {"p":"","o":"append","v":"no id here"}

data: [DONE]
`), 7)
	_, err := Search(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-req"}, cfg, "q")
	searchErr := assertSearchError(t, err, SearchCodeMalformed)
	if searchErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("malformed status = %d, want 502", searchErr.HTTPStatus)
	}
}

// ---------------------------------------------------------------------------
// TestExtractSearchResult — the extraction port, unit-level: the create_time
// max, the status fallback chain (finish_details → metadata.status → the
// recursive "status" scan), the answer text assembly, and the inline URL
// regex with its trailing-punctuation cleanup.
// ---------------------------------------------------------------------------

func TestExtractSearchResult(t *testing.T) {
	t.Run("max create_time and status chain", func(t *testing.T) {
		mapping := map[string]any{
			"early": map[string]any{"message": map[string]any{
				"id": "e", "author": map[string]any{"role": "assistant"},
				"create_time": 1.0,
				"content":     map[string]any{"content_type": "text", "parts": []any{"early"}},
				"metadata":    map[string]any{"finish_details": map[string]any{"type": "finished_successfully"}},
			}},
			"late": map[string]any{"message": map[string]any{
				"id": "l", "author": map[string]any{"role": "assistant"},
				"create_time": 2.0,
				"content":     map[string]any{"content_type": "text", "parts": []any{"late"}},
				"metadata":    map[string]any{"status": "still_working"},
			}},
			"user": map[string]any{"message": map[string]any{
				"id": "u", "author": map[string]any{"role": "user"},
				"create_time": 0.5,
				"content":     map[string]any{"content_type": "text", "parts": []any{"q"}},
			}},
		}
		got := extractSearchResult("c-x", mapping)
		if got.AssistantMessageID != "l" || got.CreateTime != 2.0 {
			t.Fatalf("selected message = %q/%v, want the latest assistant (create_time 2.0)", got.AssistantMessageID, got.CreateTime)
		}
		if got.Answer != "late" {
			t.Fatalf("Answer = %q", got.Answer)
		}
		if got.Status != "still_working" {
			t.Fatalf("Status = %q, want the metadata.status fallback", got.Status)
		}
	})

	t.Run("recursive status scan fallback", func(t *testing.T) {
		mapping := map[string]any{
			"m": map[string]any{"message": map[string]any{
				"id": "m-1", "author": map[string]any{"role": "assistant"},
				"create_time": 1.0,
				"content":     map[string]any{"content_type": "text", "parts": []any{"a"}},
				"metadata": map[string]any{
					"nested": map[string]any{"status": "deep_status"},
				},
			}},
		}
		got := extractSearchResult("c-x", mapping)
		if got.Status != "deep_status" {
			t.Fatalf("Status = %q, want the recursive scan result", got.Status)
		}
	})

	t.Run("empty mapping is the zero result", func(t *testing.T) {
		got := extractSearchResult("c-x", nil)
		if got.ConversationID != "c-x" || got.Answer != "" || got.Status != "" ||
			got.AssistantMessageID != "" || got.CreateTime != 0 || len(got.Sources) != 0 {
			t.Fatalf("zero result = %+v", got)
		}
	})

	t.Run("inline URL trailing punctuation cleanup", func(t *testing.T) {
		mapping := map[string]any{
			"m": map[string]any{"message": map[string]any{
				"id": "m-1", "author": map[string]any{"role": "assistant"},
				"create_time": 1.0,
				"content": map[string]any{"content_type": "text", "parts": []any{
					"see https://example.com/x., and https://example.com/y。 more https://example.com/x",
				}},
			}},
		}
		got := extractSearchResult("c-x", mapping)
		if len(got.Sources) != 2 {
			t.Fatalf("sources = %+v, want the two cleaned inline URLs (deduped)", got.Sources)
		}
		if got.Sources[0].URL != "https://example.com/x" || got.Sources[1].URL != "https://example.com/y" {
			t.Fatalf("source urls = %q/%q, want trailing punctuation stripped", got.Sources[0].URL, got.Sources[1].URL)
		}
	})

	t.Run("content text field and dict parts", func(t *testing.T) {
		mapping := map[string]any{
			"m": map[string]any{"message": map[string]any{
				"id": "m-1", "author": map[string]any{"role": "assistant"},
				"create_time": 1.0,
				"content": map[string]any{
					"content_type": "text",
					"text":         "leading text field",
					"parts": []any{
						map[string]any{"summary": "dict summary"},
						"plain part",
					},
				},
			}},
		}
		got := extractSearchResult("c-x", mapping)
		if got.Answer != "leading text field\ndict summary\nplain part" {
			t.Fatalf("Answer = %q, want text + summary + parts joined", got.Answer)
		}
	})
}

// TestExtractSearchSources — the metadata walk: the url fallback chain
// (url → link → source_url → metadata.url), the title/snippet/source_type
// fallback chains, dedupe by cleaned url, DFS order.
func TestExtractSearchSources(t *testing.T) {
	payload := map[string]any{
		"metadata": map[string]any{
			"sources": []any{
				map[string]any{"title": "T1", "url": "https://one.example", "snippet": "S1", "source_type": "web"},
				map[string]any{"name": "T2", "link": "https://two.example", "text": "S2", "type": "api"},
				map[string]any{"source": "T3", "source_url": "https://three.example", "description": "S3"},
				// url from the nested metadata fallback, duplicate of T1's url.
				map[string]any{"metadata": map[string]any{"url": "https://one.example"}},
				// no url at all: skipped.
				map[string]any{"title": "T4"},
			},
		},
	}
	got := extractSearchSources(payload)
	want := []SearchSource{
		{Title: "T1", URL: "https://one.example", Snippet: "S1", SourceType: "web"},
		{Title: "T2", URL: "https://two.example", Snippet: "S2", SourceType: "api"},
		{Title: "T3", URL: "https://three.example", Snippet: "S3", SourceType: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// TestCleanSearchText — the answer renderer: the private-use annotation
// markers resolve to readable labels, internal pointers drop, the trailing
// unclosed marker strips, and whitespace-before-punctuation collapses.
// ---------------------------------------------------------------------------

func TestCleanSearchText(t *testing.T) {
	open, close := "\uE200", "\uE201"
	field := "\uE202"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"url annotation with label and url",
			"From " + open + "url" + field + "Example" + field + "https://example.com/a" + close + " the answer.",
			"From Example (https://example.com/a) the answer.",
		},
		{
			"url annotation without a label falls back to the url",
			"see " + open + "url" + field + "" + field + "https://example.com/b" + close + ".",
			"see https://example.com/b.",
		},
		{
			"cite annotation keeps the readable part",
			"per " + open + "cite" + field + "Reuters" + close + " it is so.",
			"per Reuters it is so.",
		},
		{
			// The reference only collapses whitespace immediately before
			// punctuation; dropping an annotation leaves its surrounding
			// spaces intact, so the result keeps the double space.
			"internal turn/source annotations drop",
			"a " + open + "cite" + field + "turn1a2b" + close + " b " + open + "cite" + field + "sources" + close + " c",
			"a  b  c",
		},
		{
			"digit-only annotation parts drop",
			"x " + open + "cite" + field + "12" + close + " y",
			"x  y",
		},
		{
			"trailing unclosed marker strips to the end",
			"done " + open + "url" + field + "Example" + field + "https://example.com/z",
			"done",
		},
		{
			"whitespace before punctuation collapses",
			"word  \t .",
			"word.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanSearchText(test.in); got != test.want {
				t.Fatalf("cleanSearchText = %q, want %q", got, test.want)
			}
		})
	}
}

// TestSearchCitedTextAndAnnotations — the openai-format rendering: the
// Sources block appends numbered entries and one url_citation annotation per
// listed url; sourceless answers render the cleaned answer alone.
func TestSearchCitedTextAndAnnotations(t *testing.T) {
	result := SearchResult{
		Answer: "The capital of France is Paris.",
		Sources: []SearchSource{
			{Title: "Wikipedia", URL: "https://en.wikipedia.org/wiki/Paris"},
			{Title: "", URL: "https://example.org/paris"},
		},
	}
	text, annotations := searchCitedText(result)
	wantText := "The capital of France is Paris.\n\nSources:\n1. Wikipedia - https://en.wikipedia.org/wiki/Paris\n2. https://example.org/paris\n"
	if text != strings.TrimSpace(wantText) {
		t.Fatalf("text = %q, want %q", text, strings.TrimSpace(wantText))
	}
	if len(annotations) != 2 {
		t.Fatalf("annotations = %+v, want two url_citations", annotations)
	}
	first := annotations[0]
	if first.Title != "Wikipedia" || first.URL != "https://en.wikipedia.org/wiki/Paris" {
		t.Fatalf("annotation[0] = %+v", first)
	}
	// The annotation range points at the url inside the text.
	if text[first.StartIndex:first.EndIndex] != first.URL {
		t.Fatalf("annotation[0] range %d..%d does not span %q in %q", first.StartIndex, first.EndIndex, first.URL, text)
	}
	if annotations[1].Title != "https://example.org/paris" {
		t.Fatalf("annotation[1] title = %q, want the url fallback", annotations[1].Title)
	}

	bareText, bareCites := searchCitedText(SearchResult{Answer: "no sources here"})
	if bareText != "no sources here" || len(bareCites) != 0 {
		t.Fatalf("sourceless render = %q with %d citations, want the bare cleaned answer", bareText, len(bareCites))
	}
}
