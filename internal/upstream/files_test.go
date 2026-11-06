package upstream

// files_test.go pins the reference-image upload trio (oracle
// openai_backend_api.py _upload_image): exact call order, byte-pinned bodies
// and headers (including the x-ms-blob-type / x-ms-version pair on the blob
// PUT), and typed rejection of a failed PUT.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// osReadFixture loads one testdata file without a *testing.T (for scripted
// closures); tests read through their own t.Fatalf path instead.
func osReadFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

// pngBytes is a 1x1 transparent PNG — opaque test data, never a secret.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 'I', 'D', 'A',
	'T', 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xAE,
	0x42, 0x60, 0x82,
}

// ---------------------------------------------------------------------------
// drawFake — scripts the whole drawing flow. Do answers per "METHOD URL"
// closure (closures may carry per-call state for sequenced hits on one URL);
// DoStream replays fixture chunks in 7-byte pieces or stalls until Close.
// ---------------------------------------------------------------------------

type drawFake struct {
	mu       sync.Mutex
	requests []Req
	handlers map[string]func() Resp
	chunks   [][]byte
	idx      int
	stall    bool
	closed   bool
}

func newDrawFake(t *testing.T) *drawFake {
	t.Helper()
	return &drawFake{handlers: map[string]func() Resp{}}
}

func (f *drawFake) on(key string, makeResp func() Resp) {
	f.handlers[key] = makeResp
}

func (f *drawFake) status(key string, status int, body []byte) {
	f.on(key, func() Resp {
		return Resp{StatusCode: status, Headers: http.Header{}, Body: body}
	})
}

func (f *drawFake) fixture(key, fixture string, status int) {
	f.on(key, func() Resp {
		raw, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			return Resp{StatusCode: 500, Body: []byte(fmt.Sprintf("fixture read failed: %v", err))}
		}
		return Resp{StatusCode: status, Headers: http.Header{}, Body: raw}
	})
}

func (f *drawFake) Do(ctx context.Context, req Req) (Resp, error) {
	if err := ctx.Err(); err != nil { // real transports refuse a dead context up front
		return Resp{}, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	handler, ok := f.handlers[req.Method+" "+req.URL]
	f.mu.Unlock()
	if !ok {
		return Resp{}, fmt.Errorf("drawFake: no handler scripted for %s %s", req.Method, req.URL)
	}
	return handler(), nil
}

func (f *drawFake) DoStream(_ context.Context, req Req) (*Stream, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	chunks, stall := f.chunks, f.stall
	f.mu.Unlock()

	var stallClosed chan struct{}
	if stall {
		stallClosed = make(chan struct{})
	}
	return &Stream{
		StatusCode: 200,
		Headers:    http.Header{},
		read: func() ([]byte, error) {
			f.mu.Lock()
			if stall {
				f.mu.Unlock()
				<-stallClosed // the watchdog's Close is the only wake-up
				return nil, fmt.Errorf("drawFake: stall stream was closed")
			}
			if f.idx >= len(chunks) {
				f.mu.Unlock()
				return nil, io.EOF
			}
			chunk := chunks[f.idx]
			f.idx++
			f.mu.Unlock()
			return chunk, nil
		},
		close: func() error {
			f.mu.Lock()
			f.closed = true
			if stall {
				close(stallClosed)
			}
			f.mu.Unlock()
			return nil
		},
	}, nil
}

func (f *drawFake) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *drawFake) requestsFor(method, url string) []Req {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Req
	for _, req := range f.requests {
		if req.Method == method && req.URL == url {
			out = append(out, req)
		}
	}
	return out
}

