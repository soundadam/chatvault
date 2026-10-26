// Package vault is chatvault's own store: one SQLite file, vault.sqlite,
// holding every message ever imported, plus per-source working directories
// for the platform's raw and decrypted copies.
//
// Layout of the vault directory:
//
//	vault.sqlite                      messages, contacts, derived and user tables
//	sources/<platform>/<account>/     keys.json, state.json, snapshot/, plain/
//	export/<target>/                  default home of export targets
package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// EnvHome overrides the default vault directory.
const EnvHome = "CHATVAULT_HOME"

// DefaultDir is $CHATVAULT_HOME, else ~/Documents/ChatVault.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Documents", "ChatVault"), nil
}

// Vault is an open vault directory.
type Vault struct {
	Dir string
	DB  *sql.DB
}

// SchemaVersion is the newest schema this build knows.
func SchemaVersion() int { return len(migrations) }

// Open opens (creating if needed) the vault at dir and brings its schema up
// to date. A vault written by a newer chatvault is refused, not guessed at.
func Open(dir string) (*Vault, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "vault.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	v := &Vault{Dir: dir, DB: db}
	if err := v.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return v, nil
}

// Exists reports whether dir already holds a vault.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "vault.sqlite"))
	return err == nil
}

func (v *Vault) Close() error { return v.DB.Close() }

func (v *Vault) migrate() error {
	var cur int
	if err := v.DB.QueryRow("PRAGMA user_version").Scan(&cur); err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("vault schema %d is newer than this chatvault knows (%d); update chatvault", cur, len(migrations))
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := v.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SourceDir is the working directory of one platform account.
func (v *Vault) SourceDir(platform, account string) string {
	return filepath.Join(v.Dir, "sources", platform, account)
}

// Source is one imported account.
type Source struct {
	ID       int64
	Platform string
	Account  string
	Self     string
}

// EnsureSource returns the source row for platform/account, creating it.
func (v *Vault) EnsureSource(ctx context.Context, platform, account, self string) (Source, error) {
	s := Source{Platform: platform, Account: account, Self: self}
	err := v.DB.QueryRowContext(ctx,
		`INSERT INTO sources (platform, account, self) VALUES (?, ?, ?)
		 ON CONFLICT (platform, account) DO UPDATE SET self = coalesce(excluded.self, self)
		 RETURNING id`, platform, account, nullStr(self)).Scan(&s.ID)
	return s, err
}

// Sources lists every imported account.
func (v *Vault) Sources(ctx context.Context) ([]Source, error) {
	rows, err := v.DB.QueryContext(ctx, "SELECT id, platform, account, coalesce(self, '') FROM sources ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.Platform, &s.Account, &s.Self); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Run is one import into a source.
type Run struct {
	ID      int64
	Source  Source
	Added   int
	Revised int
	touched map[string]bool
	v       *Vault
}

// BeginRun records the start of an import.
func (v *Vault) BeginRun(ctx context.Context, src Source, toolVersion string) (*Run, error) {
	r := &Run{Source: src, v: v, touched: map[string]bool{}}
	err := v.DB.QueryRowContext(ctx,
		"INSERT INTO runs (source_id, started_at, tool_version) VALUES (?, ?, ?) RETURNING id",
		src.ID, time.Now().Unix(), toolVersion).Scan(&r.ID)
	return r, err
}

// Finish refreshes the chat summaries the run touched and closes the run.
func (r *Run) Finish(ctx context.Context) error {
	tx, err := r.v.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	up, err := tx.PrepareContext(ctx, `
		INSERT INTO chats (source_id, chat, kind, first_time, last_time, message_count)
		SELECT ?, ?, ?, min(time), max(time), count(*) FROM messages WHERE source_id = ? AND chat = ?
		ON CONFLICT (source_id, chat) DO UPDATE SET
		    first_time = excluded.first_time, last_time = excluded.last_time,
		    message_count = excluded.message_count, kind = excluded.kind`)
	if err != nil {
		return err
	}
	defer up.Close()
	for chat := range r.touched {
		if _, err := up.ExecContext(ctx, r.Source.ID, chat, ChatKind(chat), r.Source.ID, chat); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runs SET finished_at = ?, added = ?, revised = ? WHERE id = ?",
		time.Now().Unix(), r.Added, r.Revised, r.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ErrNoVault means the directory has no vault yet.
var ErrNoVault = errors.New("no vault here yet; run `chatvault sync` first")
