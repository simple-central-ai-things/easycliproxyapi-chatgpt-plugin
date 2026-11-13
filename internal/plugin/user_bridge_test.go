package plugin

// user_bridge_test.go pins the compact PROTOCOL REMINDER bridge that tool
// mode appends to the last user message (see appendUserBridge in tools.go).

import (
	"strings"
	"testing"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

func TestUserBridgeConstant(t *testing.T) {
	if !strings.Contains(userBridge, "PROTOCOL REMINDER") {
		t.Fatalf("userBridge must be the compact reminder:\n%s", userBridge)
	}
	if !strings.Contains(userBridge, scramOpen) {
		t.Fatalf("userBridge must reference the scrambled contract tag")
	}
	if len(userBridge) > 800 {
		t.Fatalf("userBridge must stay compact (this is a reminder, not the master): %d chars", len(userBridge))
	}
}

// The refusal-to-emit shape (live evidence 2026-11-12, coding-task e2e on
// Claude Code / DSH): the model declined to emit the tool XML and answered in
// prose ("I can't / haven't executed a tool"). The master must name that prose
// as a wasted turn and command the XML, both in the system footer and the
// recency-edge bridge.
func TestPromptForbidsProseRefusal(t *testing.T) {
	if !strings.Contains(toolPromptFooter, "NEVER answer in prose that you cannot use a tool") {
		t.Fatalf("toolPromptFooter RULES must forbid prose refusal to use a tool:\n%s", toolPromptFooter)
	}
	if !strings.Contains(userBridge, "prose saying you cannot or did not call a tool also executes nothing") {
		t.Fatalf("userBridge must name the prose refusal as executing nothing:\n%s", userBridge)
	}
	if len(userBridge) > 800 {
		t.Fatalf("userBridge must stay compact: %d chars", len(userBridge))
	}
}

// v0.2.13 prompt-engineering principles (from the all-models coding eval,
// 2026-11-13): the residual failure shape was models that create one file then
// STOP or narrate the next step. The footer previously told the model to
// "answer briefly" after every tool result, which biases toward premature stop
// on multi-step tasks. These rules fix that and add the two other shapes the
// eval surfaced (narrate-instead-of-act, fabricate-unobserved output).
func TestFooterContinuationAndHonestyRules(t *testing.T) {
	// continue the loop when steps remain; only answer briefly when complete
	if !strings.Contains(toolPromptFooter, "immediately emit the next step's tool call") ||
		!strings.Contains(toolPromptFooter, "do not stop and wait") {
		t.Fatalf("footer must keep the multi-step loop going when steps remain:\n%s", toolPromptFooter)
	}
	// the old unconditional "answer briefly after a result" stop-bias must be gone
	if strings.Contains(toolPromptFooter, "After a tool result arrives, answer the user briefly") {
		t.Fatalf("footer still carries the unconditional stop-after-result bias:\n%s", toolPromptFooter)
	}
	// anti-narration
	if !strings.Contains(toolPromptFooter, "do not describe it in prose") {
		t.Fatalf("footer must forbid narrating a planned action instead of emitting the call:\n%s", toolPromptFooter)
	}
	// anti-fabrication
	if !strings.Contains(toolPromptFooter, "did not observe") {
		t.Fatalf("footer must forbid reporting output the model did not observe:\n%s", toolPromptFooter)
	}
}

// v0.2.14: the all-models eval showed the task-level engineered prompt (V2)
// lifted flaky models, but the same principles added to the SYSTEM footer
// (v0.2.13) did not transfer on a plain task - salience lives at the user
// message. The bridge is the plugin's user-level injection point (PROVENANCE:
// master in system + short reminder in user reached 5/5), so the principles
// belong here.
func TestBridgeCarriesEngineeringPrinciples(t *testing.T) {
	if !strings.Contains(userBridge, "do not describe it in prose") {
		t.Fatalf("bridge must forbid narrating the next step instead of emitting it:\n%s", userBridge)
	}
	if !strings.Contains(userBridge, "do not stop and wait") {
		t.Fatalf("bridge must keep the multi-step loop going:\n%s", userBridge)
	}
	if !strings.Contains(userBridge, "did not observe") {
		t.Fatalf("bridge must forbid reporting unobserved output:\n%s", userBridge)
	}
	// the bridge stays a reminder, not the master
	if len(userBridge) > 800 {
		t.Fatalf("bridge must stay compact: %d chars", len(userBridge))
	}
}

func TestAppendUserBridgeLastUser(t *testing.T) {
	in := []upstream.ConversationMessage{
		{Role: "system", Content: "client system"},
		{Role: "user", Content: "the request"},
	}
	out := appendUserBridge(in)
	if len(out) != 2 || out[0].Content != "client system" {
		t.Fatalf("appendUserBridge changed messages it should not:\n%+v", out)
	}
	if !strings.HasSuffix(out[1].Content, "the request\n\n"+userBridge) {
		t.Fatalf("last user message must end with the bridge:\n%+v", out[1])
	}
}

func TestAppendUserBridgeSkipsNonUserTail(t *testing.T) {
	in := []upstream.ConversationMessage{
		{Role: "user", Content: "the request"},
		{Role: "assistant", Content: "a reply"},
	}
	out := appendUserBridge(in)
	for _, m := range out {
		if strings.Contains(m.Content, "PROTOCOL REMINDER") {
			t.Fatalf("bridge appended to a non-user-tail conversation:\n%+v", out)
		}
	}
}

func TestAppendUserBridgeToolResultTurn(t *testing.T) {
	// the agent-loop turn whose last user message is the folded tool result
	in := []upstream.ConversationMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "an xml reply"},
		{Role: "user", Content: "Tool result call_1: wrote a.txt"},
	}
	out := appendUserBridge(in)
	if !strings.HasSuffix(out[3].Content, userBridge) {
		t.Fatalf("the folded tool-result user message must carry the bridge:\n%+v", out[3])
	}
	if strings.HasSuffix(out[1].Content, userBridge) {
		t.Fatalf("only the LAST user message carries the bridge:\n%+v", out[1])
	}
}

func TestShapeChatRequestBridgeOnlyInToolMode(t *testing.T) {
	tools := []byte(`[{"type":"function","function":{"name":"write","description":"write","parameters":{"type":"object"}}}]`)
	msgs, _, toolMode, err := shapeChatRequest(
		[]byte(`{"messages":[{"role":"user","content":"do it"}],"tools":`+string(tools)+`}`), false)
	if err != nil || !toolMode {
		t.Fatalf("tool-mode shaping = (%v, %v, %+v), want tool mode", msgs, toolMode, err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" || !strings.HasSuffix(last.Content, userBridge) {
		t.Fatalf("tool-mode messages must end with the bridged user message:\n%+v", last)
	}

	plain, _, plainMode, err := shapeChatRequest(
		[]byte(`{"messages":[{"role":"user","content":"do it"}]}`), false)
	if err != nil || plainMode {
		t.Fatalf("no-tools shaping = (%v, %v, %+v), want plain mode", plain, plainMode, err)
	}
	for _, m := range plain {
		if strings.Contains(m.Content, "PROTOCOL REMINDER") {
			t.Fatalf("the bridge must not appear without tools:\n%+v", m)
		}
	}
}
