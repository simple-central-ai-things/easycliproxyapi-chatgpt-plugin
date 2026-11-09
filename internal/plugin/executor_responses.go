package plugin

// executor_responses.go implements the OpenAI Responses API format
// (openai-response, Phase 2 F1): the host passes /v1/responses requests
// untranslated (SourceFormat "openai-response"), so this file owns only the
// INBOUND decode (instructions + input → conversation messages, per the
// reference's messages_from_input) and the OUTBOUND shaping (the Responses
// envelope and SSE event sequence instead of chat.completion). The upstream
// turn is the SAME conversation pipeline as the openai format — sentinel
// handshake, conversation SSE, the 401 refresh-retry + persist loop and the
// zero-content typed 502 — all reused from executor.go / executor_text.go.
//
// Tools: an image_generation tool routes the whole request to the Phase-1
// image pipeline (the reference's is_text_response_request decides BEFORE
// any parsing; extract_response_prompt / extract_response_image extract the
// drawing prompt and the LAST input image) and answers with
// image_generation_call output items carrying the base64 result and the
// reference's ig_<n> ids. OpenAI function tools reuse the chat format's XML
// emulation (tools.go) and the reply parses into function_call output items
// — a deliberate deviation from the reference, which injects a "tools
// unavailable" system note instead, chosen to match the org's
// function-tools ruling. Any other tool type (e.g. web_search_preview)
// keeps the chat format's typed 400 tools_unsupported refusal.
//
// Streaming: plain text turns are delta-streamed (response.created →
// output_item.added → output_text.delta* → output_text.done →
// output_item.done → response.completed with usage) with close-once; the
// Responses stream has NO data: [DONE] sentinel — response.completed is the
// terminator. Tool and image requests buffer the whole turn first and emit
// the item sequence at once (documented tradeoff, as in the chat format).
//
// Ids and usage: resp_<uuid4().hex> / msg_<uuid4().hex> per the reference's
// uuid convention (unlike chatcmpl-, Responses ids carry no dashes); usage
// is the reference's token_usage shape (input_tokens / output_tokens /
// total_tokens plus *_tokens_details) filled with the documented bytes/4
// estimate — never exact counts.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// responseFormat is the executor format name the registration advertises
// and the dispatcher branches on.
const responseFormat = "openai-response"

// isResponseRequest reports whether the executor request is the
// openai-response format (either tag — the host passes the client's
// protocol untranslated).
func isResponseRequest(req executorRequest) bool {
	return req.SourceFormat == responseFormat || req.Format == responseFormat
}

// ---------------------------------------------------------------------------
// Ids and the Responses envelope object.
// ---------------------------------------------------------------------------

// uuidHex builds one fresh UUIDv4 rendered as 32 lowercase hex chars — the
// reference's f"resp_{uuid.uuid4().hex}" / f"msg_{uuid.uuid4().hex}"
// convention (the Responses ids carry no dashes, unlike turnID's chatcmpl-).
func uuidHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("uuid generation: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[:])
}

func respID() string { return "resp_" + uuidHex() }

func msgID() string { return "msg_" + uuidHex() }

// responseObject builds one Responses envelope object — the reference's
// response_completed response block (lines 265-288): id/object/created_at/
// status/error/incomplete_details/model/output/parallel_tool_calls, with
// usage attached only when non-nil. error and incomplete_details are JSON
// null, never absent (the client contract).
func responseObject(id, model string, created int64, status string, output []map[string]any, usage map[string]any) map[string]any {
	response := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"error":               nil,
		"incomplete_details":  nil,
		"model":               model,
		"output":              output,
		"parallel_tool_calls": false,
	}
	if usage != nil {
		response["usage"] = usage
	}
	return response
}

// responsesUsage is the reference's token_usage shape (utils/image_tokens.py
// lines 290-312) filled with the documented bytes/4 estimate — never an
// exact count.
func responsesUsage(inputText, inputImage, outputText, outputImage int) map[string]any {
	input := inputText + inputImage
	output := outputText + outputImage
	return map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  input + output,
		"input_tokens_details": map[string]any{
			"text_tokens":   inputText,
			"image_tokens":  inputImage,
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"text_tokens":      outputText,
			"image_tokens":     outputImage,
			"reasoning_tokens": 0,
		},
	}
}

// ---------------------------------------------------------------------------
// The request decode: Responses input/instructions → conversation messages.
// ---------------------------------------------------------------------------

// responsesShapePayload mirrors the Responses request fields this executor
// consumes. The effort spellings are pointers so a PRESENT-but-empty key
// beats a later spelling (the reference's `in body` precedence, lines
// 59-67); a missing key leaves room for the config default.
type responsesShapePayload struct {
	Input           any                 `json:"input"`
	Instructions    string              `json:"instructions"`
	Tools           json.RawMessage     `json:"tools"`
	Reasoning       *responsesReasoning `json:"reasoning"`
	ThinkingEffort  *string             `json:"thinking_effort"`
	ReasoningEffort *string             `json:"reasoning_effort"`
}

// responsesReasoning is the request's optional reasoning block; only its
// effort spelling is consumed (thinking_effort_from_body, line 59-62).
type responsesReasoning struct {
	Effort string `json:"effort"`
}

