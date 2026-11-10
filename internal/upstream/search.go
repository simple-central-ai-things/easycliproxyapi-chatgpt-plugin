package upstream

// search.go ports the reference's web-search flow (openai_backend_api.py
// search / _prepare_search_conversation / _run_search_conversation /
// _wait_search_result / _get_search_conversation / _extract_search_result /
// _extract_search_sources / _search_message_text / _find_search_value /
// _walk_search_dicts / _clean_search_url, and services/protocol/
// web_search_tool.py clean_search_text / normalized_sources /
// text_with_url_citations).
//
// The flow reuses the conversation SSE + poll machinery rather than forking
// it: the f/conversation SSE leg runs through runConversationStream (the same
// seam the drawing route drives, oracle _image_headers), and the result wait
// drives PollConversation with the search stop condition (a finished status
// OR the answer text stable for two consecutive polls) as the Done predicate.
//
// One Search run is one attempt against ONE credential. The account-pool
// retries (401 refresh) are the executor's job, which wraps Search the way it
// wraps GenerateImage.
//
// Wire payload order and fingerprint fields are byte-identical to the oracle
// (json.dumps default dialect via appendPyJSONDefault); structural fields are
// pinned by search_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// The search route reuses the drawing route's conduit paths (oracle
// _prepare_search_conversation / _run_search_conversation both target
// /backend-api/f/conversation[/prepare]).
const (
	searchPreparePath = drawingPreparePath // "/backend-api/f/conversation/prepare"
	searchSSEPath     = drawingSSEPath     // "/backend-api/f/conversation"

	// The oracle's placeholder conduit token sent on the search prepare leg
	// (the real conduit token is acquired by that call and only the SSE leg
	// carries it).
	searchNoTokenPlaceholder = "no-token"
	// The oracle's client_reported_search_source marker (a search kicked off
	// by the composer's web icon).
	searchSourceMarker = "conversation_composer_web_icon"
)

// SearchError is the typed failure contract for the search pipeline. Code is
// stable (the executor maps it to envelope error classes); Message never
// carries upstream body bytes or token material (secret rule).
type SearchError struct {
	Code           string
	HTTPStatus     int
	Retryable      bool
	RetryAfter     int
	Message        string
	ConversationID string
}

func (e *SearchError) Error() string {
	return e.Code + ": " + e.Message
}

// SearchError codes (stable; the executor matches on these).
const (
	SearchCodeMalformed   = "search_response_malformed"
	SearchCodePollTimeout = "search_poll_timeout"
)

// The SEARCH_DONE_STATUS set (oracle openai_backend_api.py line 76).
var searchDoneStatuses = map[string]bool{
	"finished_successfully":       true,
	"finished_partial_completion": true,
}

// The SEARCH_URL_RE inline-url scanner (oracle line 78). The trailing
// punctuation cleanup is applied afterwards (cleanSearchURL).
var searchURLRe = regexp.MustCompile(`https?://[^\s"'<>）)\]}]+`)

// The annotation marker runes (private-use, like the conversation sanitizer).
const (
	searchMarkerOpen  = "\uE200"
	searchMarkerClose = "\uE201"
	searchMarkerField = "\uE202"
)

var (
	// \uE200 payload \uE201 — one closed annotation.
	searchAnnotationRe = regexp.MustCompile("\uE200([^\uE201]*)\uE201")
	// a trailing unclosed marker strips to the end of the text.
	searchTrailingAnnotationRe = regexp.MustCompile("\uE200[^\uE201]*$")
	// whitespace run immediately before sentence punctuation collapses.
	searchPunctSpaceRe = regexp.MustCompile(`\s+([.,;:!?])`)
	// a pure-digit annotation part (oracle re.fullmatch(r"\d+", value)).
	searchDigitsRe = regexp.MustCompile(`^\d+$`)
)

