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
	"strings"
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

// legacyDir is where WeChat 3.x for macOS kept its data, relative to Root.
const legacyDir = "../../Library/Application Support/com.tencent.xinWeChat"

// ErrNoWeChat means there is no WeChat 4.x data under the root.
var ErrNoWeChat = errors.New("no WeChat 4.x data")

// noData explains why root holds no account: WeChat is not installed or
// never logged in, or only WeChat 3.x data is there, which chatvault does
// not read.
func noData(root string) error {
	if st, err := os.Stat(filepath.Join(root, legacyDir)); err == nil && st.IsDir() {
		return fmt.Errorf("%w at %s; only WeChat 3.x data is on this Mac, and chatvault reads WeChat 4.x: update WeChat, log in once and let it load your chats", ErrNoWeChat, root)
	}
	return fmt.Errorf("%w at %s; install WeChat for Mac 4, log in and let it load your chats", ErrNoWeChat, root)
}

// Accounts lists the account directories under root that have databases.
// With none, the error says why.
func Accounts(root string) ([]Account, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, noData(root)
	}
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
	if len(out) == 0 {
		return nil, noData(root)
	}
	return out, nil
}

// CheckLayout reports whether the account's databases are laid out as
// WeChat 4.x lays them out, so a different layout fails with a reason
// rather than as missing messages.
func (a Account) CheckLayout() error {
	dbs, err := ListDBs(a.DBDir())
	if err != nil {
		return err
	}
	var contact, message bool
	for _, rel := range dbs {
		contact = contact || rel == filepath.Join("contact", "contact.db")
		message = message || MessageDB(rel)
	}
	if !contact || !message {
		return fmt.Errorf("%s does not look like WeChat 4.x data (no contact/contact.db or message/message_*.db); chatvault reads only WeChat 4.x", a.DBDir())
	}
	return nil
}

// FindAccount resolves name (a directory name, a path, or "" when there is
// exactly one account).
func FindAccount(root, name string) (Account, error) {
	if name != "" && filepath.IsAbs(name) {
		return Account{Dir: name}, nil
	}
	all, err := Accounts(root)
	if err != nil {
		return Account{}, err
	}
	if name != "" {
		for _, a := range all {
			if a.ID() == name || a.Username() == name {
				return a, nil
			}
		}
		return Account{}, fmt.Errorf("no account %q under %s; the accounts are %s", name, root, accountIDs(all))
	}
	if len(all) != 1 {
		return Account{}, fmt.Errorf("found %d accounts under %s (%s); pick one with --account", len(all), root, accountIDs(all))
	}
	return all[0], nil
}

func accountIDs(as []Account) string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.ID()
	}
	return strings.Join(ids, ", ")
}

// KeyForms describes the keys files LoadKeys accepts.
const KeyForms = `{"keys": ["<64 hex>", ...]} or {"message/message_0.db": {"enc_key": "<64 hex>"}, ...}`

// LoadKeys reads the candidate keys from a keys file: either
// {"keys": ["<hex>", ...]} or the output of wechat-key-macos,
// {"message/message_0.db": {"enc_key": "<hex>"}, ...}. Errors name the
// entry at fault, never a key.
func LoadKeys(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s is not JSON; a keys file looks like %s", path, KeyForms)
	}
	var plain struct {
		Keys []string `json:"keys"`
	}
	type entry struct{ where, hex string }
	var hexes []entry
	if json.Unmarshal(data, &plain) == nil && plain.Keys != nil {
		for i, h := range plain.Keys {
			hexes = append(hexes, entry{fmt.Sprintf("keys[%d]", i), h})
		}
	} else {
		var perDB map[string]json.RawMessage
		if json.Unmarshal(data, &perDB) != nil {
			return nil, fmt.Errorf("%s: not a keys file; expected %s", path, KeyForms)
		}
		for db, raw := range perDB {
			var v struct {
				EncKey string `json:"enc_key"`
			}
			if json.Unmarshal(raw, &v) == nil && v.EncKey != "" {
				hexes = append(hexes, entry{db, v.EncKey})
			}
		}
	}
	slices.SortFunc(hexes, func(a, b entry) int { return strings.Compare(a.where, b.where) })
	seen := map[string]bool{}
	var keys [][]byte
	for _, e := range hexes {
		h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e.hex)), "0x")
		k, err := hex.DecodeString(h)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s: the key for %s is not 64 hex characters (32 bytes)", path, e.where)
		}
		if !seen[h] {
			seen[h] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no keys in it; expected %s", path, KeyForms)
	}
	return keys, nil
}
