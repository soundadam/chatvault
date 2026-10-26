package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/soundadam/chatvault/internal/export"
	"github.com/soundadam/chatvault/internal/pipeline"
	"github.com/soundadam/chatvault/internal/vault"
	"github.com/soundadam/chatvault/internal/wechat"
)

func (app *App) newSyncCommand() *cobra.Command {
	var opt pipeline.Options
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Copy, decrypt, merge, parse and export what is new",
		Long: `sync brings the vault and its exports up to date with WeChat. Databases
that have not changed since the last sync are skipped, as are messages the
vault already has; exports rewrite only the months that gained messages.

WeChat can stay open: sync copies its databases (instantly on APFS) and
works on the copies.`,
		GroupID: groupSync,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := app.open(true)
			if err != nil {
				return err
			}
			defer v.Close()
			opt.ToolVersion = app.Identity.Version
			p := newProgress(app.Err, app.StderrTTY)
			rep, err := pipeline.Sync(cmd.Context(), v, opt, p.event)
			p.clear()
			printReport(app.Out, rep)
			return err
		},
	}
	cmd.Flags().StringVar(&opt.Account, "account", "", "WeChat account directory or username (default: the most recently used)")
	cmd.Flags().BoolVar(&opt.NoExport, "no-export", false, "stop after parsing; leave exports as they are")
	return cmd
}

// progress draws one updating line on a terminal, and nothing elsewhere.
type progress struct {
	w    io.Writer
	tty  bool
	last time.Time
}

func newProgress(w io.Writer, tty bool) *progress { return &progress{w: w, tty: tty} }

func (p *progress) event(e pipeline.Event) {
	if e.Err != nil {
		p.clear()
		fmt.Fprintf(p.w, "%s: %v\n", e.Stage, e.Err)
		return
	}
	if !p.tty || (time.Since(p.last) < 80*time.Millisecond && e.Done != e.Total) {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(p.w, "\r\033[K%-7s %d/%d %s", e.Stage, e.Done, e.Total, e.Item)
}

func (p *progress) clear() {
	if p.tty {
		fmt.Fprint(p.w, "\r\033[K")
	}
}

func printReport(w io.Writer, r *pipeline.Report) {
	if r == nil || len(r.Durations) == 0 {
		return
	}
	d := func(s string) string { return r.Durations[s].Round(time.Millisecond).String() }
	fmt.Fprintf(w, "account   %s\n", r.Account)
	fmt.Fprintf(w, "decrypt   %d changed, %d failed (%s)\n", len(r.Changed), len(r.Failed), d(pipeline.StageDecrypt))
	for _, f := range r.Failed {
		fmt.Fprintf(w, "          no key for %s\n", f)
	}
	if len(r.Failed) > 0 {
		fmt.Fprintln(w, "          the rest synced; for these, import a fresh keys.json (`chatvault keys import`)")
	}
	fmt.Fprintf(w, "merge     %d new, %d revised (%s)\n", r.Added, r.Revised, d(pipeline.StageMerge))
	fmt.Fprintf(w, "parse     %d (%s)\n", r.Parsed, d(pipeline.StageParse))
	printExports(w, r.Exports)
}

func printExports(w io.Writer, rs []export.Result) {
	for _, e := range rs {
		what := "up to date"
		if e.Written > 0 || e.Removed > 0 {
			what = fmt.Sprintf("%d files in %d chats, %d removed", e.Written, e.Chats, e.Removed)
			if e.Full {
				what += ", full rebuild"
			}
		}
		fmt.Fprintf(w, "export    %s: %s (%s)\n", e.Target, what, e.Took.Round(time.Millisecond))
	}
}

func (app *App) newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Show what the vault holds and when it last synced",
		GroupID: groupSync,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return app.status(cmd.Context()) },
	}
}

