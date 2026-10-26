package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/soundadam/chatvault/internal/pipeline"
	"github.com/soundadam/chatvault/internal/wechat"
	"github.com/soundadam/chatvault/internal/wechat/keyextract"
)

func (app *App) newKeysExtractCommand() *cobra.Command {
	var (
		account string
		wait    int
		resign  bool
	)
	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Read the account's database keys on this Mac",
		Long: `Reads the account's database keys and stores them in the vault. macOS only;
run it in a terminal. The first run prepares a private helper environment.`,
		Example: `  chatvault keys extract`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.extractKeys(cmd.Context(), account, wait, resign)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "WeChat account directory or username")
	cmd.Flags().IntVar(&wait, "wait", 180, "seconds to keep reading")
	cmd.Flags().BoolVar(&resign, "resign", false, "prepare the app without asking first (quit and reopen it afterwards)")
	return cmd
}

// extractKeys reads the account's database keys, then imports them the way
// `keys import` does.
func (app *App) extractKeys(ctx context.Context, account string, wait int, resign bool) error {
	if runtime.GOOS != "darwin" {
		return errors.New("`keys extract` works on macOS only")
	}
	if wait < 10 || wait > 900 {
		return errors.New("--wait must be between 10 and 900 seconds")
	}
	if !app.StdinTTY {
		return errors.New("`keys extract` needs a terminal; run it in Terminal")
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return errors.New("sudo not found")
	}
	acct, err := wechat.FindAccount(wechat.Root(), account)
	if err != nil {
		return err
	}
	if err := acct.CheckLayout(); err != nil {
		return err
	}
	pid, err := keyextract.PID()
	if err != nil {
		return fmt.Errorf("%w; open WeChat, log in, and run this again", err)
	}
	appPath, err := keyextract.App()
	if err != nil {
		return err
	}
	debuggable, err := keyextract.Debuggable(ctx, appPath)
	if err != nil {
		return err
	}
	if !debuggable {
		fmt.Fprintln(app.Out, "WeChat needs to be prepared before chatvault can read its databases.")
		fmt.Fprintf(app.Out, "chatvault can prepare %s now. That changes the app; reinstalling WeChat\n", appPath)
		fmt.Fprintln(app.Out, "restores Apple's signature. WeChat has to be quit and reopened afterwards.")
		if !resign && !app.confirm("Prepare WeChat now?") {
			return errors.New("nothing changed; run `chatvault keys extract` again when you are ready")
		}
		if err := keyextract.Resign(ctx, appPath, app.In, app.Out, app.Err); err != nil {
			return err
		}
		fmt.Fprintln(app.Out, "Done. Quit WeChat completely (⌘Q in WeChat), open it again and log in.")
		for {
			if !app.confirm("Is WeChat open and logged in again?") {
				return errors.New("run `chatvault keys extract` again once WeChat is open again")
			}
			again, err := keyextract.PID()
			if err != nil {
				fmt.Fprintln(app.Out, "WeChat is not running yet.")
				continue
			}
			if again == pid {
				fmt.Fprintln(app.Out, "That is still the old process; quit WeChat completely and open it again.")
				continue
			}
			break
		}
	}

	python, err := keyextract.Python(ctx)
	if err != nil {
		return err
	}
	env, err := keyextract.Prepare(ctx, python, func(format string, a ...any) {
		fmt.Fprintf(app.Out, format+"\n", a...)
	})
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "chatvault-keys-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	keysFile := filepath.Join(tmp, "keys.json")

	fmt.Fprintf(app.Out, "Reading for up to %s. In WeChat, open several chats, Contacts, Favorites and Search\n", (time.Duration(wait) * time.Second).String())
	fmt.Fprintln(app.Out, "so that more databases are read. sudo asks for your password now.")
	if err := keyextract.Capture(ctx, env, acct.DBDir(), keysFile, wait, app.In, app.Out, app.Err); err != nil {
		return fmt.Errorf("%w\ncheck that WeChat stays open, that the terminal has Full Disk Access, and that WeChat was opened after it was last prepared", err)
	}

	keys, err := wechat.LoadKeys(keysFile)
	if err != nil {
		return fmt.Errorf("the extractor's output: %w", err)
	}
	opened, missing, err := wechat.KeyCoverage(acct, keys)
	if err != nil {
		return err
	}
	if len(opened) == 0 {
		return fmt.Errorf("none of the %d keys opens a database of %s; nothing imported (are they from this account?)", len(keys), acct.ID())
	}

	v, err := app.open(true)
	if err != nil {
		return err
	}
	defer v.Close()
	merged := keys
	switch old, err := wechat.LoadKeys(pipeline.KeysPath(v, acct)); {
	case err == nil:
		merged = wechat.MergeKeys(old, keys)
	case !errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(app.Err, "existing keys ignored: %v\n", err)
	}
	if len(merged) != len(keys) {
		mergedFile := filepath.Join(tmp, "merged.json")
		if err := wechat.SaveKeys(mergedFile, merged); err != nil {
			return err
		}
		keysFile = mergedFile
		if opened, missing, err = wechat.KeyCoverage(acct, merged); err != nil {
			return err
		}
	}
	if _, err := pipeline.ImportKeys(v, acct, keysFile); err != nil {
		return err
	}
	fmt.Fprintf(app.Out, "%d keys stored for %s; they open %d of %d databases\n", len(merged), acct.ID(), len(opened), len(opened)+len(missing))
	for _, m := range missing {
		fmt.Fprintf(app.Out, "  no key for %s (open that feature in WeChat and run this again)\n", m)
	}
	fmt.Fprintln(app.Out, "next: chatvault sync")
	return nil
}

// confirm asks a yes/no question on the terminal; an empty answer is no.
func (app *App) confirm(question string) bool {
	fmt.Fprintf(app.Out, "%s [y/N] ", question)
	line, _ := bufio.NewReader(app.In).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
