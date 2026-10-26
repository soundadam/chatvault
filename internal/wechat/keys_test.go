package wechat

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveKeysRoundTrip(t *testing.T) {
	keys := [][]byte{bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := SaveKeys(path, keys); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	got, err := LoadKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Fatalf("read %d keys, wrote %d", len(got), len(keys))
	}
	for i := range keys {
		if !bytes.Equal(got[i], keys[i]) {
			t.Errorf("key %d = %s, want %s", i, hex.EncodeToString(got[i]), hex.EncodeToString(keys[i]))
		}
	}
}

func TestMergeKeys(t *testing.T) {
	a, b, c := bytes.Repeat([]byte{0xaa}, 32), bytes.Repeat([]byte{0xbb}, 32), bytes.Repeat([]byte{0xcc}, 32)
	got := MergeKeys([][]byte{a, b}, [][]byte{b, c}, nil)
	want := [][]byte{a, b, c}
	if len(got) != len(want) {
		t.Fatalf("merged %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("key %d = %x, want %x", i, got[i], want[i])
		}
	}
}