func (f *drawFake) requestsForMethod(method string) []Req {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Req
	for _, req := range f.requests {
		if req.Method == method {
			out = append(out, req)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared seams and helpers (image_drawing_test.go reuses these).
// ---------------------------------------------------------------------------

// fixedEpoch anchors drawingNow in tests so the conversation-recovery window
// arithmetic is deterministic against the fixture update_time values.
var fixedEpoch = time.Unix(1700000080, 0)

// fakeDrawingClock swaps the drawing clock seams: every sleep is recorded
// (never elapsed), jitter is zero, now is fixed.
func fakeDrawingClock(t *testing.T, sleeps *[]time.Duration) {
	t.Helper()
	oldSleep, oldJitter, oldNow := drawingSleep, drawingJitter, drawingNow
	drawingSleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	drawingJitter = func(time.Duration) time.Duration { return 0 }
	drawingNow = func() time.Time { return fixedEpoch }
	t.Cleanup(func() {
		drawingSleep, drawingJitter, drawingNow = oldSleep, oldJitter, oldNow
	})
}

func testDrawingCfg() model.Config {
	return model.Config{
		DrawingUpstreamModel:  "gpt-5-5",
		ThinkingEffort:        "auto",
		ImagePollTimeoutSecs:  120,
		ImagePollIntervalSecs: 10,
	}
}

func sseFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read SSE fixture %s: %v", name, err)
	}
	return chunkBytes(raw, 7)
}

func assertImageError(t *testing.T, err error, code string) {
	t.Helper()
	var imgErr *ImageError
	if !asImageError(err, &imgErr) {
		t.Fatalf("error = %v, want typed *ImageError", err)
	}
	if imgErr.Code != code {
		t.Fatalf("error code = %q, want %q", imgErr.Code, code)
	}
}

func assertProgress(t *testing.T, steps []string, want ...string) {
	t.Helper()
	if len(steps) != len(want) {
		t.Fatalf("progress steps = %v, want %v", steps, want)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Fatalf("progress steps = %v, want %v", steps, want)
		}
	}
}

// ---------------------------------------------------------------------------
// TestUploadTrioOrder — 3 calls, exact URLs, byte-pinned bodies and headers.
// ---------------------------------------------------------------------------

