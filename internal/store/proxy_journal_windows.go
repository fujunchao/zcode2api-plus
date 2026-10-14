//go:build windows

package store

import "golang.org/x/sys/windows"

// Windows 以原生替换并请求写穿完成；不通过先删目标再重命名制造恢复日志空窗。
// 临时文件和目标同目录，文件内容已经在调用前 FlushFileBuffers。
func replaceStrictJournal(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
