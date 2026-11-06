package plugin

// executor.go owns the executor RPC surface shared by every format the
// provider serves: the host wire request decode, the dispatch handlers
// (executor.execute / executor.execute_stream), the 401 refresh-retry loop
// (Review Focus #3) and the typed upstream-failure → envelope mapping
// (the error-class table pinned by the plan). The OpenAI text format lands
// in executor_text.go; the openai-image format lands in Task 13.
//
// The 401 contract: when the turn START (sentinel handshake or the
// conversation stream start) fails with a typed invalid_token, the executor
// forces the internal refresh (auth.go refreshToken — the same function
// behind auth.refresh) and retries the start exactly once. A failed
// refresh or a second 401 ends in typed 401 invalid_token with no further
// refresh. The refreshed record is NOT persisted by the executor: the
// executor response carries no auth_update surface, and the host's own
// refresh lifecycle (NextRefreshAfter deadlines, fileName stability pinned
// by Task 3) owns persistence.
//
// Classification rule (controller ruling): errors are classified from the
// upstream HTTP status and the typed errors upstream already produces
// (*upstream.ConvError / *upstream.SentinelError) — never from the seam
// error code string, which seamError drops Retryable/HTTPStatus from.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/upstream"
)

// executorRequest mirrors the host wire shape of executor.execute /
// executor.execute_stream (the host's rpcExecutorRequest,
// internal/pluginhost/rpc_schema.go:80-84): the SDK request marshaled
// WITHOUT tags (so the keys are the Go field names, PascalCase) plus the
// downstream stream_id (streaming only) and the host_callback_id the host
// uses to scope the transport to this RPC.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleExecutorExecute implements executor.execute (non-stream text turn):
// shape the request, run the turn start with the refresh-retry loop, drain
// the event channel and answer with the chat.completion envelope.
func (m *Manager) handleExecutorExecute(request []byte) ([]byte, error) {
	req, err := decodeExecutorRequest(request)
	if err != nil {
		return errEnvelope(err), nil
	}
	turn, err := m.prepareTextTurn(req)
	if err != nil {
		return errEnvelope(err), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	defer cancel()

	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		return errEnvelope(err), nil
	}
	payload, err := assembleTextCompletion(ch, turn)
	if err != nil {
		return errEnvelope(err), nil
	}
	// The host decodes the result as the untagged SDK ExecutorResponse
	// (PascalCase keys): ok(result) marshals it in exactly that shape.
	return ok(pluginapi.ExecutorResponse{Payload: payload}), nil
}

// handleExecutorExecuteStream implements executor.execute_stream (streaming
// text turn): the same start path, then the pump goroutine emits OpenAI
// chunk frames downstream via host.stream.emit and closes exactly once
// via host.stream.close; the handler answers an empty result immediately
// (the mistral streamPump / commandcode emitAll streaming-over-ABI pattern).
func (m *Manager) handleExecutorExecuteStream(request []byte) ([]byte, error) {
	req, err := decodeExecutorRequest(request)
	if err != nil {
		return errEnvelope(err), nil
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errEnvelope(typedErr("invalid_stream", "downstream stream id missing", 400)), nil
	}
	turn, err := m.prepareTextTurn(req)
	if err != nil {
		return errEnvelope(err), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBudget(turn.cfg))
	ch, err := m.runTextTurnStart(ctx, turn, req.HostCallbackID)
	if err != nil {
		cancel()
		return errEnvelope(err), nil
	}

	// The pump outlives Handle: it is bounded by the SSE hard cap (the
	// watchdog inside runConversationStream) and ends when the event
	// channel closes, so it must not inherit a short-lived context.
	go m.pumpTextStream(req.StreamID, ch, turn, cancel)
	return ok(map[string]any{}), nil
}

// decodeExecutorRequest parses one executor RPC body. Failures are typed
// invalid_request errors — never Go errors escaping Handle.
func decodeExecutorRequest(request []byte) (executorRequest, *pluginabi.Error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return req, typedErr("invalid_request", "malformed executor request", 400)
	}
	if req.AuthProvider != "" && req.AuthProvider != providerKey {
		return req, typedErr("invalid_credential", "wrong provider credential", 401)
	}
	if req.Format != "" && req.Format != "openai" {
		// The openai-image format is Task 13's surface; refuse it
		// cleanly instead of crashing.
		return req, typedErr("unsupported_protocol", "requires the openai executor format", 400)
	}
	return req, nil
}

// turnBudget is the whole-turn context deadline (sentinel start + stream
// start +, for non-stream, the drain): the config's request-timeout-secs.
func turnBudget(cfg Config) time.Duration {
	return time.Duration(cfg.RequestTimeoutSecs) * time.Second
}