// decodeResponsesPayload parses one Responses request body. Failures are
// typed invalid_request errors — never Go errors escaping Handle.
func decodeResponsesPayload(body []byte) (responsesShapePayload, *pluginabi.Error) {
	if len(body) == 0 {
		return responsesShapePayload{}, typedErr("invalid_request", "empty responses payload", 400)
	}
	var payload responsesShapePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return responsesShapePayload{}, typedErr("invalid_request", "the responses payload is not valid JSON", 400)
	}
	return payload, nil
}

// shapeResponseRequest decodes one Responses request body into the upstream
// conversation messages, the resolved thinking effort and the tool mode —
// the signature mirrors shapeChatRequest. Per the reference's
// messages_from_input: instructions become the leading system message, a
// plain-string input one user message, item lists accumulate bare content
// parts into user messages and map message items by role; function_call /
// function_call_output items fold exactly like the chat format's agent
// loop and reasoning items are skipped (the upstream carries no reasoning
// history). Image content parts are refused typed on the text path — they
// route the request to the image pipeline via the image_generation tool.
func shapeResponseRequest(body []byte, searchMode bool) ([]upstream.ConversationMessage, string, bool, *pluginabi.Error) {
	payload, apiErr := decodeResponsesPayload(body)
	if apiErr != nil {
		return nil, "", false, apiErr
	}

	// The tools refusal (no usable name / non-array) precedes every other
	// work and costs zero network. The image_generation tool never reaches
	// the text path — dispatch routes it first — so shapeTools' nameless
	// refusal doubles as the built-in-tools (web_search etc.) refusal. In
	// search mode the web_search tools ARE the request (the gate verified
	// no function tool is mixed in) and are skipped like the chat format.
	var prompt string
	var toolMode bool
	if !searchMode {
		var err *pluginabi.Error
		prompt, toolMode, err = shapeTools(payload.Tools)
		if err != nil {
			return nil, "", false, err
		}
	}

	messages, apiErr := messagesFromResponsesInput(payload.Input, payload.Instructions)
	if apiErr != nil {
		return nil, "", false, apiErr
	}
	if !hasNonSystemMessage(messages) && strings.TrimSpace(payload.Instructions) == "" {
		return nil, "", false, typedErr("invalid_request",
			"the responses payload needs at least one input message", 400)
	}

	// The tool prompt joins the conversation's system message(s) (the
	// reference's merge_system, as in the chat format).
	messages = mergeSystemPrompt(messages, prompt)
	return messages, responsesThinkingEffort(payload), toolMode, nil
}

// responsesThinkingEffort ports the reference's thinking_effort_from_body
// (precedence: the reasoning block, then thinking_effort, then
// reasoning_effort) and normalize_thinking_effort (lines 48-67).
func responsesThinkingEffort(payload responsesShapePayload) string {
	if payload.Reasoning != nil {
		return normalizeResponsesEffort(payload.Reasoning.Effort)
	}
	if payload.ThinkingEffort != nil {
		return normalizeResponsesEffort(*payload.ThinkingEffort)
	}
	if payload.ReasoningEffort != nil {
		return normalizeResponsesEffort(*payload.ReasoningEffort)
	}
	return ""
}

// normalizeResponsesEffort ports the reference's normalize_thinking_effort:
// low/medium/high pass through, xhigh/extended collapse to "extended", and
// everything else (including none) resolves to "" — the config default then
// applies at the turn.
func normalizeResponsesEffort(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "none":
		return ""
	case "low", "medium", "high":
		return normalized
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}

// responsesContentPartTypes is the reference's RESPONSE_CONTENT_PART_TYPES
// (line 45): the part types that make an item a bare content part.
var responsesContentPartTypes = map[string]bool{
	"text":        true,
	"input_text":  true,
	"output_text": true,
	"image_url":   true,
	"input_image": true,
	"image":       true,
}

// isResponsesContentPart ports the reference's _is_response_content_part
// (lines 125-129).
func isResponsesContentPart(part map[string]any) bool {
	partType := strings.TrimSpace(jsonStringOr(part["type"]))
	return responsesContentPartTypes[partType] || (partType != "message" && part["image_url"] != nil)
}

