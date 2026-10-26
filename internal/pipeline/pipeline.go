// Package pipeline runs a sync: copy and decrypt the platform's databases,
// merge what changed into the vault, parse new messages, and bring every
// export target up to date. Each stage skips work an earlier sync already
// did, so a sync with nothing new takes moments.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/soundadam/chatvault/internal/export"
	"github.com/soundadam/chatvault/internal/vault"
	"github.com/soundadam/chatvault/internal/wechat"
)

// Stage names, in order.
const (
	StageDecrypt = "decrypt"
	StageMerge   = "merge"
	StageParse   = "parse"
	StageExport  = "export"
)

// Event is progress within a stage.
type Event struct {
	Stage string
	Item  string // database, chat or export target
	Done  int
	Total int
	Err   error
}

// Options say what to sync.
type Options struct {
	// Account picks a WeChat account by directory name or username; empty
	// takes the only (or most recently used) one.
	Account     string
	ToolVersion string
	// NoExport skips the export stage.
	NoExport bool
}

// Report is the outcome of a sync.
type Report struct {
	Account   string
	Changed   []string // databases with a new plain copy
	Failed    []string // databases no key opened
	Added     int
	Revised   int
	Parsed    int
	Exports   []export.Result
	Durations map[string]time.Duration
}

// KeysPath is where a source's database keys live.
func KeysPath(v *vault.Vault, acct wechat.Account) string {
	return filepath.Join(v.SourceDir("wechat", acct.ID()), "keys.json")
}

// ErrNoKeys means keys have not been imported for the account.
var ErrNoKeys = errors.New("no database keys yet; import them with `chatvault keys import keys.json`")

// Sync runs every stage.
func Sync(ctx context.Context, v *vault.Vault, opt Options, progress func(Event)) (*Report, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	rep := &Report{Durations: map[string]time.Duration{}}
	acct, err := wechat.FindAccount(wechat.Root(), opt.Account)
	if err != nil {
		return rep, err
	}
	rep.Account = acct.ID()
	if err := acct.CheckLayout(); err != nil {
		return rep, err
	}
	dir := v.SourceDir("wechat", acct.ID())
	p := wechat.Paths{
		Snapshot: filepath.Join(dir, "snapshot"),
		Plain:    filepath.Join(dir, "plain"),
		State:    filepath.Join(dir, "state.json"),
		Keys:     KeysPath(v, acct),
	}
	keys, err := wechat.LoadKeys(p.Keys)
	if errors.Is(err, os.ErrNotExist) {
		return rep, fmt.Errorf("%s: %w", acct.ID(), ErrNoKeys)
	}
	if err != nil {
		return rep, err
	}
	st, err := wechat.LoadState(p.State)
	if err != nil {
		return rep, err
	}

	t := time.Now()
	rep.Changed, rep.Failed, err = wechat.Prepare(ctx, acct, p, st, keys, func(e wechat.Event) {
		progress(Event{Stage: StageDecrypt, Item: e.DB, Done: e.Done, Total: e.Total, Err: e.Err})
	})
	if serr := st.Save(p.State); err == nil {
		err = serr
	}
	rep.Durations[StageDecrypt] = time.Since(t)
	if err != nil {
		return rep, err
	}

	t = time.Now()
	src, err := v.EnsureSource(ctx, "wechat", acct.ID(), acct.Username())
	if err != nil {
		return rep, err
	}
	var runs int
	if err := v.DB.QueryRowContext(ctx, "SELECT count(*) FROM runs").Scan(&runs); err != nil {
		return rep, err
	}
	if err := merge(ctx, v, src, acct, p, st, opt.ToolVersion, rep, progress); err != nil {
		return rep, err
	}
	if runs == 0 {
		if err := defaultTargets(ctx, v); err != nil {
			return rep, err
		}
	}
	rep.Durations[StageMerge] = time.Since(t)

	t = time.Now()
	rep.Parsed, err = v.Reparse(ctx, func(done, total int) {
		progress(Event{Stage: StageParse, Done: done, Total: total})
	})
	rep.Durations[StageParse] = time.Since(t)
	if err != nil {
		return rep, err
	}

	if opt.NoExport {
		return rep, nil
	}
	t = time.Now()
	rep.Exports, err = Export(ctx, v, progress)
	rep.Durations[StageExport] = time.Since(t)
	return rep, err
}

