package store

// Statistics queries for the /stats dashboard. Latency percentiles are
// computed in Go from the full sorted column — call volume here is
// private-chat scale, not a firehose.

type KindStats struct {
	Kind             string
	Calls            int64
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	TotalMS          int64
}

type ChatStats struct {
	ChatID   int64
	Title    string
	Username string
	Enabled  bool
	Msgs     int64
	InMsgs   int64
	OutMsgs  int64
	Done     int64
	Tokens   int64
}

func (s *Store) LLMStatsByKind() ([]KindStats, error) {
	rows, err := s.db.Query(`
SELECT kind, COUNT(*), SUM(prompt_tokens), SUM(cached_tokens), SUM(completion_tokens), SUM(duration_ms)
FROM llm_calls WHERE error = '' GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindStats
	for rows.Next() {
		var k KindStats
		if err := rows.Scan(&k.Kind, &k.Calls, &k.PromptTokens, &k.CachedTokens, &k.CompletionTokens, &k.TotalMS); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TranslateLatencies returns sorted successful translate-call durations.
func (s *Store) TranslateLatencies() ([]int, error) {
	rows, err := s.db.Query(`SELECT duration_ms FROM llm_calls WHERE kind='translate' AND error='' AND duration_ms > 0 ORDER BY duration_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var ms int
		if err := rows.Scan(&ms); err != nil {
			return nil, err
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

func (s *Store) ChatStatsList() ([]ChatStats, error) {
	rows, err := s.db.Query(`
SELECT c.id, COALESCE(NULLIF(c.title,''), COALESCE(NULLIF(c.username,''), CAST(c.tg_user_id AS TEXT))),
       COALESCE(c.username, ''), c.enabled,
       COUNT(m.id),
       COALESCE(SUM(CASE WHEN m.direction='in' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN m.direction='out' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN m.status='done' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(m.tokens_in + m.tokens_out), 0)
FROM chats c LEFT JOIN messages m ON m.chat_id = c.id
GROUP BY c.id ORDER BY COUNT(m.id) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatStats
	for rows.Next() {
		var cs ChatStats
		var enabled int
		if err := rows.Scan(&cs.ChatID, &cs.Title, &cs.Username, &enabled, &cs.Msgs, &cs.InMsgs, &cs.OutMsgs, &cs.Done, &cs.Tokens); err != nil {
			return nil, err
		}
		cs.Enabled = enabled != 0
		out = append(out, cs)
	}
	return out, rows.Err()
}

func (s *Store) MessageStatusCounts() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM messages GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
