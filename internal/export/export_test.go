package export

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soundadam/chatvault/internal/vault"
)

func at(s string) int64 {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

func sync(t *testing.T, v *vault.Vault, src vault.Source, chat string, msgs ...vault.Message) {
	t.Helper()
	ctx := context.Background()
	run, err := v.BeginRun(ctx, src, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Messages(ctx, chat, msgs); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Reparse(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func exportAll(t *testing.T, v *vault.Vault) Result {
	t.Helper()
	ts, err := Targets(context.Background(), v)
	if err != nil || len(ts) != 1 {
		t.Fatalf("targets: %v %v", ts, err)
	}
	r, err := Run(context.Background(), v, ts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIncrementalExport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	src, err := v.EnsureSource(ctx, "wechat", "acct", "me")
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, v, "md", "", Config{Format: "md"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "export", "md")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(out, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	sync(t, v, src, "bob",
		vault.Message{ServerID: 1, LocalType: 1, Time: at("2026-09-30 10:00"), Sender: "bob", Content: []byte("hi")},
		vault.Message{ServerID: 2, LocalType: 1, Time: at("2026-10-01 09:00"), Sender: "me", Content: []byte("hello")},
	)
	r := exportAll(t, v)
	if !r.Full || r.Written != 2 {
		t.Fatalf("first export = %+v", r)
	}
	if got := read("bob/2026-10.md"); !strings.Contains(got, "**我**: hello") {
		t.Fatalf("2026-10.md:\n%s", got)
	}

	// Nothing new: nothing written.
	if r := exportAll(t, v); r.Written != 0 || r.Removed != 0 {
		t.Fatalf("idle export = %+v", r)
	}

	// A new message rewrites only its month; a recall marks the original.
	sync(t, v, src, "bob",
		vault.Message{ServerID: 1, LocalType: 10000, Time: at("2026-09-30 10:00"), Content: []byte(`<sysmsg type="revokemsg"><revokemsg><content>"bob" 撤回了一条消息</content></revokemsg></sysmsg>`)},
		vault.Message{ServerID: 3, LocalType: 1, Time: at("2026-10-02 09:00"), Sender: "bob", Content: []byte("again")},
	)
	if r := exportAll(t, v); r.Full || r.Written != 2 {
		t.Fatalf("incremental export = %+v", r)
	}
	if got := read("bob/2026-09.md"); !strings.Contains(got, "已撤回") || !strings.Contains(got, "hi") {
		t.Fatalf("recalled message lost:\n%s", got)
	}

	// Refiling moves the chat's files and removes the old ones.
	alias, folder := "Bobby", "friends"
	if err := v.SetPref(ctx, src.ID, "bob", vault.Pref{Alias: &alias, Folder: &folder}); err != nil {
		t.Fatal(err)
	}
	if r := exportAll(t, v); r.Written != 2 || r.Removed != 2 {
		t.Fatalf("refile export = %+v", r)
	}
	read("friends/Bobby/2026-10.md")
	if _, err := os.Stat(filepath.Join(out, "bob")); !os.IsNotExist(err) {
		t.Fatalf("old folder still there: %v", err)
	}

	// Hiding removes it from the export.
	hide := true
	if err := v.SetPref(ctx, src.ID, "bob", vault.Pref{Hidden: &hide}); err != nil {
		t.Fatal(err)
	}
	if r := exportAll(t, v); r.Removed != 2 {
		t.Fatalf("hide export = %+v", r)
	}
}

func TestChatPathsUnique(t *testing.T) {
	chats := []vault.Chat{
		{SourceID: 1, Chat: "a", ContactName: "Sam"},
		{SourceID: 1, Chat: "b", ContactName: "sam"},
		{SourceID: 1, Chat: "c", ContactName: "a/b:c"},
	}
	p := chatPaths(chats)
	if p[key(1, "a")] == p[key(1, "b")] {
		t.Fatalf("clash: %v", p)
	}
	if got := p[key(1, "c")]; got != "a_b_c" {
		t.Fatalf("clean = %q", got)
	}
}

func TestVaultMoves(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	v, err := vault.Open(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, v, "md", "", Config{Format: "md"}); err != nil {
		t.Fatal(err)
	}
	v.Close()
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	v, err = vault.Open(filepath.Join(root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ts, err := Targets(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(v.Dir, "export", "md"); ts[0].Dir != want {
		t.Fatalf("dir = %q, want %q", ts[0].Dir, want)
	}
}

func TestFormats(t *testing.T) {
	ctx := context.Background()
	for _, f := range Formats {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()
			v, err := vault.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			src, err := v.EnsureSource(ctx, "wechat", "acct", "me")
			if err != nil {
				t.Fatal(err)
			}
			if err := Save(ctx, v, f, "", Config{Format: f}); err != nil {
				t.Fatal(err)
			}
			sync(t, v, src, "bob",
				vault.Message{ServerID: 1, LocalType: 1, Time: at("2026-10-01 10:00"), Sender: "bob", Content: []byte("hi\nthere")},
				vault.Message{ServerID: 2, LocalType: 1, Time: at("2026-10-01 10:01"), Sender: "me", Content: []byte("hello")},
			)
			exportAll(t, v)
			b, err := os.ReadFile(filepath.Join(dir, "export", f, "bob", "2026-10."+f))
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			switch f {
			case "json":
				var msgs []line
				if err := json.Unmarshal(b, &msgs); err != nil || len(msgs) != 2 || msgs[0].Text != "hi\nthere" || !msgs[1].FromMe {
					t.Fatalf("json = %v %+v\n%s", err, msgs, got)
				}
			case "jsonl":
				if n := strings.Count(got, "\n"); n != 2 {
					t.Fatalf("jsonl has %d lines:\n%s", n, got)
				}
			case "txt":
				if !strings.Contains(got, "10:00 bob: hi\n  there\n") || !strings.Contains(got, "10:01 我: hello") || strings.ContainsAny(got, "#*") {
					t.Fatalf("txt:\n%s", got)
				}
			case "md":
				if !strings.Contains(got, "- 10:01 **我**: hello") {
					t.Fatalf("md:\n%s", got)
				}
			}
		})
	}
}
