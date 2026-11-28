// desktopclipboard 在 GUI 测试的显示会话中提供真实的文件剪贴板，stdin 关闭即退出。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/egoist/mygo"
)

func main() {
	mygo.App.WhenReady(func() {
		if err := mygo.Clipboard.WriteFiles(os.Args[1:]...); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ready")
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); mygo.App.Quit() }()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