// messagesFromResponsesInput ports the reference's messages_from_input
// (lines 150-190) to the string-content messages the upstream posts.
func messagesFromResponsesInput(input any, instructions string) ([]upstream.ConversationMessage, *pluginabi.Error) {
	messages := make([]upstream.ConversationMessage, 0, 4)
	if system := strings.TrimSpace(instructions); system != "" {
		messages = append(messages, upstream.ConversationMessage{Role: "system", Content: system})
	}
	switch value := input.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			messages = append(messages, upstream.ConversationMessage{Role: "user", Content: strings.TrimSpace(value)})
		}
	case map[string]any:
		if isResponsesContentPart(value) {
			text, apiErr := joinResponsesContentParts([]any{value})
			if apiErr != nil {
				return nil, apiErr
			}
			return appendResponsesMessage(messages, "user", text), nil
		}
		return appendResponsesItem(messages, value)
	case []any:
		if len(value) > 0 {
			allParts := true
			for _, item := range value {
				part, ok := item.(map[string]any)
				if !ok || !isResponsesContentPart(part) {
					allParts = false
					break
				}
			}
			if allParts {
				text, apiErr := joinResponsesContentParts(value)
				if apiErr != nil {
					return nil, apiErr
				}
				return appendResponsesMessage(messages, "user", text), nil
			}
		}
		var pending []any
		for _, item := range value {
			part, isPart := item.(map[string]any)
			if isPart && isResponsesContentPart(part) {
				pending = append(pending, item)
				continue
			}
			if len(pending) > 0 {
				text, apiErr := joinResponsesContentParts(pending)
				if apiErr != nil {
					return nil, apiErr
				}
				messages = appendResponsesMessage(messages, "user", text)
				pending = nil
			}
			if !isPart {
				continue
			}
			var apiErr *pluginabi.Error
			messages, apiErr = appendResponsesItem(messages, part)
			if apiErr != nil {
				return nil, apiErr
			}
		}
		if len(pending) > 0 {
			text, apiErr := joinResponsesContentParts(pending)
			if apiErr != nil {
				return nil, apiErr
			}
			messages = appendResponsesMessage(messages, "user", text)
		}
	}
	return messages, nil
}

// appendResponsesMessage ports the reference's _append_response_message:
// blank content appends nothing, a blank role defaults to user.
func appendResponsesMessage(messages []upstream.ConversationMessage, role, content string) []upstream.ConversationMessage {
	content = strings.TrimSpace(content)
	if content == "" {
		return messages
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "user"
	}
	return append(messages, upstream.ConversationMessage{Role: role, Content: content})
}

// appendResponsesItem maps one item-list entry: function_call and
// function_call_output fold exactly like the chat format's agent loop
// (tools.go), reasoning items are skipped, and everything else is a message
// item per its role/content (the reference's _message_content_from_response_item).
func appendResponsesItem(messages []upstream.ConversationMessage, item map[string]any) ([]upstream.ConversationMessage, *pluginabi.Error) {
	switch itemType := strings.TrimSpace(jsonStringOr(item["type"])); itemType {
	case "function_call":
		arguments := jsonStringOr(item["arguments"])
		if isJSONComposite(item["arguments"]) {
			arguments = jsonText(item["arguments"])
		}
		return append(messages, upstream.ConversationMessage{
			Role: "assistant",
			Content: foldAssistantToolCalls("", []shapeAssistantToolCall{{
				Function: shapeToolFunction{Name: jsonStringOr(item["name"]), Arguments: arguments},
			}}),
		}), nil
	case "function_call_output":
		output := jsonStringOr(item["output"])
		if isJSONComposite(item["output"]) {
			output = jsonText(item["output"])
		}
		return append(messages, upstream.ConversationMessage{
			Role:    "user",
			Content: "Tool result " + jsonStringOr(item["call_id"]) + ": " + output,
		}), nil
	case "reasoning":
		return messages, nil
	default:
		content, apiErr := responseItemContent(item)
		if apiErr != nil {
			return nil, apiErr
		}
		return appendResponsesMessage(messages, jsonStringOr(item["role"]), content), nil
	}
}

// isJSONComposite reports whether a decoded JSON value renders as
// jsonText (objects and arrays) rather than jsonStringOr (scalars).
func isJSONComposite(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// responseItemContent resolves one message item's content (the reference's
// _message_content_from_response_item): a string passes through, a parts
// list joins its text parts, and absent content falls back to the prompt
// extraction over the item itself.
func responseItemContent(item map[string]any) (string, *pluginabi.Error) {
	switch content := item["content"].(type) {
	case string:
		return content, nil
	case []any:
		return joinResponsesContentParts(content)
	default:
		return extractResponsePrompt([]any{item}), nil
	}
}

// joinResponsesContentParts joins one content-part array into the plain
// text the upstream posts: text/input_text/output_text parts in order;
// image parts are refused typed — the text path has no image surface
// (image inputs route the request to the image pipeline via the
// image_generation tool). Parts of any other type contribute nothing.
func joinResponsesContentParts(parts []any) (string, *pluginabi.Error) {
	text := ""
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(jsonStringOr(part["type"])) {
		case "image", "input_image", "image_url":
			return "", typedErr("invalid_request",
				"image input parts require the image_generation tool; they do not travel the responses text path", 400)
		case "text", "input_text", "output_text", "":
			text += jsonStringOr(part["text"], part["input_text"])
		}
	}
	return text, nil
}