// searchClock is the same-package test seam for the wait-loop sleeps (the
// polling seam drives PollConversation; searchSleep/searchJitter are swapped
// by searchFakeClock in tests).
var (
	searchSleep  = ctxSleep
	searchJitter = randDuration
)

// SearchSource is one cited source extracted from the search answer (oracle
// _extract_search_sources entry: title / url / snippet / source_type).
type SearchSource struct {
	Title      string
	URL        string
	Snippet    string
	SourceType string
}

// SearchResult is the outcome of one Search run: the extracted assistant
// answer, its cited sources (metadata walk + inline URL scan, deduped), and
// the conversation identifiers. The reference's POST /v1/search response
// shape is this struct plus the account email (which the executor owns).
type SearchResult struct {
	Answer             string
	Sources            []SearchSource
	ConversationID     string
	Status             string
	AssistantMessageID string
	CreateTime         float64
}

// Citation is one url_citation span in the rendered cited answer (oracle
// text_with_url_citations annotation entry).
type Citation struct {
	StartIndex int
	EndIndex   int
	URL        string
	Title      string
}

// Search runs the full web-search flow for one credential: conduit prepare
// (system_hints ["search"], the "no-token" placeholder) → f/conversation SSE
// (force_use_search, capture the conversation id) → poll the conversation
// document until the assistant answer settles (finished status or stable
// text). tokens are the sentinel bundle the executor acquired.
func Search(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, cfg model.Config, query string) (SearchResult, error) {
	conduit, err := prepareSearchConversation(ctx, u, cred, cfg.SearchUpstreamModel, query)
	if err != nil {
		return SearchResult{}, err
	}
	hardCap := time.Duration(cfg.SearchTimeoutSecs) * time.Second
	conversationID, err := runSearchSSE(ctx, u, cred, tokens, conduit, cfg.SearchUpstreamModel, query, hardCap)
	if err != nil {
		return SearchResult{}, err
	}
	return waitSearchResult(ctx, u, cred, conversationID, cfg)
}

// toSearchError folds a transport/status failure (ConvError, SentinelError,
// or a bare transport error) into the search typed error, keeping the codes
// identical to the conversation seam. Caller cancellations pass through raw
// so the executor unwinds them instead of retrying a dead request.
func toSearchError(err error, conversationID string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var convErr *ConvError
	if errors.As(err, &convErr) {
		return &SearchError{
			Code:           convErr.Code,
			HTTPStatus:     convErr.HTTPStatus,
			Retryable:      convErr.Retryable,
			RetryAfter:     convErr.RetryAfter,
			Message:        convErr.message,
			ConversationID: conversationID,
		}
	}
	return &SearchError{
		Code:           "upstream_status",
		Retryable:      true,
		Message:        "the upstream call failed",
		ConversationID: conversationID,
	}
}

func searchTransportError(message string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &SearchError{Code: "upstream_status", Retryable: true, Message: message}
}

// ---------------------------------------------------------------------------
// Prepare (oracle _prepare_search_conversation).
// ---------------------------------------------------------------------------

