package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("store: not found")

type Chat struct {
	ID         int64
	PeerType   string
	TgUserID   int64
	AccessHash int64
	Username   string
	Title      string
	Persona    string
	Summary    string
	Window     string // JSON array of message ids currently in the L3 window
	Enabled    bool
	CreatedAt  string
	UpdatedAt  string
}

type ChatListItem struct {
	Chat
	LastSnippet string
	LastTime    string
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// UpsertChatFromTG creates or refreshes a discovered peer. A non-zero
// access_hash always wins (it may rotate server-side); username/title
// only overwrite when non-empty so a cached display name survives.
func (s *Store) UpsertChatFromTG(peerType string, tgUserID, accessHash int64, username, title string) (int64, error) {
	const q = `
INSERT INTO chats (peer_type, tg_user_id, access_hash, username, title, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(peer_type, tg_user_id) DO UPDATE SET
  access_hash = CASE WHEN excluded.access_hash != 0 THEN excluded.access_hash ELSE chats.access_hash END,
  username    = COALESCE(NULLIF(excluded.username, ''), chats.username),
  title       = COALESCE(NULLIF(excluded.title, ''), chats.title),
  updated_at  = excluded.updated_at
RETURNING id`
	var id int64
	err := s.db.QueryRow(q, peerType, tgUserID, accessHash, username, title, now(), now()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert chat: %w", err)
	}
	return id, nil
}

func scanChat(row interface{ Scan(...any) error }) (*Chat, error) {
	var c Chat
	var enabled int
	err := row.Scan(&c.ID, &c.PeerType, &c.TgUserID, &c.AccessHash, &c.Username, &c.Title,
		&c.Persona, &c.Summary, &c.Window, &enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Enabled = enabled != 0
	return &c, nil
}

const chatCols = `id, peer_type, tg_user_id, access_hash, username, title, persona, summary, window, enabled, created_at, updated_at`

func (s *Store) GetChat(id int64) (*Chat, error) {
	return scanChat(s.db.QueryRow(`SELECT `+chatCols+` FROM chats WHERE id = ?`, id))
}

func (s *Store) GetChatByTgUser(tgUserID int64) (*Chat, error) {
	return scanChat(s.db.QueryRow(`SELECT `+chatCols+` FROM chats WHERE peer_type='user' AND tg_user_id = ?`, tgUserID))
}

// ListChats returns every discovered chat with its latest message for
// the sidebar. Enabled chats sort first, then by recent activity.
func (s *Store) ListChats() ([]ChatListItem, error) {
	const q = `
SELECT c.id, c.peer_type, c.tg_user_id, c.access_hash, c.username, c.title,
       c.persona, c.summary, c.window, c.enabled, c.created_at, c.updated_at,
       COALESCE(m.snippet, ''), COALESCE(m.t, '')
FROM chats c
LEFT JOIN (
  SELECT chat_id,
         substr(CASE WHEN direction='out' THEN source_text
                     ELSE COALESCE(NULLIF(target_text,''), source_text) END, 1, 60) AS snippet,
         created_at AS t,
         ROW_NUMBER() OVER (PARTITION BY chat_id ORDER BY id DESC) AS rn
  FROM messages
) m ON m.chat_id = c.id AND m.rn = 1
ORDER BY c.enabled DESC, m.t DESC, c.updated_at DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()
	var out []ChatListItem
	for rows.Next() {
		var li ChatListItem
		var enabled int
		if err := rows.Scan(&li.ID, &li.PeerType, &li.TgUserID, &li.AccessHash, &li.Username, &li.Title,
			&li.Persona, &li.Summary, &li.Window, &enabled, &li.CreatedAt, &li.UpdatedAt,
			&li.LastSnippet, &li.LastTime); err != nil {
			return nil, err
		}
		li.Enabled = enabled != 0
		out = append(out, li)
	}
	return out, rows.Err()
}

func (s *Store) SetChatEnabled(id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE chats SET enabled=?, updated_at=? WHERE id=?`, v, now(), id)
	return err
}

func (s *Store) SetChatPersona(id int64, persona string) error {
	_, err := s.db.Exec(`UPDATE chats SET persona=?, updated_at=? WHERE id=?`, persona, now(), id)
	return err
}

// UpdateChatContext persists the compacted summary and window atomically;
// the per-chat worker is the only writer, no extra locking needed.
func (s *Store) UpdateChatContext(id int64, summary, window string) error {
	_, err := s.db.Exec(`UPDATE chats SET summary=?, window=?, updated_at=? WHERE id=?`,
		summary, window, now(), id)
	return err
}

func (s *Store) ListGlossary(chatID int64) ([][3]string, error) {
	rows, err := s.db.Query(`SELECT src_term, dst_term, COALESCE(note,'') FROM glossary WHERE chat_id=? ORDER BY src_term`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var e [3]string
		if err := rows.Scan(&e[0], &e[1], &e[2]); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