// hasNonSystemMessage reports whether the mapped messages carry any user or
// assistant content — the shape-level analogue of the chat format's
// "at least one message" refusal.
func hasNonSystemMessage(messages []upstream.ConversationMessage) bool {
	for _, message := range messages {
		if strings.TrimSpace(message.Role) != "system" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The image_generation tool route.
// ---------------------------------------------------------------------------

// responsesImageTool returns the request's image_generation tool definition
// (the reference's response_image_tool, lines 78-82). Its presence — not
// any payload field — decides the route before anything else parses (the
// reference's is_text_response_request).
func responsesImageTool(payload responsesShapePayload) (map[string]any, bool) {
	if len(payload.Tools) == 0 || string(payload.Tools) == "null" {
		return nil, false
	}
	var tools []map[string]any
	if err := json.Unmarshal(payload.Tools, &tools); err != nil {
		return nil, false // malformed tools refuse later in shapeTools
	}
	for _, tool := range tools {
		if strings.TrimSpace(jsonStringOr(tool["type"])) == "image_generation" {
			return tool, true
		}
	}
	return nil, false
}

// prepareResponsesImageTurn shapes one Responses request carrying an
// image_generation tool: the reference's prompt extraction (input_text
// parts and user messages only — system/assistant never contribute), its
// image extraction (the LAST input image wins), the tool's size/quality,
// and the route — a published text model runs the drawing route, a
// published image base name keeps its route, anything else refuses typed
// model_unknown. A missing prompt is the reference's typed 400 "input text
// is required" BEFORE any upstream call. Returns (nil, nil) when the
// request has no image tool (the text path).
func (m *Manager) prepareResponsesImageTurn(req executorRequest) (*imageTurn, *pluginabi.Error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	payload, apiErr := decodeResponsesPayload(body)
	if apiErr != nil {
		return nil, apiErr
	}
	tool, found := responsesImageTool(payload)
	if !found {
		return nil, nil
	}

	cred, err := FromStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, typedErr("invalid_credential", "malformed "+providerKey+" credential record", 400)
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	route := imageRouteDrawing
	if _, isText := trimTextModelPrefix(req.Model, cfg.ModelPrefix); !isText {
		base, found := strings.CutPrefix(req.Model, cfg.ModelPrefix+"/")
		if !found {
			return nil, typedErr("model_unknown",
				fmt.Sprintf("model %q is not a published model (%s/openai/<slug> or %s/<image base>)",
					req.Model, cfg.ModelPrefix, cfg.ModelPrefix), 400)
		}
		resolved, ok := imageRouteFor(base, cfg)
		if !ok {
			return nil, typedErr("model_unknown",
				fmt.Sprintf("model %q is not a published model (%s/openai/<slug> or %s/<image base>)",
					req.Model, cfg.ModelPrefix, cfg.ModelPrefix), 400)
		}
		route = resolved
	}

	u, err := upstream.NewUpstream(cfg, m.host)
	if err != nil {
		return nil, typedErr("upstream_error", "upstream transport unavailable: "+err.Error(), 500)
	}

	prompt := extractResponsePrompt(payload.Input)
	if prompt == "" {
		return nil, typedErr("invalid_request", "input text is required", 400)
	}

	// The image source may be fetched over http(s) through the upstream
	// transport; it is bounded separately from the turn budget (the
	// openai-image parse precedent).
	parseCtx, parseCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parseCancel()
	references, apiErr := extractResponseReferences(payload.Input, u, parseCtx)
	if apiErr != nil {
		return nil, apiErr
	}

	return &imageTurn{
		cfg:          cfg,
		cred:         cred,
		upstream:     u,
		route:        route,
		modelID:      req.Model,
		prompt:       prompt,
		size:         jsonStringOr(tool["size"]),
		quality:      jsonStringOr(tool["quality"]),
		n:            1,
		references:   references,
		authFileName: execAuthFileName(req),
	}, nil
}

// extractResponsePrompt ports the reference's extract_response_prompt: the
// image route's drawing prompt — input_text parts and user messages only
// (system/assistant roles never contribute), joined with newlines.
func extractResponsePrompt(input any) string {
	switch value := input.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		if role := strings.ToLower(strings.TrimSpace(jsonStringOr(value["role"]))); role != "" && role != "user" {
			return ""
		}
		return responseContentPrompt(value["content"])
	case []any:
		parts := make([]string, 0)
		for _, item := range value {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(jsonStringOr(obj["type"])) == "input_text" {
				if text := strings.TrimSpace(jsonStringOr(obj["text"])); text != "" {
					parts = append(parts, text)
				}
				continue
			}
			if role := strings.ToLower(strings.TrimSpace(jsonStringOr(obj["role"]))); role != "" && role != "user" {
				continue
			}
			if prompt := responseContentPrompt(obj["content"]); prompt != "" {
				parts = append(parts, prompt)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// responseContentPrompt ports the reference's extract_prompt_from_message_content:
// a string content verbatim, a parts list contributes its text parts.
func responseContentPrompt(content any) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		parts := make([]string, 0)
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			partType := strings.TrimSpace(jsonStringOr(part["type"]))
			if partType == "text" || partType == "input_text" {
				text := strings.TrimSpace(jsonStringOr(part["text"], part["input_text"]))
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// extractResponseReferences decodes the request's single input image (the
// reference's extract_response_image: the LAST input image wins) into one
// reference with stdlib-resolved dimensions — or none.
func extractResponseReferences(input any, u upstream.Upstream, ctx context.Context) ([]upstream.ReferenceImage, *pluginabi.Error) {
	source := responseImageSource(input)
	if source == "" {
		return nil, nil
	}
	data, mime, err := decodeImageSource(source, u, ctx)
	if err != nil {
		return nil, typedErr("invalid_request", "could not decode the input image", 400)
	}
	ref := upstream.ReferenceImage{Data: data, MimeType: mime}
	applyImageDims(&ref)
	return []upstream.ReferenceImage{ref}, nil
}

// responseImageSource ports the reference's extract_response_image: a bare
// input_image part or the LAST image-bearing part/message in the input list.
func responseImageSource(input any) string {
	switch value := input.(type) {
	case map[string]any:
		if strings.TrimSpace(jsonStringOr(value["type"])) == "input_image" {
			return imagePartSource(value)
		}
		if parts, ok := value["content"].([]any); ok {
			return firstImagePartSource(parts)
		}
		return ""
	case []any:
		for i := len(value) - 1; i >= 0; i-- {
			item, ok := value[i].(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(jsonStringOr(item["type"])) == "input_image" {
				if source := imagePartSource(item); source != "" {
					return source
				}
			}
			if parts, ok := item["content"].([]any); ok {
				if source := firstImagePartSource(parts); source != "" {
					return source
				}
			}
		}
	}
	return ""
}

// firstImagePartSource reads the first image part of one content list (the
// reference's extract_image_from_message_content reads image_url /
// input_image / image parts).
func firstImagePartSource(parts []any) string {
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		partType := strings.TrimSpace(jsonStringOr(part["type"]))
		if partType != "image" && partType != "input_image" && partType != "image_url" {
			continue
		}
		if source := imagePartSource(part); source != "" {
			return source
		}
	}
	return ""
}

// imagePartSource reads one image part's source: image_url (a string or a
// {url} object), url, or a base64 source block — the source string the
// shared decodeImageSource resolves (data URL / bare base64 / http fetch).
func imagePartSource(part map[string]any) string {
	if source := imageSourceValue(part["image_url"]); source != "" {
		return source
	}
	if source := imageSourceValue(part["url"]); source != "" {
		return source
	}
	if source, ok := part["source"].(map[string]any); ok &&
		strings.TrimSpace(jsonStringOr(source["type"])) == "base64" {
		return jsonStringOr(source["data"])
	}
	return ""
}

// imageSourceValue resolves one image_url-shaped value: a string passes
// through, an object contributes its url (or image_url) field.
func imageSourceValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		return jsonStringOr(typed["url"], typed["image_url"])
	}
	return ""
}

// ---------------------------------------------------------------------------
// Dispatch entry points (wired from executor.go).
// ---------------------------------------------------------------------------

// executeResponses is the non-stream Responses turn: the image_generation
// tool routes to the image pipeline, everything else reuses the text turn
// start (sentinel + conversation SSE + the 401 refresh-retry loop) and
// answers with the response.completed envelope object (the reference's
// collect_response).
func (m *Manager) executeResponses(req executorRequest) ([]byte, error) {
	if imageTurn, apiErr := m.prepareResponsesImageTurn(req); apiErr != nil {
		return errEnvelope(apiErr), nil
	} else if imageTurn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), turnBudget(imageTurn.cfg))
		defer cancel()
		results, runErr := m.runImageTurn(ctx, imageTurn, req.HostCallbackID)
		payload, apiErr := finalizeResponsesImageTurn(imageTurn, results, runErr)
		if apiErr != nil {
			return errEnvelope(apiErr), nil
		}
		return ok(pluginapi.ExecutorResponse{Payload: payload}), nil
	}

	turn, apiErr := m.prepareResponseTextTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	defer cancel()

	if turn.searchMode {
		result, apiErr := m.runSearchTurn(ctx, turn, req.HostCallbackID)
		if apiErr != nil {
			return errEnvelope(apiErr), nil
		}
		cited, _, apiErr := searchCitedAnswer(result)
		if apiErr != nil {
			return errEnvelope(apiErr), nil
		}
		envelope := responseObject(turn.turnID, turn.modelID, turn.created, "completed",
			responseTextItems(turn, cited),
			responsesUsage(turn.promptTokens, 0, estimateTokens(cited), 0))
		raw, _ := json.Marshal(envelope)
		return ok(pluginapi.ExecutorResponse{Payload: raw}), nil
	}

	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		return errEnvelope(err), nil
	}
	payload, err := assembleResponseCompletion(ch, turn)
	if err != nil {
		return errEnvelope(err), nil
	}
	return ok(pluginapi.ExecutorResponse{Payload: payload}), nil
}

