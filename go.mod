module zcode2api

// 语言版本兼最低工具链：CI 的 setup-go@v5 只读这一行（不认 toolchain 指令），
// 发版二进制即由它决定。1.25.0 可达 29 个已修复的标准库漏洞（govulncheck
// 二进制模式实测），1.25.14 为零；升级补丁版本时同步修改这里。
go 1.25.14

// 直接依赖（其余 require 为间接依赖，交由 go mod tidy 维护）：
//   modernc.org/sqlite    纯 Go SQLite（无 CGo），账号库存储
//   github.com/go-rod/rod 验证码求解，驱动 cloakbrowser 下载的 Chromium
//   golang.org/x/sys     Windows/Unix 数据库进程独占锁

require (
	github.com/go-rod/rod v0.116.2
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.58.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/ysmood/fetchup v0.2.3 // indirect
	github.com/ysmood/goob v0.4.0 // indirect
	github.com/ysmood/got v0.40.0 // indirect
	github.com/ysmood/gson v0.7.3 // indirect
	github.com/ysmood/leakless v0.9.0 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
