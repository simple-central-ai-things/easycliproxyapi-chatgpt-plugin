package upstream

// poll.go ports the reference's post-stream conversation document poll and
// its task queries:
//
//   - openai_backend_api.py _get_conversation (GET /backend-api/conversation/{id})
//   - openai_backend_api.py delete_conversation (PATCH {"is_visible": false})
//   - openai_backend_api.py _poll_image_results wait/backoff loop (initial
//     10s + jitter, steady interval, exponential backoff capped at 16s,
//     Retry-After honored on 429/5xx, all sleeps clamped to the budget)
//   - openai_backend_api.py _query_backend_tasks + check_task_error (GET
//     /backend-api/tasks, client-side filtering, structured error detection)
//
// The image-specific settle/check-before-hit machinery stays with the
// drawing route (Task 9): it drives its own hit predicate through
// PollOptions.Done.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// The oracle's poll cadence constants (image_poll_initial_wait_secs default
// 10, the 2**min(attempt,4) exponent capped at 16s, jitter 0..0.5s on
// retries, 0..min(2s, 0.2*initial_wait) on the initial wait).
const (
	defaultPollInitialWait = 10 * time.Second
	maxPollBackoff         = 16 * time.Second
	retryJitterMax         = 500 * time.Millisecond
	initialWaitJitterCap   = 2 * time.Second
)

// PollOptions tunes one PollConversation run. Zero Timeout/Interval/
// InitialWait fall back to the oracle defaults.
type PollOptions struct {
	// Timeout is the whole poll budget; exhaustion raises the typed
	// conversation_poll_timeout.
	Timeout time.Duration
	// Interval is the steady wait between successful fetches.
	Interval time.Duration
	// InitialWait is the pre-first-fetch wait (default 10s + jitter).
	InitialWait time.Duration
	// Done, when set, is consulted after every successful fetch; the poll
	// returns that document once it holds (the oracle's file-id hit
	// predicate lives here for the drawing route). When nil, the first
	// successfully fetched document is returned.
	Done func(ConversationDoc) bool

	// SkipInitialWait disables the pre-first-fetch wait: the oracle's
	// search poll starts immediately, unlike the drawing route's 10s
	// commit delay allowance.
	SkipInitialWait bool
	// RetryableStatuses additionally classifies these upstream statuses as
	// retryable (backed off and re-polled) instead of failing the budget
	// immediately — the oracle's search swallow set treats 404/409/423 on a
	// fresh conversation as "not ready yet", not an error.
	RetryableStatuses map[int]bool

	// sleep and jitter are same-package test seams (fake clock); zero
	// values select the real implementations.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(max time.Duration) time.Duration
}

// NewPollOptions derives the poll cadence from the plugin config, mirroring
// the oracle reusing image_poll_timeout_secs / image_poll_interval_secs.
func NewPollOptions(cfg model.Config) PollOptions {
	return PollOptions{
		Timeout:     time.Duration(cfg.ImagePollTimeoutSecs) * time.Second,
		Interval:    time.Duration(cfg.ImagePollIntervalSecs) * time.Second,
		InitialWait: defaultPollInitialWait,
	}
}

// ConversationDoc is one fetched conversation document. Mapping is the
// message_id → node ({message: {...}}) map the oracle's
// _extract_image_tool_records and policy scan read; Raw keeps the full body
// for downstream scans the doc layer does not type.
type ConversationDoc struct {
	Raw     json.RawMessage
	Mapping map[string]any
}

