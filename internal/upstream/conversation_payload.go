package upstream

// conversation_payload.go ports the reference's conversation request body
// builder (openai_backend_api.py _conversation_payload +
// _api_messages_to_conversation_messages + _normalize_thinking_effort). The
// body keeps the reference's byte-identical fingerprint fields (Global
// Constraints: timezone "Asia/Shanghai" for authed calls,
// timezone_offset_min -480, the pinned client_contextual_info screen
// geometry, variant_purpose "comparison_implicit", force_use_sse true,
// history_and_training_disabled true); TestPayloadGolden pins the serialized
// bytes against a fixture generated with the reference's own json.dumps
// default dialect.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The oracle's _chat_target: authed calls go to /backend-api/conversation
// with the Asia/Shanghai timezone payload field, anonymous calls to the
// /backend-anon/ variant with America/Los_Angeles.
const (
	conversationPath         = "/backend-api/conversation"
	defaultConversationTZ    = "Asia/Shanghai"
	anonConversationTimezone = "America/Los_Angeles"
)

// ConversationMessage is one OpenAI-shaped chat message for a conversation
// turn. Content is the plain text the oracle posts: the text payload path
// never carries image or tool parts (image inputs travel the conduit
// prepare flow of the drawing route, not this builder).
type ConversationMessage struct {
	Role    string // "system" | "user" | "assistant"; "" defaults to "user" like the oracle
	Content string
}

// ConversationRequest is everything BuildConversationPayload needs. The
// caller resolves thinking_effort (request override or config default) before
// building; the builder only normalizes the vocabulary.
type ConversationRequest struct {
	Messages []ConversationMessage
	Model    string
	// Timezone defaults to "Asia/Shanghai" (the authed target); the
	// anonymous path passes "America/Los_Angeles" per the oracle's
	// _chat_target.
	Timezone string
	// ThinkingEffort maps per NormalizeThinkingEffort; empty/none/auto
	// omit the field entirely.
	ThinkingEffort string
	// ParentMessageID defaults to a fresh uuid4 (the oracle always
	// generates one).
	ParentMessageID string
}

// conversationUUID is the uuid4 seam (headers.go newUUID); TestPayloadGolden
// pins it to a deterministic sequence.
var conversationUUID = newUUID

// NormalizeThinkingEffort ports the oracle's _normalize_thinking_effort:
// none/auto (and unknown values) omit the field; low/medium/high/standard/max
// pass through as-is; xhigh and extended collapse to "extended".
func NormalizeThinkingEffort(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "none", "auto":
		return ""
	case "low", "medium", "high", "standard", "max":
		return normalized
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}

// BuildConversationPayload builds the upstream POST body from the oracle's
// _conversation_payload: the fixed fingerprint block plus one fresh uuid4 per
// message id, parent_message_id and websocket_request_id. It returns a plain
// map (orderless); serializeConversationPayload renders it in the oracle's
// insertion order for the wire bytes.
func BuildConversationPayload(req ConversationRequest) (map[string]any, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("BuildConversationPayload: at least one message is required")
	}
	timezone := req.Timezone
	if strings.TrimSpace(timezone) == "" {
		timezone = defaultConversationTZ
	}

	messages := make([]any, 0, len(req.Messages))
	for _, item := range req.Messages {
		role := strings.TrimSpace(item.Role)
		if role == "" {
			role = "user" // the oracle's item.get("role", "user")
		}
		messages = append(messages, map[string]any{
			"id":     conversationUUID(),
			"author": map[string]any{"role": role},
			"content": map[string]any{
				"content_type": "text",
				"parts":        []any{item.Content},
			},
		})
	}

	parent := req.ParentMessageID
	if strings.TrimSpace(parent) == "" {
		parent = conversationUUID()
	}

	payload := map[string]any{
		"action":                        "next",
		"messages":                      messages,
		"model":                         req.Model,
		"parent_message_id":             parent,
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true,
		"history_and_training_disabled": true,
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"timezone":                      timezone,
		"timezone_offset_min":           -480,
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          conversationUUID(),
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 120,
			"page_height":       900,
			"page_width":        1400,
			"pixel_ratio":       2,
			"screen_height":     1440,
			"screen_width":      2560,
		},
	}
	if effort := NormalizeThinkingEffort(req.ThinkingEffort); effort != "" {
		payload["thinking_effort"] = effort
	}
	return payload, nil
}