// executeResponsesStream is the stream Responses turn: the image route runs
// non-streaming and emits the buffered item sequence (images are not
// token-streamable); function-tool turns buffer like the chat format's
// ruling #3; plain text turns get the delta pump. Every path answers an
// empty result immediately and closes the downstream stream exactly once.
func (m *Manager) executeResponsesStream(req executorRequest) ([]byte, error) {
	if imageTurn, apiErr := m.prepareResponsesImageTurn(req); apiErr != nil {
		return errEnvelope(apiErr), nil
	} else if imageTurn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), turnBudget(imageTurn.cfg))
		results, runErr := m.runImageTurn(ctx, imageTurn, req.HostCallbackID)
		if _, apiErr := finalizeResponsesImageTurn(imageTurn, results, runErr); apiErr != nil {
			// The drain already ran: close once with the typed text and do
			// NOT re-run the pipeline (the openai-image stream precedent).
			m.closeImageStreamOnce(req.StreamID, envelopeText(apiErr), cancel)
			return ok(map[string]any{}), nil
		}
		m.emitResponsesImageStream(req.StreamID, imageTurn, results, cancel)
		return ok(map[string]any{}), nil
	}

	turn, apiErr := m.prepareResponseTextTurn(req)
	if apiErr != nil {
		return errEnvelope(apiErr), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))

	if turn.searchMode {
		// Streaming search is buffered (the whole run completes inside
		// Handle, as tool turns are) and emits the item sequence once.
		result, apiErr := m.runSearchTurn(ctx, turn, req.HostCallbackID)
		if apiErr != nil {
			cancel()
			m.closeResponsesStreamOnce(req.StreamID, envelopeText(apiErr), cancel)
			return ok(map[string]any{}), nil
		}
		cited, _, apiErr := searchCitedAnswer(result)
		if apiErr != nil {
			cancel()
			m.closeResponsesStreamOnce(req.StreamID, envelopeText(apiErr), cancel)
			return ok(map[string]any{}), nil
		}
		m.emitResponsesSearchStream(req.StreamID, turn, cited, cancel)
		return ok(map[string]any{}), nil
	}

	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		cancel()
		return errEnvelope(err), nil
	}

	if turn.toolMode {
		// Tool requests are not token-streamed (the chat format's ruling
		// #3): drain to completion inside Handle, then emit the item
		// sequence in one shot.
		m.emitBufferedResponseToolStream(req.StreamID, ch, turn, cancel)
		return ok(map[string]any{}), nil
	}
	go m.pumpResponseStream(req.StreamID, ch, turn, cancel)
	return ok(map[string]any{}), nil
}

