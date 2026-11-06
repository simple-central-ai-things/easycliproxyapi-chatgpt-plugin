// Package upstream owns the plugin's outbound transport to chatgpt.com.
//
// It defines the Upstream seam every later upstream task (sentinel,
// conversation SSE, quota, both image routes) calls through: two transport
// implementations behind one interface — hostUpstream (default; routes
// through the host.http.do / host.http.do_stream callbacks so host proxy
// policy applies) and directUpstream (a plugin-owned stdlib http.Client,
// used when tls-mode is "utls", since the host transport cannot carry a
// custom TLS hello) — plus the byte-pinned session header builder
// (headers.go) that every upstream request carries.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// Caller is the host RPC bridge seam (the Task 1 plugin.caller shape): one
// host callback round-trip per Call — method name plus JSON request bytes in,
// JSON envelope bytes out. main.go injects the C ABI; tests inject a
// recording fake.
type Caller interface {
	Call(method string, request []byte) ([]byte, error)
}

// Req is one upstream HTTP request in plugin-neutral form.
type Req struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
	// ContentType is applied as the Content-Type header when Headers does
	// not already set one.
	ContentType string
	// HostCallbackID scopes the host transport to the calling RPC — the
	// host_callback_id the host embedded in that RPC's request, threaded the
	// same way as in Tasks 3/4 (auth.go / login.go). Empty leaves the host
	// to fall back to the plugin-instance context.
	HostCallbackID string
}

// Resp is one completed upstream HTTP response.
type Resp struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Stream is an open upstream response body. Read returns the next chunk and
// io.EOF on a clean end of stream; Close releases the upstream stream and is
// idempotent. Stream is owned by one goroutine; Close is safe from another.
type Stream struct {
	StatusCode int
	Headers    http.Header

	read func() ([]byte, error)

	closeOnce sync.Once
	closeErr  error
	close     func() error
}

// Read returns the next payload chunk; io.EOF when the stream is drained.
func (s *Stream) Read() ([]byte, error) {
	if s.read == nil {
		return nil, io.EOF
	}
	return s.read()
}

