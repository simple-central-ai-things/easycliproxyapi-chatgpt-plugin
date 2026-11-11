package plugin

// scrambled_contract_test.go pins the scrambled tool-call spelling: the
// ChatGPT web backend strips the standard single tool-call token from the
// assistant stream (live echo evidence 2026-11-11, PROVENANCE.md), so the
// emitted contract (the tool prompt and the agent-loop fold) teaches the
// scrambled tag, and the canonical parser vector is scrambled too. The
// standard spelling stays accepted defensively by the parser and stripper.
// Every standard spelling is assembled from parts: message pipelines
// (including agent transcripts) intercept them as control sequences.

import (
	"strings"
	"testing"
)

// The scrambled token, assembled from parts like tools.go does.
const (
	scramOpen  = "<" + "loot_llac" + ">"
	scramClose = "</" + "loot_llac" + ">"
)

func TestParseToolCallsScrambledCanonical(t *testing.T) {
	text := "I will check.\n<tool_calls>\n" + scramOpen + "\n<tool" +
		"_name>get_weather</tool" + "_name>\n<param" +
		"eters>{\"city\":\"Paris\",\"units\":\"celsius\"}</param" +
		"eters>\n" + scramClose + "\n</tool_calls>"
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

func TestStripToolMarkupScrambled(t *testing.T) {
	got := stripToolMarkup("Prose before.\n<tool_calls>\n" + scramOpen + "\n<tool" +
		"_name>x</tool" + "_name>\n<param" + "eters>{}</param" +
		"eters>\n" + scramClose + "\n</tool_calls>\nProse after.")
	if got != "Prose before.\n\nProse after." {
		t.Fatalf("stripped = %q, want the prose with the scrambled markup removed", got)
	}
}

func TestFoldAssistantToolCallsEmitsScrambled(t *testing.T) {
	folded := foldAssistantToolCalls("Let me check.", []shapeAssistantToolCall{
		{Function: shapeToolFunction{Name: "get_weather", Arguments: `{}`}},
	})
	if !strings.Contains(folded, scramOpen) || !strings.Contains(folded, scramClose) {
		t.Fatalf("folded must emit the scrambled contract:\n%s", folded)
	}
	stdOpen := "<" + "tool" + "_cal" + "l" + ">"
	if strings.Contains(folded, stdOpen) {
		t.Fatalf("folded must not emit the platform-stripped standard spelling")
	}
}

func TestToolPromptMasterSections(t *testing.T) {
	prompt := buildToolPrompt([]any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "write",
			"description": "Write content to a file",
			"parameters":  map[string]any{"required": []any{"path", "content"}},
		}},
	})
	for _, section := range []string{
		"You are the reasoning core", "HOW ACTIONS WORK HERE:",
		"THE PROTOCOL (mandatory spelling):", "AVAILABLE TOOLS:",
		"AFTER RESULTS", "RULES:",
	} {
		if !strings.Contains(prompt, section) {
			t.Fatalf("prompt missing section %q:\n%s", section, prompt)
		}
	}
	if !strings.Contains(prompt, scramOpen) || !strings.Contains(prompt, scramClose) {
		t.Fatalf("prompt must carry the scrambled contract")
	}
	stdOpen := "<" + "tool" + "_cal" + "l" + ">"
	if strings.Contains(prompt, stdOpen) {
		t.Fatalf("prompt must not teach the platform-stripped standard spelling")
	}
}

func TestToolPromptAfterResultsRules(t *testing.T) {
	prompt := buildToolPrompt([]any{
		map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	})
	for _, rule := range []string{
		"Never use internal markup (badges, icons, or escape-style tags) in your answers.",
		"Do not re-call a tool that already succeeded.",
	} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("prompt missing the answer-quality rule %q:\n%s", rule, prompt)
		}
	}
}
