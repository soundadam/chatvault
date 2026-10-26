package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one message as a source hands it to the vault.
type Message struct {
	ServerID int64
	LocalID  int64
	SortSeq  int64
	// LocalType is WeChat's local_type: type in the low 32 bits, subtype in
	// the high 32.
	LocalType  int64
	Time       int64
	Sender     string
	Content    []byte
	SourceXML  []byte
	PackedInfo []byte
	Origin     string
}

// Key is the message's identity within its chat: the server ID when there
// is one, else time, type and a content digest. Never the local ID: that is
// unique only within one of WeChat's database shards.
func (m *Message) Key() string {
	if m.ServerID != 0 {
		return fmt.Sprintf("s:%d", m.ServerID)
	}
	d := sha256.Sum256(m.Content)
	return fmt.Sprintf("l:%d:%d:%x", m.Time, m.LocalType, d[:8])
}

func (m *Message) hash() []byte {
	h := sha256.New()
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], uint64(m.LocalType))
	h.Write(t[:])
	h.Write(m.Content)
	return h.Sum(nil)[:16]
}

// Messages adds one chat's messages to the vault. Messages already there
// with the same content are skipped; with different content, the new form is
// kept as a revision.
func (r *Run) Messages(ctx context.Context, chat string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := r.v.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type known struct {
		id   int64
		hash []byte
	}
	have := map[string]known{}
	rows, err := tx.QueryContext(ctx, "SELECT msg_key, id, hash FROM messages WHERE source_id = ? AND chat = ?", r.Source.ID, chat)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var kn known
		if err := rows.Scan(&k, &kn.id, &kn.hash); err != nil {
			rows.Close()
			return err
		}
		have[k] = kn
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	ins, err := tx.PrepareContext(ctx, `INSERT INTO messages
		(source_id, chat, msg_key, server_id, local_id, sort_seq, type, subtype, time, sender,
		 content, source_xml, packed_info, origin, hash, run_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	defer ins.Close()
	rev, err := tx.PrepareContext(ctx, `INSERT INTO revisions
		(message_id, run_id, type, subtype, content, source_xml, hash) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	defer rev.Close()

	added, revised := 0, 0
	for i := range msgs {
		m := &msgs[i]
		key, h := m.Key(), m.hash()
		typ, sub := m.LocalType&0xffffffff, m.LocalType>>32
		if kn, ok := have[key]; ok {
			if bytes.Equal(kn.hash, h) {
				continue
			}
			res, err := rev.ExecContext(ctx, kn.id, r.ID, typ, sub, m.Content, m.SourceXML, h)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				revised++
			}
			continue
		}
		res, err := ins.ExecContext(ctx, r.Source.ID, chat, key, nullInt(m.ServerID), m.LocalID, m.SortSeq,
			typ, sub, m.Time, nullStr(m.Sender), m.Content, m.SourceXML, m.PackedInfo, m.Origin, h, r.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
			id, _ := res.LastInsertId()
			have[key] = known{id: id, hash: h}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.Added += added
	r.Revised += revised
	if added > 0 {
		r.touched[chat] = true
	}
	return nil
}

// Contact is a contact or group as the source shows it now.
type Contact struct {
	Username string
	Remark   string
	Nickname string
	Alias    string
	Raw      map[string]any
}

// Name is how the contact is shown: remark, else nickname, else alias.
func (c *Contact) Name() string {
	for _, s := range []string{c.Remark, c.Nickname, c.Alias} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// Contacts records the source's contacts. A contact whose fields changed
// since it was last seen keeps its previous form in contact_history.
func (r *Run) Contacts(ctx context.Context, cs []Contact) error {
	tx, err := r.v.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old := map[string]string{}
	rows, err := tx.QueryContext(ctx, "SELECT username, raw FROM contacts WHERE source_id = ?", r.Source.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var u, raw string
		if err := rows.Scan(&u, &raw); err != nil {
			rows.Close()
			return err
		}
		old[u] = raw
	}
	rows.Close()

	up, err := tx.PrepareContext(ctx, `INSERT INTO contacts (source_id, username, name, remark, nickname, alias, raw, run_id)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (source_id, username) DO UPDATE SET
		name = excluded.name, remark = excluded.remark, nickname = excluded.nickname,
		alias = excluded.alias, raw = excluded.raw, run_id = excluded.run_id`)
	if err != nil {
		return err
	}
	defer up.Close()
	hist, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO contact_history (source_id, username, run_id, raw)
		SELECT source_id, username, run_id, raw FROM contacts WHERE source_id = ? AND username = ?`)
	if err != nil {
		return err
	}
	defer hist.Close()
	for i := range cs {
		c := &cs[i]
		raw, err := json.Marshal(c.Raw)
		if err != nil {
			return err
		}
		prev, seen := old[c.Username]
		if seen && prev == string(raw) {
			continue
		}
		if seen {
			if _, err := hist.ExecContext(ctx, r.Source.ID, c.Username); err != nil {
				return err
			}
		}
		if _, err := up.ExecContext(ctx, r.Source.ID, c.Username, nullStr(c.Name()), nullStr(c.Remark),
			nullStr(c.Nickname), nullStr(c.Alias), string(raw), r.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChatKind classifies a chat by its username.
func ChatKind(chat string) string {
	switch {
	case strings.HasSuffix(chat, "@chatroom"):
		return "group"
	case strings.HasSuffix(chat, "@openim"):
		return "openim"
	case strings.HasPrefix(chat, "gh_"):
		return "official"
	case strings.HasPrefix(chat, "wxid_") || !strings.ContainsAny(chat, "@"):
		return "direct"
	default:
		return "other"
	}
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
