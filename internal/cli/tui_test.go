package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appkit "github.com/soundadam/appkit/go"
	"github.com/soundadam/appkit/go/tuiv2"

	"github.com/soundadam/chatvault/internal/export"
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

func TestUIShowsExportFile(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	if err := export.Save(ctx, m.v, "md", "", export.Config{Format: "md"}); err != nil {
		t.Fatal(err)
	}
	ts, err := export.Targets(ctx, m.v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := export.Run(ctx, m.v, ts[0], nil); err != nil {
		t.Fatal(err)
	}
	var revealed string
	old := revealPath
	revealPath = func(p string) error { revealed = p; return nil }
	t.Cleanup(func() { revealPath = old })

	want := filepath.Join(m.v.Dir, "export", "md", "alice", "2026-10.md")
	_, cmd := m.Update(key("o"))
	m.Update(cmd())
	if revealed != want || !strings.Contains(view(m), "shown: ") {
		t.Fatalf("o in the list revealed %q, want %q:\n%s", revealed, want, view(m))
	}

	_, cmd = m.Update(key("enter"))
	for _, msg := range drain(cmd) {
		m.Update(msg)
	}
	if got := view(m); !strings.Contains(got, filepath.Join("alice", "2026-10.md")) {
		t.Fatalf("chat view has no export path:\n%s", got)
	}
	revealed = ""
	_, cmd = m.Update(key("o"))
	m.Update(cmd())
	if revealed != want {
		t.Fatalf("o in the chat revealed %q, want %q", revealed, want)
	}
}

func TestUIChatFillsAndScrollsAcrossMonths(t *testing.T) {
	m := testModel(t)
	ctx := context.Background()
	src, err := m.v.EnsureSource(ctx, "wechat", "acct", "me")
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.v.BeginRun(ctx, src, "test")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []vault.Message
	sep := time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local).Unix()
	for i := range 40 {
		msgs = append(msgs, vault.Message{ServerID: 1000 + int64(i), LocalType: 1, Time: sep + int64(i)*60, Sender: "alice",
			Content: []byte(fmt.Sprintf("september %d", i))})
	}
	msgs = append(msgs, vault.Message{ServerID: 2000, LocalType: 1, Time: sep + 86400, Sender: "alice",
		Content: []byte(strings.Repeat("长", 100))})
	if err := run.Messages(ctx, "alice", msgs); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.v.Reparse(ctx, nil); err != nil {
		t.Fatal(err)
	}
	m.Update(m.load()())
	m.applyFilter()
	for m.selected().Chat != "alice" {
		m.Update(key("j"))
	}
	open := func(cmd tea.Cmd) {
		for _, msg := range drain(cmd) {
			m.Update(msg)
		}
	}
	_, cmd := m.Update(key("enter"))
	open(cmd)
	if m.months[m.month].Month.Month() != time.October {
		t.Fatalf("opened %v", m.months[m.month].Month)
	}

	// Up at the top of October goes on into the end of September.
	_, cmd = m.Update(key("k"))
	open(cmd)
	if m.months[m.month].Month.Month() != time.September || m.scroll != m.maxScroll() {
		t.Fatalf("after up: month %v scroll %d/%d", m.months[m.month].Month, m.scroll, m.maxScroll())
	}
	got := strings.Split(strings.TrimSuffix(view(m), "\n"), "\n")
	body := got[2 : 2+m.bodyRows()]
	for i, l := range body {
		if strings.TrimSpace(ansi.Strip(l)) == "" && i > 0 && i < len(body)-1 && !strings.Contains(body[i+1], "──") {
			t.Fatalf("blank row %d in a full month:\n%s", i, view(m))
		}
		if ansi.StringWidth(l) > 80 {
			t.Fatalf("row %d wider than the screen: %q", i, l)
		}
	}
	if !strings.Contains(view(m), "长长长") || strings.Contains(view(m), "长…") {
		t.Fatalf("long message not wrapped:\n%s", view(m))
	}

	// Down at the end of September comes back to the start of October.
	_, cmd = m.Update(key("j"))
	open(cmd)
	if m.months[m.month].Month.Month() != time.October || m.scroll != 0 {
		t.Fatalf("after down: month %v scroll %d", m.months[m.month].Month, m.scroll)
	}
}
