package upstream

// conversation_test.go pins the Task 7 contract: the byte-pinned payload,
// the thinking-effort mapping, the patch protocol, assistant filtering,
// marker sanitization, unknown-event tolerance (Review Focus #2), and the
// hard-timeout watchdog with no goroutine leak.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/plugin"
)

// ---------------------------------------------------------------------------
// Fake upstream for stream tests: records every Req, serves Do from a queue,
// and serves DoStream from scripted byte chunks (or stalls until Close).
// ---------------------------------------------------------------------------

type streamFake struct {
	mu       sync.Mutex
	requests []Req
	doQueue  []Resp
	chunks   [][]byte
	idx      int
	stall    bool
	closed   bool
}

func (f *streamFake) Do(_ context.Context, req Req) (Resp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.doQueue) == 0 {
		return Resp{}, fmt.Errorf("streamFake: do queue exhausted after %d calls", len(f.requests))
	}
	resp := f.doQueue[0]
	f.doQueue = f.doQueue[1:]
	return resp, nil
}

func (f *streamFake) DoStream(_ context.Context, req Req) (*Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	var stallClosed chan struct{}
	if f.stall {
		stallClosed = make(chan struct{})
	}
	return &Stream{
		StatusCode: 200,
		Headers:    http.Header{},
		read: func() ([]byte, error) {
			f.mu.Lock()
			if f.stall {
				f.mu.Unlock()
				<-stallClosed // the watchdog's Close is the only wake-up
				return nil, errors.New("streamFake: stall stream was closed")
			}
			if f.idx >= len(f.chunks) {
				f.mu.Unlock()
				return nil, io.EOF
			}
			chunk := f.chunks[f.idx]
			f.idx++
			f.mu.Unlock()
			return chunk, nil
		},
		close: func() error {
			f.mu.Lock()
			f.closed = true
			if f.stall {
				close(stallClosed)
			}
			f.mu.Unlock()
			return nil
		},
	}, nil
}

func (f *streamFake) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// fixtureCredential (headers_test.go) is reused here: pre-set fingerprint
// and a fixture access token, never a real secret.

func testPayload(t *testing.T) map[string]any {
	t.Helper()
	payload, err := BuildConversationPayload(ConversationRequest{
		Messages: []ConversationMessage{{Role: "user", Content: "hello"}},
		Model:    "gpt-5",
	})
	if err != nil {
		t.Fatalf("BuildConversationPayload: %v", err)
	}
	return payload
}

// runFixtureStream plays one SSE fixture through StreamConversation with the
// stream delivered in 7-byte chunks (forcing line assembly across chunk
// boundaries) and returns the full drained event list.
func runFixtureStream(t *testing.T, fixture string, opts ...StreamOption) ([]ConvEvent, *streamFake) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixture, "stream.sse"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	fake := &streamFake{chunks: chunkBytes(raw, 7)}
	ch, err := StreamConversation(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-chat-requirements"},
		testPayload(t), opts...)
	if err != nil {
		t.Fatalf("StreamConversation: %v", err)
	}
	var events []ConvEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if !fake.wasClosed() {
		t.Errorf("stream was never closed by the reader")
	}
	return events, fake
}

func chunkBytes(data []byte, size int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := size
		if n > len(data) {
			n = len(data)
		}
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

func eventKinds(events []ConvEvent) []string {
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func assertKinds(t *testing.T, events []ConvEvent, want ...string) {
	t.Helper()
	got := eventKinds(events)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", got, want)
		}
	}
}

func lastEvent(t *testing.T, events []ConvEvent) ConvEvent {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("no events emitted")
	}
	return events[len(events)-1]
}

// ---------------------------------------------------------------------------
// TestPayloadGolden — the byte-pinned upstream body.
// ---------------------------------------------------------------------------

