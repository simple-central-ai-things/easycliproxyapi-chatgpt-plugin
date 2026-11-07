package plugin

// executor.go owns the executor RPC surface shared by every format the
// provider serves: the host wire request decode, the dispatch handlers
// (executor.execute / executor.execute_stream), the 401 refresh-retry loop
// (Review Focus #3) and the typed upstream-failure → envelope mapping
// (the error-class table pinned by the plan). The OpenAI text format lives in
// executor_text.go; the openai-image format (the host's /v1/images/generations
// and /v1/images/edits, arriving untranslated) lives in executor_image.go.
//
// The 401 contract: when the turn START (sentinel handshake or the
// conversation stream start) fails with a typed invalid_token, the executor
// forces the internal refresh (auth.go refreshToken — the same function
// behind auth.refresh), PERSISTS the rotated record via host.auth.save,
// and retries the start exactly once. A failed refresh, a failed persist or
// a second 401 ends in typed 401 invalid_token with no further refresh.
// The persist is mandatory, not best-effort: an OpenAI refresh rotates the
// refresh token, and the executor response carries no auth surface, so
// skipping the save would leave the pre-rotation (invalid) token in the
// auth file and break the host's next scheduled auth.refresh.
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
	"path"
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
	if isImageRequest(req) {
		return m.executeImage(req)
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
	if isImageRequest(req) {
		return m.executeImageStream(req)
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

	if turn.toolMode {
		// Tools + streaming: buffer the whole turn and emit the answer in
		// one shot (controller ruling #3, documented tradeoff — tool
		// requests are not token-streamed). The buffered drain runs to
		// completion inside Handle, like executor.execute, so no pump
		// goroutine is spawned.
		m.emitBufferedToolStream(req.StreamID, ch, turn, cancel)
		return ok(map[string]any{}), nil
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
	if req.Format != "" && req.Format != "openai" && req.Format != "openai-image" {
		return req, typedErr("unsupported_protocol", "requires the openai or openai-image executor format", 400)
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
// second only after a forced refresh succeeded AND was persisted.
//
// Persistence matters because an OpenAI refresh ROTATES the refresh token:
// refreshToken overwrites the record's RefreshToken when the endpoint
// returns a new one. The executor response carries no auth surface
// (Payload/Headers/Metadata only), so without an explicit host.auth.save
// the auth file would keep the pre-rotation (now invalid) refresh token,
// the host would never advance NextRefreshAfter, and its next scheduled
// auth.refresh would fail with invalid_grant until re-login. A failed
// save is treated as a failed refresh: retrying with an unpersisted
// credential would lose the rotation either way.
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
			// Persist the rotated record BEFORE the retry so the host's
			// pool holds the new refresh token even if this RPC dies now.
			if saveErr := m.saveCredential(turn.authFileName, refreshed); saveErr != nil {
				return nil, typedErr("invalid_token",
					"the forced refresh succeeded but the rotated credential could not be saved", 401)
			}
			turn.cred = refreshed
			continue
		}
		return nil, mapUpstreamError(err)
	}
	return nil, mapUpstreamError(startErr)
}

// execAuthFileName resolves the auth file name a refreshed record must be
// saved under: the host-selected AuthID (the pool identifier IS the file
// name — auth.go refreshFileName precedent), falling back to the source
// path's base when the identifier is empty. Empty means the record's name
// is unknown to us and the refresh cannot be persisted.
func execAuthFileName(req executorRequest) string {
	if strings.TrimSpace(req.AuthID) != "" {
		return req.AuthID
	}
	if sourcePath := req.AuthAttributes["path"]; sourcePath != "" {
		return path.Base(sourcePath)
	}
	return ""
}

// saveCredential persists one refreshed record back to its auth file via
// the host.auth.save callback (SDK wire shape {"name","json"}; the host
// validates the .json suffix and the JSON itself). The request carries
// token material — the one ABI crossing that is allowed, into the host's
// own credential store. Failures are reported without echoing payload
// material.
func (m *Manager) saveCredential(name string, cred Credential) error {
	if name == "" {
		return errors.New("the auth file name is unknown; the refreshed credential cannot be saved")
	}
	raw, _ := json.Marshal(pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: cred.StorageJSON(),
	})
	resp, err := m.host.Call(pluginabi.MethodHostAuthSave, raw)
	if err != nil {
		return errors.New("host auth save transport failed")
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return errors.New("host auth save response is not a valid envelope")
	}
	if !env.OK {
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		return fmt.Errorf("host auth save was rejected (code %q)", code)
	}
	return nil
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
