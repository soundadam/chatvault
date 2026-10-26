//go:build !darwin

package wechat

import "os"

func copyFile(src, dst string) error {
	tmp := dst + ".tmp"
	if err := plainCopy(src, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
