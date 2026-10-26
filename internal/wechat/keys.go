package wechat

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/soundadam/chatvault/internal/sqlcipher"
)

// KeyCoverage tries keys against the first page of each of the account's
// databases, reading only, and returns the databases they open and those
// they do not.
func KeyCoverage(acct Account, keys [][]byte) (opened, missing []string, err error) {
	rels, err := ListDBs(acct.DBDir())
	if err != nil {
		return nil, nil, err
	}
	rels = slices.DeleteFunc(rels, skipDB)
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		jobs = make(chan string)
	)
	for range min(runtime.NumCPU(), 8) {
		wg.Go(func() {
			for rel := range jobs {
				ok := opens(filepath.Join(acct.DBDir(), rel), keys)
				mu.Lock()
				if ok {
					opened = append(opened, rel)
				} else {
					missing = append(missing, rel)
				}
				mu.Unlock()
			}
		})
	}
	for _, rel := range rels {
		jobs <- rel
	}
	close(jobs)
	wg.Wait()
	slices.Sort(opened)
	slices.Sort(missing)
	return opened, missing, nil
}

func opens(path string, keys [][]byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	page1 := make([]byte, sqlcipher.PageSize)
	if _, err := f.ReadAt(page1, 0); err != nil {
		return false
	}
	_, ok := sqlcipher.MatchKey(page1, keys)
	return ok
}