// payloadKeyOrder is the oracle's _conversation_payload dict insertion order
// (thinking_effort last, because the oracle appends it after the fixed block).
var payloadKeyOrder = []string{
	"action",
	"messages",
	"model",
	"parent_message_id",
	"conversation_mode",
	"conversation_origin",
	"force_paragen",
	"force_paragen_model_slug",
	"force_rate_limit",
	"force_use_sse",
	"history_and_training_disabled",
	"reset_rate_limits",
	"suggestions",
	"supported_encodings",
	"system_hints",
	"timezone",
	"timezone_offset_min",
	"variant_purpose",
	"websocket_request_id",
	"client_contextual_info",
	"thinking_effort",
}

// serializeConversationPayload renders a built payload as the exact bytes the
// oracle posts: json.dumps default dialect (", " / ": " separators,
// ensure_ascii=True — appendPyJSONDefault) with dict keys in the oracle's
// insertion order (pyObj). Unknown keys (none from the builder) append
// sorted at the end.
func serializeConversationPayload(payload map[string]any) ([]byte, error) {
	known := make(map[string]bool, len(payloadKeyOrder))
	for _, key := range payloadKeyOrder {
		known[key] = true
	}
	obj := pyObj{}
	add := func(key string, value any) {
		obj.pairs = append(obj.pairs, pyPair{key: key, val: value})
	}
	for _, key := range payloadKeyOrder {
		value, ok := payload[key]
		if !ok {
			continue
		}
		switch key {
		case "messages":
			add(key, orderedMessages(value))
		case "conversation_mode":
			add(key, orderedPair(key, value, "kind"))
		case "client_contextual_info":
			add(key, orderedBlock(value, []string{
				"is_dark_mode", "time_since_loaded", "page_height", "page_width",
				"pixel_ratio", "screen_height", "screen_width",
			}))
		default:
			add(key, value)
		}
	}
	var extras []string
	for key := range payload {
		if !known[key] {
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	for _, key := range extras {
		add(key, payload[key])
	}

	body, err := appendPyJSONDefault(nil, obj)
	if err != nil {
		return nil, fmt.Errorf("serialize conversation payload: %w", err)
	}
	return body, nil
}

// orderedMessages renders the payload's message list in the oracle's
// {id, author{role}, content{content_type, parts}} shape and key order.
func orderedMessages(value any) any {
	items, _ := value.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		author, _ := msg["author"].(map[string]any)
		content, _ := msg["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		out = append(out, pyObj{pairs: []pyPair{
			{key: "id", val: msg["id"]},
			{key: "author", val: pyObj{pairs: []pyPair{{key: "role", val: author["role"]}}}},
			{key: "content", val: pyObj{pairs: []pyPair{
				{key: "content_type", val: content["content_type"]},
				{key: "parts", val: parts},
			}}},
		}})
	}
	return out
}

// orderedPair renders a one-key object ({kind}).
func orderedPair(key string, value any, field string) any {
	block, _ := value.(map[string]any)
	return pyObj{pairs: []pyPair{{key: field, val: block[field]}}}
}

// orderedBlock renders a fixed-shape nested object in the given key order,
// dropping absent keys.
func orderedBlock(value any, order []string) any {
	block, _ := value.(map[string]any)
	obj := pyObj{}
	for _, key := range order {
		if item, ok := block[key]; ok {
			obj.pairs = append(obj.pairs, pyPair{key: key, val: item})
		}
	}
	return obj
}