// closeResponsesStreamOnce closes the downstream stream exactly once carrying
// the typed error text (the text executor's closeStream convention).
func (m *Manager) closeResponsesStreamOnce(downstreamID, msg string, cancelBudget context.CancelFunc) {
	var closeOnce sync.Once
	closeOnce.Do(func() {
		cancelBudget()
		_, _ = m.host.Call(pluginabi.MethodHostStreamClose, streamCloseWire(downstreamID, msg))
	})
}

// prepareResponseTextTurn shapes one Responses text turn: the chat format's
// textTurn with the Responses decode (instructions + input → messages) and
// the resp_<hex> turn id standing in for the chatcmpl- id — every frame of
// the turn shares it. created carries the envelope's created_at.
func (m *Manager) prepareResponseTextTurn(req executorRequest) (*textTurn, *pluginabi.Error) {
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
		turnID:       respID(),
		created:      time.Now().Unix(),
		hardCap:      sseHardCapFor(cfg),
		authFileName: execAuthFileName(req),
	}

	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}

	// Web-search detection mirrors the chat format (the Responses request
	// carries web_search tools under the same "tools" field); it runs
	// before the tools refusal.
	searchMode := isWebSearchRequest(body, slug)

	messages, effort, toolMode, shapeErr := shapeResponseRequest(body, searchMode)
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
		// The search flow posts its own bodies; the cited answer renders
		// into the normal Responses envelope.
		if apiErr := prepareSearchTurn(turn, messages); apiErr != nil {
			return nil, apiErr
		}
		return turn, nil
	}

	if effort == "" {
		// The config default applies when the request carries no thinking
		// effort spelling (auto/none → omitted upstream).
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
		return nil, typedErr("invalid_request", "malformed responses messages: "+err.Error(), 400)
	}
	turn.payload = payload

	return turn, nil
}

// ---------------------------------------------------------------------------
// Non-stream assembly: the output items and the response.completed object.
// ---------------------------------------------------------------------------

// assembleResponseCompletion drains the event channel (reusing the chat
// format's zero-content typed 502 and budget-timeout contracts) and answers
// with the Responses envelope object — the reference's collect_response
// returns the response.completed event's response block.
func assembleResponseCompletion(events <-chan upstream.ConvEvent, turn *textTurn) ([]byte, *pluginabi.Error) {
	text, err := drainTextCompletion(events, turn)
	if err != nil {
		return nil, err
	}
	envelope := responseObject(turn.turnID, turn.modelID, turn.created, "completed",
		responseTextItems(turn, text),
		responsesUsage(turn.promptTokens, 0, estimateTokens(text), 0))
	raw, _ := json.Marshal(envelope)
	return raw, nil
}

// responseTextItems renders the assembled reply into the envelope's output
// items: the markup-stripped prose as the message item (always present — an
// all-markup reply with no parsed calls keeps an empty message item rather
// than an empty output), followed by the function_call items in tool mode
// (each with its fresh call_<uuid> id under call_id).
func responseTextItems(turn *textTurn, text string) []map[string]any {
	content, toolCalls, _ := resolveAnswer(turn, text)
	items := make([]map[string]any, 0, 1+len(toolCalls))
	if content != "" {
		items = append(items, responseMessageItem(content, msgID(), "completed"))
	}
	for _, call := range toolCalls {
		fn := call["function"].(map[string]any)
		items = append(items, responseFunctionCallItem(
			jsonStringOr(fn["name"]), jsonStringOr(fn["arguments"]), jsonStringOr(call["id"])))
	}
	if len(items) == 0 {
		items = append(items, responseMessageItem("", msgID(), "completed"))
	}
	return items
}

