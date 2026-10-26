package wechat

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/soundadam/chatvault/internal/sqlcipher"
)

// Sig identifies the state of a database and its WAL; equal signatures mean
// nothing to redo.
type Sig struct {
	Size     int64 `json:"size"`
	MTime    int64 `json:"mtime"`
	WALSize  int64 `json:"wal_size"`
	WALMTime int64 `json:"wal_mtime"`
}

func sigOf(path string) (Sig, error) {
	var s Sig
	st, err := os.Stat(path)
	if err != nil {
		return s, err
	}
	s.Size, s.MTime = st.Size(), st.ModTime().UnixNano()
	if w, err := os.Stat(path + "-wal"); err == nil {
		s.WALSize, s.WALMTime = w.Size(), w.ModTime().UnixNano()
	}
	return s, nil
}

// DBState is what the last run did with one database.
type DBState struct {
	Sig Sig `json:"sig"`
	// Key is the hex page key that opened it, so later runs skip matching.
	Key string `json:"key,omitempty"`
	// Plain is set once the plain copy for Sig is written.
	Plain bool `json:"plain"`
	// Merged is set once the vault has taken in the plain copy for Sig.
	Merged bool `json:"merged,omitempty"`
}

// State is kept per account in the vault, as JSON.
type State struct {
	DBs map[string]*DBState `json:"dbs"`
}

func LoadState(path string) (*State, error) {
	s := &State{DBs: map[string]*DBState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.DBs == nil {
		s.DBs = map[string]*DBState{}
	}
	return s, nil
}

func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// skipDB leaves out the databases there is no use decrypting: *_fts.db are
// search indexes built with WeChat's own tokenizer, which plain SQLite
// cannot open.
func skipDB(rel string) bool { return strings.HasSuffix(rel, "_fts.db") }

// ListDBs returns the encrypted databases under dir, relative and sorted.
func ListDBs(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".db") {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

// Event reports progress on one database.
type Event struct {
	Step  string // "snapshot" or "decrypt"
	DB    string
	Done  int
	Total int
	// Skipped means the database had not changed since the last run.
	Skipped bool
	Err     error
}

// Paths are the per-account directories in the vault.
type Paths struct {
	Snapshot string // encrypted copies
	Plain    string // decrypted copies
	State    string // state.json
	Keys     string // keys.json
}

// Prepare brings snapshot/ and plain/ up to date with the account's
// databases, in parallel, redoing only those whose signature changed. It
// returns the databases whose plain copy is new, and those no key opened.
func Prepare(ctx context.Context, acct Account, p Paths, st *State, keys [][]byte, progress func(Event)) (changed, failed []string, err error) {
	rels, err := ListDBs(acct.DBDir())
	if err != nil {
		return nil, nil, err
	}
	rels = slices.DeleteFunc(rels, skipDB)

	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
		jobs = make(chan string)
	)
	report := func(e Event) {
		mu.Lock()
		done++
		e.Done, e.Total = done, len(rels)
		if e.Err != nil {
			failed = append(failed, e.DB)
		} else if !e.Skipped {
			changed = append(changed, e.DB)
		}
		mu.Unlock()
		if progress != nil {
			progress(e)
		}
	}
	for range min(runtime.NumCPU(), 8) {
		wg.Go(func() {
			for rel := range jobs {
				mu.Lock()
				prev := st.DBs[rel]
				mu.Unlock()
				next, skipped, err := prepareOne(acct, p, rel, prev, keys)
				if next != nil {
					mu.Lock()
					st.DBs[rel] = next
					mu.Unlock()
				}
				report(Event{Step: "decrypt", DB: rel, Skipped: skipped, Err: err})
			}
		})
	}
	for _, rel := range rels {
		if ctx.Err() != nil {
			break
		}
		jobs <- rel
	}
	close(jobs)
	wg.Wait()
	slices.Sort(changed)
	slices.Sort(failed)
	return changed, failed, ctx.Err()
}

func prepareOne(acct Account, p Paths, rel string, prev *DBState, keys [][]byte) (*DBState, bool, error) {
	src := filepath.Join(acct.DBDir(), rel)
	sig, err := sigOf(src)
	if err != nil {
		return nil, false, err
	}
	plain := filepath.Join(p.Plain, rel)
	if prev != nil && prev.Plain && prev.Sig == sig {
		if _, err := os.Stat(plain); err == nil {
			return nil, true, nil
		}
	}
	snap := filepath.Join(p.Snapshot, rel)
	sig, err = snapshotOne(src, snap)
	if err != nil {
		return nil, false, err
	}

	next := &DBState{Sig: sig}
	var key []byte
	page1 := make([]byte, sqlcipher.PageSize)
	if f, err := os.Open(snap); err != nil {
		return nil, false, err
	} else {
		_, err = f.ReadAt(page1, 0)
		f.Close()
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", rel, err)
		}
	}
	if prev != nil && prev.Key != "" {
		if k, err := hex.DecodeString(prev.Key); err == nil {
			if c, err := sqlcipher.New(k, sqlcipher.Salt(page1)); err == nil && c.Verify(page1, 1) {
				key = k
			}
		}
	}
	if key == nil {
		k, ok := sqlcipher.MatchKey(page1, keys)
		if !ok {
			return next, false, fmt.Errorf("%s: no key opens it", rel)
		}
		key = k
	}
	next.Key = hex.EncodeToString(key)
	if err := sqlcipher.DecryptFile(snap, plain, key); err != nil {
		return next, false, err
	}
	next.Plain = true
	return next, false, nil
}

// snapshotOne copies a database and its WAL (not the -shm), cloning on APFS.
// WeChat may be writing meanwhile, so it copies again until the source's
// signature holds still across the copy.
func snapshotOne(src, dst string) (Sig, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Sig{}, err
	}
	var sig Sig
	for try := range 5 {
		before, err := sigOf(src)
		if err != nil {
			return sig, err
		}
		if err := copyFile(src, dst); err != nil {
			return sig, err
		}
		if _, err := os.Stat(src + "-wal"); err == nil {
			if err := copyFile(src+"-wal", dst+"-wal"); err != nil {
				return sig, err
			}
		} else {
			os.Remove(dst + "-wal")
		}
		after, err := sigOf(src)
		if err != nil {
			return sig, err
		}
		sig = after
		if after == before {
			return sig, nil
		}
		time.Sleep(time.Duration(try+1) * 200 * time.Millisecond)
	}
	// Still changing: keep the last copy. A torn copy fails its HMAC check
	// in decrypt rather than passing silently.
	return sig, nil
}