func prepareSearchConversation(ctx context.Context, u Upstream, cred model.Credential, upstreamModel, query string) (string, error) {
	payload := pyObj{pairs: []pyPair{
		{key: "action", val: "next"},
		{key: "fork_from_shared_post", val: false},
		{key: "parent_message_id", val: "client-created-root"},
		{key: "model", val: upstreamModel},
		{key: "client_prepare_state", val: "success"},
		{key: "timezone_offset_min", val: -480},
		{key: "timezone", val: defaultConversationTZ},
		{key: "conversation_mode", val: pyObj{pairs: []pyPair{{key: "kind", val: "primary_assistant"}}}},
		{key: "system_hints", val: []any{"search"}},
		{key: "partial_query", val: pyObj{pairs: []pyPair{
			{key: "id", val: newUUID()},
			{key: "author", val: pyObj{pairs: []pyPair{{key: "role", val: "user"}}}},
			{key: "content", val: pyObj{pairs: []pyPair{
				{key: "content_type", val: "text"},
				{key: "parts", val: []any{query}},
			}}},
		}}},
		{key: "supports_buffering", val: true},
		{key: "supported_encodings", val: []any{"v1"}},
		{key: "client_contextual_info", val: pyObj{pairs: []pyPair{{key: "app_name", val: "chatgpt.com"}}}},
	}}
	body, err := appendPyJSONDefault(nil, payload)
	if err != nil {
		return "", fmt.Errorf("search prepare body: %w", err)
	}

	// Oracle search prepare: the session fingerprint + bearer, the "no-token"
	// conduit placeholder, Accept */* and NO sentinel headers (the oracle's
	// _headers base carries only the fingerprint; sentinel tokens join on the
	// SSE leg, after _bootstrap).
	headers := SessionHeaders(&cred, searchPreparePath)
	headers = Authed(&cred, headers)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("X-Conduit-Token", searchNoTokenPlaceholder)

	resp, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + searchPreparePath,
		Headers:     headers,
		Body:        body,
		ContentType: "application/json",
	})
	if err != nil {
		return "", searchTransportError("the upstream search prepare call failed", err)
	}
	if convErr := statusError(resp.StatusCode, parseRetryAfter(resp.Headers)); convErr != nil {
		return "", toSearchError(convErr, "")
	}
	var prepared struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(resp.Body, &prepared); err != nil || strings.TrimSpace(prepared.ConduitToken) == "" {
		// Oracle: a missing conduit_token is a hard RuntimeError (unlike the
		// drawing prepare, which tolerates an empty token).
		return "", &SearchError{Code: SearchCodeMalformed, HTTPStatus: http.StatusBadGateway,
			Message: "the upstream search prepare response carries no conduit token"}
	}
	return prepared.ConduitToken, nil
}

// ---------------------------------------------------------------------------
// The f/conversation SSE leg (oracle _run_search_conversation).
// ---------------------------------------------------------------------------

func runSearchSSE(ctx context.Context, u Upstream, cred model.Credential, tokens SentinelTokens, conduit, upstreamModel, query string, hardCap time.Duration) (string, error) {
	body, err := searchSSEPayload(upstreamModel, query)
	if err != nil {
		return "", fmt.Errorf("search SSE body: %w", err)
	}

	// Oracle _image_headers for the SSE leg: requirements + proof only (no
	// turnstile/SO), the conduit token, and a fresh X-Oai-Turn-Trace-Id.
	headers := SessionHeaders(&cred, searchSSEPath)
	headers = Authed(&cred, headers)
	headers.Set("Accept", "text/event-stream")
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", tokens.ChatRequirements)
	if tokens.Proof != "" {
		headers.Set("OpenAI-Sentinel-Proof-Token", tokens.Proof)
	}
	headers.Set("X-Conduit-Token", conduit)
	headers.Set("X-Oai-Turn-Trace-Id", newUUID())

	// The oracle caps this POST at the search timeout; the search SSE is
	// short-lived (the answer settles via the poll, not the stream), so the
	// hard cap is the search timeout.
	ch, err := runConversationStream(ctx, u, searchSSEPath, headers, body, hardCap, "", nil)
	if err != nil {
		return "", toSearchError(err, "")
	}
	var last ConvEvent
	for ev := range ch {
		last = ev
		if ev.Kind == ConvEventError {
			return "", toSearchError(ev.Err, ev.ConversationID)
		}
	}
	if strings.TrimSpace(last.ConversationID) == "" {
		// Oracle RuntimeError("conversation_id not found in stream").
		return "", &SearchError{Code: SearchCodeMalformed, HTTPStatus: http.StatusBadGateway,
			Message: "conversation_id not found in the search stream"}
	}
	return last.ConversationID, nil
}

