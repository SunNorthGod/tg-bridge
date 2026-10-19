// Package store provides the SQLite persistence layer.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle. All queries live in this package;
// callers never touch raw SQL.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies connection
// pragmas: WAL for concurrent read while writing, busy_timeout because
// several goroutines share the single connection pool.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite serializes writes; one conn avoids SQLITE_BUSY storms.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS chats (
  id           INTEGER PRIMARY KEY,
  peer_type    TEXT NOT NULL,
  tg_user_id   INTEGER NOT NULL,
  access_hash  INTEGER NOT NULL DEFAULT 0,
  username     TEXT,
  title        TEXT,
  persona      TEXT NOT NULL DEFAULT '',
  summary      TEXT NOT NULL DEFAULT '',
  window       TEXT NOT NULL DEFAULT '',
  enabled      INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  UNIQUE(peer_type, tg_user_id)
);

CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id       INTEGER NOT NULL REFERENCES chats(id),
  tg_msg_id     INTEGER NOT NULL DEFAULT 0,
  edit_rev      INTEGER NOT NULL DEFAULT 0,
  direction     TEXT NOT NULL,
  lang_detected TEXT,
  source_text   TEXT NOT NULL,
  target_text   TEXT,
  status        TEXT NOT NULL DEFAULT 'pending',
  tokens_in     INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  tokens_out    INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

-- Partial unique index: rows with tg_msg_id=0 are local-only (failed
-- outgoing attempts) and are intentionally unconstrained.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_real_msg
  ON messages(chat_id, tg_msg_id, edit_rev) WHERE tg_msg_id != 0;

CREATE TABLE IF NOT EXISTS glossary (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id  INTEGER NOT NULL REFERENCES chats(id),
  src_term TEXT NOT NULL,
  dst_term TEXT NOT NULL,
  note     TEXT,
  UNIQUE(chat_id, src_term)
);

CREATE TABLE IF NOT EXISTS llm_calls (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id        INTEGER NOT NULL DEFAULT 0,
  kind              TEXT NOT NULL,
  endpoint          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  duration_ms       INTEGER NOT NULL DEFAULT 0,
  error             TEXT,
  created_at        TEXT NOT NULL
);
`

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
