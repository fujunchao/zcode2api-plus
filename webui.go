// Package zcode2api 前端静态资源嵌入。
// go:embed 只能引用本包目录树内的文件，而 frontend/dist 位于仓库根目录下，
// 因此 embed 声明必须放在仓库根（internal/web 无法引用），由 main 引入。
package zcode2api

import "embed"

// DistFS 嵌入的前端构建产物（frontend/dist 全树）。
//
//go:embed all:frontend/dist
var DistFS embed.FS