// searchSSEPayload renders the oracle _run_search_conversation body in its
// dict insertion order. The parent_message_id is the literal
// "client-created-root" (oracle), the message metadata carries the search
// hint while the top-level system_hints is the empty array (oracle).
func searchSSEPayload(upstreamModel, query string) ([]byte, error) {
	createTime := float64(time.Now().Unix())
	message := pyObj{pairs: []pyPair{
		{key: "id", val: newUUID()},
		{key: "author", val: pyObj{pairs: []pyPair{{key: "role", val: "user"}}}},
		{key: "create_time", val: createTime},
		{key: "content", val: pyObj{pairs: []pyPair{
			{key: "content_type", val: "text"},
			{key: "parts", val: []any{query}},
		}}},
		{key: "metadata", val: pyObj{pairs: []pyPair{
			{key: "developer_mode_connector_ids", val: []any{}},
			{key: "selected_github_repos", val: []any{}},
			{key: "selected_all_github_repos", val: false},
			{key: "system_hints", val: []any{"search"}},
			{key: "serialization_metadata", val: pyObj{pairs: []pyPair{{key: "custom_symbol_offsets", val: []any{}}}}},
		}}},
	}}
	payload := pyObj{pairs: []pyPair{
		{key: "action", val: "next"},
		{key: "messages", val: []any{message}},
		{key: "parent_message_id", val: "client-created-root"},
		{key: "model", val: upstreamModel},
		{key: "client_prepare_state", val: "success"},
		{key: "timezone_offset_min", val: -480},
		{key: "timezone", val: defaultConversationTZ},
		{key: "conversation_mode", val: pyObj{pairs: []pyPair{{key: "kind", val: "primary_assistant"}}}},
		{key: "enable_message_followups", val: true},
		{key: "system_hints", val: []any{}},
		{key: "supports_buffering", val: true},
		{key: "supported_encodings", val: []any{"v1"}},
		{key: "force_use_search", val: true},
		{key: "client_reported_search_source", val: searchSourceMarker},
		{key: "client_contextual_info", val: pyObj{pairs: []pyPair{
			{key: "is_dark_mode", val: false},
			{key: "time_since_loaded", val: 36},
			{key: "page_height", val: 925},
			{key: "page_width", val: 886},
			{key: "pixel_ratio", val: 2},
			{key: "screen_height", val: 1440},
			{key: "screen_width", val: 2560},
			{key: "app_name", val: "chatgpt.com"},
		}}},
		{key: "paragen_cot_summary_display_override", val: "allow"},
		{key: "force_parallel_switch", val: "auto"},
	}}
	return appendPyJSONDefault(nil, payload)
}

// ---------------------------------------------------------------------------
// The result wait (oracle _wait_search_result driven through PollConversation).
// ---------------------------------------------------------------------------

