package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// recordedCall captures one caller.Call invocation (the host RPC seam).
type recordedCall struct {
	method  string
	request []byte
}

// recordingCaller is the offline host seam for transport tests (the
// commandcode forwardingBridge pattern, minus the real network): it records
// every Call and answers each host.http.* method from scripted results.
type recordingCaller struct {
	mu          sync.Mutex
	calls       []recordedCall
	doResult    any
	streamStart any
	streamReads []any
	failCode    string
}

func (r *recordingCaller) Call(method string, request []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{method: method, request: append([]byte(nil), request...)})
	if r.failCode != "" {
		out, _ := json.Marshal(pluginabi.Envelope{
			OK:    false,
			Error: &pluginabi.Error{Code: r.failCode, Message: "transport unavailable"},
		})
		return out, nil
	}
	switch method {
	case pluginabi.MethodHostHTTPDo:
		return hostEnvelopeOK(r.doResult)
	case pluginabi.MethodHostHTTPDoStream:
		return hostEnvelopeOK(r.streamStart)
	case pluginabi.MethodHostHTTPStreamRead:
		if len(r.streamReads) == 0 {
			return hostEnvelopeOK(map[string]any{"done": true})
		}
		out := r.streamReads[0]
		r.streamReads = r.streamReads[1:]
		return hostEnvelopeOK(out)
	case pluginabi.MethodHostHTTPStreamClose:
		return hostEnvelopeOK(map[string]any{})
	}
	return hostEnvelopeOK(map[string]any{})
}

// hostEnvelopeOK wraps a result in the success envelope the host sends back
// (pluginabi.Envelope with the result pre-marshaled).
func hostEnvelopeOK(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func (r *recordingCaller) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if call.method == method {
			n++
		}
	}
	return n
}

func (r *recordingCaller) lastRequest(t *testing.T, method string) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].method == method {
			return r.calls[i].request
		}
	}
	t.Fatalf("caller never received %s (got %v)", method, r.methods())
	return nil
}

func (r *recordingCaller) allRequests(t *testing.T, method string) [][]byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, call := range r.calls {
		if call.method == method {
			out = append(out, call.request)
		}
	}
	return out
}

func (r *recordingCaller) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, call.method)
	}
	return out
}

func newTestHostUpstream(t *testing.T) (*hostUpstream, *recordingCaller) {
	t.Helper()
	rec := &recordingCaller{
		doResult: map[string]any{
			"StatusCode": 200,
			"Headers":    map[string][]string{"X-Request-Id": {"rid-1"}},
			"Body":       []byte(`{"ok":true}`),
		},
		streamStart: map[string]any{
			"status_code": 200,
			"headers":     map[string][]string{"Content-Type": {"text/event-stream"}},
			"stream_id":   "stream-1",
		},
		streamReads: []any{
			map[string]any{"payload": []byte("data: one\n\n")},
			map[string]any{"payload": []byte("data: two\n\n"), "done": false},
			map[string]any{"done": true},
		},
	}
	cfg, err := model.ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	u, err := NewUpstream(cfg, rec)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	host, ok := u.(*hostUpstream)
	if !ok {
		t.Fatalf("default transport = %T, want *hostUpstream", u)
	}
	return host, rec
}

func conversationReq() Req {
	cred := fixtureCredential()
	headers := SessionHeaders(&cred, "/backend-api/conversation")
	headers = Authed(&cred, headers)
	return Req{
		Method:         "POST",
		URL:            "https://chatgpt.com/backend-api/conversation",
		Headers:        headers,
		Body:           []byte(`{"action":"next"}`),
		ContentType:    "application/json",
		HostCallbackID: "cb-1",
	}
}

