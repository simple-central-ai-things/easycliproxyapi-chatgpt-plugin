package plugin

// executor_search.go implements the web-search route for the text executor.
// A request that asks for search — a web_search / web_search_preview(_…)
// tool with NO other tools, a web_search tool_choice, a web_search_options
// block, or a search model prefix on the resolved slug (oracle
// services/protocol/web_search_tool.py is_web_search_chat_request /
// has_unsupported_tools / SEARCH_CHAT_MODEL_PREFIXES) — is detected at turn
// preparation and routed through the upstream SEARCH flow (oracle
// openai_backend_api.py search: conduit prepare with system_hints
// ["search"], the f/conversation SSE with force_use_search, then the
// conversation-document poll) instead of the plain conversation pipeline.
// The requested upstream model is the config's search-upstream-model (the
// oracle's SEARCH_MODEL default gpt-5-5); the chat's own slug is only the
// detection signal and the envelope echo, exactly like the reference.
//
// The answer comes back rendered (oracle text_with_url_citations): the
// cleaned answer plus a numbered Sources block, and — in the openai format's
// non-stream envelope — url_citation annotations spanning each listed url
// (oracle chat_completion_annotations, attached to the assistant message).
// Streaming search turns are buffered (the whole run completes inside
// Handle, as tool turns are) and emitted as role/content, terminal, [DONE].
//
// A 401 on the turn (sentinel leg, prepare, SSE start) forces the internal
// refresh + persist and one retry — the Review Focus #3 contract applied to
// the search route.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// WEB_SEARCH_TOOL_TYPES (oracle web_search_tool.py).
var webSearchToolTypes = map[string]bool{
	"web_search":                    true,
	"web_search_preview":            true,
	"web_search_preview_2025_03_11": true,
}

// SEARCH_CHAT_MODEL_PREFIXES (oracle web_search_tool.py): a search model is
// the prefix itself or the prefix with a "-…" version suffix.
var searchModelPrefixes = []string{
	"gpt-4o-search-preview",
	"gpt-4o-mini-search-preview",
	"gpt-5-search-api",
}

// searchSignals mirrors the OpenAI request fields the detection reads.
// RawMessage so a present-but-null value reads as absent.
type searchSignals struct {
	Tools            json.RawMessage `json:"tools"`
	ToolChoice       json.RawMessage `json:"tool_choice"`
	WebSearchOptions json.RawMessage `json:"web_search_options"`
}

// isWebSearchRequest ports is_web_search_chat_request gated by
// has_unsupported_tools(body, WEB_SEARCH_TOOL_TYPES) — the oracle's exact
// routing condition: a search signal is present AND no tool carries a type
// outside the web-search set. A dict tool without a usable type still counts
// as non-search (the oracle compares the stripped type against the set), so
// mixed search + function tools fall back to the ordinary emulated pipeline.
// slug is the resolved upstream model name (the published id minus the
// chatgpt-proxy/openai/ prefix).
func isWebSearchRequest(body []byte, slug string) bool {
	var signals searchSignals
	if len(body) == 0 || json.Unmarshal(body, &signals) != nil {
		return false
	}

	hasSignal := searchModelPrefixMatch(slug)
	hasNonSearchTool := false

	if len(signals.Tools) > 0 && string(signals.Tools) != "null" {
		var tools []any
		if json.Unmarshal(signals.Tools, &tools) == nil {
			for _, tool := range tools {
				obj, ok := tool.(map[string]any)
				if !ok {
					continue // the oracle ignores non-dict tools
				}
				toolType := strings.TrimSpace(jsonStringOr(obj["type"]))
				if webSearchToolTypes[toolType] {
					hasSignal = true
				} else {
					hasNonSearchTool = true
				}
			}
		}
	}
	if len(signals.ToolChoice) > 0 && string(signals.ToolChoice) != "null" {
		var choice map[string]any
		if json.Unmarshal(signals.ToolChoice, &choice) == nil {
			toolType := strings.TrimSpace(jsonStringOr(choice["type"]))
			if webSearchToolTypes[toolType] {
				hasSignal = true
			}
		}
	}
	if len(signals.WebSearchOptions) > 0 && string(signals.WebSearchOptions) != "null" {
		var options map[string]any
		if json.Unmarshal(signals.WebSearchOptions, &options) == nil {
			hasSignal = true
		}
	}

	return hasSignal && !hasNonSearchTool
}

