package wechat

import (
	"os"

	"golang.org/x/sys/unix"
)

// copyFile clones src to dst: on APFS a clone is instant and shares blocks
// until either side changes.
func copyFile(src, dst string) error {
	tmp := dst + ".tmp"
	os.Remove(tmp)
	if err := unix.Clonefile(src, tmp, unix.CLONE_NOFOLLOW); err != nil {
		if err := plainCopy(src, tmp); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}
