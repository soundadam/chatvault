// Package export writes the vault out as files people and tools can read:
// one file per chat and month, under a folder per chat. Each target remembers
// the newest run it took in and the files it wrote, so a later export
// rewrites only the months that changed and removes files it no longer makes.
package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/soundadam/chatvault/internal/parse"
	"github.com/soundadam/chatvault/internal/vault"
)

// Formats are the file formats a target can write.
var Formats = []string{"jsonl", "md"}

// Config is a target's settings; changing any of them rebuilds the target.
type Config struct {
	Format string `json:"format"`
	// Hidden includes chats the user hid.
	Hidden bool `json:"hidden,omitempty"`
	// Kinds limits chats to these kinds (direct, group, ...); empty is all.
	Kinds []string `json:"kinds,omitempty"`
}

// Target is a named export.
type Target struct {
	Name    string
	Dir     string
	Config  Config
	RunID   int64
	Parser  int
	PrefsAt int64
	Updated int64
}

// Targets lists the vault's export targets.
func Targets(ctx context.Context, v *vault.Vault) ([]Target, error) {
	rows, err := v.DB.QueryContext(ctx, "SELECT name, dir, config, run_id, parser, prefs_at, coalesce(updated_at, 0) FROM exports ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		var cfg string
		if err := rows.Scan(&t.Name, &t.Dir, &cfg, &t.RunID, &t.Parser, &t.PrefsAt, &t.Updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cfg), &t.Config); err != nil {
			return nil, fmt.Errorf("export %s: %w", t.Name, err)
		}
		if !filepath.IsAbs(t.Dir) {
			t.Dir = filepath.Join(v.Dir, t.Dir)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Save creates or changes a target. A change of directory or config resets
// it, so the next export rebuilds it whole. A directory inside the vault is
// stored relative to it, so the vault can be moved.
func Save(ctx context.Context, v *vault.Vault, name, dir string, cfg Config) error {
	if !slices.Contains(Formats, cfg.Format) {
		return fmt.Errorf("unknown format %q (have %s)", cfg.Format, strings.Join(Formats, ", "))
	}
	if name == "" || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("bad export name %q", name)
	}
	if dir == "" {
		dir = filepath.Join(v.Dir, "export", name)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(v.Dir, dir); err == nil && filepath.IsLocal(rel) {
		dir = rel
	}
	slices.Sort(cfg.Kinds)
	b, _ := json.Marshal(cfg)
	_, err = v.DB.ExecContext(ctx, `INSERT INTO exports (name, format, dir, config) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET format = excluded.format, dir = excluded.dir, config = excluded.config,
		run_id = CASE WHEN dir = excluded.dir AND config = excluded.config THEN run_id ELSE 0 END`,
		name, cfg.Format, dir, string(b))
	return err
}

// Remove forgets a target, and deletes its files if asked.
func Remove(ctx context.Context, v *vault.Vault, name string, files bool) error {
	ts, err := Targets(ctx, v)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ts, func(t Target) bool { return t.Name == name })
	if i < 0 {
		return fmt.Errorf("no export named %q", name)
	}
	if files {
		paths, err := filesOf(ctx, v, name)
		if err != nil {
			return err
		}
		for p := range paths {
			os.Remove(filepath.Join(ts[i].Dir, p))
		}
		removeEmptyDirs(ts[i].Dir)
	}
	if _, err := v.DB.ExecContext(ctx, "DELETE FROM export_files WHERE export = ?", name); err != nil {
		return err
	}
	_, err = v.DB.ExecContext(ctx, "DELETE FROM exports WHERE name = ?", name)
	return err
}

// Result says what one export did.
type Result struct {
	Target  string
	Full    bool
	Written int // files written
	Removed int // files removed
	Chats   int // chats with a written file
	Took    time.Duration
}

// Progress reports files written so far, out of the planned total.
type Progress func(target string, done, total int)

type unit struct {
	chat  *vault.Chat
	month time.Time
}

type fileRow struct {
	sourceID int64
	chat     string
	month    string
}

// Run brings one target up to date.
func Run(ctx context.Context, v *vault.Vault, t Target, progress Progress) (Result, error) {
	start := time.Now()
	res := Result{Target: t.Name}
	var lastRun, prefsAt int64
	if err := v.DB.QueryRowContext(ctx, `SELECT coalesce((SELECT max(id) FROM runs), 0),
		coalesce((SELECT max(updated_at) FROM chat_prefs), 0)`).Scan(&lastRun, &prefsAt); err != nil {
		return res, err
	}
	res.Full = t.RunID == 0 || t.Parser != parse.Version
	if !res.Full && t.RunID >= lastRun && t.PrefsAt >= prefsAt {
		return res, nil
	}

	chats, err := v.Chats(ctx)
	if err != nil {
		return res, err
	}
	paths := chatPaths(chats)
	byKey := map[string]*vault.Chat{}
	for i := range chats {
		byKey[key(chats[i].SourceID, chats[i].Chat)] = &chats[i]
	}
	include := func(c *vault.Chat) bool {
		if c.Hidden && !t.Config.Hidden {
			return false
		}
		return len(t.Config.Kinds) == 0 || slices.Contains(t.Config.Kinds, c.Kind)
	}

	old, err := filesOf(ctx, v, t.Name)
	if err != nil {
		return res, err
	}

	// What to write: everything on a full run; else the months with new or
	// revised messages, and all months of chats whose name, folder or
	// visibility may have moved their files.
	var units []unit
	dirtyChats := map[string]bool{} // chats whose stale files are to be found
	if res.Full {
		for i := range chats {
			c := &chats[i]
			dirtyChats[key(c.SourceID, c.Chat)] = true
		}
	} else {
		whole, err := changedChats(ctx, v, t)
		if err != nil {
			return res, err
		}
		months, err := changedMonths(ctx, v, t.RunID)
		if err != nil {
			return res, err
		}
		for k := range whole {
			dirtyChats[k] = true
		}
		for k, ms := range months {
			if whole[k] {
				continue
			}
			c := byKey[k]
			if c == nil || !include(c) {
				continue
			}
			for m := range ms {
				units = append(units, unit{chat: c, month: m})
			}
		}
	}
	for k := range dirtyChats {
		c := byKey[k]
		if c == nil || !include(c) {
			continue
		}
		ms, err := v.Months(ctx, c.SourceID, c.Chat)
		if err != nil {
			return res, err
		}
		for _, m := range ms {
			units = append(units, unit{chat: c, month: m.Month})
		}
	}

	names := map[int64]map[string]string{}
	selves := map[int64]string{}
	written := map[string]bool{}
	for i, u := range units {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		c := u.chat
		if names[c.SourceID] == nil {
			if names[c.SourceID], err = v.Names(ctx, c.SourceID); err != nil {
				return res, err
			}
			src, err := v.Source(ctx, c.SourceID)
			if err != nil {
				return res, err
			}
			selves[c.SourceID] = src.Self
		}
		rel := filepath.Join(paths[key(c.SourceID, c.Chat)], u.month.Format("2006-01")+"."+t.Config.Format)
		n, err := writeFile(ctx, v, filepath.Join(t.Dir, rel), t.Config.Format, c, u.month, names[c.SourceID], selves[c.SourceID])
		if err != nil {
			return res, err
		}
		if _, err := v.DB.ExecContext(ctx, `INSERT OR REPLACE INTO export_files (export, path, source_id, chat, month, messages)
			VALUES (?, ?, ?, ?, ?, ?)`, t.Name, rel, c.SourceID, c.Chat, u.month.Format("2006-01"), n); err != nil {
			return res, err
		}
		written[rel] = true
		res.Written++
		if progress != nil {
			progress(t.Name, i+1, len(units))
		}
	}

	// Remove what this target wrote before and would not write now.
	for p, f := range old {
		if written[p] {
			continue
		}
		stale := res.Full || dirtyChats[key(f.sourceID, f.chat)]
		if c := byKey[key(f.sourceID, f.chat)]; c == nil || !include(c) {
			stale = true
		}
		if !stale {
			continue
		}
		if err := os.Remove(filepath.Join(t.Dir, p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, err
		}
		if _, err := v.DB.ExecContext(ctx, "DELETE FROM export_files WHERE export = ? AND path = ?", t.Name, p); err != nil {
			return res, err
		}
		res.Removed++
	}
	if res.Removed > 0 {
		removeEmptyDirs(t.Dir)
	}
	if err := writeIndex(ctx, v, t, chats, paths, include); err != nil {
		return res, err
	}
	chatsWritten := map[*vault.Chat]bool{}
	for _, u := range units {
		chatsWritten[u.chat] = true
	}
	res.Chats = len(chatsWritten)
	_, err = v.DB.ExecContext(ctx, "UPDATE exports SET run_id = ?, parser = ?, prefs_at = ?, updated_at = ? WHERE name = ?",
		lastRun, parse.Version, prefsAt, time.Now().Unix(), t.Name)
	res.Took = time.Since(start)
	return res, err
}

func key(sourceID int64, chat string) string { return fmt.Sprintf("%d\x00%s", sourceID, chat) }

func filesOf(ctx context.Context, v *vault.Vault, name string) (map[string]fileRow, error) {
	rows, err := v.DB.QueryContext(ctx, "SELECT path, source_id, chat, month FROM export_files WHERE export = ?", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]fileRow{}
	for rows.Next() {
		var p string
		var f fileRow
		if err := rows.Scan(&p, &f.sourceID, &f.chat, &f.month); err != nil {
			return nil, err
		}
		out[p] = f
	}
	return out, rows.Err()
}

// changedChats are chats whose every file may need rewriting: their prefs
// or contact entry changed since the target's last export.
func changedChats(ctx context.Context, v *vault.Vault, t Target) (map[string]bool, error) {
	out := map[string]bool{}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{"SELECT source_id, chat FROM chat_prefs WHERE updated_at > ?", t.PrefsAt},
		{"SELECT source_id, username FROM contacts WHERE run_id > ?", t.RunID},
	} {
		rows, err := v.DB.QueryContext(ctx, q.sql, q.arg)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var chat string
			if err := rows.Scan(&id, &chat); err != nil {
				rows.Close()
				return nil, err
			}
			out[key(id, chat)] = true
		}
		rows.Close()
	}
	return out, nil
}