// Close releases the upstream stream; exactly one transport close is issued
// even under repeated or concurrent calls.
func (s *Stream) Close() error {
	if s.close == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

// Upstream is the single outbound transport seam. Every later upstream task
// uses exactly this interface.
type Upstream interface {
	Do(ctx context.Context, req Req) (Resp, error)
	DoStream(ctx context.Context, req Req) (*Stream, error)
}

// NewUpstream selects the outbound transport for cfg: the host transport by
// default (tls-mode "stdlib" or unset — host proxy settings apply) and the
// direct client when tls-mode is "utls" (Task 2 verdict). host is the caller
// seam for the host transport; nil is refused there.
func NewUpstream(cfg model.Config, host Caller) (Upstream, error) {
	switch cfg.TLSMode {
	case "", "stdlib":
		if host == nil {
			return nil, errors.New("host caller is required for the host upstream transport")
		}
		return &hostUpstream{host: host}, nil
	case "utls":
		// TODO(Task 2 verdict): wire the utls hello (and the x/net/http2
		// transport it needs) and read the host proxy from
		// HostConfigSummary.ProxyURL once the Manager caches it — see
		// PROVENANCE.md. Until then utls selections fail typed per request
		// instead of silently degrading to stdlib TLS.
		return newDirectUpstream(cfg, "", true)
	default:
		return nil, fmt.Errorf("tls-mode %q is not supported by NewUpstream", cfg.TLSMode)
	}
}

// errUtlsPending marks a request refused because tls-mode "utls" awaits the
// Task 2 spike verdict and its dependency sign-off (PROVENANCE.md).
var errUtlsPending = errors.New(`utls tls-mode awaits the Task 2 spike verdict and is not wired yet; set tls-mode: stdlib to use the host transport (see PROVENANCE.md)`)

// ---------------------------------------------------------------------------
// hostUpstream — the default transport, routed through the host callbacks.
// ---------------------------------------------------------------------------

// hostUpstream issues upstream requests through host.http.do /
// host.http.do_stream + host.http.stream_read / host.http.stream_close so the
// host's proxy policy and transport apply. Wire shapes below are pinned by
// the host implementation (CLIProxyAPI internal/pluginhost host_callbacks.go):
// requests are snake_case, the do result is the untagged pluginapi.HTTPResponse
// (PascalCase keys, base64 body), the stream results are snake_case.
type hostUpstream struct {
	host Caller
}

// hostHTTPRequest is the wire shape accepted by host.http.do / do_stream.
type hostHTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// hostHTTPResult mirrors the host.http.do result payload: the host marshals
// pluginapi.HTTPResponse without json tags, so the keys are PascalCase and
// the body is base64.
type hostHTTPResult struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// hostStreamStartResult mirrors the host.http.do_stream result payload.
type hostStreamStartResult struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// hostStreamReadResult mirrors one host.http.stream_read result: a payload
// chunk, or done=true (with error set for an upstream failure).
type hostStreamReadResult struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostStreamIDRequest addresses an existing host stream.
type hostStreamIDRequest struct {
	StreamID string `json:"stream_id"`
}

func (u *hostUpstream) Do(ctx context.Context, req Req) (Resp, error) {
	if err := ctx.Err(); err != nil {
		return Resp{}, err
	}
	raw, err := u.host.Call(pluginabi.MethodHostHTTPDo, hostHTTPRequestWire(req))
	if err != nil {
		return Resp{}, fmt.Errorf("host http request failed: %w", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return Resp{}, err
	}
	if !env.OK {
		return Resp{}, seamError(env, "host http request")
	}
	var out hostHTTPResult
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return Resp{}, errors.New("host http response payload is invalid")
	}
	return Resp{StatusCode: out.StatusCode, Headers: http.Header(out.Headers), Body: out.Body}, nil
}

func (u *hostUpstream) DoStream(ctx context.Context, req Req) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := u.host.Call(pluginabi.MethodHostHTTPDoStream, hostHTTPRequestWire(req))
	if err != nil {
		return nil, fmt.Errorf("host http stream start failed: %w", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, seamError(env, "host http stream start")
	}
	var out hostStreamStartResult
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return nil, errors.New("host http stream start payload is invalid")
	}
	if out.StreamID == "" {
		return nil, errors.New("host http stream start returned no stream id")
	}
	return &Stream{
		StatusCode: out.StatusCode,
		Headers:    http.Header(out.Headers),
		read: func() ([]byte, error) {
			// The synchronous host callback cannot be interrupted; an
			// already-dead context fails fast before issuing it.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, err := u.host.Call(pluginabi.MethodHostHTTPStreamRead, streamIDRequest(out.StreamID))
			if err != nil {
				return nil, fmt.Errorf("host http stream read failed: %w", err)
			}
			env, err := decodeEnvelope(raw)
			if err != nil {
				return nil, err
			}
			if !env.OK {
				return nil, seamError(env, "host http stream read")
			}
			var r hostStreamReadResult
			if err := json.Unmarshal(env.Result, &r); err != nil {
				return nil, errors.New("host http stream read payload is invalid")
			}
			if r.Error != "" {
				return nil, errors.New(r.Error)
			}
			if r.Done {
				return nil, io.EOF
			}
			return r.Payload, nil
		},
		close: func() error {
			raw, err := u.host.Call(pluginabi.MethodHostHTTPStreamClose, streamIDRequest(out.StreamID))
			if err != nil {
				return fmt.Errorf("host http stream close failed: %w", err)
			}
			env, err := decodeEnvelope(raw)
			if err != nil {
				return err
			}
			if !env.OK {
				return seamError(env, "host http stream close")
			}
			return nil
		},
	}, nil
}

func streamIDRequest(streamID string) []byte {
	out, _ := json.Marshal(hostStreamIDRequest{StreamID: streamID})
	return out
}

// hostHTTPRequestWire marshals one Req for the host: headers cloned into
// canonical form, Content-Type applied when absent, body base64 by
// encoding/json.
func hostHTTPRequestWire(req Req) []byte {
	headers := make(http.Header, len(req.Headers))
	for key, values := range req.Headers {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if req.ContentType != "" && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", req.ContentType)
	}
	out, _ := json.Marshal(hostHTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        map[string][]string(headers),
		Body:           req.Body,
	})
	return out
}

