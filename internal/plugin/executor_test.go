package plugin

import (
	"encoding/json"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executor_test.go pins the text conversation executor (Task 12): the
// OpenAI chat payload in, the sentinel + conversation SSE turn, and the
// typed envelope out. The two review-focus contracts: a zero-content
// completion is a typed 502 no_assistant_content (never an empty 200,
// Review Focus #2) and a 401 forces one refresh then exactly one retry
// (Review Focus #3). Review Focus #6 is pinned by TestNoSecretInError:
// no credential token bytes in any response envelope or downstream
// stream frame on any error path.
//
// Every test drives Manager.Handle("executor.execute"/"executor.execute_stream")
// with the SDK-typed request (marshaled exactly the way the host does:
// untagged pluginapi.ExecutorRequest = PascalCase keys, plus the host's
// stream_id / host_callback_id injections) against one scripted fake
// host caller: host.http.do by (url, authorization), the single upstream
// SSE stream by a chunk queue (or a stall the watchdog's close wakes),
// and host.stream.emit / host.stream.close recorded for the pump.

// The pinned upstream URLs the fake scripts (Task 6/7 wire values).
const (
	execBootstrapURL    = "https://chatgpt.com/"
	execPrepareURL      = "https://chatgpt.com/backend-api/sentinel/chat-requirements/prepare"
	execFinalizeURL     = "https://chatgpt.com/backend-api/sentinel/chat-requirements/finalize"
	execConversationURL = "https://chatgpt.com/backend-api/conversation"
	execOAuthTokenURL   = "https://auth.openai.com/oauth/token"
	execPlanCheckURL    = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480"

	execUpstreamStreamID = "up-1"
	execDownstreamID     = "down-1"
)

// execBootstrapHTML is the sentinel bootstrap body: one script src and a
// data-build, so the legacy token builds offline without network access.
var execBootstrapHTML = []byte(`<html data-build="c/abc123/_"><head><script src="https://chatgpt.com/c/abc123/_/pow.js"></script></head><body>hi</body></html>`)

// execHappySSE is the fixture stream that assembles to "Hello world!":
// a non-JSON frame (raw event), the first visible assistant message add
// (message_id event), two patch appends and a bare-string append, then
// [DONE].
const execHappySSE = `data: v1

data: {"p":"","o":"add","v":{"message":{"id":"m-1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]},"status":"in_progress"},"conversation_id":"c-1"}}

data: {"p":"/message/content/parts/0","o":"append","v":"Hello"}

data: {"v":" world"}

data: {"p":"","o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":"!"}]}

data: [DONE]
`

// execNoAnswerSSE completes with only non-visible assistant messages
// (reasoning channel, web recipient): the assembled text stays empty.
const execNoAnswerSSE = `data: v1

data: {"p":"","o":"add","v":{"message":{"id":"m-3","author":{"role":"assistant"},"channel":"reasoning","content":{"content_type":"text","parts":["chain of thought"]},"conversation_id":"c-2"}}

data: {"p":"","o":"add","v":{"message":{"id":"m-4","author":{"role":"assistant"},"recipient":"web","content":{"content_type":"text","parts":["<tool command>"]},"conversation_id":"c-2"}}

data: [DONE]
`

// chunkedSSE splits the fixture into n-byte pieces so line assembly
// across read boundaries is exercised (upstream test precedent).
func chunkedSSE(sse string, size int) [][]byte {
	var out [][]byte
	for len(sse) > 0 {
		if len(sse) > size {
			out = append(out, []byte(sse[:size]))
			sse = sse[size:]
		} else {
			out = append(out, []byte(sse))
			sse = ""
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The scripted fake host caller.
// ---------------------------------------------------------------------------

type execStreamEmit struct {
	streamID string
	payload  []byte
}

type execStreamClose struct {
	streamID string
	errMsg   string
}

// execFakeCaller is the offline host seam for executor tests. host.http.do
// scripts are keyed by "url\x00authorization" so two credentials (or two
// tokens) hitting the same route carry independent responses; the single
// upstream SSE stream plays a chunk queue or stalls until the watchdog's
// stream_close wakes the parked read (emulating the host's stream abort);
// every downstream host.stream.emit / host.stream.close is recorded.
type execFakeCaller struct {
	mu           sync.Mutex
	calls        []hostCallRecord
	doScripts    map[string]hostHTTPResponse
	doHits       map[string]int
	streamStarts int

	startStatus  int
	startHeaders map[string][]string
	chunks       [][]byte
	idx          int
	stall        bool
	wake         chan struct{}
	wakeOnce     sync.Once

	emits  []execStreamEmit
	closes []execStreamClose
}

func newExecFakeCaller() *execFakeCaller {
	return &execFakeCaller{
		doScripts: map[string]hostHTTPResponse{},
		doHits:    map[string]int{},
	}
}

// withStallStream switches the conversation stream into the stalled state
// (never yields a chunk, never reports done) and pre-creates the wake
// channel so a stream_close before the first read cannot race it.
func (f *execFakeCaller) withStallStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall = true
	f.wake = make(chan struct{})
}

func (f *execFakeCaller) withStreamStart(status int, headers map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startStatus = status
	f.startHeaders = headers
}

func (f *execFakeCaller) setChunks(chunks [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunks = chunks
}

// doHit counts host.http.do calls under one script key.
func (f *execFakeCaller) doHit(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doHits[key]
}

// downstreamCounts returns the recorded emit/close counts under the lock
// (the pump writes them from its own goroutine).
func (f *execFakeCaller) downstreamCounts() (emits, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.emits), len(f.closes)
}

// doCall is the recorded wire request of one host.http.do under a script
// key (URL + bearer).
func (f *execFakeCaller) doCall(t *testing.T, key string) hostCallRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		record := f.calls[i]
		if record.method != pluginabi.MethodHostHTTPDo {
			continue
		}
		var req struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if json.Unmarshal(record.request, &req) != nil {
			continue
		}
		if req.URL+"\x00"+req.Headers.Get("Authorization") == key {
			return record
		}
	}
	t.Fatalf("caller never issued host.http.do for %q", key)
	return hostCallRecord{}
}

// conversationStart returns the recorded host.http.do_stream request for
// the conversation route (empty request when the stream never started).
func (f *execFakeCaller) conversationStart() hostCallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.calls {
		if record.method != pluginabi.MethodHostHTTPDoStream {
			continue
		}
		var req struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(record.request, &req) == nil && req.URL == execConversationURL {
			return record
		}
	}
	return hostCallRecord{}
}

