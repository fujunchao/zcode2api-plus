//go:build !windows

package store

import (
	"os"
	"path/filepath"
)

func replaceStrictJournal(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return syncStrictJournalDirectory(filepath.Dir(target))
}

// fsync 文件本体之后还须同步父目录，保证原子 rename 在断电恢复后可见。
func syncStrictJournalDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
