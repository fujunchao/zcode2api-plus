package store

import (
	"context"
	"fmt"
	"sort"
)

// SetSettings 原子更新一组设置：单事务全部落库成功后，才一次发布新的只读快照。
// 校验由调用方先完成；数据库中途失败时数据库和内存都保持原值，尤其不能只改了
// 后台密码却向客户端返回失败。鉴权读取仍走无锁快照，不等待事务。
func (s *Store) SetSettings(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		logPersistFailure("settings", "batch", err)
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range keys {
		if _, err := tx.Exec(fmt.Sprintf("INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)", metaTable), key, values[key]); err != nil {
			logPersistFailure("settings", key, err)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		logPersistFailure("settings", "commit", err)
		return err
	}
	for _, key := range keys {
		s.settings[key] = values[key]
	}
	s.publishSettings()
	return nil
}