// ---------------------------------------------------------------------------
// directUpstream — the plugin-owned stdlib http.Client fallback.
// ---------------------------------------------------------------------------

// directUpstream is the fallback transport: a stdlib http.Client the plugin
// owns end to end. It exists for tls-mode "utls", where the host transport
// cannot carry a custom TLS hello. The host proxy (HostConfigSummary.ProxyURL)
// is plumbed through proxyURL once the Manager caches a config summary;
// utlsPending gates requests that select the still-unwired utls hello.
type directUpstream struct {
	client *http.Client

	utlsPending bool
}

func newDirectUpstream(cfg model.Config, proxyURL string, utlsPending bool) (*directUpstream, error) {
	transport := &http.Transport{
		Proxy: proxyFunc(proxyURL),
	}
	// No client-level Timeout: http.Client.Timeout covers body reads and
	// would kill long SSE streams — deadlines are the caller's context
	// (Task 7's watchdog cancels them).
	return &directUpstream{client: &http.Client{Transport: transport}, utlsPending: utlsPending}, nil
}

// warnProxyFallbackOnce emits the invalid-proxy warning at most once per
// process: the transport is rebuilt per request, so a plain log call would
// turn one misconfiguration into request-path noise.
var warnProxyFallbackOnce sync.Once

// proxyFunc returns the client proxy function: the configured proxy URL when
// given, otherwise the process environment (http_proxy etc.). A malformed
// configured URL keeps the environment fallback and logs once that the URL
// was invalid — never the URL itself (it may carry credentials).
func proxyFunc(proxyURL string) func(*http.Request) (*url.URL, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return http.ProxyFromEnvironment
	}
	target, err := url.Parse(proxyURL)
	if err != nil {
		warnProxyFallbackOnce.Do(func() {
			log.Printf("chatgpt-proxy: configured proxy URL is invalid; environment proxy settings applied")
		})
		return http.ProxyFromEnvironment
	}
	return func(*http.Request) (*url.URL, error) { return target, nil }
}

func (d *directUpstream) Do(ctx context.Context, req Req) (Resp, error) {
	if d.utlsPending {
		return Resp{}, errUtlsPending
	}
	httpReq, err := directHTTPRequest(ctx, req)
	if err != nil {
		return Resp{}, err
	}
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return Resp{}, fmt.Errorf("direct http request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Resp{}, fmt.Errorf("direct http response read failed: %w", err)
	}
	return Resp{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

func (d *directUpstream) DoStream(ctx context.Context, req Req) (*Stream, error) {
	if d.utlsPending {
		return nil, errUtlsPending
	}
	httpReq, err := directHTTPRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("direct http stream start failed: %w", err)
	}
	const chunkSize = 4 << 10
	return &Stream{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		read: func() ([]byte, error) {
			buf := make([]byte, chunkSize)
			n, err := resp.Body.Read(buf)
			if n > 0 {
				// io.Reader may hand the final chunk back TOGETHER with the
				// io.EOF sentinel; surface the bytes first. net/http bodies
				// stick at EOF, so the next Read reports the clean io.EOF.
				return buf[:n], nil
			}
			// err is io.EOF (clean end) or a transport error; http bodies
			// never return n==0 with a nil error.
			return nil, err
		},
		close: func() error { return resp.Body.Close() },
	}, nil
}

// directHTTPRequest builds one stdlib request from a Req: headers cloned into
// the request, Content-Type applied when absent, ctx carried for deadlines.
func directHTTPRequest(ctx context.Context, req Req) (*http.Request, error) {
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build direct http request: %w", err)
	}
	for key, values := range req.Headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	if req.ContentType != "" && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	return httpReq, nil
}

// ---------------------------------------------------------------------------
// Shared envelope helpers.
// ---------------------------------------------------------------------------

// decodeEnvelope parses one host callback response envelope.
func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, errors.New("host response is not a valid envelope")
	}
	return env, nil
}

// seamError turns a host failure envelope into an error that names only the
// seam code — never request or response payload material (the host's message
// text is not trusted).
func seamError(env pluginabi.Envelope, what string) error {
	code := ""
	if env.Error != nil {
		code = env.Error.Code
	}
	return fmt.Errorf("%s failed (code %q)", what, code)
}
