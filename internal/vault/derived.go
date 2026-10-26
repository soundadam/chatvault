package vault

import (
	"context"
	"encoding/json"

	"github.com/soundadam/chatvault/internal/parse"
)

// Reparse fills `parsed` for every message that has no row there yet, or
// one written by an older parser. It returns how many it parsed.
func (v *Vault) Reparse(ctx context.Context, progress func(done, total int)) (int, error) {
	var total int
	const pending = `FROM messages m LEFT JOIN parsed p ON p.message_id = m.id
		WHERE p.message_id IS NULL OR p.parser < ?`
	if err := v.DB.QueryRowContext(ctx, "SELECT count(*) "+pending, parse.Version).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}
	type row struct {
		id, typ, sub int64
		chat         string
		content      []byte
	}
	done, last := 0, int64(0)
	for {
		rows, err := v.DB.QueryContext(ctx, "SELECT m.id, m.type, m.subtype, m.chat, m.content "+pending+
			" AND m.id > ? ORDER BY m.id LIMIT 5000", parse.Version, last)
		if err != nil {
			return done, err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.typ, &r.sub, &r.chat, &r.content); err != nil {
				rows.Close()
				return done, err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return done, err
		}
		if len(batch) == 0 {
			return done, nil
		}
		tx, err := v.DB.BeginTx(ctx, nil)
		if err != nil {
			return done, err
		}
		up, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO parsed (message_id, parser, kind, text, data, sender)
			VALUES (?,?,?,?,?,?)`)
		if err != nil {
			tx.Rollback()
			return done, err
		}
		for _, r := range batch {
			res := parse.Message(r.typ, r.sub, r.content, ChatKind(r.chat) == "group")
			var data any
			if len(res.Data) > 0 {
				b, err := json.Marshal(res.Data)
				if err != nil {
					up.Close()
					tx.Rollback()
					return done, err
				}
				data = string(b)
			}
			if _, err := up.ExecContext(ctx, r.id, parse.Version, res.Kind, res.Text, data, nullStr(res.Sender)); err != nil {
				up.Close()
				tx.Rollback()
				return done, err
			}
		}
		up.Close()
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done += len(batch)
		last = batch[len(batch)-1].id
		if progress != nil {
			progress(done, total)
		}
	}
}