// TestHostUpstreamRoundTrip verifies a non-streaming request travels through
// host.http.do with every field intact — callback id, method, URL, the full
// pinned header set, Content-Type applied, body byte-identical — and the
// response decodes back into Resp.
func TestHostUpstreamRoundTrip(t *testing.T) {
	u, rec := newTestHostUpstream(t)
	req := conversationReq()

	resp, err := u.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("resp.StatusCode = %d, want 200", resp.StatusCode)
	}
	if got := resp.Headers.Get("X-Request-Id"); got != "rid-1" {
		t.Fatalf("resp.Headers X-Request-Id = %q, want rid-1", got)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("resp.Body = %q, want the scripted body", resp.Body)
	}

	var wire hostHTTPRequest
	if err := json.Unmarshal(rec.lastRequest(t, pluginabi.MethodHostHTTPDo), &wire); err != nil {
		t.Fatalf("decode host.http.do request: %v", err)
	}
	if wire.HostCallbackID != "cb-1" {
		t.Fatalf("wire host_callback_id = %q, want cb-1", wire.HostCallbackID)
	}
	if wire.Method != http.MethodPost || wire.URL != req.URL {
		t.Fatalf("wire method/url = %q %q, want %q %q", wire.Method, wire.URL, req.Method, req.URL)
	}
	if !bytes.Equal(wire.Body, req.Body) {
		t.Fatalf("wire body = %q, want the exact request body bytes", wire.Body)
	}
	wireHeaders := http.Header(wire.Headers)
	if got := wireHeaders["Authorization"]; len(got) != 1 || got[0] != "Bearer fixture-access-token" {
		t.Fatalf("wire Authorization = %v, want the bearer token", got)
	}
	if got := wireHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("wire Content-Type = %q, want application/json", got)
	}
	if got := wireHeaders.Get("OAI-Device-Id"); got != fixtureCredential().Fp.OAIDeviceID {
		t.Fatalf("wire OAI-Device-Id = %q, want the fixture fingerprint id", got)
	}
	if got := wireHeaders.Get("X-OpenAI-Target-Path"); got != "/backend-api/conversation" {
		t.Fatalf("wire X-OpenAI-Target-Path = %q, want the request path", got)
	}
	if n := rec.count(pluginabi.MethodHostHTTPDo); n != 1 {
		t.Fatalf("host.http.do called %d times, want 1", n)
	}
}

// TestHostUpstreamStreamDrain verifies DoStream opens host.http.do_stream,
// drains stream_read to EOF with payloads intact, addresses every read to the
// returned stream id, and issues stream_close exactly once even when Close
// is called twice.
func TestHostUpstreamStreamDrain(t *testing.T) {
	u, rec := newTestHostUpstream(t)

	stream, err := u.DoStream(context.Background(), conversationReq())
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if stream.StatusCode != 200 {
		t.Fatalf("stream.StatusCode = %d, want 200", stream.StatusCode)
	}
	if got := stream.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", got)
	}
	if n := rec.count(pluginabi.MethodHostHTTPDoStream); n != 1 {
		t.Fatalf("host.http.do_stream called %d times, want 1", n)
	}

	var buf bytes.Buffer
	for {
		chunk, err := stream.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Read: %v", err)
		}
		buf.Write(chunk)
	}
	if buf.String() != "data: one\n\ndata: two\n\n" {
		t.Fatalf("drained stream = %q, want both scripted chunks in order", buf.String())
	}
	if n := rec.count(pluginabi.MethodHostHTTPStreamRead); n != 3 {
		t.Fatalf("host.http.stream_read called %d times, want 3 (two chunks + EOF)", n)
	}
	for _, raw := range rec.allRequests(t, pluginabi.MethodHostHTTPStreamRead) {
		var idReq hostStreamIDRequest
		if err := json.Unmarshal(raw, &idReq); err != nil {
			t.Fatalf("decode stream_read request: %v", err)
		}
		if idReq.StreamID != "stream-1" {
			t.Fatalf("stream_read addressed to %q, want stream-1", idReq.StreamID)
		}
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := rec.count(pluginabi.MethodHostHTTPStreamClose); n != 1 {
		t.Fatalf("host.http.stream_close called %d times, want exactly 1", n)
	}
}

// TestHostUpstreamSeamErrors verifies host-side failures surface as typed
// errors that name only the seam code — never request or response payload.
func TestHostUpstreamSeamErrors(t *testing.T) {
	u := &hostUpstream{host: &recordingCaller{failCode: "transport_refused"}}
	_, err := u.Do(context.Background(), Req{Method: "GET", URL: "https://chatgpt.com/x"})
	if err == nil {
		t.Fatal("Do with a failing host envelope must error")
	}
	if !strings.Contains(err.Error(), "transport_refused") {
		t.Fatalf("error = %v, want it to name the seam code", err)
	}

	_, err = u.DoStream(context.Background(), Req{Method: "GET", URL: "https://chatgpt.com/x"})
	if err == nil {
		t.Fatal("DoStream with a failing host envelope must error")
	}
	if !strings.Contains(err.Error(), "transport_refused") {
		t.Fatalf("error = %v, want it to name the seam code", err)
	}
}

