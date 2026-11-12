package plugin

// nudge_retry_test.go pins the empty-args nudge retry: turnNeedsNudge /
// applyNudge (pure logic) and the executor wiring (a scripted two-turn
// upstream where the first reply carries an empty-parameters call).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

func nudgeEmptyArgsXML() string {
	return "<tool_calls>\n" + toolCallToken + "\n<tool" + "_name>shell</tool" +
		"_name>\n<param" + "eters></param" + "eters>\n" + toolCallCloseToken + "\n</tool_calls>"
}

func nudgeGoodXML() string {
	return "<tool_calls>\n" + toolCallToken + "\n<tool" + "_name>get_weather</tool" +
		"_name>\n<param" + `eters>{"city":"Paris"}</param` + `eters>\n` + toolCallCloseToken + "\n</tool_calls>"
}

func TestTurnNeedsNudge(t *testing.T) {
	base := func() *textTurn {
		return &textTurn{toolMode: true, slug: "gpt-6",
			messages: []upstream.ConversationMessage{{Role: "user", Content: "hi"}}}
	}
	if !turnNeedsNudge(base(), nudgeEmptyArgsXML()) {
		t.Fatal("an empty-parameters call in tool mode must warrant the nudge")
	}
	if turnNeedsNudge(base(), nudgeGoodXML()) {
		t.Fatal("a fully-parameterized call must not warrant the nudge")
	}
	if turnNeedsNudge(base(), "plain prose") {
		t.Fatal("prose without calls must not warrant the nudge")
	}
	plain := base()
	plain.toolMode = false
	if turnNeedsNudge(plain, nudgeEmptyArgsXML()) {
		t.Fatal("non-tool-mode turns never nudge")
	}
	done := base()
	done.nudgeSpent = 2
	if turnNeedsNudge(done, nudgeEmptyArgsXML()) {
		t.Fatal("a turn that spent its nudge budget never retries")
	}
	once := base()
	once.nudgeSpent = 1
	if !turnNeedsNudge(once, nudgeEmptyArgsXML()) {
		t.Fatal("a turn with one nudge spent still has budget for the escalation retry")
	}
}

func TestApplyNudgeRebuildsPayload(t *testing.T) {
	turn := &textTurn{toolMode: true, slug: "gpt-6",
		messages: []upstream.ConversationMessage{{Role: "user", Content: "do it"}}}
	reply := "some prose\n" + nudgeEmptyArgsXML()
	applyNudge(turn, reply)
	if turn.nudgeSpent != 1 {
		t.Fatalf("nudgeSpent = %d, want 1 after the first nudge", turn.nudgeSpent)
	}
	if len(turn.messages) != 3 {
		t.Fatalf("messages = %+v, want assistant reply + nudge appended", turn.messages)
	}
	if turn.messages[1].Role != "assistant" || turn.messages[1].Content != reply {
		t.Fatalf("assistant replay = %+v, want the raw first reply", turn.messages[1])
	}
	if turn.messages[2].Role != "user" || !strings.Contains(turn.messages[2].Content, "empty parameters") {
		t.Fatalf("nudge message = %+v", turn.messages[2])
	}
	if turn.payload == nil {
		t.Fatal("applyNudge must rebuild the upstream payload")
	}
	msgs, _ := turn.payload["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("payload messages = %d, want 3", len(msgs))
	}
}

// sseTextEvent renders one conversation SSE append event for a test fixture.
func sseTextEvent(t *testing.T, text string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"p": "/message/content/parts/0", "o": "append", "v": text,
	})
	if err != nil {
		t.Fatalf("marshal sse event: %v", err)
	}
	return "data: " + string(body) + "\n\n"
}

const sseOpenEvent = "data: {\"p\":\"\",\"o\":\"add\",\"v\":{\"message\":{\"id\":\"m-9\",\"author\":{\"role\":\"assistant\"},\"content\":{\"content_type\":\"text\",\"parts\":[\"\"]},\"status\":\"in_progress\"},\"conversation_id\":\"c-9\"}}\n\n"