func searchModelPrefixMatch(model string) bool {
	for _, prefix := range searchModelPrefixes {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

// searchQueryFromMessages ports search_query_from_messages: the last user
// message with non-empty text (reverse scan).
func searchQueryFromMessages(messages []upstream.ConversationMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(messages[i].Role)) != "user" {
			continue
		}
		if text := strings.TrimSpace(messages[i].Content); text != "" {
			return text
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The run loop: sentinel + upstream.Search with the 401 refresh-retry.
// ---------------------------------------------------------------------------

// runSearchTurn runs the search turn with the refresh-retry loop: at most two
// attempts, the second only after a forced refresh succeeded AND was
// persisted (the 401 contract is identical to the text executor, Review Focus
// #3). It returns the typed envelope error directly — the search turn runs to
// completion inside Handle, so there is no event channel to drain.
func (m *Manager) runSearchTurn(ctx context.Context, turn *textTurn, callbackID string) (upstream.SearchResult, *pluginabi.Error) {
	var startErr error
	for attempt := 0; attempt < 2; attempt++ {
		result, err := m.attemptSearchTurn(ctx, turn)
		if err == nil {
			return result, nil
		}
		startErr = err
		if attempt == 0 && turn.cfg.Retry401Refresh && isInvalidTokenError(err) {
			refreshed, refreshErr := m.refreshToken(ctx, callbackID, turn.cred)
			if refreshErr != nil {
				return upstream.SearchResult{}, typedErr("invalid_token",
					"the upstream rejected the bearer token and the forced refresh failed", http.StatusUnauthorized)
			}
			// Persist the rotated record BEFORE the retry (the OpenAI
			// refresh rotates the refresh token — see runTextTurnStart).
			if saveErr := m.saveCredential(turn.authFileName, refreshed); saveErr != nil {
				return upstream.SearchResult{}, typedErr("invalid_token",
					"the forced refresh succeeded but the rotated credential could not be saved", http.StatusUnauthorized)
			}
			turn.cred = refreshed
			continue
		}
		break
	}
	return upstream.SearchResult{}, mapSearchError(startErr)
}

// attemptSearchTurn runs one full search attempt: the access-token check, the
// sentinel handshake, then the search flow (prepare → SSE → poll).
func (m *Manager) attemptSearchTurn(ctx context.Context, turn *textTurn) (upstream.SearchResult, error) {
	if !hasAccessToken(turn.cred) {
		return upstream.SearchResult{}, errMissingAccessToken
	}
	tokens, err := upstream.AcquireSentinel(ctx, turn.upstream, turn.cred, false)
	if err != nil {
		return upstream.SearchResult{}, err
	}
	return upstream.Search(ctx, turn.upstream, turn.cred, tokens, turn.cfg, turn.searchQuery)
}

// mapSearchError maps a search-turn failure onto the typed envelope table.
// Transport legs arrive as the folded *SearchError (toSearchError keeps the
// conversation seam's codes); the search-specific classes map to their own
// documented codes. Anything else (SentinelError, raw ConvError) reuses the
// shared text classifier.
func mapSearchError(err error) *pluginabi.Error {
	if errors.Is(err, errMissingAccessToken) {
		return typedErr("invalid_token",
			"the credential has no access token and the forced refresh produced none", http.StatusUnauthorized)
	}
	var searchErr *upstream.SearchError
	if errors.As(err, &searchErr) {
		switch searchErr.Code {
		case "invalid_token":
			return typedErr("invalid_token", searchErr.Message, http.StatusUnauthorized)
		case "rate_limited":
			message := searchErr.Message
			if searchErr.RetryAfter > 0 {
				message += fmt.Sprintf("; retry after %ds", searchErr.RetryAfter)
			}
			e := typedErr("rate_limited", message, http.StatusTooManyRequests)
			e.Retryable = true
			return e
		case "upstream_stream_timeout":
			e := typedErr("upstream_stream_timeout", searchErr.Message, http.StatusGatewayTimeout)
			e.Retryable = true
			return e
		case upstream.SearchCodePollTimeout:
			e := typedErr("search_poll_timeout", searchErr.Message, http.StatusGatewayTimeout)
			e.Retryable = true
			return e
		case upstream.SearchCodeMalformed:
			return typedErr("search_response_malformed", searchErr.Message, http.StatusBadGateway)
		default:
			status := searchErr.HTTPStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			e := typedErr("upstream_error", searchErr.Message, status)
			e.Retryable = searchErr.Retryable
			return e
		}
	}
	return mapUpstreamError(err)
}

// ---------------------------------------------------------------------------
// Rendering: the cited answer in each format's normal envelope.
// ---------------------------------------------------------------------------

// searchCitedAnswer is the reference's text_with_url_citations outcome for
// one search result: the cleaned answer plus the Sources block, and the
// url_citation spans. A settled run with no assistant answer is the typed
// zero-content 502 (the Review Focus #2 contract), never an empty 200.
func searchCitedAnswer(result upstream.SearchResult) (string, []upstream.Citation, *pluginabi.Error) {
	if strings.TrimSpace(result.Answer) == "" {
		return "", nil, noAssistantContent()
	}
	text, citations := upstream.SearchCitedAnswer(result)
	return text, citations, nil
}

// searchCompletionPayload renders the openai-format chat.completion for a
// search turn: the cited answer as the assistant content, the
// url_citation annotations attached to the message (oracle
// chat_completion_annotations), finish_reason stop, and the documented
// bytes/4 usage estimate.
func searchCompletionPayload(turn *textTurn, result upstream.SearchResult) ([]byte, *pluginabi.Error) {
	cited, citations, apiErr := searchCitedAnswer(result)
	if apiErr != nil {
		return nil, apiErr
	}
	message := map[string]any{"role": "assistant", "content": cited}
	if annotations := chatCompletionAnnotations(citations); len(annotations) > 0 {
		message["annotations"] = annotations
	}
	envelope := map[string]any{
		"id":      turn.turnID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   turn.modelID,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     turn.promptTokens,
			"completion_tokens": estimateTokens(cited),
			"total_tokens":      turn.promptTokens + estimateTokens(cited),
		},
	}
	raw, _ := json.Marshal(envelope)
	return raw, nil
}

// chatCompletionAnnotations ports the reference's chat_completion_annotations:
// the flat citation spans become the OpenAI url_citation message annotation
// objects (type + url_citation{start_index, end_index, url, title}).
func chatCompletionAnnotations(citations []upstream.Citation) []map[string]any {
	out := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		out = append(out, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"start_index": citation.StartIndex,
				"end_index":   citation.EndIndex,
				"url":         citation.URL,
				"title":       citation.Title,
			},
		})
	}
	return out
}

