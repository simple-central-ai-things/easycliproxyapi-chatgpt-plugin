package plugin

// escape_artifact_test.go pins the platform <escape> markup handling:
// stripToolMarkup removes escape blocks from prose, parseToolParams
// unwraps a sole escape parameter carrying the whole object, and the
// master prompt tells the model the markup is an artifact.

import (
	"strings"
	"testing"
)

func TestStripToolMarkupEscapeArtifact(t *testing.T) {
	for _, markup := range []string{
		"<escape></escape>",
		"<escape>helloworld</escape>",
		"<escape/>",
	} {
		if got := stripToolMarkup(markup); got != "" {
			t.Fatalf("stripToolMarkup(%s) = %q, want empty", markup, got)
		}
	}
	if got := stripToolMarkup("Before <escape>x</escape> after."); got != "Before  after." {
		t.Fatalf("stripped = %q, want the escape block removed", got)
	}
}

func TestParseToolParamsEscapeWrap(t *testing.T) {
	got := parseToolParams(`<escape>{"patchText": "*** Begin Patch"}</escape>`)
	if pt, _ := got["patchText"].(string); pt != "*** Begin Patch" {
		t.Fatalf("params = %+v, want the escape-wrapped object unwrapped", got)
	}
	got = parseToolParams(`<escape><![CDATA[{"patchText": "x"}]]></escape>`)
	if pt, _ := got["patchText"].(string); pt != "x" {
		t.Fatalf("CDATA escape wrap = %+v, want patchText=x", got)
	}
	// a schema-named parameter alongside is unaffected
	got = parseToolParams(`<patchText>p</patchText><escape>e</escape>`)
	if pt, _ := got["patchText"].(string); pt != "p" || len(got) != 2 {
		t.Fatalf("mixed params = %+v, want both kept", got)
	}
}

func TestParseToolCallsEscapedParamsBlock(t *testing.T) {
	text := "<tool_calls>\n" + scramOpen + "\n<tool" + "_name>patch</tool" +
		"_name>\n<param" + `eters><escape>{"patchText": "x"}</escape></param` +
		`eters>\n` + scramClose + "\n</tool_calls>"
	calls := parseToolCalls(text)
	if len(calls) != 1 || calls[0].name != "patch" {
		t.Fatalf("calls = %+v, want one patch call", calls)
	}
	if pt, _ := calls[0].args["patchText"].(string); pt != "x" {
		t.Fatalf("args = %+v, want patchText=x unwrapped", calls[0].args)
	}
}

func TestMasterPromptNamesEscapeArtifact(t *testing.T) {
	if !strings.Contains(toolPromptHeader, "<escape>") ||
		!strings.Contains(toolPromptHeader, "artifact") {
		t.Fatalf("the master prompt must tell the model escape markup is an artifact:\n%s", toolPromptHeader)
	}
}