func TestUploadTrioOrder(t *testing.T) {
	fake := newDrawFake(t)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files", "files-create.json", 200)
	fake.status("PUT https://upload.blob.example.com/containers/refs/blob-abc", http.StatusCreated, nil)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files/file_00000000a1b2c3d4e5f60718293a4b/uploaded", "files-uploaded.json", 200)

	assets, err := UploadReferenceImages(context.Background(), fake, fixtureCredential(), []ReferenceImage{
		{Data: pngBytes, Width: 4, Height: 3},
	})
	if err != nil {
		t.Fatalf("UploadReferenceImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(assets))
	}
	asset := assets[0]
	wantID := "file_00000000a1b2c3d4e5f60718293a4b"
	if asset.FileID != wantID || asset.FileName != "image_1.png" || asset.MimeType != "image/png" ||
		asset.Size != len(pngBytes) || asset.Width != 4 || asset.Height != 3 {
		t.Fatalf("asset = %+v, want the uploaded metadata", asset)
	}

	if len(fake.requests) != 3 {
		t.Fatalf("requests = %d, want the trio (3)", len(fake.requests))
	}

	// Step 1: create — session headers + bearer + pinned body.
	create := fake.requests[0]
	if create.Method != http.MethodPost || create.URL != chatGPTBaseURL+"/backend-api/files" {
		t.Fatalf("step 1 = %s %s, want POST /backend-api/files", create.Method, create.URL)
	}
	wantBody := fmt.Sprintf(`{"file_name": "image_1.png", "file_size": %d, "use_case": "multimodal", "width": 4, "height": 3}`, len(pngBytes))
	if string(create.Body) != wantBody {
		t.Fatalf("step 1 body = %s, want %s", create.Body, wantBody)
	}
	if got := create.Headers.Get("X-OpenAI-Target-Path"); got != "/backend-api/files" {
		t.Fatalf("step 1 X-OpenAI-Target-Path = %q", got)
	}
	if got := create.Headers.Get("X-OpenAI-Target-Route"); got != "/backend-api/files" {
		t.Fatalf("step 1 X-OpenAI-Target-Route = %q", got)
	}
	if got := create.Headers.Get("Accept"); got != "application/json" {
		t.Fatalf("step 1 Accept = %q", got)
	}
	if got := create.Headers.Get("Authorization"); got != "Bearer fixture-access-token" {
		t.Fatalf("step 1 Authorization = %q", got)
	}
	if got := create.Headers.Get("OAI-Client-Version"); got != pinnedClientVersion {
		t.Fatalf("step 1 fingerprint header missing (OAI-Client-Version = %q)", got)
	}

	// Step 2: blob PUT — the oracle's exact raw header set, raw image bytes.
	put := fake.requests[1]
	if put.Method != http.MethodPut || put.URL != "https://upload.blob.example.com/containers/refs/blob-abc" {
		t.Fatalf("step 2 = %s %s, want PUT {upload_url}", put.Method, put.URL)
	}
	if string(put.Body) != string(pngBytes) {
		t.Fatalf("step 2 body is not the raw image bytes")
	}
	if got := put.Headers.Get("x-ms-blob-type"); got != "BlockBlob" {
		t.Fatalf("step 2 x-ms-blob-type = %q, want BlockBlob", got)
	}
	if got := put.Headers.Get("x-ms-version"); got != "2020-04-08" {
		t.Fatalf("step 2 x-ms-version = %q, want 2020-04-08", got)
	}
	if got := put.Headers.Get("Content-Type"); got != "image/png" {
		t.Fatalf("step 2 Content-Type = %q, want the image mime", got)
	}
	if got := put.Headers.Get("Accept-Language"); got != "en-US,en;q=0.8" {
		t.Fatalf("step 2 Accept-Language = %q, want the oracle blob value", got)
	}
	if got := put.Headers.Get("Origin"); got != chatGPTBaseURL {
		t.Fatalf("step 2 Origin = %q", got)
	}
	if put.Headers.Get("Authorization") != "" {
		t.Fatalf("step 2 must not carry Authorization (the upload_url carries its own SAS)")
	}
	if put.Headers.Get("X-OpenAI-Target-Path") != "" {
		t.Fatalf("step 2 must not carry X-OpenAI-Target-* (the oracle PUT is a raw blob call)")
	}

	// Step 3: confirm — byte-exempt "{}" body, session headers + bearer.
	confirm := fake.requests[2]
	if confirm.Method != http.MethodPost || confirm.URL != chatGPTBaseURL+"/backend-api/files/"+wantID+"/uploaded" {
		t.Fatalf("step 3 = %s %s, want POST /backend-api/files/{file_id}/uploaded", confirm.Method, confirm.URL)
	}
	if string(confirm.Body) != "{}" {
		t.Fatalf("step 3 body = %s, want {}", confirm.Body)
	}
	if got := confirm.Headers.Get("X-OpenAI-Target-Path"); got != "/backend-api/files/"+wantID+"/uploaded" {
		t.Fatalf("step 3 X-OpenAI-Target-Path = %q", got)
	}
}

// TestUploadTrioRejectsBadPutStatus — a failed blob PUT ends the upload typed,
// before the confirm call.

func TestUploadTrioRejectsBadPutStatus(t *testing.T) {
	fake := newDrawFake(t)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files", "files-create.json", 200)
	fake.status("PUT https://upload.blob.example.com/containers/refs/blob-abc", http.StatusForbidden, []byte(`{"message": "denied"}`))

	_, err := UploadReferenceImages(context.Background(), fake, fixtureCredential(), []ReferenceImage{
		{Data: pngBytes, Width: 4, Height: 3},
	})
	assertImageError(t, err, ImageCodeUpstreamStatus)
	var imgErr *ImageError
	if !asImageError(err, &imgErr) {
		t.Fatalf("error = %v, want typed *ImageError", err)
	}
	if imgErr.HTTPStatus != http.StatusForbidden || imgErr.Retryable {
		t.Fatalf("error = %+v, want 403 non-retryable", imgErr)
	}
	if got := len(fake.requests); got != 2 {
		t.Fatalf("requests = %d, want 2 (the confirm must not run after a failed PUT)", got)
	}
}

func TestUploadReferenceImagesEmpty(t *testing.T) {
	fake := newDrawFake(t)
	assets, err := UploadReferenceImages(context.Background(), fake, fixtureCredential(), nil)
	if err != nil || len(assets) != 0 {
		t.Fatalf("no references: got %v, %v — want empty success", assets, err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("no references must issue no upstream calls (got %d)", len(fake.requests))
	}
}

func TestUploadCancelledContext(t *testing.T) {
	fake := newDrawFake(t)
	fake.fixture("POST "+chatGPTBaseURL+"/backend-api/files", "files-create.json", 200)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: the transport reports context.Canceled

	_, err := UploadReferenceImages(ctx, fake, fixtureCredential(), []ReferenceImage{
		{Data: pngBytes, Width: 1, Height: 1},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the raw context.Canceled (never a retryable upstream_status)", err)
	}
}

func TestUploadTrioRejects401OnCreate(t *testing.T) {
	fake := newDrawFake(t)
	fake.status("POST "+chatGPTBaseURL+"/backend-api/files", http.StatusUnauthorized, nil)

	_, err := UploadReferenceImages(context.Background(), fake, fixtureCredential(), []ReferenceImage{
		{Data: pngBytes, Width: 1, Height: 1},
	})
	assertImageError(t, err, ImageCodeInvalidToken)
}
