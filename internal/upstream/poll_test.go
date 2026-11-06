package upstream

// poll_test.go pins the poll contract with a fake clock (sleep recorder +
// zero jitter): the initial 10s wait, Retry-After honored on 429, the
// exponential backoff capped at 16s, the typed immediate-fail on 401, and the
// budget exhaustion.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/plugin"
)

// sleepRecorder is the fake clock: it records every requested sleep instead
// of waiting, so backoff arithmetic is asserted deterministically.
type sleepRecorder struct {
	durations []time.Duration
}

func (r *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	r.durations = append(r.durations, d)
	return nil
}

func deterministicOpts(rec *sleepRecorder, timeout, interval, initial time.Duration) PollOptions {
	return PollOptions{
		Timeout:     timeout,
		Interval:    interval,
		InitialWait: initial,
		jitter:      func(time.Duration) time.Duration { return 0 },
		sleep:       rec.sleep,
	}
}

func wantSleeps(t *testing.T, rec *sleepRecorder, seconds ...float64) {
	t.Helper()
	if len(rec.durations) != len(seconds) {
		t.Fatalf("sleeps = %v, want %v seconds", rec.durations, seconds)
	}
	for i, want := range seconds {
		if got := rec.durations[i].Seconds(); got != want {
			t.Fatalf("sleep[%d] = %v, want %v (all: %v)", i, got, want, rec.durations)
		}
	}
}

const pollDocFixture = `{"mapping":{"m-1":{"message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["hi"]}}}}}`

func pollResp(status int, retryAfter, body string) Resp {
	headers := http.Header{}
	if retryAfter != "" {
		headers.Set("Retry-After", retryAfter)
	}
	return Resp{StatusCode: status, Headers: headers, Body: []byte(body)}
}

// ---------------------------------------------------------------------------
// TestPollRetryAfter — 429 with Retry-After: 2 is honored: the next attempt
// waits exactly 2s (jitter zeroed), replacing the exponential base.
// ---------------------------------------------------------------------------

