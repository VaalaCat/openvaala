package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
)

// MemoryWorkspace 是跨对话记忆的工作区：根目录下的 user 是用户级记忆，project 是某个项目的记忆。
type MemoryWorkspace interface {
	Workspace
	// GetProjectName 返回 project 目录所属的项目：项目记忆目录相对记忆根目录的路径，例如 projects/local/-tmp-x。
	GetProjectName() string
}

// memoryWorkspaceImpl 把根目录下的 user、project 两个目录映射到 serve 机器上的两份记忆目录；
// 只能读写文件，不能启动进程、建立连接，也没有主目录。
type memoryWorkspaceImpl struct {
	// projectName 是项目记忆目录相对记忆根目录的路径。
	projectName string
	// userWorkspace、projectWorkspace 是以两份记忆目录为根的本机工作区，路径换成相对路径后交给它们。
	userWorkspace    Workspace
	projectWorkspace Workspace
}

// MemoryWorkspaceOpt 是记忆根目录在 serve 机器上的绝对路径，以及项目记忆目录相对它的路径。
type MemoryWorkspaceOpt struct {
	MemoryDirectory string
	ProjectName     string
}

// NewMemoryWorkspaceOpt 保存记忆根目录和项目记忆目录的相对路径；用户级记忆目录固定是根目录下的 user。
func NewMemoryWorkspaceOpt(memoryDirectory, projectName string) *MemoryWorkspaceOpt {
	// 1. 创建记忆目录配置。
	return &MemoryWorkspaceOpt{
		MemoryDirectory: memoryDirectory,
		ProjectName:     projectName,
	}
}

// NewMemoryWorkspace 校验记忆根目录是绝对路径，创建记忆工作区；不创建目录，第一次写入时才建。
func NewMemoryWorkspace(opt *MemoryWorkspaceOpt) (MemoryWorkspace, error) {
	// 1. 用户级和项目级目录各建一个本机工作区，由它检查绝对路径。
	if opt == nil {
		return nil, errors.New("memory workspace options are required")
	}
	userWorkspace, err := NewLocalWorkspace(NewLocalWorkspaceOpt(filepath.Join(opt.MemoryDirectory, "user")))
	if err != nil {
		return nil, fmt.Errorf("memory user directory: %w", err)
	}
	projectWorkspace, err := NewLocalWorkspace(NewLocalWorkspaceOpt(filepath.Join(opt.MemoryDirectory, opt.ProjectName)))
	if err != nil {
		return nil, fmt.Errorf("memory project directory: %w", err)
	}
	return &memoryWorkspaceImpl{
		projectName:      opt.ProjectName,
		userWorkspace:    userWorkspace,
		projectWorkspace: projectWorkspace,
	}, nil
}

// GetProjectName 返回项目记忆目录相对记忆根目录的路径。
func (w *memoryWorkspaceImpl) GetProjectName() string {
	// 1. 直接返回创建时保存的项目名。
	return w.projectName
}

// GetRootDirectory 返回记忆工作区的根目录 /。
func (w *memoryWorkspaceImpl) GetRootDirectory() string {
	// 1. 根目录固定为 /，下面只有 user 和 project。
	return "/"
}

// GetHomeDirectory 拒绝记忆工作区的主目录查询。
func (w *memoryWorkspaceImpl) GetHomeDirectory(context.Context) (string, error) {
	// 1. 返回 errors.ErrUnsupported。
	return "", errors.ErrUnsupported
}

// ReadFile 读取记忆文件的一页内容。
func (w *memoryWorkspaceImpl) ReadFile(ctx context.Context, filePath string, offset int64, limit int) (*FileContent, error) {
	// 1. 顶层目录不是普通文件，不在 user、project 下的路径不存在，其余交给对应的记忆目录。
	work, realPath, cleaned := w.getTarget(filePath)
	switch {
	case isMemoryTopDirectory(cleaned):
		return nil, ErrNotRegularFile
	case work == nil:
		return nil, fmt.Errorf("%s: %w", cleaned, fs.ErrNotExist)
	}
	return work.ReadFile(ctx, realPath, offset, limit)
}

// WriteFile 写入记忆文件。
func (w *memoryWorkspaceImpl) WriteFile(ctx context.Context, filePath string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 只有 user、project 目录下的文件能写，其余交给对应的记忆目录。
	work, realPath, cleaned := w.getTarget(filePath)
	if work == nil || isMemoryTopDirectory(cleaned) {
		return ErrOutsideWorkspace
	}
	return work.WriteFile(ctx, realPath, content, mode, createOnly)
}

// OpenFileReader 打开记忆文件的读取端。
func (w *memoryWorkspaceImpl) OpenFileReader(ctx context.Context, filePath string) (io.ReadCloser, error) {
	// 1. 顶层目录不是普通文件，不在 user、project 下的路径不存在，其余交给对应的记忆目录。
	work, realPath, cleaned := w.getTarget(filePath)
	switch {
	case isMemoryTopDirectory(cleaned):
		return nil, ErrNotRegularFile
	case work == nil:
		return nil, fmt.Errorf("%s: %w", cleaned, fs.ErrNotExist)
	}
	return work.OpenFileReader(ctx, realPath)
}