func (f *execFakeCaller) scriptKey(url, bearer string) string {
	return url + "\x00" + bearer
}

func (f *execFakeCaller) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, hostCallRecord{method: method, request: append([]byte(nil), request...)})
	f.mu.Unlock()

	switch method {
	case pluginabi.MethodHostHTTPDo:
		var req struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		_ = json.Unmarshal(request, &req)
		key := req.URL + "\x00" + req.Headers.Get("Authorization")
		f.mu.Lock()
		f.doHits[key]++
		resp, scripted := f.doScripts[key]
		f.mu.Unlock()
		if !scripted {
			return hostOK(hostHTTPResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"no script"}`)}), nil
		}
		return hostOK(resp), nil

	case pluginabi.MethodHostHTTPDoStream:
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(request, &req)
		f.mu.Lock()
		if req.URL == execConversationURL {
			f.streamStarts++
		}
		status := f.startStatus
		headers := map[string][]string{}
		for key, values := range f.startHeaders {
			headers[key] = values
		}
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		return hostOK(map[string]any{"status_code": status, "headers": headers, "stream_id": execUpstreamStreamID}), nil

	case pluginabi.MethodHostHTTPStreamRead:
		f.mu.Lock()
		if f.stall {
			wake := f.wake
			f.mu.Unlock()
			<-wake // the watchdog's stream_close is the only wake-up
			return hostOK(map[string]any{"error": "stream aborted"}), nil
		}
		if f.idx >= len(f.chunks) {
			f.mu.Unlock()
			return hostOK(map[string]any{"done": true}), nil
		}
		chunk := f.chunks[f.idx]
		f.idx++
		f.mu.Unlock()
		return hostOK(map[string]any{"payload": chunk}), nil

	case pluginabi.MethodHostHTTPStreamClose:
		f.mu.Lock()
		f.wakeOnce.Do(func() {
			if f.wake != nil {
				close(f.wake)
			}
		})
		f.mu.Unlock()
		return hostOK(map[string]any{}), nil

	case pluginabi.MethodHostStreamEmit:
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
		}
		_ = json.Unmarshal(request, &req)
		f.mu.Lock()
		f.emits = append(f.emits, execStreamEmit{streamID: req.StreamID, payload: append([]byte(nil), req.Payload...)})
		f.mu.Unlock()
		return hostOK(map[string]any{}), nil

	case pluginabi.MethodHostStreamClose:
		var req struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(request, &req)
		f.mu.Lock()
		f.closes = append(f.closes, execStreamClose{streamID: req.StreamID, errMsg: req.Error})
		f.mu.Unlock()
		return hostOK(map[string]any{}), nil

	default:
		return hostOK(map[string]any{}), nil
	}
}

// happySentinelScripts scripts the full sentinel handshake for one bearer:
// the bootstrap carries no Authorization header (page-load leg), prepare
// and finalize do.
func happySentinelScripts(bearer string) map[string]hostHTTPResponse {
	return map[string]hostHTTPResponse{
		execBootstrapURL + "\x00":         {StatusCode: http.StatusOK, Body: execBootstrapHTML},
		execPrepareURL + "\x00" + bearer:  {StatusCode: http.StatusOK, Body: []byte(`{"prepare_token":"pt-1"}`)},
		execFinalizeURL + "\x00" + bearer: {StatusCode: http.StatusOK, Body: []byte(`{"token":"req-1","so_token":"so-1"}`)},
	}
}

// execCredential builds one fixture credential; tokens are distinctive
// strings TestNoSecretInError scans for.
func execCredential() Credential {
	return Credential{
		AccessToken:  "SECRET-ACCESS-TOKEN-EXECUTOR",
		RefreshToken: "SECRET-REFRESH-TOKEN-EXECUTOR",
		IDToken:      "SECRET-ID-TOKEN-EXECUTOR",
		Email:        "executor@example.com",
		Plan:         "plus",
		Fp: Fingerprint{
			OAIDeviceID:  "executor-device",
			OAISessionID: "executor-session",
		},
	}
}

// execManager registers the given config and returns the Manager.
func execManager(t *testing.T, host caller, cfgYAML string) *Manager {
	t.Helper()
	m, err := New(host)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := m.Handle(pluginabi.MethodPluginRegister, lifecycleRequestBody(t, cfgYAML))
	if err != nil {
		t.Fatalf("Handle(plugin.register): %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("plugin.register envelope not OK: %s", resp)
	}
	return m
}

// execRequestBody marshals the SDK-typed request exactly the way the host
// does (rpcExecutorRequest): untagged ExecutorRequest = PascalCase keys,
// plus the host-injected stream_id and host_callback_id.
func execRequestBody(t *testing.T, req pluginapi.ExecutorRequest, streamID string) []byte {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal executor request: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("merge executor request: %v", err)
	}
	merged["stream_id"] = streamID
	merged["host_callback_id"] = "cb-1"
	body, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged executor request: %v", err)
	}
	return body
}

func execRequest(t *testing.T, cred Credential, model string, payload any, stream bool) pluginapi.ExecutorRequest {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal chat payload: %v", err)
	}
	return pluginapi.ExecutorRequest{
		AuthID:       "auth-1",
		AuthProvider: providerKey,
		Model:        model,
		Format:       "openai",
		Stream:       stream,
		SourceFormat: "openai",
		Payload:      raw,
		StorageJSON:  cred.StorageJSON(),
	}
}

// decodeExecResult asserts a success envelope and returns the result bytes.
func decodeExecResult(t *testing.T, resp []byte) []byte {
	t.Helper()
	env := decodeEnv(t, resp)
	if !env.OK || env.Error != nil {
		t.Fatalf("executor envelope not OK: %s", resp)
	}
	return env.Result
}

// decodeExecError asserts an error envelope and returns its typed error.
func decodeExecError(t *testing.T, resp []byte) *pluginabi.Error {
	t.Helper()
	env := decodeErrorEnvelope(t, resp)
	return env.Error
}

// chatCompletion mirrors the pinned chat.completion envelope shape.
type chatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

var chatcmplIDRe = regexp.MustCompile(`^chatcmpl-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// openAIChunk mirrors the pinned chat.completion.chunk frame shape.
type openAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// decodeChunkFrame parses one emitted SSE frame: "data: <json>\n\n".
func decodeChunkFrame(t *testing.T, payload []byte) openAIChunk {
	t.Helper()
	if !strings.HasPrefix(string(payload), "data: ") || !strings.HasSuffix(string(payload), "\n\n") {
		t.Fatalf("emitted payload is not a data:-framed SSE line: %q", payload)
	}
	var chunk openAIChunk
	if err := json.Unmarshal(payload[len("data: "):len(payload)-2], &chunk); err != nil {
		t.Fatalf("decode chunk frame: %v (%q)", err, payload)
	}
	return chunk
}

// waitForStreamEnd polls until the pump emitted wantEmits frames and closed
// the downstream stream exactly once (the pump is asynchronous).
func waitForStreamEnd(t *testing.T, fake *execFakeCaller, wantEmits int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		emits, closes := fake.downstreamCounts()
		if emits == wantEmits && closes == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream did not end: emits=%d want=%d, closes=%d want=1", emits, wantEmits, closes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// TestTextNonStreamHappyPath — fixture SSE → exact completion envelope shape,
// model field echoes the requested id, usage fields present, and the
// upstream body carries the stripped slug, system message and mapped effort.
// ---------------------------------------------------------------------------

func TestTextNonStreamHappyPath(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "Be brief."},
				map[string]any{"role": "user", "content": "Say hello"},
			},
			"reasoning_effort": "xhigh",
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}

	var out struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeExecResult(t, resp), &out); err != nil {
		t.Fatalf("decode executor response: %v", err)
	}

	var completion chatCompletion
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload is not a chat.completion: %v (%s)", err, out.Payload)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want exactly one", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if !chatcmplIDRe.MatchString(completion.ID) {
		t.Fatalf("id = %q, want chatcmpl-<uuid>", completion.ID)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", completion.Object)
	}
	if completion.Model != "chatgpt-proxy/openai/gpt-5" {
		t.Fatalf("model = %q, want the requested published id echoed", completion.Model)
	}
	if completion.Created <= 0 {
		t.Fatalf("created = %d, want a unix timestamp", completion.Created)
	}
	if choice.Index != 0 || choice.Message.Role != "assistant" {
		t.Fatalf("choice[0] = %+v, want index 0 assistant", choice)
	}
	if choice.Message.Content != "Hello world!" {
		t.Fatalf("content = %q, want the assembled \"Hello world!\"", choice.Message.Content)
	}
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	// Documented estimate: len/4 per message. "Be brief."=9→2,
	// "Say hello"=9→2, output "Hello world!"=12→3, total 7.
	if completion.Usage.PromptTokens != 4 || completion.Usage.CompletionTokens != 3 || completion.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want the documented len/4 estimate {4 3 7}", completion.Usage)
	}

	// The upstream POST carries the stripped slug, the system message as a
	// plain message (author.role "system") and the mapped effort.
	record := fake.conversationStart()
	if record.method == "" {
		t.Fatalf("the conversation stream never started")
	}
	var wire struct {
		Body []byte `json:"body"`
	}
	if json.Unmarshal(record.request, &wire) != nil {
		t.Fatalf("decode do_stream request: %s", record.request)
	}
	body := string(wire.Body)
	for _, want := range []string{`"model": "gpt-5"`, `"role": "system"`, `"thinking_effort": "extended"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("conversation body missing %s:\n%s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// TestTextStreamChunkOrder — role chunk first, deltas in order, terminal
// last, one emit per frame, host.stream.close exactly once (clean).
// ---------------------------------------------------------------------------

func TestTextStreamChunkOrder(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.setChunks(chunkedSSE(execHappySSE, 7))
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		}, true), execDownstreamID))
	if err != nil {
		t.Fatalf("Handle(executor.execute_stream): %v", err)
	}
	result := decodeExecResult(t, resp)
	if strings.TrimSpace(string(result)) != "{}" {
		t.Fatalf("async stream response = %s, want an empty result (the pump emits downstream)", result)
	}

	// role, "Hello", " world", "!", terminal, [DONE]
	waitForStreamEnd(t, fake, 6)

	fake.mu.Lock()
	emits := append([]execStreamEmit(nil), fake.emits...)
	closes := append([]execStreamClose(nil), fake.closes...)
	fake.mu.Unlock()

	for i, emit := range emits {
		if emit.streamID != execDownstreamID {
			t.Fatalf("emit[%d] stream_id = %q, want the host-allocated downstream id", i, emit.streamID)
		}
	}
	if string(emits[5].payload) != "data: [DONE]\n\n" {
		t.Fatalf("last emit = %q, want the [DONE] sentinel", emits[5].payload)
	}

	seenID := ""
	wantDeltas := []string{"Hello", " world", "!"}
	for i, want := range wantDeltas {
		chunk := decodeChunkFrame(t, emits[i+1].payload)
		if seenID == "" {
			seenID = chunk.ID
			if !chatcmplIDRe.MatchString(seenID) {
				t.Fatalf("chunk id = %q, want chatcmpl-<uuid>", seenID)
			}
		} else if chunk.ID != seenID {
			t.Fatalf("chunk[%d] id = %q, want the turn's single id %q", i, chunk.ID, seenID)
		}
		if chunk.Object != "chat.completion.chunk" || chunk.Model != "chatgpt-proxy/openai/gpt-5" {
			t.Fatalf("chunk[%d] = %+v, want chat.completion.chunk echoing the model", i, chunk)
		}
		if chunk.Choices[0].FinishReason != nil {
			t.Fatalf("chunk[%d] finish_reason = %v, want null before the terminal", i, *chunk.Choices[0].FinishReason)
		}
		if chunk.Choices[0].Delta.Content != want {
			t.Fatalf("chunk[%d] delta = %q, want %q", i, chunk.Choices[0].Delta.Content, want)
		}
	}

	// First frame: the role chunk (assistant role, empty content).
	role := decodeChunkFrame(t, emits[0].payload)
	if role.ID != seenID {
		t.Fatalf("role chunk id = %q, want the turn id %q", role.ID, seenID)
	}
	if role.Choices[0].Delta.Role != "assistant" || role.Choices[0].Delta.Content != "" {
		t.Fatalf("role chunk delta = %+v, want {role:assistant, content:?}", role.Choices[0].Delta)
	}

	// Terminal frame: empty delta, finish_reason stop, usage estimate.
	terminal := decodeChunkFrame(t, emits[4].payload)
	stop := "stop"
	if terminal.Choices[0].FinishReason == nil || *terminal.Choices[0].FinishReason != stop {
		t.Fatalf("terminal finish_reason = %v, want stop", terminal.Choices[0].FinishReason)
	}
	if terminal.Choices[0].Delta.Content != "" {
		t.Fatalf("terminal delta = %q, want empty", terminal.Choices[0].Delta.Content)
	}
	if terminal.Usage == nil || terminal.Usage.PromptTokens != 2 || terminal.Usage.CompletionTokens != 3 || terminal.Usage.TotalTokens != 5 {
		t.Fatalf("terminal usage = %+v, want the documented estimate {2 3 5}", terminal.Usage)
	}

	// The downstream stream closes exactly once, cleanly.
	if len(closes) != 1 || closes[0].streamID != execDownstreamID || closes[0].errMsg != "" {
		t.Fatalf("closes = %+v, want exactly one clean close of %s", closes, execDownstreamID)
	}
}

// ---------------------------------------------------------------------------
// TestZeroContentIs502 — a completed turn with no visible assistant message
// is a typed 502 no_assistant_content, never an empty 200 (Review Focus #2).
// ---------------------------------------------------------------------------

func TestZeroContentIs502(t *testing.T) {
	cred := execCredential()

	t.Run("non-stream", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "no_assistant_content" || got.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("error = %+v, want typed 502 no_assistant_content", got)
		}
	})

	t.Run("stream", func(t *testing.T) {
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle(executor.execute_stream): %v", err)
		}
		decodeExecResult(t, resp)

		// The pump ends with exactly one close; wait for it.
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, closes := fake.downstreamCounts()
			if closes == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pump never closed the downstream stream")
			}
			time.Sleep(5 * time.Millisecond)
		}

		// The role chunk may have gone out when the message id appeared,
		// but no terminal chunk and no [DONE] may ever be emitted.
		fake.mu.Lock()
		emits := append([]execStreamEmit(nil), fake.emits...)
		closes := append([]execStreamClose(nil), fake.closes...)
		fake.mu.Unlock()

		for _, emit := range emits {
			chunk := decodeChunkFrame(t, emit.payload)
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil {
					t.Fatalf("a terminal chunk was emitted on a zero-content stream: %s", emit.payload)
				}
			}
		}
		if len(closes) != 1 || !strings.Contains(closes[0].errMsg, "no_assistant_content") {
			t.Fatalf("closes = %+v, want one close naming no_assistant_content", closes)
		}
	})
}

// ---------------------------------------------------------------------------
// TestToolsRefused — any non-empty tools array is refused with typed 400
// tools_unsupported BEFORE any upstream call.
// ---------------------------------------------------------------------------

func TestToolsRefused(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
			},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	got := decodeExecError(t, resp)
	if got.Code != "tools_unsupported" || got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %+v, want typed 400 tools_unsupported", got)
	}

	fake.mu.Lock()
	upstreamCalls := 0
	for _, record := range fake.calls {
		if record.method == pluginabi.MethodHostHTTPDo || record.method == pluginabi.MethodHostHTTPDoStream {
			upstreamCalls++
		}
	}
	fake.mu.Unlock()
	if upstreamCalls != 0 {
		t.Fatalf("the tools refusal made %d upstream calls, want zero (refuse before any network)", upstreamCalls)
	}
}

// ---------------------------------------------------------------------------
// TestUnknownModelRefused — image models and foreign models are refused
// cleanly with typed 400 model_unknown (images are Task 13; this task must
// not crash on them).
// ---------------------------------------------------------------------------

func TestUnknownModelRefused(t *testing.T) {
	cred := execCredential()
	for _, model := range []string{
		"chatgpt-proxy/gpt-image-2-proxy",
		"chatgpt-proxy/codex-gpt-image-2",
		"other/model-x",
	} {
		t.Run(model, func(t *testing.T) {
			fake := newExecFakeCaller()
			m := execManager(t, fake, "")

			resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
				execRequest(t, cred, model, map[string]any{
					"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				}, false), ""))
			if err != nil {
				t.Fatalf("Handle(executor.execute): %v", err)
			}
			got := decodeExecError(t, resp)
			if got.Code != "model_unknown" || got.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("error = %+v, want typed 400 model_unknown", got)
			}

			fake.mu.Lock()
			upstreamCalls := 0
			for _, record := range fake.calls {
				if record.method == pluginabi.MethodHostHTTPDo || record.method == pluginabi.MethodHostHTTPDoStream {
					upstreamCalls++
				}
			}
			fake.mu.Unlock()
			if upstreamCalls != 0 {
				t.Fatalf("%s made %d upstream calls, want zero", model, upstreamCalls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test401RefreshRetryOnce — a 401 start forces the internal refresh, then
// exactly one retry; a failed refresh or a second 401 ends in typed 401
// with no further refresh (Review Focus #3).
// ---------------------------------------------------------------------------

func Test401RefreshRetryOnce(t *testing.T) {
	chat := func() map[string]any {
		return map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
	}

	t.Run("refresh succeeds, one retry", func(t *testing.T) {
		cred := execCredential()
		fresh := "Bearer fresh-access-token"
		old := "Bearer " + cred.AccessToken
		fake := newExecFakeCaller()
		for key, resp := range happySentinelScripts(fresh) {
			fake.doScripts[key] = resp
		}
		// The first attempt's prepare (authed leg) rejects the old bearer.
		fake.doScripts[fake.scriptKey(execPrepareURL, old)] = hostHTTPResponse{StatusCode: http.StatusUnauthorized}
		fake.doScripts[fake.scriptKey(execPlanCheckURL, fresh)] = hostHTTPResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"accounts":{"default":{"account":{"plan_type":"plus"}}}}`),
		}
		// The refresh POST carries no Authorization header.
		fake.doScripts[fake.scriptKey(execOAuthTokenURL, "")] = hostHTTPResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"access_token":"fresh-access-token","refresh_token":"fresh-refresh-token","id_token":"fresh-id-token"}`),
		}
		fake.setChunks(chunkedSSE(execHappySSE, 7))
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		result := decodeExecResult(t, resp)
		var out struct {
			Payload []byte `json:"Payload"`
		}
		if json.Unmarshal(result, &out) != nil {
			t.Fatalf("decode executor response: %s", result)
		}
		if !strings.Contains(string(out.Payload), "Hello world!") {
			t.Fatalf("completion = %s, want the retried turn's answer", out.Payload)
		}

		if hits := fake.doHit(fake.scriptKey(execOAuthTokenURL, "")); hits != 1 {
			t.Fatalf("the token endpoint was hit %d times, want exactly one refresh", hits)
		}
		fake.mu.Lock()
		starts := fake.streamStarts
		fake.mu.Unlock()
		if starts != 1 {
			t.Fatalf("the conversation stream started %d times, want exactly one (the retry)", starts)
		}
		// The retry carried the refreshed bearer end to end.
		record := fake.conversationStart()
		if record.method == "" {
			t.Fatalf("no conversation start recorded")
		}
		var wire struct {
			Headers http.Header `json:"headers"`
		}
		if json.Unmarshal(record.request, &wire) != nil {
			t.Fatalf("decode conversation start: %s", record.request)
		}
		if wire.Headers.Get("Authorization") != fresh {
			t.Fatalf("retry Authorization = %q, want the refreshed bearer", wire.Headers.Get("Authorization"))
		}
	})

	t.Run("refresh failure ends in typed 401", func(t *testing.T) {
		cred := execCredential()
		old := "Bearer " + cred.AccessToken
		fake := newExecFakeCaller()
		fake.doScripts = map[string]hostHTTPResponse{
			execBootstrapURL + "\x00":           {StatusCode: http.StatusOK, Body: execBootstrapHTML},
			fake.scriptKey(execPrepareURL, old): {StatusCode: http.StatusUnauthorized},
			fake.scriptKey(execOAuthTokenURL, ""): {
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":"invalid_grant","error_description":"refresh rejected"}`),
			},
		}
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "invalid_token" || got.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("error = %+v, want typed 401 invalid_token", got)
		}
		if hits := fake.doHit(fake.scriptKey(execOAuthTokenURL, "")); hits != 1 {
			t.Fatalf("the token endpoint was hit %d times, want exactly one", hits)
		}
		fake.mu.Lock()
		starts := fake.streamStarts
		fake.mu.Unlock()
		if starts != 0 {
			t.Fatalf("the conversation stream started %d times, want zero after a failed refresh", starts)
		}
	})

	t.Run("second 401 ends in typed 401, no second refresh", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		fake.doScripts = map[string]hostHTTPResponse{
			execBootstrapURL + "\x00": {StatusCode: http.StatusOK, Body: execBootstrapHTML},
			// Both attempts' prepare (authed leg) reject.
			fake.scriptKey(execPrepareURL, "Bearer "+cred.AccessToken):  {StatusCode: http.StatusUnauthorized},
			fake.scriptKey(execPrepareURL, "Bearer fresh-access-token"): {StatusCode: http.StatusUnauthorized},
			fake.scriptKey(execOAuthTokenURL, ""): {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"access_token":"fresh-access-token"}`),
			},
		}
		m := execManager(t, fake, "")

		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "invalid_token" || got.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("error = %+v, want typed 401 invalid_token", got)
		}
		if hits := fake.doHit(fake.scriptKey(execOAuthTokenURL, "")); hits != 1 {
			t.Fatalf("the token endpoint was hit %d times, want exactly one (the retry must not refresh again)", hits)
		}
	})
}

// ---------------------------------------------------------------------------
// Test429RetryAfterPassthrough — a 429 stream start with Retry-After maps to
// typed retryable rate_limited carrying the retry-after value.
// ---------------------------------------------------------------------------

func Test429RetryAfterPassthrough(t *testing.T) {
	cred := execCredential()
	fake := newExecFakeCaller()
	fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
	fake.withStreamStart(http.StatusTooManyRequests, map[string][]string{"Retry-After": {"12"}})
	m := execManager(t, fake, "")

	resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
		execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, false), ""))
	if err != nil {
		t.Fatalf("Handle(executor.execute): %v", err)
	}
	got := decodeExecError(t, resp)
	if got.Code != "rate_limited" || got.HTTPStatus != http.StatusTooManyRequests || !got.Retryable {
		t.Fatalf("error = %+v, want retryable 429 rate_limited", got)
	}
	if !strings.Contains(got.Message, "retry after 12s") {
		t.Fatalf("message = %q, want the Retry-After value carried", got.Message)
	}
}

// ---------------------------------------------------------------------------
// TestStreamHardTimeout — a stalled stream past the hard cap yields typed
// 504 upstream_stream_timeout (non-stream) or one typed close (stream),
// and every goroutine exits (no leak).
// ---------------------------------------------------------------------------

func TestStreamHardTimeout(t *testing.T) {
	chat := func() map[string]any {
		return map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
	}

	t.Run("non-stream", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.withStallStream()
		m := execManager(t, fake, "")

		// Shrink the cap to milliseconds: the config only expresses seconds.
		beforeCap := sseHardCapFor
		sseHardCapFor = func(Config) time.Duration { return time.Millisecond }
		t.Cleanup(func() { sseHardCapFor = beforeCap })

		before := runtime.NumGoroutine()
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle(executor.execute): %v", err)
		}
		got := decodeExecError(t, resp)
		if got.Code != "upstream_stream_timeout" || got.HTTPStatus != http.StatusGatewayTimeout || !got.Retryable {
			t.Fatalf("error = %+v, want retryable 504 upstream_stream_timeout", got)
		}

		settleGoroutines(t, before)
	})

	t.Run("stream", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.withStallStream()
		m := execManager(t, fake, "")

		beforeCap := sseHardCapFor
		sseHardCapFor = func(Config) time.Duration { return time.Millisecond }
		t.Cleanup(func() { sseHardCapFor = beforeCap })

		before := runtime.NumGoroutine()
		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle(executor.execute_stream): %v", err)
		}
		decodeExecResult(t, resp)

		waitForStreamEnd(t, fake, 0)
		fake.mu.Lock()
		closes := append([]execStreamClose(nil), fake.closes...)
		emits := len(fake.emits)
		fake.mu.Unlock()
		if closes[0].streamID != execDownstreamID || !strings.Contains(closes[0].errMsg, "upstream_stream_timeout") {
			t.Fatalf("closes = %+v, want one close naming upstream_stream_timeout", closes)
		}
		if emits != 0 {
			t.Fatalf("a stalled stream emitted %d frames, want zero", emits)
		}

		settleGoroutines(t, before)
	})
}

// settleGoroutines pins that every goroutine the run spawned exited: the
// SSE hard-cap watchdog, the reader and (in stream mode) the pump must all
// unwind when the channel closes.
func settleGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d remain (before=%d)", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// TestNoSecretInError (Review Focus #6) — credential token bytes must be
// absent from every response envelope, emitted downstream frame and close
// message across the error paths.
// ---------------------------------------------------------------------------

func TestNoSecretInError(t *testing.T) {
	secrets := []string{
		"SECRET-ACCESS-TOKEN-EXECUTOR",
		"SECRET-REFRESH-TOKEN-EXECUTOR",
		"SECRET-ID-TOKEN-EXECUTOR",
	}

	// assertClean scans every surface a secret could reach: the response
	// envelope bytes, each downstream emit payload and each close message.
	assertClean := func(t *testing.T, fake *execFakeCaller, resp []byte) {
		t.Helper()
		surfaces := []string{string(resp)}
		fake.mu.Lock()
		for _, emit := range fake.emits {
			surfaces = append(surfaces, string(emit.payload))
		}
		for _, closeRec := range fake.closes {
			surfaces = append(surfaces, closeRec.errMsg)
		}
		fake.mu.Unlock()
		for _, surface := range surfaces {
			for _, secret := range secrets {
				if strings.Contains(surface, secret) {
					t.Fatalf("an error surface carries %s bytes: %s", secret, surface)
				}
			}
		}
	}

	chat := func() map[string]any {
		return map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
	}

	t.Run("zero content 502", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.setChunks(chunkedSSE(execNoAnswerSSE, 7))
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		assertClean(t, fake, resp)
	})

	t.Run("tools refusal 400", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools":    []any{map[string]any{"type": "function"}},
			}, false), ""))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		assertClean(t, fake, resp)
	})

	t.Run("unknown model 400", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/gpt-image-2-proxy", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		assertClean(t, fake, resp)
	})

	t.Run("sentinel failure 500", func(t *testing.T) {
		cred := execCredential()
		bearer := "Bearer " + cred.AccessToken
		fake := newExecFakeCaller()
		fake.doScripts = map[string]hostHTTPResponse{
			execBootstrapURL + "\x00":               {StatusCode: http.StatusOK, Body: execBootstrapHTML},
			fake.scriptKey(execPrepareURL, bearer):  {StatusCode: http.StatusOK, Body: []byte(`{"prepare_token":"pt-1"}`)},
			fake.scriptKey(execFinalizeURL, bearer): {StatusCode: http.StatusInternalServerError},
		}
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		assertClean(t, fake, resp)
	})

	t.Run("refresh failure 401", func(t *testing.T) {
		cred := execCredential()
		bearer := "Bearer " + cred.AccessToken
		fake := newExecFakeCaller()
		fake.doScripts = map[string]hostHTTPResponse{
			execBootstrapURL + "\x00":              {StatusCode: http.StatusOK, Body: execBootstrapHTML},
			fake.scriptKey(execPrepareURL, bearer): {StatusCode: http.StatusUnauthorized},
			fake.scriptKey(execOAuthTokenURL, ""): {
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":"invalid_grant","error_description":"refresh rejected"}`),
			},
		}
		m := execManager(t, fake, "")
		resp, err := m.Handle(pluginabi.MethodExecutorExecute, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), false), ""))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		assertClean(t, fake, resp)
	})

	t.Run("streamed timeout carries no secrets downstream", func(t *testing.T) {
		cred := execCredential()
		fake := newExecFakeCaller()
		fake.doScripts = happySentinelScripts("Bearer " + cred.AccessToken)
		fake.withStallStream()
		m := execManager(t, fake, "")

		beforeCap := sseHardCapFor
		sseHardCapFor = func(Config) time.Duration { return time.Millisecond }
		t.Cleanup(func() { sseHardCapFor = beforeCap })

		resp, err := m.Handle(pluginabi.MethodExecutorExecuteStream, execRequestBody(t,
			execRequest(t, cred, "chatgpt-proxy/openai/gpt-5", chat(), true), execDownstreamID))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		waitForStreamEnd(t, fake, 0)
		assertClean(t, fake, resp)
	})
}