func TestPollRetryAfter(t *testing.T) {
	u := &scriptedUpstream{responses: []Resp{
		pollResp(429, "2", `{"error":"slow down"}`),
		pollResp(200, "", pollDocFixture),
	}}
	rec := &sleepRecorder{}
	doc, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
		deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second))
	if err != nil {
		t.Fatalf("PollConversation: %v", err)
	}
	if len(doc.Mapping) != 1 {
		t.Fatalf("doc.Mapping = %d entries, want 1", len(doc.Mapping))
	}
	// Initial wait, then the 429 backoff at the Retry-After value.
	wantSleeps(t, rec, 10, 2)

	if len(u.requests) != 2 {
		t.Fatalf("poll issued %d requests, want 2", len(u.requests))
	}
	for i, req := range u.requests {
		if req.Method != http.MethodGet || req.URL != "https://chatgpt.com/backend-api/conversation/c-1" {
			t.Fatalf("poll request[%d] = %s %s", i, req.Method, req.URL)
		}
		if got := req.Headers.Get("Accept"); got != "application/json" {
			t.Fatalf("poll Accept[%d] = %q", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// TestPollBackoffCap — without Retry-After the backoff doubles 2/4/8/16 and
// stays capped at 16s.
// ---------------------------------------------------------------------------

func TestPollBackoffCap(t *testing.T) {
	responses := []Resp{}
	for i := 0; i < 5; i++ {
		responses = append(responses, pollResp(503, "", `{"error":"unavailable"}`))
	}
	responses = append(responses, pollResp(200, "", pollDocFixture))
	u := &scriptedUpstream{responses: responses}
	rec := &sleepRecorder{}

	if _, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
		deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second)); err != nil {
		t.Fatalf("PollConversation: %v", err)
	}
	wantSleeps(t, rec, 10, 2, 4, 8, 16, 16)
	if len(u.requests) != 6 {
		t.Fatalf("poll issued %d requests, want 6", len(u.requests))
	}
}

// ---------------------------------------------------------------------------
// TestPoll5xxTransient — 503 is retried; 401 fails typed and immediately.
// ---------------------------------------------------------------------------

func TestPoll5xxTransient(t *testing.T) {
	t.Run("503 retried then success", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{
			pollResp(503, "", `{"error":"unavailable"}`),
			pollResp(200, "", pollDocFixture),
		}}
		rec := &sleepRecorder{}
		doc, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
			deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second))
		if err != nil {
			t.Fatalf("PollConversation: %v", err)
		}
		if len(doc.Mapping) != 1 {
			t.Fatalf("doc.Mapping = %d entries, want 1", len(doc.Mapping))
		}
		wantSleeps(t, rec, 10, 2)
	})

	t.Run("401 typed invalid_token immediately", func(t *testing.T) {
		// A second scripted success proves the poll never reached attempt 2.
		u := &scriptedUpstream{responses: []Resp{
			pollResp(401, "", `{"error":"unauthorized"}`),
			pollResp(200, "", pollDocFixture),
		}}
		rec := &sleepRecorder{}
		_, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
			deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second))
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("error = %v, want typed invalid_token", err)
		}
		if len(u.requests) != 1 {
			t.Fatalf("poll issued %d requests, want exactly 1 (no retry on 401)", len(u.requests))
		}
		// Only the initial wait happened; no retry sleep.
		wantSleeps(t, rec, 10)
	})

	t.Run("transport error backs off like a status retry", func(t *testing.T) {
		u := &failOnceUpstream{}
		rec := &sleepRecorder{}
		doc, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
			deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second))
		if err != nil {
			t.Fatalf("PollConversation: %v", err)
		}
		if len(doc.Mapping) != 1 {
			t.Fatalf("doc.Mapping = %d entries, want 1", len(doc.Mapping))
		}
		wantSleeps(t, rec, 10, 2)
		if u.calls != 2 {
			t.Fatalf("transport calls = %d, want 2", u.calls)
		}
	})
}

// failOnceUpstream fails the first Do with a transport error, then serves the
// doc — the oracle treats network failures like retryable statuses.
type failOnceUpstream struct{ calls int }

func (u *failOnceUpstream) Do(ctx context.Context, req Req) (Resp, error) {
	u.calls++
	if u.calls == 1 {
		return Resp{}, errors.New("connection reset")
	}
	return Resp{StatusCode: 200, Headers: http.Header{}, Body: []byte(pollDocFixture)}, nil
}

func (u *failOnceUpstream) DoStream(context.Context, Req) (*Stream, error) {
	return nil, errors.New("failOnceUpstream: DoStream unused")
}

// ---------------------------------------------------------------------------
// Budget exhaustion: sleeps clamp to the remaining budget, then the typed
// conversation_poll_timeout fires.
// ---------------------------------------------------------------------------

func TestPollBudgetExhausted(t *testing.T) {
	u := &scriptedUpstream{responses: []Resp{
		pollResp(503, "", `{"error":"unavailable"}`),
		pollResp(503, "", `{"error":"unavailable"}`),
		pollResp(200, "", pollDocFixture), // never reached: budget is gone
	}}
	rec := &sleepRecorder{}
	_, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1",
		deterministicOpts(rec, 15*time.Second, 10*time.Second, 10*time.Second))
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("error = %v, want typed conversation_poll_timeout", err)
	}
	// 10s initial wait, 2s retry (budget left: 3s), 4s retry clamped to the
	// last 3s — then the budget is exhausted and no third attempt runs.
	wantSleeps(t, rec, 10, 2, 3)
	if len(u.requests) != 2 {
		t.Fatalf("poll issued %d requests, want 2", len(u.requests))
	}
}

// ---------------------------------------------------------------------------
// Done predicate: the poll keeps fetching until the predicate holds.
// ---------------------------------------------------------------------------