// searchPollSwallow is the oracle's not-ready set: these statuses on a fresh
// conversation are backed off and re-polled instead of failing (429/5xx are
// already retryable via statusError; a 401 is never swallowed).
var searchPollSwallow = map[int]bool{
	http.StatusNotFound:            true,
	http.StatusConflict:            true,
	http.StatusLocked:              true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

func waitSearchResult(ctx context.Context, u Upstream, cred model.Credential, conversationID string, cfg model.Config) (SearchResult, error) {
	opts := PollOptions{
		Timeout:           time.Duration(cfg.SearchTimeoutSecs) * time.Second,
		Interval:          time.Duration(cfg.SearchPollIntervalSecs) * time.Second,
		SkipInitialWait:   true,
		RetryableStatuses: searchPollSwallow,
		sleep:             searchSleep,
		jitter:            searchJitter,
	}

	var lastResult SearchResult
	sawExtraction := false
	lastAnswer := ""
	stableHits := 0

	// Done ports the oracle's stop condition: an answer is present AND
	// (a finished status, or the answer stable for two consecutive polls).
	opts.Done = func(doc ConversationDoc) bool {
		extraction := extractSearchResult(conversationID, doc.Mapping)
		lastResult, sawExtraction = extraction, true
		if extraction.Answer == "" {
			return false
		}
		if searchDoneStatuses[extraction.Status] {
			return true
		}
		if extraction.Answer == lastAnswer {
			stableHits++
		} else {
			stableHits = 0
			lastAnswer = extraction.Answer
		}
		return stableHits >= 2
	}

	if _, err := PollConversation(ctx, u, cred, conversationID, opts); err != nil {
		// Only the poll-timeout unwinds into the oracle's last-partial
		// extraction; every other error (a 401, a malformed document, a
		// transport failure, caller cancellation) propagates typed.
		var convErr *ConvError
		if errors.As(err, &convErr) && convErr.Code != "conversation_poll_timeout" {
			return SearchResult{}, toSearchError(err, conversationID)
		}
		// Oracle _wait_search_result: on deadline, return the last
		// extraction if one was seen (even an empty answer — the executor
		// maps that to the zero-content contract), else a typed timeout.
		if sawExtraction {
			return lastResult, nil
		}
		return SearchResult{}, &SearchError{
			Code: SearchCodePollTimeout, HTTPStatus: http.StatusGatewayTimeout, Retryable: true,
			Message:        "timed out waiting for the search result",
			ConversationID: conversationID,
		}
	}
	return lastResult, nil
}

// ---------------------------------------------------------------------------
// Extraction (oracle _extract_search_result + helpers).
// ---------------------------------------------------------------------------

// extractSearchResult ports _extract_search_result: the assistant message with
// the latest create_time, its status (finish_details.type → metadata.status →
// recursive "status" scan), its answer text, and its sources (metadata walk +
// inline URL scan, deduped by cleaned url).
func extractSearchResult(conversationID string, mapping map[string]any) SearchResult {
	var assistantMessages []map[string]any
	for _, node := range mapping {
		nodeMap, _ := node.(map[string]any)
		message, _ := nodeMap["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		if role, _ := author["role"].(string); role == "assistant" {
			assistantMessages = append(assistantMessages, message)
		}
	}
	var message map[string]any
	for _, candidate := range assistantMessages {
		if message == nil || jsonFloatValue(candidate["create_time"]) >= jsonFloatValue(message["create_time"]) {
			message = candidate
		}
	}
	if message == nil {
		message = map[string]any{}
	}

	metadata, _ := message["metadata"].(map[string]any)
	finishDetails, _ := metadata["finish_details"].(map[string]any)

	answer := searchMessageText(message)
	sources := extractSearchSources(message)
	for _, url := range searchURLRe.FindAllString(answer, -1) {
		if cleaned := cleanSearchURL(url); cleaned != "" && !sourceURLSeen(sources, cleaned) {
			sources = append(sources, SearchSource{URL: cleaned})
		}
	}

	return SearchResult{
		ConversationID:     conversationID,
		Status:             searchStatus(finishDetails, metadata, message),
		Answer:             answer,
		Sources:            sources,
		AssistantMessageID: stringOf(message["id"]),
		CreateTime:         jsonFloatValue(message["create_time"]),
	}
}

// searchStatus ports the oracle's status chain: finish_details.type OR
// metadata.status OR a recursive "status" scan of the message, stripped.
func searchStatus(finishDetails, metadata map[string]any, message map[string]any) string {
	status := stringOf(finishDetails["type"])
	if status == "" {
		status = stringOf(metadata["status"])
	}
	if status == "" {
		status = findSearchValue(message, "status")
	}
	return strings.TrimSpace(status)
}

// searchMessageText ports _search_message_text: the content.text field plus
// the parts (string parts as-is; dict parts contribute their text/summary/
// content keys), joined by newlines and stripped.
func searchMessageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	var parts []string
	if text, ok := content["text"].(string); ok {
		parts = append(parts, text)
	}
	if rawParts, ok := content["parts"].([]any); ok {
		for _, part := range rawParts {
			switch typed := part.(type) {
			case string:
				parts = append(parts, typed)
			case map[string]any:
				for _, key := range []string{"text", "summary", "content"} {
					if value := stringOf(typed[key]); value != "" {
						parts = append(parts, value)
					}
				}
			}
		}
	}
	var kept []string
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// extractSearchSources ports _extract_search_sources: walk every dict in the
// message (DFS, maps in sorted-key order for determinism — Go maps drop the
// JSON insertion order the oracle reads), and record the first source whose
// cleaned url has not been seen (url → link → source_url → metadata.url
// fallback chain; title/name/source, snippet/text/description, and
// type/source_type fallback chains).
func extractSearchSources(payload any) []SearchSource {
	var sources []SearchSource
	for _, obj := range walkSearchDicts(payload) {
		metadata, _ := obj["metadata"].(map[string]any)
		url := cleanSearchURL(firstSearchString(obj, "url", "link", "source_url"))
		if url == "" {
			url = cleanSearchURL(firstSearchString(metadata, "url"))
		}
		if url == "" || sourceURLSeen(sources, url) {
			continue
		}
		sources = append(sources, SearchSource{
			Title:      strings.TrimSpace(firstSearchString(obj, "title", "name", "source")),
			URL:        url,
			Snippet:    strings.TrimSpace(firstSearchString(obj, "snippet", "text", "description")),
			SourceType: strings.TrimSpace(firstSearchString(obj, "type", "source_type")),
		})
	}
	return sources
}

func sourceURLSeen(sources []SearchSource, url string) bool {
	for _, item := range sources {
		if item.URL == url {
			return true
		}
	}
	return false
}

// walkSearchDicts ports _walk_search_dicts: every dict reachable from payload
// in depth-first order (the payload itself first). Map children are visited
// in sorted-key order so the walk — and therefore first-seen-url dedupe — is
// deterministic across runs.
func walkSearchDicts(payload any) []map[string]any {
	switch typed := payload.(type) {
	case map[string]any:
		out := []map[string]any{typed}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			out = append(out, walkSearchDicts(typed[key])...)
		}
		return out
	case []any:
		var out []map[string]any
		for _, item := range typed {
			out = append(out, walkSearchDicts(item)...)
		}
		return out
	}
	return nil
}

