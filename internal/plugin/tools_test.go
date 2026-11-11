package plugin

// tools_test.go pins the XML tool-emulation ports of the reference's
// services/protocol/anthropic_v1_messages.py: build_tool_prompt / _tool_meta
// / merge_system, parse_tool_calls / xml_value / parse_tool_params and
// strip_tool_markup. The XML format (tool_calls / tool_call / tool_name /
// parameters + CDATA) is the reference's EXACT contract, so the parser
// vectors here are the same shapes the upstream model is instructed to emit.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// callIDRe mirrors turnID's uuid4 convention: call_ + a dashed UUIDv4 hex.
var callIDRe = regexp.MustCompile(`^call_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ---------------------------------------------------------------------------
// buildToolPrompt / toolMeta.
// ---------------------------------------------------------------------------

func TestBuildToolPromptFunctionShape(t *testing.T) {
	prompt := buildToolPrompt([]any{
		map[string]any{
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
		},
	})
	// Go marshals map keys sorted, so the schema renders deterministically.
	want := toolPromptHeader + `Tool: get_weather
Description: Get the weather for a city
Parameters: {"properties":{"city":{"type":"string"}},"required":["city"],"type":"object"}` + "\n\n" + toolPromptFooter
	if prompt != want {
		t.Fatalf("prompt =\n%s\nwant\n%s", prompt, want)
	}
}

func TestBuildToolPromptFlatShape(t *testing.T) {
	prompt := buildToolPrompt([]any{
		map[string]any{
			"name":        "search",
			"description": "Search the web",
			"parameters":  map[string]any{"q": "string"},
		},
	})
	if !strings.Contains(prompt, "Tool: search\nDescription: Search the web\nParameters: {\"q\":\"string\"}") {
		t.Fatalf("prompt missing the flat-shape tool block:\n%s", prompt)
	}
	if !strings.HasPrefix(prompt, "You are the reasoning core") {
		t.Fatalf("prompt must open with the Available tools header:\n%s", prompt)
	}
}

func TestBuildToolPromptSkipsNamelessTools(t *testing.T) {
	prompt := buildToolPrompt([]any{
		map[string]any{"type": "web_search_preview"}, // built-in tool: no name
		map[string]any{"type": "function", "function": map[string]any{"name": "ok"}},
		"not-a-tool",
	})
	if strings.Contains(prompt, "web_search") {
		t.Fatalf("the nameless built-in tool leaked into the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Tool: ok") {
		t.Fatalf("the named tool is missing:\n%s", prompt)
	}
}

func TestBuildToolPromptEmpty(t *testing.T) {
	if prompt := buildToolPrompt(nil); prompt != "" {
		t.Fatalf("nil tools must build no prompt, got %q", prompt)
	}
	if prompt := buildToolPrompt([]any{}); prompt != "" {
		t.Fatalf("an empty tools array must build no prompt, got %q", prompt)
	}
	if prompt := buildToolPrompt([]any{map[string]any{"type": "web_search_preview"}}); prompt != "" {
		t.Fatalf("all-nameless tools must build no prompt, got %q", prompt)
	}
}

// ---------------------------------------------------------------------------
// shapeTools — the refusal boundary.
// ---------------------------------------------------------------------------

func TestShapeToolsRefusal(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		prompt, mode, err := shapeTools(nil)
		if prompt != "" || mode || err != nil {
			t.Fatalf("absent tools = (%q, %v, %+v), want no tools", prompt, mode, err)
		}
	})
	t.Run("null", func(t *testing.T) {
		prompt, mode, err := shapeTools([]byte("null"))
		if prompt != "" || mode || err != nil {
			t.Fatalf("null tools = (%q, %v, %+v), want no tools", prompt, mode, err)
		}
	})
	t.Run("empty array", func(t *testing.T) {
		prompt, mode, err := shapeTools([]byte("[]"))
		if prompt != "" || mode || err != nil {
			t.Fatalf("empty tools array = (%q, %v, %+v), want no tools", prompt, mode, err)
		}
	})
	t.Run("non-array is refused", func(t *testing.T) {
		_, _, err := shapeTools([]byte(`{"name":"x"}`))
		if err == nil || err.Code != "tools_unsupported" || err.HTTPStatus != 400 {
			t.Fatalf("non-array tools = %+v, want typed 400 tools_unsupported", err)
		}
	})
	t.Run("no usable name is refused", func(t *testing.T) {
		_, _, err := shapeTools([]byte(`[{"type":"function"},{"type":"web_search_preview"}]`))
		if err == nil || err.Code != "tools_unsupported" || err.HTTPStatus != 400 {
			t.Fatalf("all-nameless tools = %+v, want typed 400 tools_unsupported", err)
		}
	})
	t.Run("well-formed function tools are emulated", func(t *testing.T) {
		prompt, mode, err := shapeTools([]byte(`[{"type":"function","function":{"name":"get_weather"}}]`))
		if err != nil || !mode {
			t.Fatalf("function tools = (%v, %v, %+v), want tool mode", prompt, mode, err)
		}
		if !strings.Contains(prompt, "Tool: get_weather") {
			t.Fatalf("prompt missing the tool:\n%s", prompt)
		}
	})
}

// ---------------------------------------------------------------------------
// mergeSystemPrompt.
// ---------------------------------------------------------------------------

func TestMergeSystemPrompt(t *testing.T) {
	t.Run("empty extra is a no-op", func(t *testing.T) {
		in := []upstream.ConversationMessage{{Role: "user", Content: "hi"}}
		if out := mergeSystemPrompt(in, ""); len(out) != 1 {
			t.Fatalf("empty extra changed the messages: %+v", out)
		}
	})
	t.Run("appends to an existing system message", func(t *testing.T) {
		in := []upstream.ConversationMessage{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "hi"},
		}
		out := mergeSystemPrompt(in, "TOOL PROMPT")
		if len(out) != 2 || out[0].Content != "Be brief.\n\nTOOL PROMPT" || out[0].Role != "system" {
			t.Fatalf("merged = %+v, want the system message extended in place", out)
		}
	})
	t.Run("an empty system message is replaced", func(t *testing.T) {
		in := []upstream.ConversationMessage{
			{Role: "system", Content: "  "},
			{Role: "user", Content: "hi"},
		}
		out := mergeSystemPrompt(in, "TOOL PROMPT")
		if out[0].Content != "TOOL PROMPT" {
			t.Fatalf("merged system content = %q, want just the prompt (reference merge_system)", out[0].Content)
		}
	})
	t.Run("creates a leading system message when absent", func(t *testing.T) {
		in := []upstream.ConversationMessage{{Role: "user", Content: "hi"}}
		out := mergeSystemPrompt(in, "TOOL PROMPT")
		if len(out) != 2 || out[0].Role != "system" || out[0].Content != "TOOL PROMPT" || out[1].Content != "hi" {
			t.Fatalf("merged = %+v, want a new leading system message", out)
		}
	})
}

// ---------------------------------------------------------------------------
// foldAssistantToolCalls — the agent-loop replay of a prior assistant turn.
// ---------------------------------------------------------------------------

func TestFoldAssistantToolCalls(t *testing.T) {
	folded := foldAssistantToolCalls("Let me check.", []shapeAssistantToolCall{
		{Function: shapeToolFunction{Name: "get_weather", Arguments: `{}`}},
	})
	want := "Let me check.\n<tool_calls>\n" + toolCallToken + "\n<tool_name>get_weather</tool_name>\n<parameters>{}</parameters>\n" + toolCallCloseToken + "\n</tool_calls>"
	if folded != want {
		t.Fatalf("folded =\n%s\nwant\n%s", folded, want)
	}
}

// ---------------------------------------------------------------------------
// parseToolCalls + the XML helpers.
// ---------------------------------------------------------------------------

func TestParseToolCallsCanonicalXML(t *testing.T) {
	text := `I will check.
<tool_calls>
<tool_call>
<tool_name>get_weather</tool_name>
<parameters>{"city":"Paris","units":"celsius"}</parameters>
</tool_call>
</tool_calls>`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want exactly one", calls)
	}
	if calls[0].name != "get_weather" {
		t.Fatalf("call name = %q, want get_weather", calls[0].name)
	}
	if city, _ := calls[0].args["city"].(string); city != "Paris" {
		t.Fatalf("city arg = %v, want Paris (args=%+v)", calls[0].args["city"], calls[0].args)
	}
}

func TestParseToolCallsCDATAParams(t *testing.T) {
	// The reference's output contract puts parameters in CDATA; the JSON
	// inside must survive the unwrap.
	text := `<tool_calls>
<tool_call>
<tool_name>search</tool_name>
<parameters><![CDATA[{"q": "hello", "n": 5}]]></parameters>
</tool_call>
</tool_calls>`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if q, _ := calls[0].args["q"].(string); q != "hello" {
		t.Fatalf("q arg = %v, want hello (args=%+v)", calls[0].args["q"], calls[0].args)
	}
	if n, _ := calls[0].args["n"].(float64); n != 5 {
		t.Fatalf("n arg = %v, want 5", calls[0].args["n"])
	}
}

func TestParseToolCallsCDATAValues(t *testing.T) {
	// Per-parameter CDATA values (the reference example shape).
	text := `<tool_calls>
<tool_call>
<tool_name>search</tool_name>
<parameters><q><![CDATA[hello &amp; world]]></q><n><![CDATA[5]]></n></parameters>
</tool_call>
</tool_calls>`
	calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if q, _ := calls[0].args["q"].(string); q != "hello & world" {
		t.Fatalf("q arg = %q, want the html-unescaped value", calls[0].args["q"])
	}
	if n, _ := calls[0].args["n"].(float64); n != 5 {
		t.Fatalf("n arg = %v, want 5 (JSON-parsed from the unwrapped value)", calls[0].args["n"])
	}
}

func TestParseToolCallsVariantsAndFences(t *testing.T) {
	// The reference strips code fences BEFORE scanning, so a tool call
	// wrapped in a fence is dropped, not parsed.
	text := "```xml\n<function_call>\n<name>fenced</name>\n<input>{}</input>\n</function_call>\n```\n" +
		"<function_call>\n<name>alt_one</name>\n<input>{\"a\":1}</input>\n</function_call>\n" +
		"<invoke>\n<tool_name>alt_two</tool_name>\n<arguments>{}</arguments>\n</invoke>"
	calls := parseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want the unfenced function_call and the invoke variant (the fenced one is stripped)", calls)
	}
	if calls[0].name != "alt_one" || calls[1].name != "alt_two" {
		t.Fatalf("call names = %q, %q, want alt_one, alt_two", calls[0].name, calls[1].name)
	}
	if a, _ := calls[0].args["a"].(float64); a != 1 {
		t.Fatalf("input arg = %v, want 1", calls[0].args["a"])
	}
}

func TestParseToolCallsProseOnly(t *testing.T) {
	if calls := parseToolCalls("Plain prose, no markup."); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if calls := parseToolCalls(""); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none for empty text", calls)
	}
}

func TestXmlValue(t *testing.T) {
	t.Run("CDATA unwrap", func(t *testing.T) {
		got := xmlValue(`<city><![CDATA[Paris]]></city>`, "city")
		if got != "Paris" {
			t.Fatalf("xmlValue = %q, want the CDATA-unwrapped Paris", got)
		}
	})
	t.Run("html unescape", func(t *testing.T) {
		got := xmlValue(`<q>&quot;hello&quot; &amp; more</q>`, "q")
		if got != `"hello" & more` {
			t.Fatalf("xmlValue = %q, want the entity-unescaped value", got)
		}
	})
	t.Run("absent tag", func(t *testing.T) {
		if got := xmlValue("no tags here", "city"); got != "" {
			t.Fatalf("xmlValue = %q, want empty for an absent tag", got)
		}
	})
	t.Run("surrounding whitespace trimmed", func(t *testing.T) {
		got := xmlValue(`<a>  spaced  </a>`, "a")
		if got != "spaced" {
			t.Fatalf("xmlValue = %q, want spaced", got)
		}
	})
}

func TestParseToolParamsLenient(t *testing.T) {
	t.Run("plain JSON object", func(t *testing.T) {
		got := parseToolParams(`{"city":"Paris"}`)
		if city, _ := got["city"].(string); city != "Paris" {
			t.Fatalf("params = %+v, want the JSON object", got)
		}
	})
	t.Run("non-object JSON falls back to empty", func(t *testing.T) {
		if got := parseToolParams(`["a", 1]`); len(got) != 0 {
			t.Fatalf("params = %+v, want {} for non-object JSON (reference)", got)
		}
		if got := parseToolParams(`42`); len(got) != 0 {
			t.Fatalf("params = %+v, want {} for scalar JSON", got)
		}
	})
	t.Run("XML fallback for tag pairs", func(t *testing.T) {
		got := parseToolParams("<city>Paris</city><units>celsius</units>")
		if city, _ := got["city"].(string); city != "Paris" {
			t.Fatalf("params = %+v, want city=Paris from the XML fallback", got)
		}
		if units, _ := got["units"].(string); units != "celsius" {
			t.Fatalf("params = %+v, want units=celsius", got)
		}
	})
	t.Run("garbage yields empty", func(t *testing.T) {
		if got := parseToolParams("not json at all"); len(got) != 0 {
			t.Fatalf("params = %+v, want {} for unparseable text", got)
		}
	})
}

func TestStripToolMarkup(t *testing.T) {
	// The reference replaces the markup with "" and strips the ends: the
	// newlines around the block survive the removal.
	got := stripToolMarkup("Prose before.\n<tool_calls>\n<tool_call>\n<tool_name>x</tool_name>\n<parameters>{}</parameters>\n</tool_call>\n</tool_calls>\nProse after.")
	if got != "Prose before.\n\nProse after." {
		t.Fatalf("stripped = %q, want the prose with the markup removed", got)
	}
	// Every variant the reference strips.
	for _, markup := range []string{
		"<tool_calls>x</tool_calls>",
		"<tool_call>x</tool_call>",
		"<function_call>x</function_call>",
		"<invoke>x</invoke>",
	} {
		if got := stripToolMarkup(markup); got != "" {
			t.Fatalf("stripToolMarkup(%s) = %q, want empty", markup, got)
		}
	}
}

func TestCallIDFormat(t *testing.T) {
	if id := callID(); !callIDRe.MatchString(id) {
		t.Fatalf("call id = %q, want call_<32 hex>", id)
	}
}
