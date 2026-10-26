package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appkit "github.com/soundadam/appkit/go"
	"github.com/soundadam/appkit/go/tuiv2"

	"github.com/soundadam/chatvault/internal/vault"
)

func TestSetupHints(t *testing.T) {
	for _, tc := range []struct {
		s        setup
		cli, tui string
	}{
		{setup{step: stepWeChat, why: errors.New("no WeChat 4.x data at /x")}, "no WeChat 4.x data at /x; then run `chatvault check`", "Next: no WeChat"},
		{setup{step: stepKeys}, "chatvault keys import keys.json", "Next: import the database keys"},
		{setup{step: stepSync}, "run `chatvault sync`", "Press s to sync"},
		{setup{}, "", "Press s to sync"},
	} {
		if got := tc.s.hint(); tc.cli == "" && got != "" || !strings.Contains(got, tc.cli) {
			t.Errorf("step %d hint = %q, want %q", tc.s.step, got, tc.cli)
		}
		if got := tc.s.uiHint(); !strings.Contains(got, tc.tui) {
			t.Errorf("step %d uiHint = %q, want %q", tc.s.step, got, tc.tui)
		}
	}
}

func TestFindKeysFile(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a", "keys.json"), filepath.Join(dir, "b", "keys.json")
	if _, err := findKeysFile([]string{a, b}); err == nil || !strings.Contains(err.Error(), a+", "+b) {
		t.Fatalf("none found: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := findKeysFile([]string{a, b}); err != nil || got != b {
		t.Fatalf("found %q, %v", got, err)
	}
}

func TestUIEmptyVaultSaysWhatToDo(t *testing.T) {
	ctx := context.Background()
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	app := &App{Identity: appkit.App{Name: "chatvault", Version: "dev"}}
	m := &uiModel{app: app, ctx: ctx, v: v, updater: tuiv2.New(app.Identity)}
	m.filter = textinput.New()
	m.input = textinput.New()
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m.Update(loadedMsg{setup: setup{step: stepKeys}})
	got := view(m)
	if !strings.Contains(got, "The vault is empty.") || !strings.Contains(got, "chatvault keys import") {
		t.Fatalf("empty view:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(l); w > 60 {
			t.Fatalf("line wider than the screen (%d): %q", w, l)
		}
	}

	m.Update(loadedMsg{setup: setup{step: stepSync}})
	if got := view(m); !strings.Contains(got, "Press s to sync") {
		t.Fatalf("ready view:\n%s", got)
	}
}