func TestPollDonePredicate(t *testing.T) {
	pending := `{"mapping":{}}`
	rec := &sleepRecorder{}
	opts := deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second)
	opts.Done = func(doc ConversationDoc) bool { return len(doc.Mapping) > 0 }

	u := &scriptedUpstream{responses: []Resp{
		pollResp(200, "", pending),
		pollResp(200, "", pollDocFixture),
	}}
	doc, err := PollConversation(context.Background(), u, fixtureCredential(), "c-1", opts)
	if err != nil {
		t.Fatalf("PollConversation: %v", err)
	}
	if len(doc.Mapping) != 1 {
		t.Fatalf("doc.Mapping = %d entries, want 1 (the predicate must gate the return)", len(doc.Mapping))
	}
	// Initial wait, then the steady interval after the non-matching doc, then
	// the matching one returns immediately.
	wantSleeps(t, rec, 10, 10)
	if len(u.requests) != 2 {
		t.Fatalf("poll issued %d requests, want 2", len(u.requests))
	}
}

// ---------------------------------------------------------------------------
// HideConversation — the PATCH {"is_visible": false} wire shape.
// ---------------------------------------------------------------------------

func TestHideConversationSendsPatch(t *testing.T) {
	u := &scriptedUpstream{responses: []Resp{pollResp(200, "", `{"is_visible":false}`)}}
	if err := HideConversation(context.Background(), u, fixtureCredential(), "c-1"); err != nil {
		t.Fatalf("HideConversation: %v", err)
	}
	if len(u.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(u.requests))
	}
	req := u.requests[0]
	if req.Method != http.MethodPatch || req.URL != "https://chatgpt.com/backend-api/conversation/c-1" {
		t.Fatalf("request = %s %s", req.Method, req.URL)
	}
	if string(req.Body) != `{"is_visible": false}` {
		t.Fatalf("body = %q, want the oracle's python-dumps bytes", req.Body)
	}
	if got := req.ContentType; got != "application/json" {
		t.Fatalf("ContentType = %q", got)
	}
	// Wire end state: concrete Target-Path, template Target-Route, /c/ referer.
	if got := req.Headers.Get("X-OpenAI-Target-Path"); got != "/backend-api/conversation/c-1" {
		t.Fatalf("X-OpenAI-Target-Path = %q", got)
	}
	if got := req.Headers.Get("X-OpenAI-Target-Route"); got != "/backend-api/conversation/{conversation_id}" {
		t.Fatalf("X-OpenAI-Target-Route = %q", got)
	}
	if got := req.Headers.Get("Referer"); got != "https://chatgpt.com/c/c-1" {
		t.Fatalf("Referer = %q", got)
	}

	t.Run("401 typed", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(401, "", `{"error":"unauthorized"}`)}}
		err := HideConversation(context.Background(), u, fixtureCredential(), "c-1")
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("error = %v, want invalid_token", err)
		}
	})
}

// ---------------------------------------------------------------------------
// FetchTasks + Task error detection.
// ---------------------------------------------------------------------------

const tasksFixture = `{
  "tasks": [
    {"task_id": "t-1", "conversation_id": "c-1",
     "image_gen_message": {"metadata": {"is_error": true},
                           "content": {"content_type": "text", "parts": ["err", "one"]}}},
    {"task_id": "t-2", "original_conversation_id": "c-1",
     "image_gen_message": {"metadata": {"is_error": true},
                           "content": {"content_type": "multimodal_text", "parts": []}}},
    {"task_id": "t-3", "conversation_id": "c-2",
     "image_gen_message": {"metadata": {"is_error": false},
                           "content": {"content_type": "text", "parts": ["ok"]}}},
    {"task_id": "t-4"}
  ]
}`

