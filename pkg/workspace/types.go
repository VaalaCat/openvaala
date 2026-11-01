package workspace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// ── Workspace 文件结果 ──

// FileContent 是 ReadFile 读到的一段内容。
type FileContent struct {
	Content []byte
	// Size 是文件报告的大小；/proc 等文件可能为 0。
	Size int64
	// EOF 这次读取是否已经读到文件末尾？
	EOF bool
}

// FileMetadata 描述一个路径的类型、大小、权限位与修改时间。
type FileMetadata struct {
	IsDirectory bool
	// IsFile 是不是普通文件？
	IsFile     bool
	IsSymlink  bool
	Size       int64
	Mode       fs.FileMode
	ModifiedAt time.Time
}

// DirectoryEntry 是目录中的一个直接子项。
type DirectoryEntry struct {
	Name        string
	IsDirectory bool
	IsFile      bool
}

// DirectoryFile 是 ListDirectoryFiles 收集到的一项。
type DirectoryFile struct {
	// RelativePath 是相对遍历根目录的路径，用 / 分隔。
	RelativePath string
	// Mode 是跟随符号链接后的权限位。
	Mode        fs.FileMode
	IsDirectory bool
}

// ── Workspace 进程请求 ──

// ProcessRequest 描述一条要启动的 bash 命令。
type ProcessRequest struct {
	// Command 是交给 bash -c 执行的命令文本。
	Command string
	// Directory 是命令的工作目录，绝对路径。
	Directory string
	// TTY 是否在终端里运行命令？在终端里运行时输出全部写入 Stdout。
	TTY bool
	// Rows、Cols 是 TTY 命令的初始终端行列数；为零时按 24×80。
	Rows, Cols uint16
	// Stdin 是非 TTY 命令的一次性输入，nil 时读到空输入；TTY 命令的输入用 Process.WriteTerminalInput，传 Stdin 返回错误。
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ── 虚拟 Workspace 文件 ──

// VirtualFile 是虚拟工作区的一个初始文件。
type VirtualFile struct {
	// Content 是现成的文件内容。
	Content []byte
	// Load 在第一次读取时调用一次，返回文件内容。
	Load func(ctx context.Context) ([]byte, error)
}

// ── Workspace 错误 ──

var (
	// ErrOutsideWorkspace 表示写操作的目标不在工作区根目录内。
	ErrOutsideWorkspace = errors.New("path is outside the workspace root")
	// ErrNotRegularFile 表示读取目标不是普通文件。
	ErrNotRegularFile = errors.New("path is not a regular file")
	// ErrTTYUnsupported 表示这个工作区不能在终端里运行命令。
	ErrTTYUnsupported = errors.New("this workspace does not support interactive commands")
	// ErrTTYInputUnsupported 表示向不在终端里运行的命令写终端输入。
	ErrTTYInputUnsupported = errors.New("this command is not running in a terminal")
	// ErrTTYStdinUnsupported 表示 TTY 命令的请求带了 Stdin；TTY 命令的输入只能经 Process.WriteTerminalInput 写入。
	ErrTTYStdinUnsupported = errors.New("interactive commands take input through the terminal, not Stdin")
)