func TestPayloadGolden(t *testing.T) {
	t.Run("byte-pinned serialization", func(t *testing.T) {
		old := conversationUUID
		pinned := []string{
			"00000000-0000-4000-0000-000000000001",
			"00000000-0000-4000-0000-000000000002",
			"00000000-0000-4000-0000-000000000003",
			"00000000-0000-4000-0000-000000000004",
		}
		i := 0
		conversationUUID = func() string {
			value := pinned[i]
			i++
			return value
		}
		t.Cleanup(func() { conversationUUID = old })

		payload, err := BuildConversationPayload(ConversationRequest{
			Messages: []ConversationMessage{
				{Role: "system", Content: "You are terse."},
				{Role: "user", Content: "Hello, 世界"},
			},
			Model:          "gpt-5",
			ThinkingEffort: "high",
		})
		if err != nil {
			t.Fatalf("BuildConversationPayload: %v", err)
		}
		body, err := serializeConversationPayload(payload)
		if err != nil {
			t.Fatalf("serializeConversationPayload: %v", err)
		}

		golden, err := os.ReadFile(filepath.Join("testdata", "conversation-payload", "golden.json"))
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		want := strings.TrimSuffix(string(golden), "\n")
		if string(body) != want {
			t.Fatalf("payload serialization drifted from the golden:\ngot:  %s\nwant: %s", body, want)
		}
	})

	t.Run("global constraint fields asserted explicitly", func(t *testing.T) {
		payload, err := BuildConversationPayload(ConversationRequest{
			Messages:       []ConversationMessage{{Role: "user", Content: "hi"}},
			Model:          "gpt-5",
			ThinkingEffort: "xhigh",
		})
		if err != nil {
			t.Fatalf("BuildConversationPayload: %v", err)
		}
		assertField := func(key string, want any) {
			t.Helper()
			if got, ok := payload[key]; !ok || got != want {
				t.Fatalf("payload[%q] = %v (present=%v), want %v", key, got, ok, want)
			}
		}
		assertField("action", "next")
		assertField("model", "gpt-5")
		assertField("timezone", "Asia/Shanghai")
		assertField("timezone_offset_min", -480)
		assertField("variant_purpose", "comparison_implicit")
		assertField("force_use_sse", true)
		assertField("history_and_training_disabled", true)
		assertField("force_paragen", false)
		assertField("force_rate_limit", false)
		assertField("reset_rate_limits", false)
		assertField("conversation_origin", nil)
		assertField("thinking_effort", "extended") // xhigh collapses to extended

		mode, ok := payload["conversation_mode"].(map[string]any)
		if !ok || mode["kind"] != "primary_assistant" {
			t.Fatalf("conversation_mode = %v, want {kind: primary_assistant}", payload["conversation_mode"])
		}

		geometry := map[string]any{
			"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900,
			"page_width": 1400, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560,
		}
		info, ok := payload["client_contextual_info"].(map[string]any)
		if !ok {
			t.Fatalf("client_contextual_info missing or not an object")
		}
		for key, want := range geometry {
			if info[key] != want {
				t.Fatalf("client_contextual_info[%q] = %v, want %v", key, info[key], want)
			}
		}

		for _, key := range []string{"suggestions", "supported_encodings", "system_hints"} {
			if list, ok := payload[key].([]any); !ok || len(list) != 0 {
				t.Fatalf("payload[%q] = %v, want an empty list", key, payload[key])
			}
		}

		if id, _ := payload["parent_message_id"].(string); id == "" {
			t.Fatalf("parent_message_id must be a fresh uuid4")
		}
		if id, _ := payload["websocket_request_id"].(string); id == "" {
			t.Fatalf("websocket_request_id must be a fresh uuid4")
		}

		messages, ok := payload["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %v, want one message", payload["messages"])
		}
		message, _ := messages[0].(map[string]any)
		if id, _ := message["id"].(string); id == "" {
			t.Fatalf("message id must be a fresh uuid4")
		}
		author, _ := message["author"].(map[string]any)
		if author["role"] != "user" {
			t.Fatalf("message author.role = %v, want user", author["role"])
		}
		content, _ := message["content"].(map[string]any)
		if content["content_type"] != "text" {
			t.Fatalf("content_type = %v, want text", content["content_type"])
		}
		parts, _ := content["parts"].([]any)
		if len(parts) != 1 || parts[0] != "hi" {
			t.Fatalf("parts = %v, want [hi]", parts)
		}
	})

	t.Run("empty messages refused", func(t *testing.T) {
		if _, err := BuildConversationPayload(ConversationRequest{Model: "gpt-5"}); err == nil {
			t.Fatalf("BuildConversationPayload with no messages must fail")
		}
	})

	t.Run("anon timezone pass-through", func(t *testing.T) {
		payload, err := BuildConversationPayload(ConversationRequest{
			Messages: []ConversationMessage{{Content: "hi"}},
			Model:    "gpt-5",
			Timezone: anonConversationTimezone,
		})
		if err != nil {
			t.Fatalf("BuildConversationPayload: %v", err)
		}
		if payload["timezone"] != "America/Los_Angeles" {
			t.Fatalf("timezone = %v, want America/Los_Angeles", payload["timezone"])
		}
		// empty role defaults to user (the oracle's item.get("role", "user"))
		message := payload["messages"].([]any)[0].(map[string]any)
		author := message["author"].(map[string]any)
		if author["role"] != "user" {
			t.Fatalf("default role = %v, want user", author["role"])
		}
	})
}