// emitSearchStream is the streaming openai-format search turn (oracle
// stream_web_search_chat_completion): the whole result is emitted in one
// role+content chunk followed by the terminal stop chunk (with usage) and
// the [DONE] sentinel — search answers are not token-streamed, and the
// annotations are dropped in stream form like the reference.
// host.stream.close is issued exactly once on every exit path.
func (m *Manager) emitSearchStream(downstreamID string, turn *textTurn, result upstream.SearchResult, cancelBudget context.CancelFunc) {
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
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}

	cited, _, apiErr := searchCitedAnswer(result)
	if apiErr != nil {
		closeStream(envelopeText(apiErr))
		return
	}
	finish := "stop"
	usage := map[string]any{
		"prompt_tokens":     turn.promptTokens,
		"completion_tokens": estimateTokens(cited),
		"total_tokens":      turn.promptTokens + estimateTokens(cited),
	}
	if !emit(chunkFrame(turn, map[string]any{"role": "assistant", "content": cited}, nil)) {
		return
	}
	if !emit(chunkFrame(turn, map[string]any{}, &finish, usage)) {
		return
	}
	emit([]byte("data: [DONE]\n\n"))
}

// emitResponsesSearchStream is the streaming Responses search turn: the
// buffered item sequence (response.created → output_item.added/done →
// response.completed) emitted once from the search result — the same
// sequence the buffered function-tool stream emits. No [DONE] sentinel:
// response.completed is the Responses terminator.
func (m *Manager) emitResponsesSearchStream(downstreamID string, turn *textTurn, text string, cancelBudget context.CancelFunc) {
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
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}
	send := func(event map[string]any) bool {
		return emit(responseEventFrame(event))
	}

	usage := responsesUsage(turn.promptTokens, 0, estimateTokens(text), 0)
	items := responseTextItems(turn, text)
	if !send(responseCreatedEvent(turn.turnID, turn.modelID, turn.created)) {
		return
	}
	for index, item := range items {
		if !send(outputItemAddedEvent(index, outputItemInProgress(item))) {
			return
		}
		if !send(outputItemDoneEvent(index, item)) {
			return
		}
	}
	send(responseCompletedEvent(turn.turnID, turn.modelID, turn.created, items, usage))
}

// emitClaudeSearchStream is the streaming claude search turn: the Anthropic
// event sequence (message_start → content blocks → message_delta →
// message_stop) emitted once from the search result as a single text block.
// No data: [DONE] sentinel — message_stop is the terminator.
func (m *Manager) emitClaudeSearchStream(downstreamID string, turn *textTurn, text string, cancelBudget context.CancelFunc) {
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
			closeStream("the downstream stream is unavailable")
			return false
		}
		return true
	}
	send := func(event map[string]any) bool {
		return emit(claudeEventFrame(event))
	}

	// searchMode turns keep toolMode false: the cited text is prose.
	content, stopReason := claudeContentBlocks(turn, text)
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
		if !send(map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}) {
			return
		}
		if !send(map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": block["text"]},
		}) {
			return
		}
		if !send(map[string]any{"type": "content_block_stop", "index": index}) {
			return
		}
	}
	if !send(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateTokens(text)},
	}) {
		return
	}
	send(map[string]any{"type": "message_stop", "created": turn.created})
}

// prepareSearchTurn sets the turn's search fields (query, prompt estimate)
// after the messages decode, refusing typed BEFORE any upstream call when
// the request carries no user query (oracle search_query_from_messages →
// 400 "messages or prompt is required for web search").
func prepareSearchTurn(turn *textTurn, messages []upstream.ConversationMessage) *pluginabi.Error {
	query := searchQueryFromMessages(messages)
	if query == "" {
		return typedErr("invalid_request", "messages or prompt is required for web search", 400)
	}
	turn.searchMode = true
	turn.searchQuery = query
	turn.promptTokens = estimateTokens(query)
	return nil
}