// runTextTurnStart runs the turn start (sentinel handshake + conversation
// SSE start) with the 401 refresh-retry loop: at most two attempts, the
// second only after a forced refresh succeeded.
func (m *Manager) runTextTurnStart(ctx context.Context, turn *textTurn, callbackID string) (<-chan upstream.ConvEvent, *pluginabi.Error) {
	var startErr error
	for attempt := 0; attempt < 2; attempt++ {
		ch, err := m.attemptTextTurnStart(ctx, turn)
		if err == nil {
			return ch, nil
		}
		startErr = err
		if attempt == 0 && turn.cfg.Retry401Refresh && isInvalidTokenError(err) {
			refreshed, refreshErr := m.refreshToken(ctx, callbackID, turn.cred)
			if refreshErr != nil {
				// A failed forced refresh ends in typed 401; the message
				// stays static (refresh details vary and are not ours).
				return nil, typedErr("invalid_token",
					"the upstream rejected the bearer token and the forced refresh failed", 401)
			}
			turn.cred = refreshed
			continue
		}
		return nil, mapUpstreamError(err)
	}
	return nil, mapUpstreamError(startErr)
}

// attemptTextTurnStart runs one sentinel handshake + conversation SSE start
// with the turn's credential. A credential without an access token is
// refused typed before any upstream call (the anonymous upstream chain is a
// non-goal) — the refusal classifies as invalid_token so the refresh-retry
// loop forces a refresh first, which is exactly what refresh is for.
func (m *Manager) attemptTextTurnStart(ctx context.Context, turn *textTurn) (<-chan upstream.ConvEvent, error) {
	if !hasAccessToken(turn.cred) {
		return nil, errMissingAccessToken
	}
	tokens, err := upstream.AcquireSentinel(ctx, turn.upstream, turn.cred, false)
	if err != nil {
		return nil, err
	}
	return upstream.StreamConversation(ctx, turn.upstream, turn.cred, tokens, turn.payload,
		upstream.WithSSEHardCap(turn.hardCap),
		upstream.WithRequestMessages(turn.messages))
}

// errMissingAccessToken is a turn start refused because the credential
// carries no access token; it classifies like an invalid_token.
var errMissingAccessToken = errors.New("credential has no access token")

// isInvalidTokenError classifies a typed 401 from the upstream's own error
// values: the conversation/profile legs surface it as *ConvError with
// invalid_token, the sentinel legs as *SentinelError carrying the upstream
// 401 status. Seam error strings are never parsed (controller ruling).
func isInvalidTokenError(err error) bool {
	if errors.Is(err, errMissingAccessToken) {
		return true
	}
	var convErr *upstream.ConvError
	if errors.As(err, &convErr) {
		return convErr.Code == "invalid_token" || convErr.HTTPStatus == http.StatusUnauthorized
	}
	var sentinelErr *upstream.SentinelError
	if errors.As(err, &sentinelErr) {
		return sentinelErr.HTTPStatus == http.StatusUnauthorized
	}
	return false
}

// mapUpstreamError maps a turn-start failure onto the typed envelope table
// (plan Review Focus error classes). Message text comes from the upstream's
// own static typed errors — never upstream body bytes (secret rule).
func mapUpstreamError(err error) *pluginabi.Error {
	if errors.Is(err, errMissingAccessToken) {
		return typedErr("invalid_token",
			"the credential has no access token and the forced refresh produced none", 401)
	}
	var convErr *upstream.ConvError
	if errors.As(err, &convErr) {
		switch {
		case convErr.Code == "invalid_token" || convErr.HTTPStatus == http.StatusUnauthorized:
			return typedErr("invalid_token", convErr.Error(), http.StatusUnauthorized)
		case convErr.Code == "rate_limited":
			message := convErr.Error()
			if convErr.RetryAfter > 0 {
				message += fmt.Sprintf("; retry after %ds", convErr.RetryAfter)
			}
			e := typedErr("rate_limited", message, http.StatusTooManyRequests)
			e.Retryable = true
			return e
		case convErr.Code == "upstream_stream_timeout":
			e := typedErr("upstream_stream_timeout", convErr.Error(), http.StatusGatewayTimeout)
			e.Retryable = true
			return e
		case convErr.HTTPStatus == http.StatusNotFound:
			return typedErr("no_available_model", convErr.Error(), http.StatusNotFound)
		default:
			status := convErr.HTTPStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			e := typedErr("upstream_error", convErr.Error(), status)
			e.Retryable = convErr.Retryable
			return e
		}
	}
	var sentinelErr *upstream.SentinelError
	if errors.As(err, &sentinelErr) {
		switch {
		case sentinelErr.HTTPStatus == http.StatusUnauthorized:
			return typedErr("invalid_token", sentinelErr.Error(), http.StatusUnauthorized)
		case sentinelErr.HTTPStatus == http.StatusTooManyRequests:
			e := typedErr("rate_limited", sentinelErr.Error(), http.StatusTooManyRequests)
			e.Retryable = true
			return e
		default:
			status := sentinelErr.HTTPStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			e := typedErr("upstream_error", sentinelErr.Error(), status)
			e.Retryable = sentinelErr.Retryable
			return e
		}
	}
	e := typedErr("upstream_error", "the upstream request failed: "+err.Error(), http.StatusBadGateway)
	e.Retryable = true
	return e
}

// turnID builds one OpenAI-style completion id: chatcmpl- + a fresh UUIDv4
// (the reference's uuid4 convention; the upstream never sends an id we can
// reuse, so nothing is invented about the conversation itself).
func turnID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("uuid generation: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("chatcmpl-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