// firstSearchString ports the oracle's `or` fallback chain over JSON values:
// the first non-empty stringified value among the keys, else "".
func firstSearchString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := searchStringValue(obj[key]); s != "" {
			return s
		}
	}
	return ""
}

// searchStringValue is Python's str() over the JSON scalar set the oracle
// stringifies (strings verbatim, numbers/bools rendered, everything else "").
func searchStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

// cleanSearchURL ports _clean_search_url: strip, then rstrip the trailing
// punctuation set (ASCII + fullwidth).
func cleanSearchURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), ".,;，。；")
}

// findSearchValue ports _find_search_value: a recursive scan for the first
// non-empty string value under key (dicts check their own key first, then
// their values in sorted-key order for determinism — Go maps drop the JSON
// insertion order the oracle reads; pathological docs only, lists are
// insertion-ordered in practice; lists scan items; strings try a JSON
// decode, and a string payload for key "conversation_id" is regex-scanned).
func findSearchValue(payload any, key string) string {
	switch typed := payload.(type) {
	case string:
		if key == "conversation_id" {
			if match := searchConversationIDRe.FindStringSubmatch(typed); match != nil {
				return match[1]
			}
		}
		var decoded any
		if err := json.Unmarshal([]byte(typed), &decoded); err != nil {
			return ""
		}
		return findSearchValue(decoded, key)
	case map[string]any:
		if value, ok := typed[key].(string); ok && value != "" {
			return value
		}
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if found := findSearchValue(typed[k], key); found != "" {
				return found
			}
		}
		return ""
	case []any:
		for _, item := range typed {
			if found := findSearchValue(item, key); found != "" {
				return found
			}
		}
	}
	return ""
}

