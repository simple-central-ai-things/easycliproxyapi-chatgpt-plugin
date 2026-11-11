package plugin

// tools.go is the OpenAI function-tools emulation for the text executor,
// ported from the reference's proven XML tool-emulation pattern
// (services/protocol/anthropic_v1_messages.py build_tool_prompt /
// _tool_meta / parse_tool_calls / xml_value / parse_tool_params /
// strip_tool_markup). The upstream ChatGPT conversation API has no tool
// surface, so tools are emulated over plain text:
//
//   - REQUEST: the tools array is rendered as a tool prompt (per-tool
//     schemas plus the variant-D master prompt (mechanics, scrambled
//     protocol, after-results rules) merged into the conversation's system
//     message(s); a role:"tool" result folds in as user text
//     ("Tool result {tool_call_id}: {content}"); a prior assistant
//     tool_calls message folds back into the reference's tool_use XML text
//     blocks so a replayed agent loop round-trips.
//   - REPLY: the assembled assistant text is scanned for the exact XML
//     (the scrambled single tool-call tag, the <tool_calls> wrapper,
//     tool_name, parameters + CDATA);
//     each resolvable call becomes an OpenAI tool_calls entry with a fresh
//     call_<uuid> id and the JSON-string arguments, finish_reason becomes
//     "tool_calls" and the prose keeps only the markup-stripped remainder.
//
// Only tools that carry a usable function name (the OpenAI
// {"type":"function","function":{...}} shape or the flat
// {"name","description","parameters"} shape) are emulated. A tools array
// where no tool has a name — e.g. built-in web-search tools — cannot be
// emulated and keeps the honest typed 400 tools_unsupported refusal.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// The reference's XML tool contract is built around one single tool-call
// tag (the one between <tool_calls> and </tool_calls>). The CANONICAL
// spelling is the scrambled anagram loot_llac: the ChatGPT web backend
// strips the standard single tool-call token from the assistant stream
// (live echo evidence, PROVENANCE.md 2026-11-11), so a standard-spelling
// contract can never round-trip. legacyToolCallCloseToken keeps the
// standard close accepted defensively by the parser. Every spelling is
// assembled from parts so no control sequence ever appears contiguously
// in this source: message pipelines (including agent transcripts)
// intercept them, and the golden tests depend on these bytes being stable.
const (
	toolCallToken            = "<" + "loot_llac" + ">"
	toolCallCloseToken       = "</" + "loot_llac" + ">"
	legacyToolCallCloseToken = "</" + "tool_call" + ">"
)

// toolPromptHeader / toolPromptFooter are the live-tuned variant-D master
// prompt (PROVENANCE.md 2026-11-11): the reference's short "Tool use rules"
// block is ignored by GPT-6-era models in the system message (0/11), while
// this master - mechanics, the scrambled-contract rationale and the
// after-results answer rules - emitted the protocol XML in 9/10 standalone
// turns and 5/5 clean completion answers. The per-tool schema blocks sit
// between header and footer.
const toolPromptHeader = "You are the reasoning core of a coding agent. Read this protocol carefully - it defines " +
	"the ONLY way you can act.\n\n" +
	"HOW ACTIONS WORK HERE:\n" +
	"- You have NO direct access to the user's files, shell, or machine. You cannot create, " +
	"read, or edit anything by yourself. Never pretend you did.\n" +
	"- The caller (the agent harness sitting in front of you) parses your reply as plain text. " +
	"When it sees the protocol XML below, it executes the action and sends you the result back " +
	"as a 'Tool result' message.\n" +
	"- If you reply with prose like 'I created a.txt', NOTHING happens. The ONLY thing that " +
	"causes an action is the exact XML below.\n\n" +
	"THE PROTOCOL (mandatory spelling):\n" +
	"- To call a tool, output ONLY the XML and no prose/markdown:\n" +
	"<tool_calls>\n" + toolCallToken + "\n" +
	"<tool_name>TOOL_NAME</tool_name>\n" +
	"<parameters><PARAM><![CDATA[value]]></PARAM></parameters>\n" +
	toolCallCloseToken + "\n</tool_calls>\n" +
	"- The tag names are deliberately scrambled. This is REQUIRED: the chat platform silently " +
	"strips the standard tool-call markup from your reply. Always use " + toolCallToken + " / " +
	toolCallCloseToken + " exactly.\n" +
	"- Put parameters under <parameters> using the exact schema names.\n" +
	"- Earlier assistant messages may carry platform <escape> markup; it is an artifact, never a parameter - every parameter goes under its own schema-named tag.\n\n" +
	"AVAILABLE TOOLS:\n"

