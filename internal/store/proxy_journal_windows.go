//go:build windows

package store

// Windows 的 os.Rename 使用系统文件替换，文件内容已在调用前 FlushFileBuffers；
// 普通目录句柄不支持 os.File.Sync。运行期事务仍由 SQLite 与恢复日志共同保护。
func syncStrictJournalDirectory(string) error { return nil }
