package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// MessageStore keeps a local copy of chats and messages so they can be listed
// and searched without hitting WhatsApp's servers.
type MessageStore struct {
	db *sql.DB
	q  querier // db, or a transaction inside WithTx
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Chat struct {
	JID             string    `json:"jid"`
	Name            string    `json:"name"`
	IsGroup         bool      `json:"is_group"`
	LastMessageTime time.Time `json:"last_message_time"`
	LastMessage     string    `json:"last_message,omitempty"`
	LastSender      string    `json:"last_sender,omitempty"`
}

type Message struct {
	ChatJID    string    `json:"chat_jid"`
	ChatName   string    `json:"chat_name,omitempty"`
	ID         string    `json:"id"`
	SenderJID  string    `json:"sender_jid"`
	SenderName string    `json:"sender_name,omitempty"`
	FromMe     bool      `json:"from_me"`
	Timestamp  time.Time `json:"timestamp"`
	Type       string    `json:"type"`
	Text       string    `json:"text,omitempty"`
	MediaType  string    `json:"media_type,omitempty"`
	MimeType   string    `json:"mime_type,omitempty"`
	FileName   string    `json:"file_name,omitempty"`
	QuotedID   string    `json:"quoted_id,omitempty"`
	Edited     bool      `json:"edited,omitempty"`
	Deleted    bool      `json:"deleted,omitempty"`

	// Raw is the serialized waE2E.Message for media messages, used to download the media later.
	Raw []byte `json:"-"`
}

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid               TEXT PRIMARY KEY,
	name              TEXT NOT NULL DEFAULT '',
	is_group          INTEGER NOT NULL DEFAULT 0,
	last_message_time INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS chats_last_message ON chats (last_message_time DESC);

CREATE TABLE IF NOT EXISTS messages (
	rowid       INTEGER PRIMARY KEY,
	chat_jid    TEXT NOT NULL,
	id          TEXT NOT NULL,
	sender_jid  TEXT NOT NULL DEFAULT '',
	sender_name TEXT NOT NULL DEFAULT '',
	from_me     INTEGER NOT NULL DEFAULT 0,
	timestamp   INTEGER NOT NULL,
	type        TEXT NOT NULL DEFAULT 'text',
	text        TEXT NOT NULL DEFAULT '',
	media_type  TEXT NOT NULL DEFAULT '',
	mime_type   TEXT NOT NULL DEFAULT '',
	file_name   TEXT NOT NULL DEFAULT '',
	quoted_id   TEXT NOT NULL DEFAULT '',
	edited      INTEGER NOT NULL DEFAULT 0,
	deleted     INTEGER NOT NULL DEFAULT 0,
	raw         BLOB,
	UNIQUE (chat_jid, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_time ON messages (chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS messages_time ON messages (timestamp);
CREATE INDEX IF NOT EXISTS messages_id ON messages (id);

-- Full-text index over message text, attachment names and sender names.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5 (
	text, file_name, sender_name,
	content = 'messages', content_rowid = 'rowid',
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts (rowid, text, file_name, sender_name) VALUES (new.rowid, new.text, new.file_name, new.sender_name);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts (messages_fts, rowid, text, file_name, sender_name) VALUES ('delete', old.rowid, old.text, old.file_name, old.sender_name);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF text, file_name, sender_name ON messages BEGIN
	INSERT INTO messages_fts (messages_fts, rowid, text, file_name, sender_name) VALUES ('delete', old.rowid, old.text, old.file_name, old.sender_name);
	INSERT INTO messages_fts (rowid, text, file_name, sender_name) VALUES (new.rowid, new.text, new.file_name, new.sender_name);
END;
`

// sqliteDSN builds a modernc.org/sqlite connection string with the pragmas every connection needs.
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)&_txlock=immediate"
}

func OpenMessageStore(path string) (*MessageStore, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	if _, err = db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		_ = db.Close()
		return nil, err
	}
	restrictPermissions(path)
	return &MessageStore{db: db, q: db}, nil
}

// restrictPermissions makes a SQLite database (and its WAL files) readable only by its owner.
func restrictPermissions(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
}

func (s *MessageStore) Close() error {
	return s.db.Close()
}

// WithTx runs fn with a store whose writes all happen in one transaction, which is far
// faster than committing every message separately during history sync.
func (s *MessageStore) WithTx(ctx context.Context, fn func(tx *MessageStore) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(&MessageStore{db: s.db, q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpsertChat records a chat. An empty name never overwrites a known one, and
// the last message time only moves forward.
func (s *MessageStore) UpsertChat(ctx context.Context, jid, name string, isGroup bool, lastMessage time.Time) error {
	var ts int64
	if !lastMessage.IsZero() {
		ts = lastMessage.Unix()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO chats (jid, name, is_group, last_message_time) VALUES (?, ?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			is_group = excluded.is_group,
			last_message_time = MAX(chats.last_message_time, excluded.last_message_time)
	`, jid, name, isGroup, ts)
	return err
}

func (s *MessageStore) ChatName(ctx context.Context, jid string) (string, error) {
	var name string
	err := s.q.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

func (s *MessageStore) SaveMessage(ctx context.Context, m *Message) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO messages (chat_jid, id, sender_jid, sender_name, from_me, timestamp, type, text,
		                      media_type, mime_type, file_name, quoted_id, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, id) DO UPDATE SET
			sender_name = CASE WHEN excluded.sender_name <> '' THEN excluded.sender_name ELSE messages.sender_name END,
			text = CASE WHEN messages.edited THEN messages.text ELSE excluded.text END,
			media_type = excluded.media_type,
			mime_type = excluded.mime_type,
			file_name = excluded.file_name,
			raw = COALESCE(excluded.raw, messages.raw)
	`, m.ChatJID, m.ID, m.SenderJID, m.SenderName, m.FromMe, m.Timestamp.Unix(), m.Type, m.Text,
		m.MediaType, m.MimeType, m.FileName, m.QuotedID, m.Raw)
	return err
}

func (s *MessageStore) EditMessage(ctx context.Context, chatJID, id, newText string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE messages SET text = ?, edited = 1 WHERE chat_jid = ? AND id = ?`, newText, chatJID, id)
	return err
}

func (s *MessageStore) DeleteMessage(ctx context.Context, chatJID, id string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE messages SET deleted = 1 WHERE chat_jid = ? AND id = ?`, chatJID, id)
	return err
}

func (s *MessageStore) GetChat(ctx context.Context, jid string) (*Chat, error) {
	chats, err := s.queryChats(ctx, "WHERE c.jid = ?", "", 1, 0, jid)
	if err != nil || len(chats) == 0 {
		return nil, err
	}
	return &chats[0], nil
}

func (s *MessageStore) CountMessages(ctx context.Context, chatJID string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, chatJID).Scan(&n)
	return n, err
}

// ListChats returns chats ordered by most recent activity. query filters by
// chat name or JID (case-insensitive substring).
func (s *MessageStore) ListChats(ctx context.Context, query string, groupsOnly, directOnly bool, limit, offset int) ([]Chat, error) {
	var where []string
	var args []any
	if query != "" {
		where = append(where, "(c.name LIKE ? ESCAPE '\\' OR c.jid LIKE ? ESCAPE '\\')")
		q := likePattern(query)
		args = append(args, q, q)
	}
	if groupsOnly {
		where = append(where, "c.is_group = 1")
	}
	if directOnly {
		where = append(where, "c.is_group = 0")
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	return s.queryChats(ctx, clause, "ORDER BY c.last_message_time DESC", limit, offset, args...)
}

func (s *MessageStore) queryChats(ctx context.Context, where, order string, limit, offset int, args ...any) ([]Chat, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT c.jid, c.name, c.is_group, c.last_message_time,
		       COALESCE(lm.text, ''), COALESCE(lm.type, ''), COALESCE(lm.sender_name, ''), COALESCE(lm.from_me, 0)
		FROM chats c
		LEFT JOIN messages lm ON lm.rowid = (
			SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY timestamp DESC LIMIT 1
		)
		`+where+` `+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chats []Chat
	for rows.Next() {
		var c Chat
		var ts int64
		var lastType, lastSender string
		var lastFromMe bool
		if err = rows.Scan(&c.JID, &c.Name, &c.IsGroup, &ts, &c.LastMessage, &lastType, &lastSender, &lastFromMe); err != nil {
			return nil, err
		}
		if ts > 0 {
			c.LastMessageTime = time.Unix(ts, 0)
		}
		c.LastMessage = truncate(c.LastMessage, 200)
		if c.LastMessage == "" && lastType != "" && lastType != "text" {
			c.LastMessage = "[" + lastType + "]"
		}
		if lastFromMe {
			c.LastSender = "me"
		} else {
			c.LastSender = lastSender
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

type MessageFilter struct {
	ChatJID   string
	SenderJID string
	Query     string
	Before    time.Time
	After     time.Time
	Limit     int
	Offset    int
}

// Messages returns messages matching the filter, newest first. Query uses the
// full-text index (every word must match, as a word prefix) and falls back to a
// substring scan when the index finds nothing.
func (s *MessageStore) Messages(ctx context.Context, f MessageFilter) ([]Message, error) {
	if f.Query == "" {
		return s.filterMessages(ctx, f, "", nil)
	}
	if match := ftsQuery(f.Query); match != "" {
		msgs, err := s.filterMessages(ctx, f, "m.rowid IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)", []any{match})
		if err != nil || len(msgs) > 0 || f.Offset > 0 {
			return msgs, err
		}
	}
	var conds []string
	var args []any
	for _, word := range strings.Fields(f.Query) {
		conds = append(conds, "(m.text LIKE ? ESCAPE '\\' OR m.file_name LIKE ? ESCAPE '\\')")
		q := likePattern(word)
		args = append(args, q, q)
	}
	return s.filterMessages(ctx, f, strings.Join(conds, " AND "), args)
}

func (s *MessageStore) filterMessages(ctx context.Context, f MessageFilter, extra string, extraArgs []any) ([]Message, error) {
	var where []string
	var args []any
	if extra != "" {
		where = append(where, extra)
		args = append(args, extraArgs...)
	}
	if f.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, f.ChatJID)
	}
	if f.SenderJID != "" {
		where = append(where, "m.sender_jid = ?")
		args = append(args, f.SenderJID)
	}
	if !f.Before.IsZero() {
		where = append(where, "m.timestamp < ?")
		args = append(args, f.Before.Unix())
	}
	if !f.After.IsZero() {
		where = append(where, "m.timestamp > ?")
		args = append(args, f.After.Unix())
	}
	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	return s.queryMessages(ctx, clause+" ORDER BY m.timestamp DESC, m.rowid DESC LIMIT ? OFFSET ?", append(args, f.Limit, f.Offset)...)
}

// ftsQuery turns free text into an FTS5 query where every word must appear as a prefix.
func ftsQuery(q string) string {
	var terms []string
	for _, word := range strings.Fields(q) {
		word = strings.ReplaceAll(word, `"`, `""`)
		terms = append(terms, `"`+word+`"*`)
	}
	return strings.Join(terms, " ")
}

func (s *MessageStore) GetMessage(ctx context.Context, chatJID, id string) (*Message, error) {
	msgs, err := s.queryMessages(ctx, "WHERE m.chat_jid = ? AND m.id = ?", chatJID, id)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &msgs[0], nil
}

// FindMessage looks up a message by ID when the chat isn't known.
func (s *MessageStore) FindMessage(ctx context.Context, id string) (*Message, error) {
	msgs, err := s.queryMessages(ctx, "WHERE m.id = ? LIMIT 1", id)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &msgs[0], nil
}

// Context returns up to n messages before and after the given message, in chronological order.
func (s *MessageStore) Context(ctx context.Context, target *Message, before, after int) ([]Message, error) {
	older, err := s.queryMessages(ctx, `WHERE m.chat_jid = ? AND m.timestamp <= ? AND m.id <> ?
		ORDER BY m.timestamp DESC LIMIT ?`, target.ChatJID, target.Timestamp.Unix(), target.ID, before)
	if err != nil {
		return nil, err
	}
	newer, err := s.queryMessages(ctx, `WHERE m.chat_jid = ? AND m.timestamp >= ? AND m.id <> ?
		ORDER BY m.timestamp ASC LIMIT ?`, target.ChatJID, target.Timestamp.Unix(), target.ID, after)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(older)+len(newer)+1)
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	out = append(out, *target)
	return append(out, newer...), nil
}

// OldestMessage returns the oldest stored message in a chat, used as the anchor for history requests.
func (s *MessageStore) OldestMessage(ctx context.Context, chatJID string) (*Message, error) {
	msgs, err := s.queryMessages(ctx, "WHERE m.chat_jid = ? ORDER BY m.timestamp ASC LIMIT 1", chatJID)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &msgs[0], nil
}

func (s *MessageStore) Stats(ctx context.Context) (chats, messages int, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages)`).Scan(&chats, &messages)
	return
}

func (s *MessageStore) queryMessages(ctx context.Context, tail string, args ...any) ([]Message, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT m.chat_jid, COALESCE(c.name, ''), m.id, m.sender_jid, m.sender_name, m.from_me, m.timestamp,
		       m.type, m.text, m.media_type, m.mime_type, m.file_name, m.quoted_id, m.edited, m.deleted, m.raw
		FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var ts int64
		if err = rows.Scan(&m.ChatJID, &m.ChatName, &m.ID, &m.SenderJID, &m.SenderName, &m.FromMe, &ts,
			&m.Type, &m.Text, &m.MediaType, &m.MimeType, &m.FileName, &m.QuotedID, &m.Edited, &m.Deleted, &m.Raw); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
