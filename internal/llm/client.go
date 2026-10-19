// Package llm implements a minimal OpenAI chat-completions-compatible
// HTTP client with per-endpoint retry and cross-endpoint fallback.
//
// A hand-rolled client instead of a generic SDK: DeepSeek reports prefix
// cache usage via the non-standard usage.prompt_cache_hit_tokens field,
// which drives the cache hit-rate metric, so the response struct must
// be under our control.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"tgbridge/internal/config"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Messages  []Message
	MaxTokens int
	Kind      string // logging label only: "translate" | "summarize"
}

type Usage struct {
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
}

type ChatResponse struct {
	Content    string
	Usage      Usage
	Model      string
	Endpoint   string
	DurationMS int
	Attempts   int
}

type Client struct {
	endpoints        []config.Endpoint
	httpClient       *http.Client
	maxRetries       int
	temperature      float64
	defaultMaxTokens int
}

func New(endpoints []config.Endpoint, cfg config.LLM) *Client {
	return &Client{
		endpoints:  endpoints,
		httpClient: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
		maxRetries: cfg.MaxRetries,
		temperature: cfg.Temperature,
		defaultMaxTokens: cfg.MaxTokens,
	}
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model       string       `json:"model"`
	Messages    []apiMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens"`
	Stream      bool         `json:"stream"`
}

type apiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message apiMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens          int  `json:"prompt_tokens"`
		CompletionTokens      int  `json:"completion_tokens"`
		PromptCacheHitTokens   int  `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails   *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (r *apiResponse) cachedTokens() int {
	if r.Usage.PromptCacheHitTokens > 0 {
		return r.Usage.PromptCacheHitTokens
	}
	if r.Usage.PromptTokensDetails != nil {
		return r.Usage.PromptTokensDetails.CachedTokens
	}
	return 0
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("llm endpoint: HTTP %d: %s", e.status, e.body)
}

// failFast reports errors that retrying cannot fix (bad request shape,
// bad credentials); they skip straight to the next endpoint.
func failFast(status int) bool {
	return status == 400 || status == 401 || status == 403 || status == 404
}

// Chat executes the request: each endpoint gets maxRetries+1 attempts
// with exponential backoff (1s, 4s, ...), then the next endpoint takes
// over. The prompt prefix is never mutated between retries so
// provider-side prefix caches stay warm.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.defaultMaxTokens
	}
	body := apiRequest{
		Messages:    make([]apiMessage, len(req.Messages)),
		Temperature: c.temperature,
		MaxTokens:   maxTokens,
		Stream:      false,
	}
	for i, m := range req.Messages {
		body.Messages[i] = apiMessage{Role: m.Role, Content: m.Content}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}

	start := time.Now()
	var lastErr error
	attempts := 0
	for _, ep := range c.endpoints {
		for try := 0; try <= c.maxRetries; try++ {
			if err := ctx.Err(); err != nil {
				return ChatResponse{}, err
			}
			attempts++
			reqBody := body
			reqBody.Model = ep.Model
			payload, err = json.Marshal(reqBody)
			if err != nil {
				return ChatResponse{}, err
			}
			res, err := c.call(ctx, ep, payload)
			if err == nil {
				res.Attempts = attempts
				res.DurationMS = int(time.Since(start).Milliseconds())
				return res, nil
			}
			lastErr = err
			var he *httpError
			if errors.As(err, &he) && failFast(he.status) {
				break // try next endpoint immediately
			}
			if try < c.maxRetries {
				backoff := time.Duration(1<<(2*try)) * time.Second // 1s, 4s
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ChatResponse{}, ctx.Err()
				}
			}
		}
	}
	return ChatResponse{}, fmt.Errorf("all endpoints failed after %d attempts: %w", attempts, lastErr)
}

func (c *Client) call(ctx context.Context, ep config.Endpoint, payload []byte) (ChatResponse, error) {
	url := ep.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+ep.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, &httpError{status: resp.StatusCode, body: truncate(string(raw), 300)}
	}
	var parsed apiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("empty choices from %s", ep.Name)
	}
	return ChatResponse{
		Content:  parsed.Choices[0].Message.Content,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CachedTokens:     parsed.cachedTokens(),
			CompletionTokens: parsed.Usage.CompletionTokens,
		},
		Model:    parsed.Model,
		Endpoint: ep.Name,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
