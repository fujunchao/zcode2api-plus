package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"zcode2api/internal/config"
)

var ErrDatabaseInUse = errors.New("数据库正在被其他服务或 CLI 使用，请使用管理后台，或停止服务后再运行 CLI")

// NewExclusive 用于持有进程内快照的服务与 CLI 入口。
func NewExclusive() (*Store, error) {
	path, err := filepath.Abs(config.DBPath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 同一路径的符号链接别名必须竞争同一把锁。
	if resolved, e := filepath.EvalSymlinks(path); e == nil {
		path = resolved
	} else {
		dir, e := filepath.EvalSymlinks(filepath.Dir(path))
		if e != nil {
			return nil, e
		}
		path = filepath.Join(dir, filepath.Base(path))
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	s, err := New()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.processLock = f
	return s, nil
}