func (app *App) status(ctx context.Context) error {
	v, err := app.open(false)
	if err != nil {
		return err
	}
	defer v.Close()
	s, err := v.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(app.Out, "vault     %s\n", v.Dir)
	fmt.Fprintf(app.Out, "messages  %d in %d chats (%d revised forms kept)\n", s.Messages, s.Chats, s.Revisions)
	fmt.Fprintf(app.Out, "contacts  %d\n", s.Contacts)
	last := "never"
	if s.LastRun > 0 {
		last = time.Unix(s.LastRun, 0).Format("2006-01-02 15:04")
	}
	fmt.Fprintf(app.Out, "synced    %s (%d runs)\n", last, s.Runs)
	if s.Runs == 0 {
		if h := app.diagnose(ctx).hint(); h != "" {
			fmt.Fprintf(app.Out, "next      %s\n", h)
		}
	}
	ts, err := export.Targets(ctx, v)
	if err != nil {
		return err
	}
	for _, t := range ts {
		fmt.Fprintf(app.Out, "export    %s → %s\n", t.Name, t.Dir)
	}
	return nil
}

func (app *App) newChatsCommand() *cobra.Command {
	var all bool
	var kind string
	cmd := &cobra.Command{
		Use:     "chats [filter]",
		Short:   "List chats, newest first",
		GroupID: groupBrowse,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := app.open(false)
			if err != nil {
				return err
			}
			defer v.Close()
			chats, err := v.Chats(cmd.Context())
			if err != nil {
				return err
			}
			q := ""
			if len(args) > 0 {
				q = args[0]
			}
			tw := tabwriter.NewWriter(app.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "#\tNAME\tKIND\tMESSAGES\tLAST\tFOLDER\tCHAT")
			for i, c := range chats {
				if (c.Hidden && !all) || (kind != "" && c.Kind != kind) || !matches(&c, q) {
					continue
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n", i+1, c.Name(), c.Kind, c.Count,
					time.Unix(c.Last, 0).Format("2006-01-02"), c.Folder, c.Chat)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include hidden chats")
	cmd.Flags().StringVar(&kind, "kind", "", "only chats of this kind: direct, group, official, openim")
	return cmd
}

func matches(c *vault.Chat, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	for _, s := range []string{c.Name(), c.ContactName, c.Chat, c.Folder} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// findChat resolves a chat by its number in `chats`, its username, or a
// name that only one chat has or contains.
func findChat(chats []vault.Chat, q string) (*vault.Chat, error) {
	if n, err := strconv.Atoi(q); err == nil && n >= 1 && n <= len(chats) {
		return &chats[n-1], nil
	}
	for _, exact := range []func(*vault.Chat) bool{
		func(c *vault.Chat) bool { return c.Chat == q },
		func(c *vault.Chat) bool { return c.Name() == q || c.ContactName == q },
	} {
		var hit []*vault.Chat
		for i := range chats {
			if exact(&chats[i]) {
				hit = append(hit, &chats[i])
			}
		}
		if len(hit) == 1 {
			return hit[0], nil
		}
	}
	var hit []*vault.Chat
	for i := range chats {
		if matches(&chats[i], q) {
			hit = append(hit, &chats[i])
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return nil, fmt.Errorf("no chat matches %q", q)
	}
	var names []string
	for _, c := range hit[:min(len(hit), 8)] {
		names = append(names, c.Name()+" ("+c.Chat+")")
	}
	return nil, fmt.Errorf("%d chats match %q: %s", len(hit), q, strings.Join(names, ", "))
}

func (app *App) newShowCommand() *cobra.Command {
	var month string
	cmd := &cobra.Command{
		Use:     "show <chat>",
		Short:   "Print a chat's messages (one month, the latest by default)",
		GroupID: groupBrowse,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			v, err := app.open(false)
			if err != nil {
				return err
			}
			defer v.Close()
			chats, err := v.Chats(ctx)
			if err != nil {
				return err
			}
			c, err := findChat(chats, args[0])
			if err != nil {
				return err
			}
			var from time.Time
			if month != "" {
				from, err = time.ParseInLocation("2006-01", month, time.Local)
				if err != nil {
					return fmt.Errorf("--month wants YYYY-MM: %w", err)
				}
			} else {
				from = vault.Month(c.Last)
			}
			names, err := v.Names(ctx, c.SourceID)
			if err != nil {
				return err
			}
			src, err := v.Source(ctx, c.SourceID)
			if err != nil {
				return err
			}
			return v.Entries(ctx, c.SourceID, c.Chat, from.Unix(), from.AddDate(0, 1, 0).Unix(), func(e *vault.Entry) error {
				_, err := fmt.Fprintln(app.Out, formatEntry(e, c, names, src.Self))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&month, "month", "", "month to show, YYYY-MM")
	return cmd
}

func formatEntry(e *vault.Entry, c *vault.Chat, names map[string]string, self string) string {
	who := e.Sender
	switch {
	case e.Sender == "":
		who = "?"
	case e.Sender == self:
		who = "我"
	case e.Sender == c.Chat:
		who = c.Name()
	case names[e.Sender] != "":
		who = names[e.Sender]
	}
	t := time.Unix(e.Time, 0).Format("01-02 15:04")
	text := strings.ReplaceAll(e.Text, "\n", " ⏎ ")
	if e.Kind == "system" || e.Kind == "recall" {
		return fmt.Sprintf("%s  · %s", t, text)
	}
	if e.Recalled {
		text += " [已撤回]"
	}
	return fmt.Sprintf("%s  %s: %s", t, who, text)
}

func (app *App) newChatCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "chat",
		Short:   "Name, file and hide chats (kept across syncs, used by exports)",
		GroupID: groupBrowse,
	}
	var alias, folder string
	var hide, show bool
	set := &cobra.Command{
		Use:   "set <chat>",
		Short: "Set a chat's alias, export folder, or visibility",
		Example: `  chatvault chat set 张三 --alias 老张 --folder friends/college
  chatvault chat set 某某公众号 --hide
  chatvault chat set 老张 --alias "" --folder ""   # back to defaults`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			v, err := app.open(false)
			if err != nil {
				return err
			}
			defer v.Close()
			chats, err := v.Chats(ctx)
			if err != nil {
				return err
			}
			c, err := findChat(chats, args[0])
			if err != nil {
				return err
			}
			var p vault.Pref
			if cmd.Flags().Changed("alias") {
				p.Alias = &alias
			}
			if cmd.Flags().Changed("folder") {
				f := strings.Trim(folder, "/")
				p.Folder = &f
			}
			if hide || show {
				p.Hidden = &hide
			}
			if p == (vault.Pref{}) {
				return errors.New("nothing to set: give --alias, --folder, --hide or --show")
			}
			if err := v.SetPref(ctx, c.SourceID, c.Chat, p); err != nil {
				return err
			}
			fmt.Fprintf(app.Out, "%s (%s) updated; `chatvault export` moves its files\n", c.Name(), c.Chat)
			return nil
		},
	}
	set.Flags().StringVar(&alias, "alias", "", "name to use instead of the contact's")
	set.Flags().StringVar(&folder, "folder", "", "folder in exports, e.g. family or work/team")
	set.Flags().BoolVar(&hide, "hide", false, "leave out of lists and exports")
	set.Flags().BoolVar(&show, "show", false, "undo --hide")
	set.MarkFlagsMutuallyExclusive("hide", "show")
	cmd.AddCommand(set)
	return cmd
}

func (app *App) newExportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Bring every export target up to date (or manage targets)",
		Long: `An export target is a folder the vault keeps current: one file per chat
and month, at <dir>/<folder>/<chat name>/<YYYY-MM>.<format>, with chats.json
listing the chats. Re-running rewrites only months with new messages, and
moves or removes files when chats are renamed, refiled or hidden.

A new vault starts with two targets, md and jsonl, under <vault>/export/.`,
		GroupID: groupSync,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.runExports(cmd.Context(), "")
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List export targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := app.open(false)
			if err != nil {
				return err
			}
			defer v.Close()
			ts, err := export.Targets(cmd.Context(), v)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(app.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tFORMAT\tUPDATED\tDIR")
			for _, t := range ts {
				up := "never"
				if t.Updated > 0 {
					up = time.Unix(t.Updated, 0).Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.Config.Format, up, t.Dir)
			}
			return tw.Flush()
		},
	}
	var cfg export.Config
	var dir string
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or change an export target",
		Example: `  chatvault export add groups-md --format md --kind group --dir ~/Documents/群聊
  chatvault export add everything --format jsonl --hidden`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := app.open(true)
			if err != nil {
				return err
			}
			defer v.Close()
			if strings.HasPrefix(dir, "~/") {
				home, _ := os.UserHomeDir()
				dir = home + dir[1:]
			}
			if err := export.Save(cmd.Context(), v, args[0], dir, cfg); err != nil {
				return err
			}
			fmt.Fprintf(app.Out, "export %s saved; `chatvault export` writes it\n", args[0])
			return nil
		},
	}
	add.Flags().StringVar(&cfg.Format, "format", "md", "file format: "+strings.Join(export.Formats, ", "))
	add.Flags().StringVar(&dir, "dir", "", "where to write (default <vault>/export/<name>)")
	add.Flags().BoolVar(&cfg.Hidden, "hidden", false, "include hidden chats")
	add.Flags().StringSliceVar(&cfg.Kinds, "kind", nil, "only these chat kinds: direct, group, official, openim")
	var files bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove an export target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := app.open(false)
			if err != nil {
				return err
			}
			defer v.Close()
			return export.Remove(cmd.Context(), v, args[0], files)
		},
	}
	rm.Flags().BoolVar(&files, "files", false, "also delete the files it wrote")
	run := &cobra.Command{
		Use:   "run [name]",
		Short: "Update one target, or all",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return app.runExports(cmd.Context(), name)
		},
	}
	cmd.AddCommand(list, add, rm, run)
	return cmd
}

