package workspace_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// memoryFixture 是一个记忆工作区和它背后的两份真实目录；两份目录都不创建，由测试按需准备。
type memoryFixture struct {
	workspace workspace.MemoryWorkspace
	// baseDirectory 是两份记忆目录的上一级，放在里面的文件不属于任何一份记忆目录。
	baseDirectory    string
	userDirectory    string
	projectDirectory string
}

// newMemoryFixture 创建指向临时目录的记忆工作区，project 目录放在 projects/local/ 下两层，和 Runtime 的布局一致。
func newMemoryFixture(t *testing.T) *memoryFixture {
	// 1. 在临时目录下规划 user 和 project 两份记忆目录，只建工作区，不建目录。
	t.Helper()
	base := t.TempDir()
	fixture := &memoryFixture{
		baseDirectory:    base,
		userDirectory:    filepath.Join(base, "user"),
		projectDirectory: filepath.Join(base, "projects", "local", "-repo"),
	}
	work, err := workspace.NewMemoryWorkspace(workspace.NewMemoryWorkspaceOpt(base, filepath.Join("projects", "local", "-repo")))
	if err != nil {
		t.Fatal(err)
	}
	fixture.workspace = work
	return fixture
}

// writeFile 在真实目录里写一个文件，父目录不存在时一并创建。
func (f *memoryFixture) writeFile(t *testing.T, path, content string) {
	// 1. 创建父目录再写文件。
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeMemoryFile 按 apply_patch 的顺序先建父目录再写新文件。
func writeMemoryFile(ctx context.Context, work workspace.Workspace, path, content string) error {
	// 1. 创建父目录，再只写新文件。
	if err := work.CreateDirectory(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return work.WriteFile(ctx, path, []byte(content), 0o644, true)
}

// TestMemoryWorkspaceFiles 验证两份记忆目录的读写、越界路径和第一次写入时创建真实目录。
func TestMemoryWorkspaceFiles(t *testing.T) {
	// 1. 列出读、写、越界和删除的场景；act 返回读到的文本，check 核对真实目录里的结果。
	tests := []struct {
		name  string
		setup func(t *testing.T, f *memoryFixture)
		act   func(ctx context.Context, f *memoryFixture) (string, error)
		// wantErr 非空时 act 必须返回满足它的错误。
		wantErr  error
		wantText string
		check    func(t *testing.T, f *memoryFixture)
	}{
		{
			name: "read_user_file",
			setup: func(t *testing.T, f *memoryFixture) {
				f.writeFile(t, filepath.Join(f.userDirectory, "x.md"), "from user")
			},
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				content, err := f.workspace.ReadFile(ctx, "/user/x.md", 0, 100)
				if err != nil {
					return "", err
				}
				return string(content.Content), nil
			},
			wantText: "from user",
		},
		{
			name: "read_project_file_with_relative_path",
			setup: func(t *testing.T, f *memoryFixture) {
				f.writeFile(t, filepath.Join(f.projectDirectory, "topics", "x.md"), "from project")
			},
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				reader, err := f.workspace.OpenFileReader(ctx, "project/topics/x.md")
				if err != nil {
					return "", err
				}
				defer reader.Close()
				content, err := io.ReadAll(reader)
				return string(content), err
			},
			wantText: "from project",
		},
		{
			name: "write_user_file",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", writeMemoryFile(ctx, f.workspace, "/user/x.md", "written to user")
			},
			check: func(t *testing.T, f *memoryFixture) {
				assertFileContent(t, filepath.Join(f.userDirectory, "x.md"), "written to user")
			},
		},
		{
			name: "write_project_nested_file",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", writeMemoryFile(ctx, f.workspace, "/project/topics/go.md", "written to project")
			},
			check: func(t *testing.T, f *memoryFixture) {
				assertFileContent(t, filepath.Join(f.projectDirectory, "topics", "go.md"), "written to project")
			},
		},
		{
			// 数据目录是空的：CreateDirectory 要把真实的根目录建出来，权限为 0700。
			name: "first_write_creates_directories",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", writeMemoryFile(ctx, f.workspace, "/project/MEMORY.md", "- [testing](testing.md)")
			},
			check: func(t *testing.T, f *memoryFixture) {
				assertFileContent(t, filepath.Join(f.projectDirectory, "MEMORY.md"), "- [testing](testing.md)")
				info, err := os.Stat(f.projectDirectory)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o700 {
					t.Fatalf("project directory mode = %v, want 0700", info.Mode().Perm())
				}
			},
		},
		{
			name: "read_other_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				_, err := f.workspace.ReadFile(ctx, "/other/x.md", 0, 100)
				return "", err
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "write_other_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.WriteFile(ctx, "/other/x.md", []byte("x"), 0o644, false)
			},
			wantErr: workspace.ErrOutsideWorkspace,
		},
		{
			name: "create_other_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.CreateDirectory(ctx, "/other")
			},
			wantErr: workspace.ErrOutsideWorkspace,
		},
		{
			// project 目录的上两级真的有这个文件：路径要先在记忆工作区的根目录下清理，不能拼到真实目录后面。
			name: "read_traversal_to_file_above_project",
			setup: func(t *testing.T, f *memoryFixture) {
				f.writeFile(t, filepath.Join(f.baseDirectory, "projects", "secret.txt"), "secret")
			},
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				_, err := f.workspace.ReadFile(ctx, "/project/../../secret.txt", 0, 100)
				return "", err
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "read_traversal_out_of_project",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				_, err := f.workspace.ReadFile(ctx, "/project/../../etc/passwd", 0, 100)
				return "", err
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "write_traversal_out_of_project",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.WriteFile(ctx, "/project/../../etc/passwd", []byte("x"), 0o644, false)
			},
			wantErr: workspace.ErrOutsideWorkspace,
		},
		{
			name: "read_top_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				_, err := f.workspace.ReadFile(ctx, "/user", 0, 100)
				return "", err
			},
			wantErr: workspace.ErrNotRegularFile,
		},
		{
			name: "remove_user_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.Remove(ctx, "/user")
			},
			wantErr: workspace.ErrOutsideWorkspace,
		},
		{
			name: "remove_root_directory",
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.Remove(ctx, "/")
			},
			wantErr: workspace.ErrOutsideWorkspace,
		},
		{
			name: "remove_file",
			setup: func(t *testing.T, f *memoryFixture) {
				f.writeFile(t, filepath.Join(f.userDirectory, "x.md"), "old")
			},
			act: func(ctx context.Context, f *memoryFixture) (string, error) {
				return "", f.workspace.Remove(ctx, "/user/x.md")
			},
			check: func(t *testing.T, f *memoryFixture) {
				if _, err := os.Stat(filepath.Join(f.userDirectory, "x.md")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("Stat() error = %v, want the removed file to be gone", err)
				}
			},
		},
	}
	// 2. 每项用新的记忆工作区执行，先核对错误和读到的文本，再看真实目录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newMemoryFixture(t)
			if tt.setup != nil {
				tt.setup(t, fixture)
			}
			text, err := tt.act(t.Context(), fixture)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if text != tt.wantText {
				t.Fatalf("text = %q, want %q", text, tt.wantText)
			}
			if tt.check != nil {
				tt.check(t, fixture)
			}
		})
	}
}

