// Package www 把构建好的 WebUI 页面文件打包进二进制。
package www

import (
	"embed"
	"io/fs"
)

//go:embed all:out
var assets embed.FS

// GetAssets 返回打包进二进制的页面文件；构建时没有先构建前端则返回 nil。
func GetAssets() fs.FS {
	files, _ := fs.Sub(assets, "out/web")
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return nil
	}
	return files
}
