package upstream

// files.go ports the reference's image-reference upload trio
// (openai_backend_api.py _upload_image): POST /backend-api/files reserves an
// upload slot and returns the blob URL + file id, a raw PUT lands the image
// bytes on the blob store (the reference's exact eight-header set — no
// session fingerprint, no Authorization; the upload URL carries its own SAS),
// and POST /backend-api/files/{file_id}/uploaded confirms with a "{}" body.
// The bodies keep the reference's json.dumps default dialect and field order
// (appendPyJSONDefault, pinned by TestUploadTrioOrder).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// ReferenceImage is one image the caller wants attached as a drawing
// reference. The wire trio posts len(Data) as file_size and the caller-
// supplied dimensions — no image decoding happens here (the plugin has zero
// runtime dependencies), so Width/Height/MimeType come from the caller.
type ReferenceImage struct {
	// Data is the raw image bytes.
	Data []byte
	// FileName defaults to image_<n>.png (1-based position) when empty.
	FileName string
	// MimeType defaults to image/png when empty.
	MimeType string
	Width    int
	Height   int
}

// UploadedAsset is the metadata of one uploaded reference: the file id the
// image_asset_pointer parts and the metadata.attachments entries carry.
type UploadedAsset struct {
	FileID   string
	FileName string
	MimeType string
	Size     int
	Width    int
	Height   int
}

const defaultReferenceMimeType = "image/png"

// UploadReferenceImages runs the files trio once per reference image, in
// order. Any step's failure ends the run typed; no later step runs for a
// failed image, and no image starts after a sibling's failure.
func UploadReferenceImages(ctx context.Context, u Upstream, cred model.Credential, images []ReferenceImage) ([]UploadedAsset, error) {
	assets := make([]UploadedAsset, 0, len(images))
	for index, image := range images {
		asset, err := uploadOneReference(ctx, u, cred, image, index+1)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func uploadOneReference(ctx context.Context, u Upstream, cred model.Credential, image ReferenceImage, index int) (UploadedAsset, error) {
	fileName := strings.TrimSpace(image.FileName)
	if fileName == "" {
		fileName = fmt.Sprintf("image_%d.png", index)
	}
	mimeType := strings.TrimSpace(image.MimeType)
	if mimeType == "" {
		mimeType = defaultReferenceMimeType
	}

	// Step 1: reserve the upload slot.
	createPath := "/backend-api/files"
	createBody, err := appendPyJSONDefault(nil, pyObj{pairs: []pyPair{
		{key: "file_name", val: fileName},
		{key: "file_size", val: len(image.Data)},
		{key: "use_case", val: "multimodal"},
		{key: "width", val: image.Width},
		{key: "height", val: image.Height},
	}})
	if err != nil {
		return UploadedAsset{}, fmt.Errorf("upload reference image body: %w", err)
	}
	headers := SessionHeaders(&cred, createPath)
	headers = Authed(&cred, headers)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")

	resp, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + createPath,
		Headers:     headers,
		Body:        createBody,
		ContentType: "application/json",
	})
	if err != nil {
		return UploadedAsset{}, transportError("the upstream upload reservation failed", err)
	}
	if imgErr := imageStatusError(resp.StatusCode, parseRetryAfter(resp.Headers)); imgErr != nil {
		return UploadedAsset{}, imgErr
	}
	var uploadMeta struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(resp.Body, &uploadMeta); err != nil || uploadMeta.UploadURL == "" || uploadMeta.FileID == "" {
		return UploadedAsset{}, &ImageError{Code: ImageCodeUpstreamMalformed, Message: "the upstream upload reservation returned no upload_url or file_id"}
	}

	// Step 2: land the bytes on the blob store — the oracle's exact raw
	// header set (Content-Type is the image mime; no session fingerprint,
	// no Authorization, no X-OpenAI-Target-*).
	putHeaders := make(http.Header, 8)
	putHeaders.Set("Content-Type", mimeType)
	putHeaders.Set("x-ms-blob-type", "BlockBlob")
	putHeaders.Set("x-ms-version", "2020-04-08")
	putHeaders.Set("Origin", chatGPTBaseURL)
	putHeaders.Set("Referer", chatGPTBaseURL+"/")
	putHeaders.Set("User-Agent", sentinelUserAgent(&cred))
	putHeaders.Set("Accept", "application/json, text/plain, */*")
	putHeaders.Set("Accept-Language", "en-US,en;q=0.8")
	put, err := u.Do(ctx, Req{
		Method:  http.MethodPut,
		URL:     uploadMeta.UploadURL,
		Headers: putHeaders,
		Body:    image.Data,
	})
	if err != nil {
		return UploadedAsset{}, transportError("the upstream blob upload failed", err)
	}
	if imgErr := imageStatusError(put.StatusCode, parseRetryAfter(put.Headers)); imgErr != nil {
		return UploadedAsset{}, imgErr
	}

	// Step 3: confirm the upload with the byte-exact "{}" body.
	confirmPath := "/backend-api/files/" + uploadMeta.FileID + "/uploaded"
	confirmHeaders := SessionHeaders(&cred, confirmPath)
	confirmHeaders = Authed(&cred, confirmHeaders)
	confirmHeaders.Set("Content-Type", "application/json")
	confirmHeaders.Set("Accept", "application/json")
	confirm, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + confirmPath,
		Headers:     confirmHeaders,
		Body:        []byte("{}"),
		ContentType: "application/json",
	})
	if err != nil {
		return UploadedAsset{}, transportError("the upstream upload confirmation failed", err)
	}
	if imgErr := imageStatusError(confirm.StatusCode, parseRetryAfter(confirm.Headers)); imgErr != nil {
		return UploadedAsset{}, imgErr
	}

	return UploadedAsset{
		FileID:   uploadMeta.FileID,
		FileName: fileName,
		MimeType: mimeType,
		Size:     len(image.Data),
		Width:    image.Width,
		Height:   image.Height,
	}, nil
}

// imageStatusError maps one upstream status the way the oracle's ensure_ok
// would have surfaced it (any non-2xx fails), keeping the typed code stable
// for the executor: 401 is invalid_token, 429 rate_limited, 5xx retryable
// upstream_status, everything else non-retryable upstream_status.
func imageStatusError(status int, retryAfter *int) *ImageError {
	if status >= 200 && status < 300 {
		return nil
	}
	switch status {
	case http.StatusUnauthorized:
		return &ImageError{
			Code:       ImageCodeInvalidToken,
			HTTPStatus: status,
			Message:    "the upstream rejected the bearer token",
		}
	case http.StatusTooManyRequests:
		err := &ImageError{
			Code:       ImageCodeRateLimited,
			HTTPStatus: status,
			Retryable:  true,
			Message:    "the upstream rate-limited the request",
		}
		if retryAfter != nil {
			err.RetryAfter = *retryAfter
		}
		return err
	default:
		return &ImageError{
			Code:       ImageCodeUpstreamStatus,
			HTTPStatus: status,
			Retryable:  status >= http.StatusInternalServerError,
			Message:    fmt.Sprintf("the upstream returned HTTP %d", status),
		}
	}
}