var searchConversationIDRe = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)

// ---------------------------------------------------------------------------
// Rendering (oracle web_search_tool.py clean_search_text / normalized_sources /
// text_with_url_citations).
// ---------------------------------------------------------------------------

// cleanSearchText ports clean_search_text: resolves the private-use rich
// annotation markers to readable labels (dropping internal turn/source/digit
// pointers), strips a trailing unclosed marker, collapses whitespace runs
// before sentence punctuation, and trims.
func cleanSearchText(text string) string {
	text = searchAnnotationRe.ReplaceAllStringFunc(text, replaceSearchAnnotation)
	text = searchTrailingAnnotationRe.ReplaceAllString(text, "")
	text = searchPunctSpaceRe.ReplaceAllString(text, "$1")
	return strings.TrimSpace(text)
}

func replaceSearchAnnotation(match string) string {
	payload := match[len(searchMarkerOpen) : len(match)-len(searchMarkerClose)]
	var parts []string
	for _, part := range strings.Split(payload, searchMarkerField) {
		parts = append(parts, strings.TrimSpace(part))
	}
	kind := ""
	if len(parts) > 0 {
		kind = strings.ToLower(parts[0])
	}
	data := parts[1:]
	if kind == "url" {
		label, url := "", ""
		if len(data) > 0 {
			label = data[0]
		}
		if len(data) > 1 {
			url = data[1]
		}
		if label != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			return label + " (" + url + ")"
		}
		if label != "" {
			return label
		}
		return url // oracle's `label or url` fallback
	}
	return readableAnnotationPart(data)
}

// readableAnnotationPart ports _readable_annotation_part: the first part that
// is non-empty and not an internal pointer (turn/source/sources prefix or a
// pure digit run).
func readableAnnotationPart(parts []string) string {
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "turn") || strings.HasPrefix(lower, "source") || searchDigitsRe.MatchString(value) {
			continue
		}
		return value
	}
	return ""
}

// SearchCitedAnswer ports text_with_url_citations: the cleaned answer plus a
// numbered "Sources:" block, and one url_citation span per listed url. Sources
// are normalized (deduped by url, url required) so a title-less source
// renders as its url. Returns (text, citations).
func SearchCitedAnswer(result SearchResult) (string, []Citation) {
	text := cleanSearchText(result.Answer)
	citations := []Citation{}
	sources := normalizedSources(result.Sources)
	if len(sources) > 0 {
		text = strings.TrimRight(text, " \t\r\n")
		if text != "" {
			text += "\n\n"
		}
		text += "Sources:\n"
		for index, source := range sources {
			// The oracle folds the url into the line prefix when the title
			// is empty (title = source["title"] or source["url"]) and then
			// appends the url again — a reference bug that duplicates the
			// url on title-less sources. This port keeps the url in exactly
			// one place (the cited span).
			if source.Title != "" {
				text += fmt.Sprintf("%d. %s - ", index+1, source.Title)
			} else {
				text += fmt.Sprintf("%d. ", index+1)
			}
			if source.URL != "" {
				start := len(text)
				text += source.URL
				title := source.Title
				if title == "" {
					title = source.URL
				}
				citations = append(citations, Citation{
					StartIndex: start,
					EndIndex:   len(text),
					URL:        source.URL,
					Title:      title,
				})
			}
			text += "\n"
		}
	}
	return strings.TrimSpace(text), citations
}

// normalizedSources ports normalized_sources: drop url-less and duplicate
// entries, keep first occurrence, in order.
func normalizedSources(sources []SearchSource) []SearchSource {
	out := []SearchSource{}
	seen := map[string]bool{}
	for _, item := range sources {
		url := strings.TrimSpace(item.URL)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, item)
	}
	return out
}

// searchCitedText is the unexported alias the package tests call.
func searchCitedText(result SearchResult) (string, []Citation) {
	return SearchCitedAnswer(result)
}