// assertFileContent 检查真实文件的内容。
func assertFileContent(t *testing.T, path, want string) {
	// 1. 读取文件并比较内容。
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", path, content, want)
	}
}

// TestMemoryWorkspaceDirectories 验证顶层目录不依赖真实目录是否存在，以及目录列表和元数据。
func TestMemoryWorkspaceDirectories(t *testing.T) {
	// 1. 列出读目录和元数据的场景；wantNames 为 nil 且没有 wantErr 时只检查目录条目是空列表。
	tests := []struct {
		name  string
		setup func(t *testing.T, f *memoryFixture)
		path  string
		// wantNames 是 ReadDirectory 应返回的条目名。
		wantNames []string
		// wantErr 非空时 ReadDirectory 必须返回满足它的错误。
		wantErr error
		// wantMetadataErr 为 nil 时 GetMetadata 必须返回目录。
		wantMetadataErr error
	}{
		{
			name:      "root_without_real_directories",
			path:      "/",
			wantNames: []string{"project", "user"},
		},
		{
			name:      "user_without_real_directory",
			path:      "/user",
			wantNames: []string{},
		},
		{
			name:      "project_without_real_directory",
			path:      "/project",
			wantNames: []string{},
		},
		{
			name: "project_with_files",
			setup: func(t *testing.T, f *memoryFixture) {
				f.writeFile(t, filepath.Join(f.projectDirectory, "a.md"), "a")
				f.writeFile(t, filepath.Join(f.projectDirectory, "sub", "b.md"), "b")
			},
			path:      "/project",
			wantNames: []string{"a.md", "sub"},
		},
		{
			name:            "other_directory",
			path:            "/other",
			wantErr:         fs.ErrNotExist,
			wantMetadataErr: fs.ErrNotExist,
		},
		{
			name:            "missing_subdirectory",
			path:            "/user/nope",
			wantErr:         fs.ErrNotExist,
			wantMetadataErr: fs.ErrNotExist,
		},
	}
	// 2. 先读目录核对条目，再读元数据核对是目录还是不存在。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newMemoryFixture(t)
			if tt.setup != nil {
				tt.setup(t, fixture)
			}
			entries, err := fixture.workspace.ReadDirectory(t.Context(), tt.path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ReadDirectory() error = %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name)
				}
				if entries == nil || !slices.Equal(names, tt.wantNames) {
					t.Fatalf("ReadDirectory() names = %v (entries nil: %v), want %v", names, entries == nil, tt.wantNames)
				}
			}
			metadata, err := fixture.workspace.GetMetadata(t.Context(), tt.path, true)
			if tt.wantMetadataErr != nil {
				if !errors.Is(err, tt.wantMetadataErr) {
					t.Fatalf("GetMetadata() error = %v, want %v", err, tt.wantMetadataErr)
				}
				return
			}
			if err != nil || !metadata.IsDirectory {
				t.Fatalf("GetMetadata() = %+v, %v; want a directory", metadata, err)
			}
		})
	}
}

