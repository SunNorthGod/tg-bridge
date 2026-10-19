package store

import "fmt"

type LLMCall struct {
	MessageID        int64
	Kind             string // "translate" | "summarize"
	Endpoint         string
	Model            string
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	DurationMS       int
	Error            string
}

// InsertLLMCall records one completed call attempt for cost/latency
// observability; cached_tokens drives the prefix-cache hit-rate metric.
func (s *Store) InsertLLMCall(c LLMCall) error {
	_, err := s.db.Exec(`
INSERT INTO llm_calls (message_id, kind, endpoint, model, prompt_tokens, cached_tokens, completion_tokens, duration_ms, error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.MessageID, c.Kind, c.Endpoint, c.Model, c.PromptTokens, c.CachedTokens,
		c.CompletionTokens, c.DurationMS, c.Error, now())
	if err != nil {
		return fmt.Errorf("insert llm call: %w", err)
	}
	return nil
}

// CacheStats returns aggregate usage for the cost dashboard: total
// prompt tokens, cached prompt tokens, completion tokens, call count.
func (s *Store) CacheStats() (prompt, cached, completion int, calls int, err error) {
	err = s.db.QueryRow(`
SELECT COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(completion_tokens),0), COUNT(*)
FROM llm_calls WHERE error = ''`).Scan(&prompt, &cached, &completion, &calls)
	return prompt, cached, completion, calls, err
}
