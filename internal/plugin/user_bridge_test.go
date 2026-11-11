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
	if len(userBridge) > 600 {
		t.Fatalf("userBridge must stay compact (this is a reminder, not the master): %d chars", len(userBridge))
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
