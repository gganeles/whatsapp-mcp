package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MessageStore keeps a local copy of chats and messages so they can be listed
// and searched without hitting WhatsApp's servers.
type MessageStore struct {
	db *sql.DB
}

type Chat struct {
	JID             string    `json:"jid"`
	Name            string    `json:"name"`
	IsGroup         bool      `json:"is_group"`
	LastMessageTime time.Time `json:"last_message_time"`
	LastMessage     string    `json:"last_message,omitempty"`
	LastSender      string    `json:"last_sender,omitempty"`
	MessageCount    int       `json:"message_count"`
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

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid               TEXT PRIMARY KEY,
	name              TEXT NOT NULL DEFAULT '',
	is_group          INTEGER NOT NULL DEFAULT 0,
	last_message_time INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
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
	PRIMARY KEY (chat_jid, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_time ON messages (chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS messages_time ON messages (timestamp);
`

func OpenMessageStore(path string) (*MessageStore, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	return &MessageStore{db: db}, nil
}

func (s *MessageStore) Close() error {
	return s.db.Close()
}

// UpsertChat records a chat. An empty name never overwrites a known one, and
// the last message time only moves forward.
func (s *MessageStore) UpsertChat(ctx context.Context, jid, name string, isGroup bool, lastMessage time.Time) error {
	var ts int64
	if !lastMessage.IsZero() {
		ts = lastMessage.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (jid, name, is_group, last_message_time) VALUES (?, ?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			is_group = excluded.is_group,
			last_message_time = MAX(chats.last_message_time, excluded.last_message_time)
	`, jid, name, isGroup, ts)
	return err
}

func (s *MessageStore) SaveMessage(ctx context.Context, m *Message) error {
	_, err := s.db.ExecContext(ctx, `
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
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET text = ?, edited = 1 WHERE chat_jid = ? AND id = ?`, newText, chatJID, id)
	return err
}

func (s *MessageStore) DeleteMessage(ctx context.Context, chatJID, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted = 1 WHERE chat_jid = ? AND id = ?`, chatJID, id)
	return err
}

func (s *MessageStore) GetChat(ctx context.Context, jid string) (*Chat, error) {
	chats, err := s.queryChats(ctx, "WHERE c.jid = ?", "", 1, 0, jid)
	if err != nil || len(chats) == 0 {
		return nil, err
	}
	return &chats[0], nil
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.jid, c.name, c.is_group, c.last_message_time,
		       COALESCE(lm.text, ''), COALESCE(lm.type, ''), COALESCE(lm.sender_name, ''), COALESCE(lm.from_me, 0),
		       (SELECT COUNT(*) FROM messages WHERE chat_jid = c.jid)
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
		if err = rows.Scan(&c.JID, &c.Name, &c.IsGroup, &ts, &c.LastMessage, &lastType, &lastSender, &lastFromMe, &c.MessageCount); err != nil {
			return nil, err
		}
		if ts > 0 {
			c.LastMessageTime = time.Unix(ts, 0)
		}
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

// Messages returns messages matching the filter, newest first.
func (s *MessageStore) Messages(ctx context.Context, f MessageFilter) ([]Message, error) {
	var where []string
	var args []any
	if f.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, f.ChatJID)
	}
	if f.SenderJID != "" {
		where = append(where, "m.sender_jid = ?")
		args = append(args, f.SenderJID)
	}
	if f.Query != "" {
		// Every word has to appear somewhere in the text, file name or sender name.
		for _, word := range strings.Fields(f.Query) {
			where = append(where, "(m.text LIKE ? ESCAPE '\\' OR m.file_name LIKE ? ESCAPE '\\')")
			q := likePattern(word)
			args = append(args, q, q)
		}
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
	err = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages)`).Scan(&chats, &messages)
	return
}

func (s *MessageStore) queryMessages(ctx context.Context, tail string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
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
