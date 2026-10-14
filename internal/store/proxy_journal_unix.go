//go:build !windows

package store

import "os"

// fsync 文件本体之后还须同步父目录，保证原子 rename 在断电恢复后可见。
func syncStrictJournalDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