// ---------------------------------------------------------------------------
// TestThinkingEffortMapping — one subtest per oracle mapping row.
// ---------------------------------------------------------------------------

func TestThinkingEffortMapping(t *testing.T) {
	cases := []struct {
		input  string
		expect string // "" means the key must be absent
	}{
		{"", ""},
		{"none", ""},
		{"auto", ""},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"standard", "standard"},
		{"max", "max"},
		{"xhigh", "extended"},
		{"extended", "extended"},
		{"XHIGH", "extended"},
		{"bogus", ""},
	}
	for _, tc := range cases {
		t.Run("effort_"+tc.input+"_"+tc.expect, func(t *testing.T) {
			payload, err := BuildConversationPayload(ConversationRequest{
				Messages:       []ConversationMessage{{Role: "user", Content: "hi"}},
				Model:          "gpt-5",
				ThinkingEffort: tc.input,
			})
			if err != nil {
				t.Fatalf("BuildConversationPayload: %v", err)
			}
			got, present := payload["thinking_effort"].(string)
			if tc.expect == "" {
				if present {
					t.Fatalf("thinking_effort present = %q, want the key omitted", got)
				}
				return
			}
			if got != tc.expect {
				t.Fatalf("thinking_effort = %q, want %q", got, tc.expect)
			}
		})
	}

	// And the standalone mapping helper Task 12 consumes.
	for _, tc := range cases {
		if NormalizeThinkingEffort(tc.input) != tc.expect {
			t.Fatalf("NormalizeThinkingEffort(%q) = %q, want %q", tc.input, NormalizeThinkingEffort(tc.input), tc.expect)
		}
	}
}

// ---------------------------------------------------------------------------
// TestPatchApplication — differential vectors + the normal fixture stream.
// ---------------------------------------------------------------------------

