//go:build live

// TestLiveExecutorText is the operator-gated live smoke for the text
// executor (Task 12 Step 5, Review Focus #1 + #2 verified live): one
// non-stream and one streamed OpenAI chat turn against the deployed EasyCLI
// instance, asserting assistant content is non-empty and usage is present.
// It runs only when an operator builds with -tags live AND supplies the
// environment below; default CI never compiles it. Nothing it prints
// carries token bytes.
//
// Required environment:
//
//	CPA_HOST  the deployed EasyCLI instance (default http://localhost:8317)
//	CPA_KEY   the API key for that instance (default "123", the loopback
//	          development default)
//	LIVE_MODEL the published model id to exercise, e.g.
//	          chatgpt-proxy/openai/gpt-5 (must exist in the account's
//	          upstream catalog)
package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const liveTurnTimeout = 300 * time.Second

func liveCPAHost() string {
	if host := strings.TrimSpace(os.Getenv("CPA_HOST")); host != "" {
		return host
	}
	return "http://localhost:8317"
}

func liveCPAKey() string {
	if key := strings.TrimSpace(os.Getenv("CPA_KEY")); key != "" {
		return key
	}
	return "123"
}

func liveChatModel() string {
	return strings.TrimSpace(os.Getenv("LIVE_MODEL"))
}

// liveChatRequest builds one OpenAI chat completion request body.
func liveChatRequest(model string, stream bool) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}},
	})
	return body
}

// livePost runs one turn and returns the HTTP response for the caller to
// read (the body is NOT closed until the caller is done streaming it).
func livePost(t *testing.T, model string, stream bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, liveCPAHost()+"/v1/chat/completions", bytes.NewReader(liveChatRequest(model, stream)))
	if err != nil {
		t.Fatalf("build live request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+liveCPAKey())
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: liveTurnTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("live POST failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		t.Fatalf("live POST answered http %d: %s", resp.StatusCode, body)
	}
	return resp
}

func TestLiveExecutorText(t *testing.T) {
	model := liveChatModel()
	if model == "" {
		t.Fatal("operator-gated: set LIVE_MODEL (a published chatgpt-proxy/openai/<slug> id) to run the text executor smoke")
	}

	t.Run("non-stream", func(t *testing.T) {
		resp := livePost(t, model, false)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read live response: %v", err)
		}
		var completion struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &completion); err != nil {
			t.Fatalf("live response is not a chat.completion: %v", err)
		}
		if len(completion.Choices) != 1 {
			t.Fatalf("choices = %d, want exactly one", len(completion.Choices))
		}
		choice := completion.Choices[0]
		if choice.Message.Content == "" {
			t.Fatalf("assistant content is empty — a zero-content completion must not reach the client as a 200 (Review Focus #2)")
		}
		if choice.FinishReason != "stop" {
			t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
		}
		if completion.Model != model {
			t.Fatalf("model = %q, want the requested id echoed", completion.Model)
		}
		if completion.Usage.TotalTokens <= 0 {
			t.Fatalf("usage.total_tokens = %d, want a positive estimate", completion.Usage.TotalTokens)
		}
		t.Logf("non-stream turn completed: %d usage tokens (estimate)", completion.Usage.TotalTokens)
	})

	t.Run("stream", func(t *testing.T) {
		resp := livePost(t, model, true)
		defer resp.Body.Close()

		var (
			content      strings.Builder
			terminalSeen bool
			doneSeen     bool
			chunkCount   int
		)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			payload, isData := strings.CutPrefix(line, "data:")
			if !isData {
				continue
			}
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				doneSeen = true
				break
			}
			var chunk struct {
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
					Delta        struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue // keep-alive or non-JSON frame
			}
			chunkCount++
			for _, choice := range chunk.Choices {
				content.WriteString(choice.Delta.Content)
				if choice.FinishReason != nil {
					terminalSeen = true
				}
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("read live stream: %v", err)
		}
		if !doneSeen {
			t.Fatalf("the stream never terminated with data: [DONE]")
		}
		if !terminalSeen {
			t.Fatalf("no terminal finish_reason chunk in the live stream")
		}
		if content.Len() == 0 {
			t.Fatalf("the live stream produced no assistant content (chunks=%d)", chunkCount)
		}
		t.Logf("stream turn completed: %d chunks, terminal + [DONE] observed", chunkCount)
	})
}

// TestLiveExecutorTextRefusals is the operator-gated refusal smoke: tools
// and image models must be refused cleanly (typed 4xx), never crash the
// executor.
func TestLiveExecutorTextRefusals(t *testing.T) {
	model := liveChatModel()
	if model == "" {
		t.Fatal("operator-gated: set LIVE_MODEL (a published chatgpt-proxy/openai/<slug> id) to run the refusal smoke")
	}

	post := func(payload []byte) int {
		req, err := http.NewRequest(http.MethodPost, liveCPAHost()+"/v1/chat/completions", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("build live request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+liveCPAKey())
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("live POST failed: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	tools, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "noop"}}},
	})
	if status := post(tools); status != http.StatusBadRequest {
		t.Fatalf("tools refusal answered http %d, want 400", status)
	}

	if strings.HasPrefix(model, "chatgpt-proxy/openai/") {
		image, _ := json.Marshal(map[string]any{
			"model":    "chatgpt-proxy/gpt-image-2-proxy",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		})
		// Image models on the text surface are refused cleanly; the
		// images endpoints (Task 13) take them elsewhere.
		if status := post(image); status < 400 || status > 499 {
			t.Fatalf("image model on the text surface answered http %d, want a typed 4xx refusal", status)
		}
	}
}