const toolPromptFooter = "AFTER RESULTS\n" +
	"- When a 'Tool result' arrives, answer the user briefly in plain prose: what was done and " +
	"any key facts learned.\n" +
	"- Do not re-call a tool that already succeeded. Do not emit XML unless a further action is " +
	"needed.\n" +
	"- Never use internal markup (badges, icons, or escape-style tags) in your answers.\n\n" +
	"RULES:\n" +
	"- If the user asks to list/read/search files, inspect project state, run a command, or " +
	"answer from local code, you MUST call a suitable tool first. Do not say you cannot access " +
	"files - you access them THROUGH this protocol.\n" +
	"- After a tool result arrives, answer the user briefly. Do not re-call a tool that already " +
	"succeeded.\n" +
	"- Never claim a task is done without having emitted the tool-call XML for it."

// userBridge is the compact action-oriented reminder tool mode appends to
// the LAST user message. Live evidence (PROVENANCE.md 2026-11-11) on a
// captured OpenCode request (a 100KB client system prompt + 12 tools): the
// variant-D master merged into the system tail alone emitted in 0/3 turns,
// duplicating the master into the user tail in 2/5, in both in 3/5, while
// the master in the system plus this short reminder in the user reached 5/5.
// The bridge points at the master's protocol instead of repeating the schemas.
const userBridge = "PROTOCOL REMINDER - this request needs action, and your reply must carry it:\n" +
	"output ONLY the protocol XML block defined in your instructions, exactly as shown there " +
	"(the " + toolCallToken + " tag, <tool_name>, and <parameters> with CDATA values). A reply " +
	"without that XML block executes nothing."

// appendUserBridge appends the bridge to the last user message (the folded
// "Tool result" turn included) and leaves every other message untouched; it
// is a no-op when the conversation tail is not a user message, because the
// bridge must sit at the recency edge right before the model's next reply.
func appendUserBridge(messages []upstream.ConversationMessage) []upstream.ConversationMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) != "user" {
			return messages
		}
		messages[i].Content += "\n\n" + userBridge
		return messages
	}
	return messages
}

// ---------------------------------------------------------------------------
// Request shaping: the tool prompt.
// ---------------------------------------------------------------------------

// shapeTools decodes the request's tools array into the emulated tool prompt
// and the tool mode. Only tools with a usable function name are emulated;
// nameless tools are skipped. An array where no tool has a name — or a
// tools value that is not an array at all — is refused typed
// tools_unsupported BEFORE any upstream call.
func shapeTools(raw json.RawMessage) (prompt string, mode bool, err *pluginabi.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var tools []any
	if json.Unmarshal(raw, &tools) != nil {
		return "", false, typedErr("tools_unsupported",
			"the tools array must be a JSON array of function tools; only OpenAI function tools are supported (emulated via XML)", 400)
	}
	if len(tools) == 0 {
		return "", false, nil
	}
	prompt = buildToolPrompt(tools)
	if prompt == "" {
		return "", false, typedErr("tools_unsupported",
			"no tool carries a usable function name; only OpenAI function tools are supported (emulated via XML)", 400)
	}
	return prompt, true, nil
}

// buildToolPrompt renders the variant-D master prompt: the header, the
// per-tool schema blocks, and the footer (after-results + rules). Empty
// when no tool carries a name.
func buildToolPrompt(tools []any) string {
	var blocks []string
	for _, tool := range tools {
		name, description, schema := toolMeta(tool)
		if name == "" {
			continue
		}
		blocks = append(blocks, "Tool: "+name+"\nDescription: "+description+"\nParameters: "+jsonText(schema))
	}
	if len(blocks) == 0 {
		return ""
	}
	return toolPromptHeader + strings.Join(blocks, "\n") + "\n\n" + toolPromptFooter
}

// toolMeta reads one tool's name/description/schema the way the reference's
// _tool_meta does: the OpenAI {"type":"function","function":{...}} shape or
// the flat {"name","description","parameters"} shape (input_schema — the
// Anthropic spelling the reference tolerates — is accepted too).
func toolMeta(tool any) (name, description string, schema any) {
	obj, ok := tool.(map[string]any)
	if !ok {
		return "", "", map[string]any{}
	}
	fn, _ := obj["function"].(map[string]any)
	name = jsonStringOr(obj["name"], fn["name"])
	description = jsonStringOr(obj["description"], fn["description"])
	schema = firstJSONTruthy(obj["input_schema"], obj["parameters"], fn["input_schema"], fn["parameters"])
	return strings.TrimSpace(name), strings.TrimSpace(description), schema
}

// jsonStringOr is the reference's `str(x) or str(y)`: the first non-empty
// stringified value (scalars via fmt, strings verbatim).
func jsonStringOr(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return typed
			}
		case float64:
			return strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(typed)
		}
	}
	return ""
}