// TestNewUpstreamSelection pins the transport choice: stdlib/empty mode picks
// the host transport, utls picks the direct client (whose utls hello is
// pending the Task 2 verdict, so requests fail typed), and unknown modes or a
// missing host are refused.
func TestNewUpstreamSelection(t *testing.T) {
	rec := &recordingCaller{}

	t.Run("stdlib default", func(t *testing.T) {
		cfg, err := model.ParseConfig(nil)
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		u, err := NewUpstream(cfg, rec)
		if err != nil {
			t.Fatalf("NewUpstream: %v", err)
		}
		if _, ok := u.(*hostUpstream); !ok {
			t.Fatalf("TLSMode %q selected %T, want *hostUpstream", cfg.TLSMode, u)
		}
	})

	t.Run("empty mode falls back to host transport", func(t *testing.T) {
		u, err := NewUpstream(model.Config{TLSMode: ""}, rec)
		if err != nil {
			t.Fatalf("NewUpstream: %v", err)
		}
		if _, ok := u.(*hostUpstream); !ok {
			t.Fatalf("empty TLSMode selected %T, want *hostUpstream", u)
		}
	})

	t.Run("utls picks direct transport, requests fail typed", func(t *testing.T) {
		u, err := NewUpstream(model.Config{TLSMode: "utls"}, rec)
		if err != nil {
			t.Fatalf("NewUpstream: %v", err)
		}
		if _, ok := u.(*directUpstream); !ok {
			t.Fatalf("TLSMode utls selected %T, want *directUpstream", u)
		}
		_, err = u.Do(context.Background(), Req{Method: "GET", URL: "https://chatgpt.com/x"})
		if !errors.Is(err, errUtlsPending) {
			t.Fatalf("Do under pending utls = %v, want errors.Is(errUtlsPending)", err)
		}
		_, err = u.DoStream(context.Background(), Req{Method: "GET", URL: "https://chatgpt.com/x"})
		if !errors.Is(err, errUtlsPending) {
			t.Fatalf("DoStream under pending utls = %v, want errors.Is(errUtlsPending)", err)
		}
	})

	t.Run("nil host refused", func(t *testing.T) {
		if _, err := NewUpstream(model.Config{TLSMode: "stdlib"}, nil); err == nil {
			t.Fatal("NewUpstream with nil host must error")
		}
	})

	t.Run("unknown mode refused", func(t *testing.T) {
		if _, err := NewUpstream(model.Config{TLSMode: "bogus"}, rec); err == nil {
			t.Fatal("NewUpstream with unknown TLSMode must error")
		}
	})
}

// TestDirectUpstreamStdlib exercises the direct client's full stdlib path
// against a localhost httptest server (offline): request/response round-trip,
// stream drain to EOF, idempotent close, and proxy wiring from the URL.
func TestDirectUpstreamStdlib(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-T", "yes")
		fmt.Fprint(w, `{"x":1}`)
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, frame := range []string{"data: a\n\n", "data: b\n\n"} {
			io.WriteString(w, frame)
			flusher.Flush()
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := model.Config{TLSMode: "stdlib", RequestTimeoutSecs: 30}
	direct, err := newDirectUpstream(cfg, "", false)
	if err != nil {
		t.Fatalf("newDirectUpstream: %v", err)
	}

	cred := fixtureCredential()
	headers := Authed(&cred, SessionHeaders(&cred, "/backend-api/models"))

	resp, err := direct.Do(context.Background(), Req{
		Method:  http.MethodGet,
		URL:     server.URL + "/json",
		Headers: headers,
	})
	if err != nil {
		t.Fatalf("direct.Do: %v", err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"x":1}` {
		t.Fatalf("direct.Do = %d %q, want 200 {\"x\":1}", resp.StatusCode, resp.Body)
	}
	if got := resp.Headers.Get("X-T"); got != "yes" {
		t.Fatalf("direct.Do response header X-T = %q, want yes", got)
	}

	stream, err := direct.DoStream(context.Background(), Req{
		Method:  http.MethodGet,
		URL:     server.URL + "/sse",
		Headers: headers,
	})
	if err != nil {
		t.Fatalf("direct.DoStream: %v", err)
	}
	var buf bytes.Buffer
	for {
		chunk, err := stream.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream.Read: %v", err)
		}
		buf.Write(chunk)
	}
	if buf.String() != "data: a\n\ndata: b\n\n" {
		t.Fatalf("direct stream drained = %q, want both frames", buf.String())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream.Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second stream.Close: %v", err)
	}

	// The proxy URL must be wired into the client transport.
	proxied, err := newDirectUpstream(cfg, "http://127.0.0.1:9", false)
	if err != nil {
		t.Fatalf("newDirectUpstream with proxy: %v", err)
	}
	transport, ok := proxied.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", proxied.client.Transport)
	}
	if transport.TLSClientConfig != nil {
		t.Fatalf("stdlib direct transport must not carry a custom tls.Config yet")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/", nil)
	target, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy: %v", err)
	}
	if target.String() != "http://127.0.0.1:9" {
		t.Fatalf("proxy target = %q, want http://127.0.0.1:9", target)
	}
}