func (app *App) runExports(ctx context.Context, name string) error {
	v, err := app.open(false)
	if err != nil {
		return err
	}
	defer v.Close()
	if _, err := v.Reparse(ctx, nil); err != nil {
		return err
	}
	ts, err := export.Targets(ctx, v)
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		return errors.New("no export targets; add one with `chatvault export add`")
	}
	p := newProgress(app.Err, app.StderrTTY)
	found := false
	for _, t := range ts {
		if name != "" && t.Name != name {
			continue
		}
		found = true
		r, err := export.Run(ctx, v, t, func(n string, done, total int) {
			p.event(pipeline.Event{Stage: pipeline.StageExport, Item: n, Done: done, Total: total})
		})
		p.clear()
		if err != nil {
			return fmt.Errorf("export %s: %w", t.Name, err)
		}
		printExports(app.Out, []export.Result{r})
	}
	if !found {
		return fmt.Errorf("no export named %q", name)
	}
	return nil
}

func (app *App) newKeysCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "keys",
		Short:   "Manage the database keys",
		GroupID: groupSetup,
	}
	var account string
	imp := &cobra.Command{
		Use:   "import [keys.json]",
		Short: "Store an account's database keys in the vault",
		Long: `WeChat 4 encrypts each database with a key. This command takes a
keys.json you prepared, checks it, tries it on the account's databases
(reading only), and copies it into the vault (mode 0600). Keys stay valid
until WeChat is reinstalled or the account logs in afresh; import new ones
then.

Without a path it looks for keys.json in the current directory, then in
~/Downloads, ~/Desktop, ~/Documents and ~.

Accepted forms: ` + wechat.KeyForms + `.`,
		Example: `  chatvault keys import ~/Downloads/keys.json
  chatvault keys import --account wxid_abc_1a2b keys.json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			from := ""
			if len(args) > 0 {
				from = args[0]
			} else {
				p, err := findKeysFile(keysCandidates())
				if err != nil {
					return err
				}
				from = p
				fmt.Fprintf(app.Out, "using %s\n", from)
			}
			keys, err := wechat.LoadKeys(from)
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s: no such file", from)
			}
			if err != nil {
				return err
			}
			acct, err := wechat.FindAccount(wechat.Root(), account)
			if err != nil {
				return err
			}
			opened, missing, err := wechat.KeyCoverage(acct, keys)
			if err != nil {
				return err
			}
			if len(opened) == 0 {
				return fmt.Errorf("none of the %d keys in %s opens a database of %s; they may belong to another account (--account) or be out of date; nothing imported", len(keys), from, acct.ID())
			}
			v, err := app.open(true)
			if err != nil {
				return err
			}
			defer v.Close()
			if _, err := pipeline.ImportKeys(v, acct, from); err != nil {
				return err
			}
			fmt.Fprintf(app.Out, "%d keys stored for %s; they open %d of %d databases\n", len(keys), acct.ID(), len(opened), len(opened)+len(missing))
			for _, m := range missing {
				fmt.Fprintf(app.Out, "  no key for %s (sync skips it)\n", m)
			}
			fmt.Fprintln(app.Out, "next: chatvault sync")
			return nil
		},
	}
	imp.Flags().StringVar(&account, "account", "", "WeChat account directory or username")
	cmd.AddCommand(app.newKeysExtractCommand(), imp)
	return cmd
}

func (app *App) newCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "check",
		Short:   "Check that WeChat, the keys and the vault are in order, and say what to fix",
		GroupID: groupSetup,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ok := true
			// say prints a check; a failed one is followed by what to do.
			say := func(good bool, fix, format string, a ...any) {
				mark := "ok  "
				if !good {
					mark, ok = "FAIL", false
				}
				fmt.Fprintf(app.Out, "%s  %s\n", mark, fmt.Sprintf(format, a...))
				if !good && fix != "" {
					fmt.Fprintf(app.Out, "      → %s\n", fix)
				}
			}
			accts, err := wechat.Accounts(wechat.Root())
			if err != nil {
				say(false, "", "%v", err)
			} else {
				say(true, "", "WeChat 4.x accounts under %s: %d", wechat.Root(), len(accts))
			}
			for _, a := range accts {
				err := a.CheckLayout()
				say(err == nil, "log in to this account in WeChat 4.x and let it load your chats", "account %s: %v", a.ID(), errOrOK(err))
			}
			if !vault.Exists(app.VaultDir) {
				fmt.Fprintf(app.Out, "      vault %s does not exist yet; `keys import` or `sync` creates it\n", app.VaultDir)
			} else {
				v, err := vault.Open(app.VaultDir)
				say(err == nil, "", "vault %s (schema %d): %v", app.VaultDir, vault.SchemaVersion(), errOrOK(err))
				if err == nil {
					for _, a := range accts {
						app.checkKeys(say, v, a)
					}
					v.Close()
				}
			}
			if !ok {
				return errors.New("some checks failed; fix the first → above and run `chatvault check` again")
			}
			if h := app.diagnose(cmd.Context()).hint(); h != "" {
				fmt.Fprintf(app.Out, "next: %s\n", h)
			}
			return nil
		},
	}
}

// checkKeys reports whether an account's keys are imported and which of its
// databases they open.
func (app *App) checkKeys(say func(bool, string, string, ...any), v *vault.Vault, a wechat.Account) {
	keys, err := wechat.LoadKeys(pipeline.KeysPath(v, a))
	if errors.Is(err, os.ErrNotExist) {
		say(false, "prepare a keys.json and run `chatvault keys import keys.json`", "keys for %s: not imported", a.ID())
		return
	}
	if err != nil {
		say(false, "import a fresh keys.json with `chatvault keys import`", "keys for %s: %v", a.ID(), err)
		return
	}
	opened, missing, err := wechat.KeyCoverage(a, keys)
	if err != nil {
		say(false, "", "keys for %s: %v", a.ID(), err)
		return
	}
	none := ""
	if len(missing) > 0 {
		none = "; no key for " + strings.Join(missing, ", ")
	}
	say(len(missing) == 0, "sync skips these; if they matter, import a fresh keys.json with `chatvault keys import`",
		"keys for %s: %d keys open %d of %d databases%s", a.ID(), len(keys), len(opened), len(opened)+len(missing), none)
}

func errOrOK(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}
