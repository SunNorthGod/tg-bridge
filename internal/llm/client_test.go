package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tgbridge/internal/config"
)

// deepSeekServer returns a server speaking DeepSeek-style usage JSON,
// failing the first failFirst requests with HTTP 500.
func deepSeekServer(t *testing.T, hits *int32, failFirst int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		if n := atomic.AddInt32(hits, 1); n <= failFirst {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("backend boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "deepseek-chat",
			"choices": []any{
				map[string]any{"message": map[string]any{"role": "assistant", "content": "Привіт"}},
			},
			"usage": map[string]any{
				"prompt_tokens":           200,
				"completion_tokens":       50,
				"prompt_cache_hit_tokens": 120,
				"prompt_cache_miss_tokens": 80,
			},
		})
	}))
}

func baseCfg(retries int) config.LLM {
	return config.LLM{TimeoutSeconds: 5, MaxRetries: retries, Temperature: 1.0, MaxTokens: 128}
}

func TestParseDeepSeekCacheUsage(t *testing.T) {
	hits := int32(0)
	srv := deepSeekServer(t, &hits, 0)
	defer srv.Close()

	c := New([]config.Endpoint{{Name: "ep", BaseURL: srv.URL, APIKey: "k", Model: "deepseek-chat"}}, baseCfg(0))
	res, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, Kind: "translate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Привіт" {
		t.Errorf("content: %q", res.Content)
	}
	if res.Usage.PromptTokens != 200 || res.Usage.CompletionTokens != 50 {
		t.Errorf("usage: %+v", res.Usage)
	}
	if res.Usage.CachedTokens != 120 {
		t.Errorf("DeepSeek prompt_cache_hit_tokens not parsed: %+v", res.Usage)
	}
}

func TestRetryWithinEndpoint(t *testing.T) {
	hits := int32(0)
	srv := deepSeekServer(t, &hits, 1) // first request fails
	defer srv.Close()

	c := New([]config.Endpoint{{Name: "ep", BaseURL: srv.URL, APIKey: "k", Model: "deepseek-chat"}}, baseCfg(1))
	res, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, Kind: "translate",
	})
	if err != nil {
		t.Fatalf("expected success after retry: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("hits = %d, want 2", hits)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", res.Attempts)
	}
}

func TestFallbackEndpoint(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(0)
		w.WriteHeader(500)
		_, _ = w.Write([]byte("always down"))
	}))
	defer broken.Close()
	hits := int32(0)
	ok := deepSeekServer(t, &hits, 0)
	defer ok.Close()

	c := New([]config.Endpoint{
		{Name: "primary", BaseURL: broken.URL, APIKey: "k", Model: "m1"},
		{Name: "fallback", BaseURL: ok.URL, APIKey: "k", Model: "deepseek-chat"},
	}, baseCfg(0))

	res, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, Kind: "translate",
	})
	if err != nil {
		t.Fatalf("fallback endpoint must take over: %v", err)
	}
	if res.Endpoint != "fallback" {
		t.Errorf("served by %q, want fallback", res.Endpoint)
	}
}

func TestAllEndpointsFailed(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("down"))
	}))
	defer broken.Close()

	c := New([]config.Endpoint{{Name: "primary", BaseURL: broken.URL, APIKey: "k", Model: "m"}}, baseCfg(0))
	if _, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, Kind: "translate",
	}); err == nil {
		t.Fatal("expected error when every endpoint fails")
	}
}
