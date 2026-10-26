package wechat

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/soundadam/chatvault/internal/vault"
)

func openPlain(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
}

func tables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

func columns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// ReadContacts reads contact/contact.db (people, groups, official accounts).
func ReadContacts(ctx context.Context, plain string) ([]vault.Contact, error) {
	db, err := openPlain(filepath.Join(plain, "contact", "contact.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT * FROM contact")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []vault.Contact
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		raw := map[string]any{}
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte, nil:
				// blobs (extra_buffer) are protobuf; not kept as JSON
			default:
				raw[c] = v
			}
		}
		str := func(k string) string { s, _ := raw[k].(string); return s }
		if str("username") == "" {
			continue
		}
		out = append(out, vault.Contact{
			Username: str("username"), Remark: str("remark"), Nickname: str("nick_name"), Alias: str("alias"), Raw: raw,
		})
	}
	return out, rows.Err()
}

// KnownUsernames collects usernames from contacts and sessions, to name the
// Msg_<md5> tables whose chat is not in a shard's Name2Id.
func KnownUsernames(ctx context.Context, plain string) ([]string, error) {
	var out []string
	for _, q := range []struct{ db, sql string }{
		{"contact/contact.db", "SELECT username FROM contact"},
		{"session/session.db", "SELECT username FROM SessionTable"},
	} {
		db, err := openPlain(filepath.Join(plain, q.db))
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, q.sql)
		if err == nil {
			for rows.Next() {
				var u sql.NullString
				if rows.Scan(&u) == nil && u.String != "" {
					out = append(out, u.String)
				}
			}
			rows.Close()
		}
		db.Close()
	}
	return out, nil
}

var msgTable = regexp.MustCompile(`^Msg_([0-9a-f]{32})$`)

// MessageDB reports whether rel is one of the message shards.
func MessageDB(rel string) bool {
	m, _ := filepath.Match("message_*.db", filepath.Base(rel))
	return m && strings.HasPrefix(rel, "message"+string(filepath.Separator)) && !strings.Contains(filepath.Base(rel), "_fts")
}

// ReadMessages calls fn once per chat in the message shard at path, with
// all of its messages. known names the chats the shard's Name2Id lacks.
func ReadMessages(ctx context.Context, path string, known []string, fn func(chat string, msgs []vault.Message) error) error {
	db, err := openPlain(path)
	if err != nil {
		return err
	}
	defer db.Close()
	tbls, err := tables(ctx, db)
	if err != nil {
		return err
	}
	origin := filepath.Base(path)

	id2name := map[int64]string{}
	byMD5 := map[string]string{}
	for _, u := range known {
		byMD5[md5hex(u)] = u
	}
	if tbls["Name2Id"] {
		rows, err := db.QueryContext(ctx, "SELECT rowid, user_name FROM Name2Id")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			id2name[id] = name
			byMD5[md5hex(name)] = name
		}
		rows.Close()
	}

	names := make([]string, 0, len(tbls))
	for t := range tbls {
		if msgTable.MatchString(t) {
			names = append(names, t)
		}
	}
	slices.Sort(names)
	for _, t := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := msgTable.FindStringSubmatch(t)[1]
		chat, ok := byMD5[hash]
		if !ok {
			chat = "md5:" + hash
		}
		cols, err := columns(ctx, db, t)
		if err != nil {
			return err
		}
		pick := func(c string) string {
			if cols[c] {
				return c
			}
			return "NULL"
		}
		rows, err := db.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s FROM "%s" ORDER BY local_id`,
			pick("server_id"), pick("local_id"), pick("sort_seq"), pick("local_type"), pick("create_time"),
			pick("real_sender_id"), pick("message_content"), pick("source"), pick("packed_info_data"), t))
		if err != nil {
			return err
		}
		var msgs []vault.Message
		for rows.Next() {
			var (
				serverID, localID, seq, typ, ts, sender sql.NullInt64
				content, source, packed                 []byte
			)
			if err := rows.Scan(&serverID, &localID, &seq, &typ, &ts, &sender, &content, &source, &packed); err != nil {
				rows.Close()
				return err
			}
			msgs = append(msgs, vault.Message{
				ServerID: serverID.Int64, LocalID: localID.Int64, SortSeq: seq.Int64, LocalType: typ.Int64,
				Time: ts.Int64, Sender: id2name[sender.Int64], Content: content, SourceXML: source,
				PackedInfo: packed, Origin: origin,
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := fn(chat, msgs); err != nil {
			return err
		}
	}
	return nil
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