// responseMessageItem is the reference's text_output_item: one assistant
// message with a single output_text part and empty annotations.
func responseMessageItem(text, itemID, status string) map[string]any {
	return map[string]any{
		"id":     itemID,
		"type":   "message",
		"status": status,
		"role":   "assistant",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

// responseFunctionCallItem is one OpenAI function_call output item: the
// reply's parsed call with its fresh call_<uuid> id riding under call_id.
func responseFunctionCallItem(name, arguments, callID string) map[string]any {
	return map[string]any{
		"id":        "fc_" + uuidHex(),
		"call_id":   callID,
		"type":      "function_call",
		"name":      name,
		"arguments": arguments,
		"status":    "completed",
	}
}

// ---------------------------------------------------------------------------
// The stream pumps.
// ---------------------------------------------------------------------------

// responseEventFrame renders one Responses SSE frame: "data: <event>\n\n".
// The host writes every emitted payload into the SSE body verbatim — the
// chat format's chunk contract. There is NO [DONE] sentinel:
// response.completed is the Responses terminator.
func responseEventFrame(event map[string]any) []byte {
	raw, _ := json.Marshal(event)
	return sseFrame(raw)
}

// responseCreatedEvent is the reference's response_created (lines 248-262).
func responseCreatedEvent(id, model string, created int64) map[string]any {
	return map[string]any{
		"type":     "response.created",
		"response": responseObject(id, model, created, "in_progress", []map[string]any{}, nil),
	}
}

// responseCompletedEvent is the reference's response_completed (lines
// 265-288) as an SSE event.
func responseCompletedEvent(id, model string, created int64, output []map[string]any, usage map[string]any) map[string]any {
	return map[string]any{
		"type":     "response.completed",
		"response": responseObject(id, model, created, "completed", output, usage),
	}
}

func outputItemAddedEvent(outputIndex int, item map[string]any) map[string]any {
	return map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item}
}

func outputItemDoneEvent(outputIndex int, item map[string]any) map[string]any {
	return map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item}
}

// outputItemInProgress is the added-event variant of a completed output
// item (the reference announces text items in_progress before done).
func outputItemInProgress(item map[string]any) map[string]any {
	progress := make(map[string]any, len(item)+1)
	for key, value := range item {
		progress[key] = value
	}
	progress["status"] = "in_progress"
	return progress
}

// pumpResponseStream converts the ConvEvent channel into the Responses SSE
// sequence: response.created and the in_progress message item go out at
// pump start (the reference yields both before its first delta, lines
// 307-308), one output_text.delta per assistant delta, then output_text.done
// → output_item.done → response.completed with the usage estimate.
// host.stream.close is issued exactly once on every exit path, as in the
// chat pump (success, error, zero content, content policy, emit failure,
// budget cancel); there is no [DONE] frame.
func (m *Manager) pumpResponseStream(downstreamID string, events <-chan upstream.ConvEvent, turn *textTurn, cancelBudget context.CancelFunc) {
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

	itemID := msgID()
	if !emit(responseEventFrame(responseCreatedEvent(turn.turnID, turn.modelID, turn.created))) {
		return
	}
	if !emit(responseEventFrame(outputItemAddedEvent(0, responseMessageItem("", itemID, "in_progress")))) {
		return
	}

	for ev := range events {
		switch ev.Kind {
		case upstream.ConvEventAssistantDelta:
			turn.text += ev.Delta
			if !emit(responseEventFrame(outputTextDeltaEvent(itemID, ev.Delta))) {
				return
			}
		case upstream.ConvEventComplete:
			turn.text = ev.Text
			if ev.Blocked {
				closeStream(envelopeText(contentPolicyViolation()))
				return
			}
			if ev.Text == "" {
				// The chat format's zero-content contract in stream form:
				// never a terminal event for an empty answer — one typed
				// close instead (the header frames already went out).
				closeStream(envelopeText(noAssistantContent()))
				return
			}
			item := responseMessageItem(turn.text, itemID, "completed")
			usage := responsesUsage(turn.promptTokens, 0, estimateTokens(turn.text), 0)
			for _, event := range []map[string]any{
				outputTextDoneEvent(itemID, turn.text),
				outputItemDoneEvent(0, item),
				responseCompletedEvent(turn.turnID, turn.modelID, turn.created, []map[string]any{item}, usage),
			} {
				if !emit(responseEventFrame(event)) {
					return
				}
			}
			return // the deferred closeStream("") runs once, cleanly
		case upstream.ConvEventError:
			mapped := mapUpstreamError(ev.Err)
			closeStream(fmt.Sprintf("%s (http %d)", envelopeText(mapped), mapped.HTTPStatus))
			return
		}
		// ConvEventRaw / ConvEventMessageID: the patch protocol already
		// filtered raw; the item was announced at pump start — nothing to
		// do per event (unlike the chat pump's role-chunk trigger).
	}
	// Channel closed without a terminal event: the turn budget expired
	// mid-stream — typed 504, the turn may still run upstream.
	closeStream(envelopeText(streamTimeoutEnvelope()))
}

