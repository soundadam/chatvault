package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Chat is one conversation with the user's settings applied.
type Chat struct {
	SourceID    int64
	Chat        string
	Kind        string
	ContactName string // from the platform's contacts
	Alias       string // the user's own name for it
	Folder      string
	Hidden      bool
	Count       int
	First, Last int64
}

// Name is the alias, else the contact name, else the username.
func (c *Chat) Name() string {
	switch {
	case c.Alias != "":
		return c.Alias
	case c.ContactName != "":
		return c.ContactName
	}
	return c.Chat
}

// Chats lists every chat, newest activity first.
func (v *Vault) Chats(ctx context.Context) ([]Chat, error) {
	rows, err := v.DB.QueryContext(ctx, `
		SELECT c.source_id, c.chat, c.kind, coalesce(ct.name, ''), coalesce(p.alias, ''),
		       coalesce(p.folder, ''), coalesce(p.hidden, 0), c.message_count,
		       coalesce(c.first_time, 0), coalesce(c.last_time, 0)
		FROM chats c
		LEFT JOIN contacts ct ON ct.source_id = c.source_id AND ct.username = c.chat
		LEFT JOIN chat_prefs p ON p.source_id = c.source_id AND p.chat = c.chat
		ORDER BY c.last_time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.SourceID, &c.Chat, &c.Kind, &c.ContactName, &c.Alias, &c.Folder,
			&c.Hidden, &c.Count, &c.First, &c.Last); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Pref changes the user's settings for one chat; nil fields stay as they are.
type Pref struct {
	Alias  *string
	Folder *string
	Hidden *bool
}

// SetPref stores p for a chat.
func (v *Vault) SetPref(ctx context.Context, sourceID int64, chat string, p Pref) error {
	tx, err := v.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_prefs (source_id, chat, updated_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, sourceID, chat, now); err != nil {
		return err
	}
	set := func(col string, val any) error {
		_, err := tx.ExecContext(ctx, "UPDATE chat_prefs SET "+col+" = ?, updated_at = ? WHERE source_id = ? AND chat = ?",
			val, now, sourceID, chat)
		return err
	}
	if p.Alias != nil {
		if err := set("alias", nullStr(*p.Alias)); err != nil {
			return err
		}
	}
	if p.Folder != nil {
		if err := set("folder", nullStr(*p.Folder)); err != nil {
			return err
		}
	}
	if p.Hidden != nil {
		if err := set("hidden", *p.Hidden); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Names maps the source's usernames to contact names.
func (v *Vault) Names(ctx context.Context, sourceID int64) (map[string]string, error) {
	rows, err := v.DB.QueryContext(ctx, "SELECT username, name FROM contacts WHERE source_id = ? AND name IS NOT NULL", sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var u, n string
		if err := rows.Scan(&u, &n); err != nil {
			return nil, err
		}
		out[u] = n
	}
	return out, rows.Err()
}

// Entry is one message as exports and the UI show it.
type Entry struct {
	ID       int64           `json:"id"`
	Time     int64           `json:"ts"`
	Sender   string          `json:"sender,omitempty"`
	Kind     string          `json:"kind"`
	Text     string          `json:"text"`
	Data     json.RawMessage `json:"data,omitempty"`
	Recalled bool            `json:"recalled,omitempty"`
}

// Entries calls fn for each message of a chat with from <= time < to, in
// order. Messages not parsed yet come with kind "pending".
func (v *Vault) Entries(ctx context.Context, sourceID int64, chat string, from, to int64, fn func(*Entry) error) error {
	rows, err := v.DB.QueryContext(ctx, `
		SELECT m.id, m.time, coalesce(m.sender, p.sender, ''), coalesce(p.kind, 'pending'),
		       coalesce(p.text, ''), p.data,
		       EXISTS (SELECT 1 FROM revisions r WHERE r.message_id = m.id AND r.type = 10000)
		FROM messages m LEFT JOIN parsed p ON p.message_id = m.id
		WHERE m.source_id = ? AND m.chat = ? AND m.time >= ? AND m.time < ?
		ORDER BY m.time, m.sort_seq, m.id`, sourceID, chat, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	var e Entry
	for rows.Next() {
		var data sql.NullString
		if err := rows.Scan(&e.ID, &e.Time, &e.Sender, &e.Kind, &e.Text, &data, &e.Recalled); err != nil {
			return err
		}
		e.Data = nil
		if data.Valid {
			e.Data = json.RawMessage(data.String)
		}
		if err := fn(&e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Month is the first instant of the local calendar month holding t.
func Month(t int64) time.Time {
	tm := time.Unix(t, 0)
	return time.Date(tm.Year(), tm.Month(), 1, 0, 0, 0, 0, time.Local)
}

// MonthCount is how many messages a chat has in one month.
type MonthCount struct {
	Month time.Time
	Count int
}

// Months lists the months of a chat that have messages, oldest first.
func (v *Vault) Months(ctx context.Context, sourceID int64, chat string) ([]MonthCount, error) {
	rows, err := v.DB.QueryContext(ctx, "SELECT time FROM messages WHERE source_id = ? AND chat = ? ORDER BY time", sourceID, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonthCount
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		m := Month(t)
		if n := len(out); n > 0 && out[n-1].Month.Equal(m) {
			out[n-1].Count++
		} else {
			out = append(out, MonthCount{Month: m, Count: 1})
		}
	}
	return out, rows.Err()
}

// Source returns one source by ID.
func (v *Vault) Source(ctx context.Context, id int64) (Source, error) {
	s := Source{ID: id}
	err := v.DB.QueryRowContext(ctx, "SELECT platform, account, coalesce(self, '') FROM sources WHERE id = ?", id).
		Scan(&s.Platform, &s.Account, &s.Self)
	return s, err
}

// Stats summarises the vault.
type Stats struct {
	Sources, Chats, Messages, Revisions, Contacts, Runs int
	LastRun                                             int64
}

func (v *Vault) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := v.DB.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM sources), (SELECT count(*) FROM chats), (SELECT count(*) FROM messages),
		(SELECT count(*) FROM revisions), (SELECT count(*) FROM contacts), (SELECT count(*) FROM runs),
		(SELECT coalesce(max(finished_at), 0) FROM runs)`).
		Scan(&s.Sources, &s.Chats, &s.Messages, &s.Revisions, &s.Contacts, &s.Runs, &s.LastRun)
	return s, err
}