// changedMonths are the months with messages added or revised after run.
func changedMonths(ctx context.Context, v *vault.Vault, run int64) (map[string]map[time.Time]bool, error) {
	rows, err := v.DB.QueryContext(ctx, `
		SELECT source_id, chat, time FROM messages WHERE run_id > ?
		UNION
		SELECT m.source_id, m.chat, m.time FROM revisions r JOIN messages m ON m.id = r.message_id WHERE r.run_id > ?`, run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[time.Time]bool{}
	for rows.Next() {
		var id, ts int64
		var chat string
		if err := rows.Scan(&id, &chat, &ts); err != nil {
			return nil, err
		}
		k := key(id, chat)
		if out[k] == nil {
			out[k] = map[time.Time]bool{}
		}
		out[k][vault.Month(ts)] = true
	}
	return out, rows.Err()
}

// chatPaths gives each chat a directory, folder/name, unique among all
// chats. Clashing names get the end of the username added.
func chatPaths(chats []vault.Chat) map[string]string {
	sorted := make([]*vault.Chat, len(chats))
	for i := range chats {
		sorted[i] = &chats[i]
	}
	slices.SortFunc(sorted, func(a, b *vault.Chat) int {
		if a.SourceID != b.SourceID {
			return int(a.SourceID - b.SourceID)
		}
		return strings.Compare(a.Chat, b.Chat)
	})
	byName := map[string][]*vault.Chat{}
	for _, c := range sorted {
		p := chatDir(c)
		byName[strings.ToLower(p)] = append(byName[strings.ToLower(p)], c)
	}
	out := map[string]string{}
	for _, cs := range byName {
		for i, c := range cs {
			p := chatDir(c)
			if i > 0 || len(cs) > 1 {
				p += " (" + shortID(c.Chat) + ")"
			}
			out[key(c.SourceID, c.Chat)] = p
		}
	}
	return out
}

func chatDir(c *vault.Chat) string {
	name := Clean(c.Name())
	if c.Folder == "" {
		return name
	}
	var parts []string
	for _, p := range strings.Split(c.Folder, "/") {
		if p = Clean(p); p != "" && p != "_" {
			parts = append(parts, p)
		}
	}
	return filepath.Join(append(parts, name)...)
}

func shortID(chat string) string {
	chat = strings.TrimSuffix(strings.TrimSuffix(chat, "@chatroom"), "@openim")
	if len(chat) > 8 {
		chat = chat[len(chat)-8:]
	}
	return Clean(chat)
}

// Clean makes s safe as one path element.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Trim(s, ". "))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	if s == "" {
		return "_"
	}
	return s
}