// ctxSleep is the real poll sleep: a timer that unwinds on context cancel.
func ctxSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// PollConversation fetches GET /backend-api/conversation/{id} until it
// succeeds (opts.Done when set), the budget runs out, or the caller context
// is cancelled. 429/5xx and transport errors back off exponentially
// (2/4/8/16/16s, Retry-After honored when present); a 401 returns the typed
// invalid_token immediately, like the oracle's non-retryable branch.
func PollConversation(ctx context.Context, u Upstream, cred model.Credential, conversationID string, opts PollOptions) (ConversationDoc, error) {
	if strings.TrimSpace(conversationID) == "" {
		return ConversationDoc{}, errors.New("PollConversation: a conversation id is required")
	}
	if opts.sleep == nil {
		opts.sleep = ctxSleep
	}
	if opts.jitter == nil {
		opts.jitter = randDuration
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second // the oracle's image_poll_timeout_secs default
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.InitialWait <= 0 {
		opts.InitialWait = defaultPollInitialWait
	}

	authed := strings.TrimSpace(cred.AccessToken) != ""
	path := "/backend-api/conversation/" + conversationID
	if !authed {
		path = AnonPath(path)
	}

	// The budget is accounted as the sum of requested sleeps, not wall
	// clock: a fake-clock sleep in tests advances it exactly, and in
	// production every real sleep elapses ≈ what it requested. This keeps
	// the backoff arithmetic deterministic under both.
	var slept time.Duration
	origSleep := opts.sleep
	opts.sleep = func(ctx context.Context, d time.Duration) error {
		slept += d
		return origSleep(ctx, d)
	}
	remaining := func() time.Duration { return opts.Timeout - slept }

	// The oracle's initial wait: ChatGPT commits the conversation document
	// well after the SSE stream closes, and polling immediately trips the
	// transient 429 the upstream returns within ~200ms of close. The search
	// poll skips it (oracle _wait_search_result starts fetching immediately).
	if !opts.SkipInitialWait {
		if wait := opts.InitialWait + opts.jitter(min(initialWaitJitterCap, opts.InitialWait/5)); wait > 0 {
			sleepFor := min(wait, max(0, remaining()))
			if sleepFor > 0 {
				if err := opts.sleep(ctx, sleepFor); err != nil {
					return ConversationDoc{}, err
				}
			}
		}
	}

	// _retry_sleep: retry_after=0 means "retry immediately" — it must not be
	// coerced away by a falsy check.
	retrySleep := func(attempt int, retryAfter *int) (bool, error) {
		var base time.Duration
		if retryAfter != nil {
			base = time.Duration(*retryAfter) * time.Second
		} else {
			base = time.Duration(1<<uint(min(attempt, 4))) * time.Second
		}
		backoff := base + opts.jitter(retryJitterMax)
		if remaining() <= 0 {
			return false, nil
		}
		if err := opts.sleep(ctx, min(backoff, max(0, remaining()))); err != nil {
			return false, err
		}
		return true, nil
	}

	attempt := 0
	for remaining() > 0 {
		attempt++

		headers := SessionHeaders(&cred, path)
		if authed {
			headers = Authed(&cred, headers)
		}
		headers.Set("Accept", "application/json")

		resp, err := u.Do(ctx, Req{
			Method:  http.MethodGet,
			URL:     chatGPTBaseURL + path,
			Headers: headers,
		})
		if err != nil {
			// Transport failure: the oracle backs off like a status retry.
			if ok, sleepErr := retrySleep(attempt, nil); ok {
				continue
			} else if sleepErr != nil {
				return ConversationDoc{}, sleepErr
			}
			break
		}
		if err := statusError(resp.StatusCode, parseRetryAfter(resp.Headers)); err != nil {
			var convErr *ConvError
			errors.As(err, &convErr)
			// A 401 is never retried here (the oracle's immediate branch);
			// the search swallow set (404/409/423) and ordinary 429/5xx
			// back off and re-poll.
			retryable := convErr != nil &&
				resp.StatusCode != http.StatusUnauthorized &&
				(convErr.Retryable ||
					(opts.RetryableStatuses != nil && opts.RetryableStatuses[resp.StatusCode]))
			if retryable {
				if ok, sleepErr := retrySleep(attempt, parseRetryAfter(resp.Headers)); ok {
					continue
				} else if sleepErr != nil {
					return ConversationDoc{}, sleepErr
				}
				break
			}
			return ConversationDoc{}, fmt.Errorf("conversation poll: %w", err)
		}

		doc, err := parseConversationDoc(resp.Body)
		if err != nil {
			return ConversationDoc{}, fmt.Errorf("conversation poll: %w", ErrMalformedConversationDoc)
		}
		if opts.Done == nil || opts.Done(doc) {
			return doc, nil
		}

		if wait := min(opts.Interval, max(0, remaining())); wait > 0 {
			if err := opts.sleep(ctx, wait); err != nil {
				return ConversationDoc{}, err
			}
		}
	}

	return ConversationDoc{}, fmt.Errorf("conversation poll: %w", ErrPollTimeout)
}

func parseConversationDoc(body []byte) (ConversationDoc, error) {
	var doc struct {
		Mapping map[string]any `json:"mapping"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ConversationDoc{}, err
	}
	return ConversationDoc{Raw: append(json.RawMessage(nil), body...), Mapping: doc.Mapping}, nil
}

// HideConversation is the oracle's delete_conversation: a PATCH that only
// hides the conversation from the upstream account ({"is_visible": false} —
// nothing is deleted). The caller gates it (the reference's config flags
// default off); this helper performs the write when told to.
func HideConversation(ctx context.Context, u Upstream, cred model.Credential, conversationID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("HideConversation: a conversation id is required")
	}
	authed := strings.TrimSpace(cred.AccessToken) != ""
	path := "/backend-api/conversation/" + conversationID
	if !authed {
		path = AnonPath(path)
	}

	// The oracle's delete_conversation header end state: Target-Route stays
	// the template, Target-Path is the concrete path, plus a /c/ referer.
	template := "/backend-api/conversation/{conversation_id}"
	headers := SessionHeaders(&cred, template)
	headers.Set("X-OpenAI-Target-Path", path)
	if authed {
		headers = Authed(&cred, headers)
	}
	headers.Set("Accept", "*/*")
	headers.Set("Referer", chatGPTBaseURL+"/c/"+conversationID)

	body, err := appendPyJSONDefault(nil, pyObj{pairs: []pyPair{{key: "is_visible", val: false}}})
	if err != nil {
		return fmt.Errorf("hide conversation body: %w", err)
	}

	resp, err := u.Do(ctx, Req{
		Method:      http.MethodPatch,
		URL:         chatGPTBaseURL + path,
		Headers:     headers,
		Body:        body,
		ContentType: "application/json",
	})
	if err != nil {
		return fmt.Errorf("hide conversation: %w", err)
	}
	if err := statusError(resp.StatusCode, nil); err != nil {
		return fmt.Errorf("hide conversation: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task queries (the oracle's _query_backend_tasks + check_task_error).
// ---------------------------------------------------------------------------

// TaskFilter client-side filters the /backend-api/tasks response, exactly as
// the oracle filters after the fetch (the upstream endpoint accepts no
// query parameters).
type TaskFilter struct {
	// ConversationID matches tasks whose conversation_id OR
	// original_conversation_id equals it.
	ConversationID string
	// TaskID matches tasks whose task_id equals it.
	TaskID string
}

// Task is one upstream async task; Raw carries the full document (the
// oracle treats tasks as plain dicts).
type Task struct {
	Raw map[string]any
}

// Error reports the oracle's check_task_error structured detection: a task
// errors when image_gen_message.metadata.is_error is truthy, and the error
// text is the joined string parts of an image_gen_message with
// content_type "text". (The oracle computes the author-role check but its
// code only gates the message on is_error + text content; ported as code.)
func (t Task) Error() (bool, string) {
	imgMsg, _ := t.Raw["image_gen_message"].(map[string]any)
	if imgMsg == nil {
		return false, ""
	}
	metadata, _ := imgMsg["metadata"].(map[string]any)
	content, _ := imgMsg["content"].(map[string]any)
	isError := sentinelTruthy(metadata["is_error"])
	contentType, _ := content["content_type"].(string)
	isTextOnly := contentType == "text"

	message := ""
	if isError && isTextOnly {
		parts, _ := content["parts"].([]any)
		var text strings.Builder
		for _, part := range parts {
			if s, ok := part.(string); ok {
				text.WriteString(s)
			}
		}
		message = text.String()
	}
	return isError, message
}

// FetchTasks GETs /backend-api/tasks and applies the client-side filter.
func FetchTasks(ctx context.Context, u Upstream, cred model.Credential, filter TaskFilter) ([]Task, error) {
	authed := strings.TrimSpace(cred.AccessToken) != ""
	path := "/backend-api/tasks"
	if !authed {
		path = AnonPath(path)
	}

	headers := SessionHeaders(&cred, path)
	if authed {
		headers = Authed(&cred, headers)
	}
	headers.Set("Accept", "application/json")

	resp, err := u.Do(ctx, Req{
		Method:  http.MethodGet,
		URL:     chatGPTBaseURL + path,
		Headers: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("fetch tasks: %w", err)
	}
	if err := statusError(resp.StatusCode, parseRetryAfter(resp.Headers)); err != nil {
		return nil, fmt.Errorf("fetch tasks: %w", err)
	}

	var doc struct {
		Tasks []any `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("fetch tasks: %w", ErrMalformedConversationDoc)
	}

	tasks := make([]Task, 0, len(doc.Tasks))
	for _, item := range doc.Tasks {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if filter.ConversationID != "" {
			conversationID, _ := task["conversation_id"].(string)
			original, _ := task["original_conversation_id"].(string)
			if conversationID != filter.ConversationID && original != filter.ConversationID {
				continue
			}
		}
		if filter.TaskID != "" {
			taskID, _ := task["task_id"].(string)
			if taskID != filter.TaskID {
				continue
			}
		}
		tasks = append(tasks, Task{Raw: task})
	}
	return tasks, nil
}

// randDuration is the real jitter: uniform over [0, max).
func randDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Uint64() % uint64(max))
}