// TestToolsEmptyArgsNudgeRetry drives the full non-stream executor: the
// scripted upstream answers turn one with an empty-parameters call and turn
// two with a correct one; the executor must retry internally and answer the
// repaired call.
func TestToolsEmptyArgsNudgeRetry(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	first := sseOpenEvent + sseTextEvent(t, "Checking.\n"+nudgeEmptyArgsXML()) + "data: [DONE]\n\n"
	second := sseOpenEvent + sseTextEvent(t, nudgeGoodXML()) + "data: [DONE]\n\n"
	fake.setChunks(append(chunkedSSE(first, 3), chunkedSSE(second, 3)...))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "What is the weather in Paris?"},
			},
			"tools": []any{execWeatherTool},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	fake.mu.Lock()
	starts := fake.streamStarts
	fake.mu.Unlock()
	if starts != 2 {
		t.Fatalf("conversation stream starts = %d, want the nudge retry (2)", starts)
	}
	completion := decodeToolsCompletion(t, resp)
	msg := completion.Choices[0].Message
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls = %+v, want the repaired call", msg.ToolCalls)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the retry's parameters", msg.ToolCalls[0].Function.Arguments)
	}
}

// TestToolsEmptyArgsNudgeRetryEscalated pins the v0.2.9 escalation end to
// end: the upstream answers BOTH the first turn and the first nudge with an
// empty-parameters call, and only the third attempt is correct. The executor
// must spend its full nudge budget (3 conversation starts) and still answer
// the repaired call.
func TestToolsEmptyArgsNudgeRetryEscalated(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	first := sseOpenEvent + sseTextEvent(t, "Checking.\n"+nudgeEmptyArgsXML()) + "data: [DONE]\n\n"
	second := sseOpenEvent + sseTextEvent(t, nudgeEmptyArgsXML()) + "data: [DONE]\n\n"
	third := sseOpenEvent + sseTextEvent(t, nudgeGoodXML()) + "data: [DONE]\n\n"
	chunks := chunkedSSE(first, 3)
	chunks = append(chunks, chunkedSSE(second, 3)...)
	chunks = append(chunks, chunkedSSE(third, 3)...)
	fake.setChunks(chunks)
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "What is the weather in Paris?"},
			},
			"tools": []any{execWeatherTool},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	fake.mu.Lock()
	starts := fake.streamStarts
	fake.mu.Unlock()
	if starts != 3 {
		t.Fatalf("conversation stream starts = %d, want the escalated nudge retry (3)", starts)
	}
	completion := decodeToolsCompletion(t, resp)
	msg := completion.Choices[0].Message
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls = %+v, want the repaired call", msg.ToolCalls)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the third attempt's parameters", msg.ToolCalls[0].Function.Arguments)
	}
}

func TestProseClaimsAttempt(t *testing.T) {
	for _, prose := range []string{
		"The grep command failed, so I can\u0027t quote the matching line.",
		"I couldn\u0027t run that; the execution environment returned a ServerError.",
		"I was not able to verify the output.",
		"the tool is unavailable in this session",
		"No output was obtained.",
	} {
		if !proseClaimsAttempt(prose) {
			t.Fatalf("proseClaimsAttempt(%q) = false, want true", prose)
		}
	}
	for _, prose := range []string{
		"The test suite is green.",
		"Here is the answer: 42.",
		"",
	} {
		if proseClaimsAttempt(prose) {
			t.Fatalf("proseClaimsAttempt(%q) = true, want false", prose)
		}
	}
}

func TestTurnNeedsNudgeProseAttempt(t *testing.T) {
	turn := &textTurn{toolMode: true,
		messages: []upstream.ConversationMessage{{Role: "user", Content: "hi"}}}
	if !turnNeedsNudge(turn, "The shell execution was attempted, but it failed. No output was obtained.") {
		t.Fatal("a failed-attempt prose reply without XML must warrant the nudge")
	}
	if turnNeedsNudge(turn, "Here is the file content you asked for.") {
		t.Fatal("a plain informative prose reply must not warrant the nudge")
	}
}