func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range slices.Backward(dirs) {
		os.Remove(d) // fails, as wanted, unless empty
	}
}

// writeFile writes one chat-month and returns how many messages it holds.
func writeFile(ctx context.Context, v *vault.Vault, path, format string, c *vault.Chat, month time.Time,
	names map[string]string, self string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := newWriter(f, format, c)
	n := 0
	err = v.Entries(ctx, c.SourceID, c.Chat, month.Unix(), month.AddDate(0, 1, 0).Unix(), func(e *vault.Entry) error {
		n++
		return w.entry(e, senderName(e.Sender, names, self, c), e.Sender != "" && e.Sender == self)
	})
	if err == nil {
		err = w.close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, path)
}

func senderName(sender string, names map[string]string, self string, c *vault.Chat) string {
	switch {
	case sender == "":
		return ""
	case sender == self:
		return "我"
	case sender == c.Chat:
		return c.Name()
	case names[sender] != "":
		return names[sender]
	}
	return sender
}

type indexEntry struct {
	Chat     string `json:"chat"`
	Source   int64  `json:"source"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Folder   string `json:"folder,omitempty"`
	Path     string `json:"path"`
	Messages int    `json:"messages"`
	First    string `json:"first,omitempty"`
	Last     string `json:"last,omitempty"`
}

// writeIndex writes chats.json, a list of the exported chats and where
// their files are.
func writeIndex(ctx context.Context, v *vault.Vault, t Target, chats []vault.Chat, paths map[string]string, include func(*vault.Chat) bool) error {
	var idx []indexEntry
	for i := range chats {
		c := &chats[i]
		if !include(c) {
			continue
		}
		e := indexEntry{Chat: c.Chat, Source: c.SourceID, Name: c.Name(), Kind: c.Kind, Folder: c.Folder,
			Path: filepath.ToSlash(paths[key(c.SourceID, c.Chat)]), Messages: c.Count}
		if c.First > 0 {
			e.First = time.Unix(c.First, 0).Format(time.RFC3339)
			e.Last = time.Unix(c.Last, 0).Format(time.RFC3339)
		}
		idx = append(idx, e)
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(t.Dir, "chats.json")
	if err := os.WriteFile(p+".tmp", append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}
