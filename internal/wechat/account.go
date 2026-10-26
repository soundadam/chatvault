// Package wechat reads the data of WeChat 4.x for macOS: it finds the
// account directory, copies and decrypts its databases, and reads contacts
// and messages out of the plain copies.
package wechat

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// Root is where WeChat 4.x keeps one directory per logged-in account.
func Root() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files")
}

// Account is one account directory, xwechat_files/<wxid>_<suffix>.
type Account struct {
	Dir string
}

// ID is the directory name, the account's identity in the vault.
func (a Account) ID() string { return filepath.Base(a.Dir) }

// DBDir holds the encrypted databases.
func (a Account) DBDir() string { return filepath.Join(a.Dir, "db_storage") }

// MediaDir holds images (.dat), files and videos.
func (a Account) MediaDir() string { return filepath.Join(a.Dir, "msg") }

var dirSuffix = regexp.MustCompile(`_[0-9a-f]{4}$`)

// Username is the account's own WeChat username: the directory name less
// the 4-hex suffix WeChat appends.
func (a Account) Username() string { return dirSuffix.ReplaceAllString(a.ID(), "") }

// Accounts lists the account directories under root that have databases.
func Accounts(root string) ([]Account, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if st, err := os.Stat(filepath.Join(dir, "db_storage")); err == nil && st.IsDir() {
			out = append(out, Account{Dir: dir})
		}
	}
	return out, nil
}

// FindAccount resolves name (a directory name, a path, or "" when there is
// exactly one account).
func FindAccount(root, name string) (Account, error) {
	if name != "" && filepath.IsAbs(name) {
		return Account{Dir: name}, nil
	}
	all, err := Accounts(root)
	if err != nil {
		return Account{}, fmt.Errorf("no WeChat data at %s: %w", root, err)
	}
	if name != "" {
		for _, a := range all {
			if a.ID() == name || a.Username() == name {
				return a, nil
			}
		}
		return Account{}, fmt.Errorf("no account %q under %s", name, root)
	}
	if len(all) != 1 {
		return Account{}, fmt.Errorf("found %d accounts under %s; pick one with --account", len(all), root)
	}
	return all[0], nil
}

// LoadKeys reads the candidate keys from a keys file: either
// {"keys": ["<hex>", ...]} or the output of wechat-key-macos,
// {"message/message_0.db": {"enc_key": "<hex>"}, ...}.
func LoadKeys(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plain struct {
		Keys []string `json:"keys"`
	}
	var hexes []string
	if json.Unmarshal(data, &plain) == nil && plain.Keys != nil {
		hexes = plain.Keys
	} else {
		var perDB map[string]json.RawMessage
		if err := json.Unmarshal(data, &perDB); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, raw := range perDB {
			var v struct {
				EncKey string `json:"enc_key"`
			}
			if json.Unmarshal(raw, &v) == nil && v.EncKey != "" {
				hexes = append(hexes, v.EncKey)
			}
		}
	}
	slices.Sort(hexes)
	hexes = slices.Compact(hexes)
	keys := make([][]byte, 0, len(hexes))
	for _, h := range hexes {
		k, err := hex.DecodeString(h)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s: a key is not 32 bytes of hex", path)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New(path + ": no keys")
	}
	return keys, nil
}
