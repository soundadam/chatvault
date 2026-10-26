package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	appkit "github.com/soundadam/appkit/go"
	"github.com/soundadam/appkit/go/tuiv2"

	"github.com/soundadam/chatvault/internal/vault"
)

func testModel(t *testing.T) *uiModel {
	t.Helper()
	ctx := context.Background()
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	src, err := v.EnsureSource(ctx, "wechat", "acct", "me")
	if err != nil {
		t.Fatal(err)
	}
	run, err := v.BeginRun(ctx, src, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local).Unix()
	for _, chat := range []string{"alice", "bob"} {
		msgs := []vault.Message{{ServerID: ts + int64(len(chat)) + int64(chat[0]), LocalType: 1, Time: ts, Sender: chat, Content: []byte("hi from " + chat)}}
		if err := run.Messages(ctx, chat, msgs); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Reparse(ctx, nil); err != nil {
		t.Fatal(err)
	}
	app := &App{Identity: appkit.App{Name: "chatvault", Version: "dev"}}
	m := &uiModel{app: app, ctx: ctx, v: v, updater: tuiv2.New(app.Identity)}
	m.filter = textinput.New()
	m.input = textinput.New()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.Update(m.load()())
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func typeText(m *uiModel, s string) {
	for _, r := range s {
		m.Update(key(string(r)))
	}
}

func view(m *uiModel) string { return m.View().Content }

func TestUIListFilterAndOpen(t *testing.T) {
	m := testModel(t)
	if got := view(m); !strings.Contains(got, "2 messages · 2 chats") || !strings.Contains(got, "alice") {
		t.Fatalf("list view:\n%s", got)
	}

	m.Update(key("/"))
	typeText(m, "bob")
	m.Update(key("enter"))
	if len(m.shown) != 1 || m.selected().Chat != "bob" {
		t.Fatalf("filter shown = %v", m.shown)
	}

	_, cmd := m.Update(key("enter"))
	if m.mode != modeChat || cmd == nil {
		t.Fatalf("enter did not open the chat")
	}
	for _, msg := range drain(cmd) {
		m.Update(msg)
	}
	if got := view(m); !strings.Contains(got, "hi from bob") || !strings.Contains(got, "2026-10") {
		t.Fatalf("chat view:\n%s", got)
	}
	m.Update(key("esc"))
	if m.mode != modeList {
		t.Fatalf("esc did not go back")
	}
}

func TestUIFolderAndHide(t *testing.T) {
	m := testModel(t)
	m.Update(key("f"))
	if m.mode != modeEdit {
		t.Fatal("f did not open the folder prompt")
	}
	typeText(m, "friends/")
	_, cmd := m.Update(key("enter"))
	m.Update(cmd())
	c := m.selected()
	if c == nil || c.Folder != "friends" {
		t.Fatalf("folder not saved: %+v", c)
	}
	if !strings.Contains(view(m), "friends/") {
		t.Fatalf("folder not shown:\n%s", view(m))
	}

	_, cmd = m.Update(key("h"))
	m.Update(cmd())
	if len(m.shown) != 1 {
		t.Fatalf("hidden chat still shown: %d", len(m.shown))
	}
	m.Update(key("H"))
	if len(m.shown) != 2 || !strings.Contains(view(m), "(hidden)") {
		t.Fatalf("H did not show hidden chats:\n%s", view(m))
	}
}

// drain runs cmd and any batch inside it, returning the messages that are
// not the updater's own.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, drain(c)...)
		}
		return out
	}
	if _, ok := msg.(monthMsg); ok {
		return []tea.Msg{msg}
	}
	return nil
}
