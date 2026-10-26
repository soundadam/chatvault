// Package cli is chatvault's command line.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/charmbracelet/fang"
	appkit "github.com/soundadam/appkit/go"
	appcli "github.com/soundadam/appkit/go/cli"
	"github.com/spf13/cobra"

	"github.com/soundadam/chatvault/internal/vault"
)

// Help groups shown by `chatvault --help`.
const (
	groupSync   = "sync"
	groupBrowse = "browse"
	groupSetup  = "setup"
)

// App holds the process-wide dependencies of a chatvault invocation.
type App struct {
	In        io.Reader
	Out       io.Writer
	Err       io.Writer
	StdinTTY  bool
	StdoutTTY bool
	StderrTTY bool
	Identity  appkit.App

	// VaultDir is set from --vault, else the default.
	VaultDir string
}

// Execute parses args and runs the selected command, returning the process
// exit code.
func (app *App) Execute(ctx context.Context, args []string) int {
	root := app.newRootCommand()
	root.SetArgs(args)
	root.SetIn(app.In)
	root.SetOut(app.Out)
	root.SetErr(app.Err)
	if err := fang.Execute(ctx, root, fang.WithoutVersion(), fang.WithErrorHandler(errorHandler)); err != nil {
		return 1
	}
	return 0
}

// errorHandler is fang's, without title-casing the message: errors carry
// paths and chat names.
func errorHandler(w io.Writer, s fang.Styles, err error) {
	s.ErrorText = s.ErrorText.UnsetTransform()
	fang.DefaultErrorHandler(w, s, err)
}

func (app *App) newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "chatvault",
		Short: "Keep every WeChat message in one local vault, exported as files that stay current",
		Long: `chatvault copies WeChat's databases, decrypts them, and merges every message
into one SQLite vault that only ever grows: a message WeChat later recalls or
drops stays in the vault. From there it keeps export folders of Markdown or
JSON Lines up to date, one file per chat and month, rewriting only what
changed since the last sync.

Run it without arguments on a terminal to browse and sync interactively;
piped, it prints the vault's status. The vault is ~/Documents/ChatVault unless
--vault or $CHATVAULT_HOME says otherwise.`,
		Example: `  # first time: see what is missing and what to do about it
  chatvault check

  # once: hand over the database keys (see README for how to get them)
  chatvault keys import keys.json

  # copy, decrypt, merge, parse and export what is new
  chatvault sync

  # sort chats into folders for the exports
  chatvault chat set 家人群 --folder family
  chatvault export`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if app.VaultDir == "" {
				d, err := vault.DefaultDir()
				if err != nil {
					return err
				}
				app.VaultDir = d
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app.StdinTTY && app.StdoutTTY {
				return app.runUI(cmd.Context())
			}
			return app.status(cmd.Context())
		},
	}
	root.PersistentFlags().StringVar(&app.VaultDir, "vault", "", "vault directory (default $CHATVAULT_HOME or ~/Documents/ChatVault)")
	root.AddGroup(
		&cobra.Group{ID: groupSync, Title: "sync"},
		&cobra.Group{ID: groupBrowse, Title: "browse and organise"},
		&cobra.Group{ID: groupSetup, Title: "setup"},
	)
	root.AddCommand(
		app.newSyncCommand(),
		app.newExportCommand(),
		app.newStatusCommand(),
		app.newUICommand(),
		app.newChatsCommand(),
		app.newShowCommand(),
		app.newChatCommand(),
		app.newKeysCommand(),
		app.newCheckCommand(),
	)
	appcli.AddCommands(root, app.Identity)
	for _, command := range root.Commands() {
		switch command.Name() {
		case "update", "version":
			command.GroupID = groupSetup
		}
	}
	root.SetHelpCommandGroupID(groupSetup)
	root.SetCompletionCommandGroupID(groupSetup)
	return root
}

// open opens the vault; with create false, a missing vault is an error.
func (app *App) open(create bool) (*vault.Vault, error) {
	if !create && !vault.Exists(app.VaultDir) {
		err := fmt.Errorf("%s: %w", app.VaultDir, vault.ErrNoVault)
		if h := app.diagnose(context.Background()).hint(); h != "" {
			err = fmt.Errorf("%w\nnext: %s", err, h)
		}
		return nil, err
	}
	return vault.Open(app.VaultDir)
}
