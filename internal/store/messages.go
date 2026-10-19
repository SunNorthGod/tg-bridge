package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Message struct {
	ID          int64
	ChatID      int64
	TgMsgID     int64
	EditRev     int
	Direction   string // "in" | "out"
	LangDetected string
	SourceText  string
	TargetText  string
	Status      string // pending | translating | done | failed | skipped
	TokensIn    int
	CachedTokens int
	TokensOut   int
	LatencyMS   int
	CreatedAt   string
	UpdatedAt   string
}

const msgCols = `id, chat_id, tg_msg_id, edit_rev, direction, lang_detected, source_text, target_text, status, tokens_in, cached_tokens, tokens_out, latency_ms, created_at, updated_at`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ChatID, &m.TgMsgID, &m.EditRev, &m.Direction, &m.LangDetected,
		&m.SourceText, &m.TargetText, &m.Status, &m.TokensIn, &m.CachedTokens, &m.TokensOut,
		&m.LatencyMS, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &m, nil
}

func (s *Store) InsertMessage(m *Message) (int64, error) {
	const q = `
INSERT INTO messages (chat_id, tg_msg_id, edit_rev, direction, lang_detected, source_text, target_text, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	m.CreatedAt, m.UpdatedAt = now(), now()
	res, err := s.db.Exec(q, m.ChatID, m.TgMsgID, m.EditRev, m.Direction, m.LangDetected,
		m.SourceText, m.TargetText, m.Status, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) UpdateMessageResult(id int64, status, target string, tokensIn, cached, tokensOut, latencyMS int) error {
	_, err := s.db.Exec(`UPDATE messages SET status=?, target_text=?, tokens_in=?, cached_tokens=?, tokens_out=?, latency_ms=?, updated_at=? WHERE id=?`,
		status, target, tokensIn, cached, tokensOut, latencyMS, now(), id)
	return err
}

func (s *Store) SetMessageStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE messages SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	return err
}

// GetByTgMsg returns the newest revision of a Telegram message, if seen.
func (s *Store) GetByTgMsg(chatID, tgMsgID int64) (*Message, error) {
	return scanMessage(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND tg_msg_id=? ORDER BY edit_rev DESC LIMIT 1`, chatID, tgMsgID))
}

// GetMessageByID loads one row by primary key.
func (s *Store) GetMessageByID(id int64) (*Message, error) {
	return scanMessage(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id))
}

// SetMessageTgMsgID backfills the Telegram message id on an outgoing
// row inserted before sending (placeholder id 0).
func (s *Store) SetMessageTgMsgID(id, tgMsgID int64) error {
	_, err := s.db.Exec(`UPDATE messages SET tg_msg_id=?, updated_at=? WHERE id=?`, tgMsgID, now(), id)
	return err
}

// GetMessagesByIDs returns messages for the window id list, preserving
// the order of ids.
func (s *Store) GetMessagesByIDs(chatID int64, ids []int64) ([]*Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, chatID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("window query: %w", err)
	}
	defer rows.Close()
	byID := make(map[int64]*Message, len(ids))
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		byID[m.ID] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// ListMessages returns up to limit messages strictly older than beforeID
// (0 = latest), oldest first.
func (s *Store) ListMessages(chatID, beforeID int64, limit int) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`,
		chatID, beforeID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CountMessages reports the total rows for a chat (UI paging hint).
func (s *Store) CountMessages(chatID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id=?`, chatID).Scan(&n)
	return n, err
}

// ListPendingIncoming returns inbound rows stuck in pending — a crash
// recovery source at startup.
func (s *Store) ListPendingIncoming() ([]*Message, error) {
	rows, err := s.db.Query(`SELECT ` + msgCols + ` FROM messages WHERE status='pending' AND direction='in' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