// merge takes in the contacts and every message shard not yet merged in its
// current form.
func merge(ctx context.Context, v *vault.Vault, src vault.Source, acct wechat.Account, p wechat.Paths,
	st *wechat.State, toolVersion string, rep *Report, progress func(Event)) error {
	var shards []string
	for rel, d := range st.DBs {
		if d.Plain && !d.Merged && wechat.MessageDB(rel) {
			shards = append(shards, rel)
		}
	}
	contactRel := filepath.Join("contact", "contact.db")
	contacts := st.DBs[contactRel] != nil && st.DBs[contactRel].Plain && !st.DBs[contactRel].Merged
	if len(shards) == 0 && !contacts {
		return nil
	}
	run, err := v.BeginRun(ctx, src, toolVersion)
	if err != nil {
		return err
	}
	if contacts {
		cs, err := wechat.ReadContacts(ctx, p.Plain)
		if err != nil {
			return fmt.Errorf("contacts: %w", err)
		}
		if err := run.Contacts(ctx, cs); err != nil {
			return fmt.Errorf("contacts: %w", err)
		}
		st.DBs[contactRel].Merged = true
	}
	known, err := wechat.KnownUsernames(ctx, p.Plain)
	if err != nil {
		return err
	}
	for i, rel := range sortedShards(shards) {
		progress(Event{Stage: StageMerge, Item: rel, Done: i, Total: len(shards)})
		err := wechat.ReadMessages(ctx, filepath.Join(p.Plain, rel), known, func(chat string, msgs []vault.Message) error {
			return run.Messages(ctx, chat, msgs)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		st.DBs[rel].Merged = true
		if err := st.Save(p.State); err != nil {
			return err
		}
	}
	progress(Event{Stage: StageMerge, Done: len(shards), Total: len(shards)})
	if err := run.Finish(ctx); err != nil {
		return err
	}
	rep.Added, rep.Revised = run.Added, run.Revised
	return st.Save(p.State)
}

// sortedShards puts the oldest shards first, so a message seen in two
// shards is first recorded in its oldest form.
func sortedShards(s []string) []string {
	out := slices.Clone(s)
	slices.SortFunc(out, func(a, b string) int { return shardNum(a) - shardNum(b) })
	return out
}

func shardNum(rel string) int {
	var n int
	fmt.Sscanf(filepath.Base(rel), "message_%d.db", &n)
	return n
}

// defaultTargets gives a new vault one md and one jsonl export, which the
// user may change or remove; txt and json are added with export add.
func defaultTargets(ctx context.Context, v *vault.Vault) error {
	ts, err := export.Targets(ctx, v)
	if err != nil || len(ts) > 0 {
		return err
	}
	for _, f := range []string{"md", "jsonl"} {
		if err := export.Save(ctx, v, f, "", export.Config{Format: f}); err != nil {
			return err
		}
	}
	return nil
}

// Export runs every export target.
func Export(ctx context.Context, v *vault.Vault, progress func(Event)) ([]export.Result, error) {
	ts, err := export.Targets(ctx, v)
	if err != nil {
		return nil, err
	}
	var out []export.Result
	for _, t := range ts {
		r, err := export.Run(ctx, v, t, func(name string, done, total int) {
			if progress != nil {
				progress(Event{Stage: StageExport, Item: name, Done: done, Total: total})
			}
		})
		out = append(out, r)
		if err != nil {
			return out, fmt.Errorf("export %s: %w", t.Name, err)
		}
	}
	return out, nil
}

// ImportKeys copies a keys file into the account's source directory, readable
// only by the user, after checking it parses.
func ImportKeys(v *vault.Vault, acct wechat.Account, from string) (int, error) {
	keys, err := wechat.LoadKeys(from)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return 0, err
	}
	dst := KeysPath(v, acct)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	if err := os.WriteFile(dst+".tmp", data, 0o600); err != nil {
		return 0, err
	}
	return len(keys), os.Rename(dst+".tmp", dst)
}
