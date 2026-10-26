package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/soundadam/chatvault/internal/pipeline"
	"github.com/soundadam/chatvault/internal/vault"
	"github.com/soundadam/chatvault/internal/wechat"
)

// step is the next thing to do before the vault holds messages.
type step int

const (
	stepNone   step = iota // set up and synced
	stepWeChat             // no WeChat 4.x account to read
	stepKeys               // keys not imported
	stepSync               // ready, never synced
)

// setup is how far along the way to a first sync this machine is.
type setup struct {
	step step
	// why explains stepWeChat: the error from finding accounts.
	why error
}

// diagnose looks at WeChat, the vault and the imported keys, reading only.
func (app *App) diagnose(ctx context.Context) setup {
	if !vault.Exists(app.VaultDir) {
		return diagnose(ctx, nil)
	}
	v, err := vault.Open(app.VaultDir)
	if err != nil {
		return setup{}
	}
	defer v.Close()
	return diagnose(ctx, v)
}

// diagnose is app.diagnose on an open vault, or none (nil).
func diagnose(ctx context.Context, v *vault.Vault) setup {
	accts, err := wechat.Accounts(wechat.Root())
	if err != nil {
		return setup{step: stepWeChat, why: err}
	}
	if v == nil {
		return setup{step: stepKeys}
	}
	keyed := false
	for _, a := range accts {
		if _, err := os.Stat(pipeline.KeysPath(v, a)); err == nil {
			keyed = true
		}
	}
	if !keyed {
		return setup{step: stepKeys}
	}
	if s, err := v.Stats(ctx); err == nil && s.Runs == 0 {
		return setup{step: stepSync}
	}
	return setup{}
}

// hint is the next step as a line for the command line, or "".
func (s setup) hint() string {
	switch s.step {
	case stepWeChat:
		return fmt.Sprintf("%v; then run `chatvault check`", s.why)
	case stepKeys:
		return "import the database keys: `chatvault keys import keys.json`, then `chatvault sync`"
	case stepSync:
		return "keys are in; run `chatvault sync` to copy, decrypt and export your messages"
	}
	return ""
}

// uiHint is the next step as the interactive screen puts it.
func (s setup) uiHint() string {
	if s.step == stepSync {
		return "Keys are in. Press s to sync."
	}
	if h := s.hint(); h != "" {
		return "Next: " + h + "."
	}
	return "Press s to sync."
}

// keysCandidates are where `keys import` looks when given no file.
func keysCandidates() []string {
	home, _ := os.UserHomeDir()
	out := []string{"keys.json"}
	for _, d := range []string{"Downloads", "Desktop", "Documents", ""} {
		out = append(out, filepath.Join(home, d, "keys.json"))
	}
	return out
}

// findKeysFile returns the first keys.json among the candidates.
func findKeysFile(candidates []string) (string, error) {
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no keys.json in %s; give its path: chatvault keys import <file>", strings.Join(candidates, ", "))
}