// TestMemoryWorkspaceUnsupported 验证记忆工作区不能启动进程、拨号、查主目录，创建时检查记忆根目录是绝对路径，并保存项目名。
func TestMemoryWorkspaceUnsupported(t *testing.T) {
	// 1. 列出不支持的方法，每个都应返回 errors.ErrUnsupported。
	fixture := newMemoryFixture(t)
	work := fixture.workspace
	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{
			name: "start_process",
			call: func(ctx context.Context) error {
				_, err := work.StartProcess(ctx, &workspace.ProcessRequest{Command: "true", Directory: "/"})
				return err
			},
		},
		{
			name: "dial",
			call: func(ctx context.Context) error {
				_, err := work.Dial(ctx, "tcp", "127.0.0.1:1")
				return err
			},
		},
		{
			name: "home_directory",
			call: func(ctx context.Context) error {
				_, err := work.GetHomeDirectory(ctx)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("error = %v, want %v", err, errors.ErrUnsupported)
			}
		})
	}
	// 2. 根目录是 /，没有 HTTP 代理。
	if root := work.GetRootDirectory(); root != "/" {
		t.Fatalf("GetRootDirectory() = %q, want /", root)
	}
	if proxy, err := work.GetHTTPProxy(nil); proxy != nil || err != nil {
		t.Fatalf("GetHTTPProxy() = %v, %v; want nil, nil", proxy, err)
	}
	// 3. 项目名是项目记忆目录相对记忆根目录的路径；记忆根目录不是绝对路径时创建失败。
	if name := fixture.workspace.GetProjectName(); name != filepath.Join("projects", "local", "-repo") {
		t.Fatalf("GetProjectName() = %q, want the project directory relative to the memory directory", name)
	}
	if _, err := workspace.NewMemoryWorkspace(workspace.NewMemoryWorkspaceOpt("memory", "projects/local/-repo")); err == nil {
		t.Fatal("NewMemoryWorkspace() accepted a relative memory directory")
	}
}