// firstJSONTruthy is the reference's `or` chain over JSON values: the first
// Python-truthy value, else an empty object (the reference's fallback).
func firstJSONTruthy(values ...any) any {
	for _, value := range values {
		switch typed := value.(type) {
		case nil:
		case string:
			if typed != "" {
				return value
			}
		case bool:
			if typed {
				return value
			}
		case float64:
			if typed != 0 {
				return value
			}
		case []any:
			if len(typed) > 0 {
				return value
			}
		case map[string]any:
			if len(typed) > 0 {
				return value
			}
		default:
			return value
		}
	}
	return map[string]any{}
}

// jsonText renders a JSON value for the prompt: the reference uses
// json.dumps(ensure_ascii=False) — Go's encoder with HTML escaping off is
// the faithful equivalent (non-ASCII stays UTF-8, keys render compact).
func jsonText(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// mergeSystemPrompt appends the tool prompt to the conversation's system
// message(s) the way the reference's merge_system does: the first system
// message gets "<system>\n\n<extra>" (or just the extra when its content is
// empty), and a new leading system message is created when the request has
// none.
func mergeSystemPrompt(messages []upstream.ConversationMessage, extra string) []upstream.ConversationMessage {
	if extra == "" {
		return messages
	}
	for i := range messages {
		if strings.TrimSpace(messages[i].Role) != "system" {
			continue
		}
		existing := strings.TrimSpace(messages[i].Content)
		if existing == "" {
			messages[i].Content = extra
		} else {
			messages[i].Content = existing + "\n\n" + extra
		}
		return messages
	}
	return append([]upstream.ConversationMessage{{Role: "system", Content: extra}}, messages...)
}

// ---------------------------------------------------------------------------
// Request shaping: the agent-loop round-trip folds.
// ---------------------------------------------------------------------------

// shapeToolFunction mirrors one OpenAI assistant tool_call's function block.
type shapeToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// shapeAssistantToolCall mirrors one OpenAI assistant tool_call entry the
// way the executor consumes it (the arguments are already the JSON string
// the client recorded; they are folded back verbatim).
type shapeAssistantToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function shapeToolFunction `json:"function"`
}