func TestPatchApplication(t *testing.T) {
	t.Run("differential vectors", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "conversation-patch", "vectors.json"))
		if err != nil {
			t.Fatalf("read vectors: %v", err)
		}
		var cases []struct {
			Name    string
			Event   map[string]any
			Current string
			History string
			Want    string
		}
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("parse vectors: %v", err)
		}
		for _, tc := range cases {
			t.Run(tc.Name, func(t *testing.T) {
				got := assistantRawText(tc.Event, tc.Current, tc.History)
				if got != tc.Want {
					t.Fatalf("assistantRawText = %q, want %q", got, tc.Want)
				}
			})
		}
	})

	t.Run("normal fixture assembles text", func(t *testing.T) {
		events, _ := runFixtureStream(t, "conversation-normal-patches")
		assertKinds(t, events,
			ConvEventRaw,       // "v1" marker
			ConvEventMessageID, // resume_conversation_token: conversation id appears
			ConvEventMessageID, // assistant message add: message id appears
			ConvEventAssistantDelta,
			ConvEventAssistantDelta,
			ConvEventAssistantDelta,
			ConvEventRaw, // title_generation
			ConvEventRaw, // server_ste_metadata
			ConvEventComplete,
		)
		wantDeltas := []string{"Hello", " world", "!"}
		for i, want := range wantDeltas {
			ev := events[3+i]
			if ev.Delta != want {
				t.Fatalf("delta[%d] = %q, want %q", i, ev.Delta, want)
			}
		}
		complete := lastEvent(t, events)
		if complete.Text != "Hello world!" {
			t.Fatalf("complete Text = %q, want %q", complete.Text, "Hello world!")
		}
		if complete.ConversationID != "c-123" || complete.MessageID != "m-1" {
			t.Fatalf("complete ids = %q/%q, want c-123/m-1", complete.ConversationID, complete.MessageID)
		}
		if complete.ToolInvoked == nil || *complete.ToolInvoked {
			t.Fatalf("complete ToolInvoked = %v, want false", complete.ToolInvoked)
		}
		if complete.TurnUseCase != "text" {
			t.Fatalf("complete TurnUseCase = %q, want text", complete.TurnUseCase)
		}

		// The resume token must never surface downstream.
		for _, ev := range events {
			if strings.Contains(fmt.Sprint(ev.Raw), "resume-token-do-not-leak") {
				t.Fatalf("resume token leaked into event %+v", ev)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestAssistantFiltering — visibility rules at function level and stream level.
// ---------------------------------------------------------------------------

func TestAssistantFiltering(t *testing.T) {
	t.Run("visibility rules", func(t *testing.T) {
		visible := map[string]any{"author": map[string]any{"role": "assistant"}}
		mutate := func(base map[string]any, key string, value any) map[string]any {
			out := map[string]any{}
			for k, v := range base {
				out[k] = v
			}
			out[key] = value
			return out
		}
		cases := []struct {
			name    string
			message map[string]any
			want    bool
		}{
			{"visible baseline", visible, true},
			{"role user", mutate(visible, "author", map[string]any{"role": "user"}), false},
			{"no author", map[string]any{"content": map[string]any{}}, false},
			{"hidden from conversation", mutate(visible, "metadata", map[string]any{"is_visually_hidden_from_conversation": true}), false},
			{"recipient web", mutate(visible, "recipient", "web"), false},
			{"recipient all", mutate(visible, "recipient", "all"), true},
			{"recipient empty passes", mutate(visible, "recipient", ""), true},
			{"channel reasoning", mutate(visible, "channel", "reasoning"), false},
			{"channel final", mutate(visible, "channel", "final"), true},
			{"channel empty passes", mutate(visible, "channel", ""), true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := isVisibleAssistantMessage(tc.message); got != tc.want {
					t.Fatalf("isVisibleAssistantMessage = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("no-answer fixture drops filtered text", func(t *testing.T) {
		events, _ := runFixtureStream(t, "conversation-no-answer")
		complete := lastEvent(t, events)
		if complete.Kind != ConvEventComplete {
			t.Fatalf("last event kind = %q, want complete", complete.Kind)
		}
		if complete.Text != "" {
			t.Fatalf("complete Text = %q, want empty (all assistant messages filtered)", complete.Text)
		}
		if complete.MessageID != "" {
			t.Fatalf("complete MessageID = %q, want empty (filtered messages carry no id)", complete.MessageID)
		}
		if complete.ConversationID != "c-999" {
			t.Fatalf("complete ConversationID = %q, want c-999", complete.ConversationID)
		}
		for _, ev := range events {
			if ev.Kind == ConvEventAssistantDelta {
				t.Fatalf("filtered assistant text leaked a delta: %+v", ev)
			}
		}
	})

	t.Run("history replay is skipped", func(t *testing.T) {
		events, _ := runFixtureStream(t, "conversation-history-replay", WithRequestMessages([]ConversationMessage{
			{Role: "assistant", Content: "Earlier answer."},
			{Role: "user", Content: "And now?"},
		}))
		complete := lastEvent(t, events)
		if complete.Kind != ConvEventComplete {
			t.Fatalf("last event kind = %q, want complete", complete.Kind)
		}
		if complete.Text != "New answer" {
			t.Fatalf("complete Text = %q, want exactly the fresh turn's text", complete.Text)
		}
		for _, ev := range events {
			if strings.Contains(ev.Text, "Earlier answer") {
				t.Fatalf("history text leaked into events: %+v", ev)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestMarkerSanitization — the \uE200..\uE201 rich-text annotation removal.
// ---------------------------------------------------------------------------

func TestMarkerSanitization(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no markers", "plain text stays", "plain text stays"},
		{"url label with link", "\uE200url\uE202Docs\uE202https://example.com\uE201", "Docs (https://example.com)"},
		{"url label without http", "\uE200url\uE202Docs\uE202ftp://example.com\uE201", "Docs"},
		{"url without label", "\uE200url\uE202https://example.com\uE201", "https://example.com"},
		{"cite keeps readable part", "\uE200cite\uE202turn1\uE202Real Label\uE201", "Real Label"},
		{"cite drops internal turn pointer", "\uE200cite\uE202turn1\uE201", ""},
		{"cite before punctuation keeps space", "See \uE200cite\uE202turn1\uE202Real Label\uE201, done.", "See Real Label, done."},
		{"cite before punctuation drops space when empty", "See \uE200cite\uE202turn1\uE201, done.", "See, done."},
		{"trailing unclosed marker stripped", "text \uE200url\uE202x\uE202y", "text "},
		{"multiple markers", "a\uE200url\uE202L\uE202https://x\uE201b\uE200cite\uE202c1\uE201c", "aL (https://x)bc1c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeOutputText(tc.in); got != tc.want {
				t.Fatalf("sanitizeOutputText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestUnknownEventTolerated — Review Focus #2: shape drift never interrupts.
// ---------------------------------------------------------------------------

func TestUnknownEventTolerated(t *testing.T) {
	events, _ := runFixtureStream(t, "conversation-unknown-event")
	assertKinds(t, events,
		ConvEventRaw,       // "v1"
		ConvEventMessageID, // resume: conversation id
		ConvEventRaw,       // the unknown event type, surfaced not fatal
		ConvEventMessageID, // assistant message add
		ConvEventAssistantDelta,
		ConvEventComplete,
	)

	unknown, ok := events[2].Raw.(map[string]any)
	if !ok {
		t.Fatalf("unknown event raw = %T, want a decoded object", events[2].Raw)
	}
	if unknown["type"] != "mystery_event_v9" {
		t.Fatalf("unknown event type = %v, want mystery_event_v9", unknown["type"])
	}

	complete := lastEvent(t, events)
	if complete.Text != "ok" {
		t.Fatalf("stream did not complete with the assembled text: %q", complete.Text)
	}
	for _, ev := range events {
		if ev.Kind == ConvEventError {
			t.Fatalf("unknown event raised a stream error: %+v", ev)
		}
	}
}

// ---------------------------------------------------------------------------
// TestSSEHardTimeout — the watchdog fires, the stream closes, no leak.
// ---------------------------------------------------------------------------

func TestSSEHardTimeout(t *testing.T) {
	before := runtime.NumGoroutine()

	fake := &streamFake{stall: true}
	ch, err := StreamConversation(context.Background(), fake, fixtureCredential(),
		SentinelTokens{ChatRequirements: "fixture-chat-requirements"},
		testPayload(t), WithSSEHardCap(time.Millisecond))
	if err != nil {
		t.Fatalf("StreamConversation: %v", err)
	}

	var events []ConvEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly one error event", len(events))
	}
	if events[0].Kind != ConvEventError {
		t.Fatalf("event kind = %q, want error", events[0].Kind)
	}
	if !errors.Is(events[0].Err, ErrStreamTimeout) {
		t.Fatalf("error = %v, want the typed upstream_stream_timeout", events[0].Err)
	}
	if !fake.wasClosed() {
		t.Fatalf("the watchdog did not close the upstream stream")
	}

	// Every goroutine the run spawned must have exited (race detector on).
	deadline := time.Now().Add(500 * time.Millisecond)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines remain (before=%d)", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Stream request shape: authed/anon paths, sentinel headers, payload body.
// ---------------------------------------------------------------------------

func TestStreamRequestShape(t *testing.T) {
	t.Run("authed", func(t *testing.T) {
		old := conversationUUID
		conversationUUID = func() string { return "00000000-0000-4000-0000-0000000000aa" }
		t.Cleanup(func() { conversationUUID = old })

		fake := &streamFake{chunks: chunkBytes([]byte("data: [DONE]\n\n"), 7)}
		payload, err := BuildConversationPayload(ConversationRequest{
			Messages: []ConversationMessage{{Role: "user", Content: "hi"}},
			Model:    "gpt-5",
		})
		if err != nil {
			t.Fatalf("BuildConversationPayload: %v", err)
		}
		ch, err := StreamConversation(context.Background(), fake, fixtureCredential(),
			SentinelTokens{ChatRequirements: "req-token", Proof: "pow-token", SO: "so-token"}, payload)
		if err != nil {
			t.Fatalf("StreamConversation: %v", err)
		}
		for range ch {
		}

		if len(fake.requests) != 1 {
			t.Fatalf("requests = %d, want 1", len(fake.requests))
		}
		req := fake.requests[0]
		if req.Method != http.MethodPost || req.URL != "https://chatgpt.com"+conversationPath {
			t.Fatalf("request = %s %s, want POST https://chatgpt.com/backend-api/conversation", req.Method, req.URL)
		}
		if got := req.Headers.Get("Authorization"); got != "Bearer "+fixtureCredential().AccessToken {
			t.Fatalf("Authorization missing or wrong")
		}
		if got := req.Headers.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", got)
		}
		if got := req.Headers.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "req-token" {
			t.Fatalf("requirements token header = %q", got)
		}
		if got := req.Headers.Get("OpenAI-Sentinel-Proof-Token"); got != "pow-token" {
			t.Fatalf("proof token header = %q", got)
		}
		if got := req.Headers.Get("OpenAI-Sentinel-SO-Token"); got != "so-token" {
			t.Fatalf("so token header = %q", got)
		}
		if got := req.Headers.Get("OpenAI-Sentinel-Turnstile-Token"); got != "" {
			t.Fatalf("turnstile header must be absent when not acquired (got %q)", got)
		}
		if got := req.Headers.Get("X-OpenAI-Target-Path"); got != conversationPath {
			t.Fatalf("X-OpenAI-Target-Path = %q", got)
		}
		if string(req.Body) != string(mustMarshal(t, payload)) {
			t.Fatalf("body does not serialize to the pinned form")
		}
	})

	t.Run("anon", func(t *testing.T) {
		fake := &streamFake{chunks: chunkBytes([]byte("data: [DONE]\n\n"), 7)}
		ch, err := StreamConversation(context.Background(), fake, plugin.Credential{},
			SentinelTokens{ChatRequirements: "req-token"}, testPayload(t))
		if err != nil {
			t.Fatalf("StreamConversation: %v", err)
		}
		for range ch {
		}
		req := fake.requests[0]
		if req.URL != "https://chatgpt.com"+AnonPath(conversationPath) {
			t.Fatalf("anon URL = %s, want the backend-anon variant", req.URL)
		}
		if req.Headers.Get("Authorization") != "" {
			t.Fatalf("anon request must not carry Authorization")
		}
	})

	t.Run("non-2xx start is typed and closes", func(t *testing.T) {
		u := &startStatusUpstream{status: 429, retryAfter: "7"}
		ch, err := StreamConversation(context.Background(), u, fixtureCredential(),
			SentinelTokens{ChatRequirements: "x"}, testPayload(t))
		if err == nil {
			t.Fatalf("StreamConversation must return the typed error, got channel %v", ch)
		}
		var convErr *ConvError
		if !errors.As(err, &convErr) || convErr.Code != "rate_limited" || convErr.RetryAfter != 7 {
			t.Fatalf("error = %v, want typed rate_limited with RetryAfter 7", err)
		}
	})

	t.Run("401 start is invalid_token", func(t *testing.T) {
		u := &startStatusUpstream{status: 401}
		if _, err := StreamConversation(context.Background(), u, fixtureCredential(),
			SentinelTokens{}, testPayload(t)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("error = %v, want invalid_token", err)
		}
	})

	t.Run("empty payload refused", func(t *testing.T) {
		if _, err := StreamConversation(context.Background(), &streamFake{}, fixtureCredential(),
			SentinelTokens{}, map[string]any{}); err == nil {
			t.Fatalf("empty payload must be refused")
		}
	})
}

// startStatusUpstream answers DoStream with a fixed status/headers.
type startStatusUpstream struct {
	status     int
	retryAfter string
}

func (u *startStatusUpstream) Do(context.Context, Req) (Resp, error) {
	return Resp{}, errors.New("startStatusUpstream: Do unused")
}

func (u *startStatusUpstream) DoStream(_ context.Context, _ Req) (*Stream, error) {
	headers := http.Header{}
	if u.retryAfter != "" {
		headers.Set("Retry-After", u.retryAfter)
	}
	return &Stream{StatusCode: u.status, Headers: headers, close: func() error { return nil }}, nil
}

func mustMarshal(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	body, err := serializeConversationPayload(payload)
	if err != nil {
		t.Fatalf("serializeConversationPayload: %v", err)
	}
	return body
}
