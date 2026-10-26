package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsExplainsMissingData(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Data/Documents/xwechat_files")

	_, err := Accounts(root)
	if !errors.Is(err, ErrNoWeChat) || !strings.Contains(err.Error(), "install WeChat for Mac 4") {
		t.Fatalf("no data: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(base, "Data/Library/Application Support/com.tencent.xinWeChat"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Accounts(root)
	if !errors.Is(err, ErrNoWeChat) || !strings.Contains(err.Error(), "WeChat 3.x") {
		t.Fatalf("3.x data: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, "all_users"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = Accounts(root); !errors.Is(err, ErrNoWeChat) {
		t.Fatalf("root without accounts: %v", err)
	}
}

func TestCheckLayout(t *testing.T) {
	a := Account{Dir: filepath.Join(t.TempDir(), "wxid_me_1a2b")}
	touch(t, filepath.Join(a.DBDir(), "contact", "contact.db"))
	if err := a.CheckLayout(); err == nil || !strings.Contains(err.Error(), "WeChat 4.x") {
		t.Fatalf("no message shards: %v", err)
	}
	touch(t, filepath.Join(a.DBDir(), "message", "message_0.db"))
	if err := a.CheckLayout(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysErrors(t *testing.T) {
	dir := t.TempDir()
	k := strings.Repeat("ab", 32)
	for _, tc := range []struct{ body, want string }{
		{`not json`, "is not JSON"},
		{`[1, 2]`, "not a keys file"},
		{`{}`, "no keys in it"},
		{`{"keys": ["abc"]}`, "key for keys[0] is not 64 hex"},
		{`{"message/message_0.db": {"enc_key": "zz"}}`, "key for message/message_0.db is not 64 hex"},
	} {
		p := filepath.Join(dir, "keys.json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeys(p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.body, err, tc.want)
		}
	}

	p := filepath.Join(dir, "keys.json")
	body := `{"a.db": {"enc_key": "` + k + `"}, "b.db": {"enc_key": " 0x` + strings.ToUpper(k) + `"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(p)
	if err != nil || len(keys) != 1 {
		t.Fatalf("duplicate keys: %d, %v", len(keys), err)
	}
}