// foldAssistantToolCalls renders one assistant message's prior tool calls as
// the reference's tool_use XML text blocks so a replayed agent loop
// (assistant tool_calls → tool result → next turn) round-trips: the calls
// join the message text in request order, each as the exact template the
// reference emits for a tool_use block.
func foldAssistantToolCalls(prose string, calls []shapeAssistantToolCall) string {
	var builder strings.Builder
	builder.WriteString(prose)
	for _, call := range calls {
		builder.WriteString("\n<tool_calls>\n" + toolCallToken + "\n" +
			"<tool_name>" + call.Function.Name + "</tool_name>\n" +
			"<parameters>" + call.Function.Arguments + "</parameters>\n" +
			toolCallCloseToken + "\n</tool_calls>")
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// Reply parsing: the reference's XML tool contract.
// ---------------------------------------------------------------------------

// toolCall is one parsed assistant tool call: the resolved name and the
// leniently parsed parameter map.
type toolCall struct {
	name string
	args map[string]any
}

var (
	// codeFenceRe strips ```...``` fences the reference removes before
	// scanning (parse_tool_calls). The backticks are \u0060 escapes: a Go
	// raw string literal cannot contain them.
	codeFenceRe = regexp.MustCompile("(?is)\u0060{3}.*?\u0060{3}")
	// toolBlockRe finds every tool-call block in the scrambled canonical
	// spelling plus the three legacy spellings; each non-empty alternation
	// group is a block body (four capture groups, matched in order). The
	// standard-spelling close rides in via legacyToolCallCloseToken so no
	// control sequence ever appears contiguously in this source.
	toolBlockRe = regexp.MustCompile("(?is)<loot_llac\\b[^>]*>(.*?)" + toolCallCloseToken +
		"|<tool_call\\b[^>]*>(.*?)" + legacyToolCallCloseToken +
		"|<function_call\\b[^>]*>(.*?)</function_call>|<invoke\\b[^>]*>(.*?)</invoke>")
	// toolMarkupRe is the reference's strip_tool_markup: every markup block
	// (including the bare <tool_calls> wrapper) is removed from the prose.
	toolMarkupRe = regexp.MustCompile("(?is)<tool_calls\\b[^>]*>.*?</tool_calls>|<loot_llac\\b[^>]*>.*?</loot_llac>" +
		"|<tool_call\\b[^>]*>.*?" + legacyToolCallCloseToken +
		"|<function_call\\b[^>]*>.*?</function_call>|<invoke\\b[^>]*>.*?</invoke>|<escape\\b[^>]*/>|<escape\\b[^>]*>.*?</escape>")
	// cdataWrapRe is the reference's CDATA fullmatch over the stripped value.
	cdataWrapRe = regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)\]\]>$`)
	// xmlParamTagRe is the reference's XML fallback `<name>value</name>`
	// scan (RE2 has no backreference, so the tag-equality check is applied
	// post-match — see parseToolParams).
	xmlParamTagRe = regexp.MustCompile(`(?is)<([\w.-]+)\b[^>]*>(.*?)</([\w.-]+)>`)
)

// xmlTagCache memoizes the per-tag `<tag>...</tag>` regexes xmlValue builds.
var xmlTagCache sync.Map

// xmlValue ports the reference's xml_value: the first <tag>...</tag> value
// in the text, stripped, its CDATA section unwrapped, and HTML entities
// unescaped (the model may emit &lt;/&amp; inside values).
func xmlValue(text, tag string) string {
	regex, _ := xmlTagCache.LoadOrStore(tag,
		regexp.MustCompile(`(?is)<`+regexp.QuoteMeta(tag)+`\b[^>]*>(.*?)</`+regexp.QuoteMeta(tag)+`>`))
	match := regex.(*regexp.Regexp).FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if cdata := cdataWrapRe.FindStringSubmatch(value); cdata != nil {
		value = cdata[1]
	}
	return strings.TrimSpace(html.UnescapeString(value))
}

// parseToolCalls ports the reference's parse_tool_calls: code fences are
// stripped, then every single tool-call block (the scrambled canonical
// form, plus the standard and the <function_call> and <invoke> variants)
// is scanned and each block with
// a resolvable name yields one call. The name resolves from tool_name, name
// or function; the parameters from parameters, input or arguments
// (defaulting to "{}").
func parseToolCalls(text string) []toolCall {
	text = strings.TrimSpace(codeFenceRe.ReplaceAllString(text, ""))
	var calls []toolCall
	for _, match := range toolBlockRe.FindAllStringSubmatch(text, -1) {
		block := ""
		for _, part := range match[1:5] {
			if part != "" {
				block = part
				break
			}
		}
		name := xmlValue(block, "tool_name")
		if name == "" {
			name = xmlValue(block, "name")
		}
		if name == "" {
			name = xmlValue(block, "function")
		}
		if name == "" {
			continue
		}
		params := xmlValue(block, "parameters")
		if params == "" {
			params = xmlValue(block, "input")
		}
		if params == "" {
			params = xmlValue(block, "arguments")
		}
		if params == "" {
			params = "{}"
		}
		calls = append(calls, toolCall{name: name, args: parseToolParams(params)})
	}
	return calls
}

// parseToolParams ports the reference's parse_tool_params: a lenient JSON
// parse first (the documented output contract — a non-object yields {}),
// and when the parameters are not JSON an XML-tag fallback that reads
// <name>value</name> pairs, each value JSON-parsed when possible.
func parseToolParams(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		if parsed == nil {
			// JSON null decodes into a nil map; the reference's
			// isinstance(dict) check treats it as {}.
			return map[string]any{}
		}
		return parsed
	}
	params := map[string]any{}
	for _, match := range xmlParamTagRe.FindAllStringSubmatch(raw, -1) {
		if match[1] != match[3] {
			continue // the reference's backreference </\1>
		}
		params[match[1]] = parseToolValue(match[2])
	}
	// The model occasionally wraps the whole parameter object in the
	// platform's <escape> markup instead of schema-named tags (live
	// evidence 2026-11-12); a sole escape parameter carrying an object IS
	// that object.
	if len(params) == 1 {
		if obj, ok := params["escape"].(map[string]any); ok {
			return obj
		}
	}
	return params
}

// parseToolValue ports the reference's parse_tool_value: unwrap one fallback
// value (CDATA/entities via xmlValue) and JSON-parse it when it is JSON,
// else carry it verbatim as a string.
func parseToolValue(raw string) any {
	value := xmlValue("<x>"+raw+"</x>", "x")
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) == nil {
		return parsed
	}
	return value
}

// stripToolMarkup ports the reference's strip_tool_markup: every tool-call
// markup block is removed from the prose and the result trimmed.
func stripToolMarkup(text string) string {
	return strings.TrimSpace(toolMarkupRe.ReplaceAllString(text, ""))
}

// callID builds one OpenAI-style tool call id: call_ + a fresh UUIDv4 hex
// (the reference's uuid4 convention, same family as turnID's chatcmpl-).
func callID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("uuid generation: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("call_%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