func TestFetchTasksFilter(t *testing.T) {
	t.Run("by conversation id matches conversation_id and original_conversation_id", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(200, "", tasksFixture)}}
		tasks, err := FetchTasks(context.Background(), u, fixtureCredential(),
			TaskFilter{ConversationID: "c-1"})
		if err != nil {
			t.Fatalf("FetchTasks: %v", err)
		}
		var ids []string
		for _, task := range tasks {
			id, _ := task.Raw["task_id"].(string)
			ids = append(ids, id)
		}
		if strings.Join(ids, ",") != "t-1,t-2" {
			t.Fatalf("filtered task ids = %v, want [t-1 t-2]", ids)
		}
	})

	t.Run("by task id", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(200, "", tasksFixture)}}
		tasks, err := FetchTasks(context.Background(), u, fixtureCredential(),
			TaskFilter{TaskID: "t-3"})
		if err != nil {
			t.Fatalf("FetchTasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0].Raw["task_id"] != "t-3" {
			t.Fatalf("filtered tasks = %v, want exactly t-3", tasks)
		}
	})

	t.Run("no filter returns all task-shaped entries", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(200, "", tasksFixture)}}
		tasks, err := FetchTasks(context.Background(), u, fixtureCredential(), TaskFilter{})
		if err != nil {
			t.Fatalf("FetchTasks: %v", err)
		}
		if len(tasks) != 4 {
			t.Fatalf("tasks = %d, want 4", len(tasks))
		}
	})

	t.Run("request shape", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(200, "", tasksFixture)}}
		if _, err := FetchTasks(context.Background(), u, fixtureCredential(), TaskFilter{}); err != nil {
			t.Fatalf("FetchTasks: %v", err)
		}
		req := u.requests[0]
		if req.Method != http.MethodGet || req.URL != "https://chatgpt.com/backend-api/tasks" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Headers.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
	})

	t.Run("401 typed", func(t *testing.T) {
		u := &scriptedUpstream{responses: []Resp{pollResp(401, "", `{"error":"unauthorized"}`)}}
		if _, err := FetchTasks(context.Background(), u, fixtureCredential(), TaskFilter{}); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("error = %v, want invalid_token", err)
		}
	})
}

func TestTaskErrorDetection(t *testing.T) {
	u := &scriptedUpstream{responses: []Resp{pollResp(200, "", tasksFixture)}}
	tasks, err := FetchTasks(context.Background(), u, fixtureCredential(), TaskFilter{})
	if err != nil {
		t.Fatalf("FetchTasks: %v", err)
	}
	byID := map[string]Task{}
	for _, task := range tasks {
		id, _ := task.Raw["task_id"].(string)
		byID[id] = task
	}

	cases := []struct {
		id        string
		wantError bool
		wantMsg   string
	}{
		{"t-1", true, "errone"}, // is_error + text content: parts joined
		{"t-2", true, ""},       // is_error but multimodal: no message text
		{"t-3", false, ""},      // is_error false: parts ignored
		{"t-4", false, ""},      // no image_gen_message at all
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			isError, message := byID[tc.id].Error()
			if isError != tc.wantError || message != tc.wantMsg {
				t.Fatalf("Task.Error = (%v, %q), want (%v, %q)", isError, message, tc.wantError, tc.wantMsg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Anon variant of the poll path.
// ---------------------------------------------------------------------------

func TestPollAnonPath(t *testing.T) {
	u := &scriptedUpstream{responses: []Resp{pollResp(200, "", pollDocFixture)}}
	rec := &sleepRecorder{}
	if _, err := PollConversation(context.Background(), u, plugin.Credential{}, "c-1",
		deterministicOpts(rec, 120*time.Second, 10*time.Second, 10*time.Second)); err != nil {
		t.Fatalf("PollConversation: %v", err)
	}
	if got := u.requests[0].URL; got != "https://chatgpt.com/backend-anon/conversation/c-1" {
		t.Fatalf("anon poll URL = %s", got)
	}
	if u.requests[0].Headers.Get("Authorization") != "" {
		t.Fatalf("anon poll must not carry Authorization")
	}
}