// CreateFileWriter 创建记忆文件的写入端。
func (w *memoryWorkspaceImpl) CreateFileWriter(ctx context.Context, filePath string, mode fs.FileMode) (io.WriteCloser, error) {
	// 1. 只有 user、project 目录下的文件能写；先建出真实的记忆目录，再交给它。
	work, realPath, cleaned := w.getTarget(filePath)
	if work == nil || isMemoryTopDirectory(cleaned) {
		return nil, ErrOutsideWorkspace
	}
	if err := os.MkdirAll(work.GetRootDirectory(), 0o700); err != nil {
		return nil, err
	}
	return work.CreateFileWriter(ctx, realPath, mode)
}

// GetMetadata 读取记忆路径的元数据。
func (w *memoryWorkspaceImpl) GetMetadata(ctx context.Context, filePath string, followSymlinks bool) (*FileMetadata, error) {
	// 1. 顶层目录总是存在，不管真实的记忆目录建没建；不在 user、project 下的路径不存在。
	work, realPath, cleaned := w.getTarget(filePath)
	switch {
	case isMemoryTopDirectory(cleaned):
		return &FileMetadata{IsDirectory: true, Mode: 0o700}, nil
	case work == nil:
		return nil, fmt.Errorf("%s: %w", cleaned, fs.ErrNotExist)
	}
	return work.GetMetadata(ctx, realPath, followSymlinks)
}

// ReadDirectory 返回记忆目录的直接子项。
func (w *memoryWorkspaceImpl) ReadDirectory(ctx context.Context, directoryPath string) ([]*DirectoryEntry, error) {
	// 1. 根目录固定是 project 和 user；不在 user、project 下的路径不存在。
	work, realPath, cleaned := w.getTarget(directoryPath)
	switch {
	case cleaned == "/":
		return []*DirectoryEntry{
			{Name: "project", IsDirectory: true},
			{Name: "user", IsDirectory: true},
		}, nil
	case work == nil:
		return nil, fmt.Errorf("%s: %w", cleaned, fs.ErrNotExist)
	}
	// 2. 真实的记忆目录还没建时，user 和 project 是空目录。
	entries, err := work.ReadDirectory(ctx, realPath)
	if errors.Is(err, fs.ErrNotExist) && isMemoryTopDirectory(cleaned) {
		return []*DirectoryEntry{}, nil
	}
	return entries, err
}

// CreateDirectory 在记忆目录下递归创建目录。
func (w *memoryWorkspaceImpl) CreateDirectory(ctx context.Context, directoryPath string) error {
	// 1. 根目录已经存在；不在 user、project 下的路径不能建。
	work, realPath, cleaned := w.getTarget(directoryPath)
	switch {
	case cleaned == "/":
		return nil
	case work == nil:
		return ErrOutsideWorkspace
	}
	// 2. 先用 0700 建出真实的记忆目录，再建它下面的子目录。
	if err := os.MkdirAll(work.GetRootDirectory(), 0o700); err != nil {
		return err
	}
	if isMemoryTopDirectory(cleaned) {
		return nil
	}
	return work.CreateDirectory(ctx, realPath)
}

// Remove 删除记忆目录下的文件或空目录。
func (w *memoryWorkspaceImpl) Remove(ctx context.Context, filePath string) error {
	// 1. 顶层目录不能删，不在 user、project 下的路径也不在工作区内。
	work, realPath, cleaned := w.getTarget(filePath)
	if work == nil || isMemoryTopDirectory(cleaned) {
		return ErrOutsideWorkspace
	}
	return work.Remove(ctx, realPath)
}

// StartProcess 拒绝记忆工作区的进程启动。
func (w *memoryWorkspaceImpl) StartProcess(context.Context, *ProcessRequest) (Process, error) {
	// 1. 返回 errors.ErrUnsupported。
	return nil, errors.ErrUnsupported
}

// Dial 拒绝记忆工作区的网络拨号。
func (w *memoryWorkspaceImpl) Dial(context.Context, string, string) (net.Conn, error) {
	// 1. 返回 errors.ErrUnsupported。
	return nil, errors.ErrUnsupported
}

// GetHTTPProxy 总是返回 nil，记忆工作区没有网络。
func (w *memoryWorkspaceImpl) GetHTTPProxy(*http.Request) (*url.URL, error) {
	// 1. 返回 nil。
	return nil, nil
}

// getTarget 清理路径，返回它所在的那份记忆工作区、在那份工作区里的绝对路径和清理后的路径；
// 路径是根目录，或第一段不是 user、project 时，工作区为 nil。
func (w *memoryWorkspaceImpl) getTarget(target string) (work Workspace, realPath, cleaned string) {
	// 1. 加 / 前缀再清理，.. 越不出根目录；按第一段分派，剩下的部分换成相对记忆目录的路径。
	cleaned = path.Clean("/" + target)
	name, relative := splitMemoryPath(cleaned)
	switch name {
	case "user":
		work = w.userWorkspace
	case "project":
		work = w.projectWorkspace
	default:
		return nil, "", cleaned
	}
	return work, filepath.Join(work.GetRootDirectory(), relative), cleaned
}

// Close 不持有需要释放的资源，总是返回 nil。
func (w *memoryWorkspaceImpl) Close() error {
	// 1. 返回 nil。
	return nil
}