func TestApplyNudgeProseKind(t *testing.T) {
	turn := &textTurn{toolMode: true, slug: "gpt-6",
		messages: []upstream.ConversationMessage{{Role: "user", Content: "do it"}}}
	reply := "I couldn\u0027t run the command; it failed."
	applyNudge(turn, reply)
	if turn.messages[2].Content != nudgeNoCall {
		t.Fatalf("nudge = %q, want the no-call nudge", turn.messages[2].Content)
	}
	// the empty-args kind is unchanged
	turn2 := &textTurn{toolMode: true, slug: "gpt-6",
		messages: []upstream.ConversationMessage{{Role: "user", Content: "do it"}}}
	applyNudge(turn2, nudgeEmptyArgsXML())
	if turn2.messages[2].Content != nudgeEmptyArgs {
		t.Fatalf("nudge = %q, want the empty-args nudge", turn2.messages[2].Content)
	}
}

func TestTurnNeedsNudgeStrippedEmptyReply(t *testing.T) {
	turn := &textTurn{toolMode: true,
		messages: []upstream.ConversationMessage{{Role: "user", Content: "hi"}}}
	// the live shape (tools-e2e test-11 / test-6, 2026-11-12): the whole
	// reply is platform escape markup - it strips to nothing
	if !turnNeedsNudge(turn, "<escape></escape>") {
		t.Fatal("a stripped-empty reply must warrant the nudge")
	}
	if !turnNeedsNudge(turn, "  \n<escape>helloworld</escape>\n  ") {
		t.Fatal("markup-only reply must warrant the nudge")
	}
	// real content (prose or XML) never nudges for emptiness
	if turnNeedsNudge(turn, "Here is the answer.") {
		t.Fatal("visible prose must not trigger the empty-reply nudge")
	}
	plain := &textTurn{
		messages: []upstream.ConversationMessage{{Role: "user", Content: "hi"}}}
	if turnNeedsNudge(plain, "<escape></escape>") {
		t.Fatal("non-tool-mode turns never nudge")
	}
}

func TestApplyNudgeEmptyReplyKind(t *testing.T) {
	turn := &textTurn{toolMode: true, slug: "gpt-6",
		messages: []upstream.ConversationMessage{{Role: "user", Content: "do it"}}}
	applyNudge(turn, "<escape></escape>")
	if turn.messages[2].Content != nudgeEmptyReply {
		t.Fatalf("nudge = %q, want the empty-reply nudge", turn.messages[2].Content)
	}
}

// v0.2.9 (live evidence 2026-11-12, session ses_e31a73778ffeT9j3ajZzEmYA4a):
// one polite nudge is not always enough - the model answered it with another
// unusable reply. The second nudge carries the escalation demand; the first
// one must not.
func TestApplyNudgeEscalation(t *testing.T) {
	turn := &textTurn{toolMode: true, slug: "gpt-6",
		messages: []upstream.ConversationMessage{{Role: "user", Content: "do it"}}}
	applyNudge(turn, nudgeEmptyArgsXML())
	if turn.nudgeSpent != 1 {
		t.Fatalf("nudgeSpent = %d, want 1 after the first nudge", turn.nudgeSpent)
	}
	if strings.Contains(turn.messages[2].Content, "second retry") {
		t.Fatal("the first nudge must not carry the escalation demand")
	}
	applyNudge(turn, nudgeEmptyArgsXML())
	if turn.nudgeSpent != 2 {
		t.Fatalf("nudgeSpent = %d, want 2 after the second nudge", turn.nudgeSpent)
	}
	last := turn.messages[len(turn.messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "second retry") {
		t.Fatalf("escalated nudge = %+v, want the second-retry demand appended", last)
	}
	if !strings.Contains(last.Content, "empty parameters") {
		t.Fatalf("escalated nudge lost the kind instruction: %q", last.Content)
	}
	// the budget is exhausted: the turn never nudges again
	if turnNeedsNudge(turn, nudgeEmptyArgsXML()) {
		t.Fatal("a twice-nudged turn must never retry again")
	}
}