func outputTextDeltaEvent(itemID, delta string) map[string]any {
	return map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         delta,
	}
}

func outputTextDoneEvent(itemID, text string) map[string]any {
	return map[string]any{
		"type":          "response.output_text.done",
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"text":          text,
	}
}

// emitBufferedResponseToolStream is the responses streaming-with-function-
// tools path (buffered, as the chat format's ruling #3): the whole upstream
// turn drains first, then the event sequence goes out — response.created →
// per output item (message prose first, then the parsed function calls)
// output_item.added / output_item.done → response.completed with usage.
// host.stream.close is issued exactly once on every exit path; no [DONE].
func (m *Manager) emitBufferedResponseToolStream(downstreamID string, events <-chan upstream.ConvEvent, turn *textTurn, cancelBudget context.CancelFunc) {
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

	text, err := drainTextCompletion(events, turn)
	if err != nil {
		closeStream(fmt.Sprintf("%s (http %d)", envelopeText(err), err.HTTPStatus))
		return
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
	if !send(responseCompletedEvent(turn.turnID, turn.modelID, turn.created, items, usage)) {
		return
	}
}

// emitResponsesImageStream emits the buffered image event sequence after
// the (non-streamed) pipeline run: response.created → per image item
// output_item.added / output_item.done → response.completed with usage.
// host.stream.close is issued exactly once on every exit path; no [DONE].
// The added event makes the reference's stream_image_response item
// announcement explicit (the reference omits it for image items; real
// OpenAI emits added before every done).
func (m *Manager) emitResponsesImageStream(downstreamID string, turn *imageTurn, results []upstream.ImageResult, cancelBudget context.CancelFunc) {
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

	id := respID()
	created := time.Now().Unix()
	if !send(responseCreatedEvent(id, turn.modelID, created)) {
		return
	}
	items, outputTokens := responsesImageItems(results, turn.prompt)
	for index, item := range items {
		if !send(outputItemAddedEvent(index, outputItemInProgress(item))) {
			return
		}
		if !send(outputItemDoneEvent(index, item)) {
			return
		}
	}
	if !send(responseCompletedEvent(id, turn.modelID, created, items, responsesImageUsage(turn, outputTokens))) {
		return
	}
}

// ---------------------------------------------------------------------------
// Image-run finalization: the image_generation_call output items.
// ---------------------------------------------------------------------------

// responsesImageItems renders the pipeline results as image_generation_call
// items with the reference's ig_<n> ids and base64 results
// (image_output_items, lines 233-245); the second return is the output
// image bytes/4 for the usage estimate.
func responsesImageItems(results []upstream.ImageResult, prompt string) ([]map[string]any, int) {
	items := make([]map[string]any, 0)
	outputTokens := 0
	for _, result := range results {
		revised := strings.TrimSpace(result.RevisedPrompt)
		if revised == "" {
			revised = prompt // the reference's `or prompt` fallback
		}
		for _, img := range result.Images {
			outputTokens += len(img.Data) / 4
			items = append(items, map[string]any{
				"id":             fmt.Sprintf("ig_%d", len(items)+1),
				"type":           "image_generation_call",
				"status":         "completed",
				"result":         base64.StdEncoding.EncodeToString(img.Data),
				"revised_prompt": revised,
			})
		}
	}
	return items, outputTokens
}

// responsesImageUsage is the documented estimate in the reference's image
// usage shape (image_usage → token_usage): prompt text + reference image
// bytes on the input side, output image bytes on the output side.
func responsesImageUsage(turn *imageTurn, outputImageTokens int) map[string]any {
	inputImageTokens := 0
	for _, ref := range turn.references {
		inputImageTokens += len(ref.Data) / 4
	}
	return responsesUsage(len(turn.prompt)/4, inputImageTokens, 0, outputImageTokens)
}

// finalizeResponsesImageTurn renders one image run outcome as either the
// Responses envelope (image_generation_call output items + usage) or a
// typed error. Every typed image outcome is a typed error here — the
// responses format has no byte-less data[] fallback (the openai-image
// executor's message-as-error-off path does not apply: behind the registry
// host a byte-less entry surfaces as a 502 anyway).
func finalizeResponsesImageTurn(turn *imageTurn, results []upstream.ImageResult, runErr error) ([]byte, *pluginabi.Error) {
	if runErr != nil {
		if errors.Is(runErr, errImageRefreshFailed) {
			return nil, typedErr("invalid_token",
				"the upstream rejected the bearer token and the forced refresh failed", http.StatusUnauthorized)
		}
		var imgErr *upstream.ImageError
		if errors.As(runErr, &imgErr) {
			switch imgErr.Code {
			case upstream.ImageCodeContentPolicy, upstream.ImageCodeTextReply:
				return nil, typedErr("content_policy_violation", imgErr.Message, http.StatusBadRequest)
			}
		}
		return nil, mapImageError(runErr)
	}
	items, outputTokens := responsesImageItems(results, turn.prompt)
	envelope := responseObject(respID(), turn.modelID, time.Now().Unix(), "completed",
		items, responsesImageUsage(turn, outputTokens))
	raw, _ := json.Marshal(envelope)
	return raw, nil
}
