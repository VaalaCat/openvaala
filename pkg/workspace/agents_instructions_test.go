package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestAgentsInstructions 验证 Git 根、文件目录及工作区外路径的规则读取。
func TestAgentsInstructions(t *testing.T) {
	// 1. 逐项创建规则和 Git 标记，读取后核对规则路径及正文。
	t.Setenv("HOME", t.TempDir())
	for _, tt := range []struct {
		name, git, target string
		want              []string
	}{
		{
			"git_directory",
			"dir",
			"",
			[]string{"AGENTS.md", "app/AGENTS.md"},
		},
		{
			"git_file",
			"file",
			"",
			[]string{"AGENTS.md", "app/AGENTS.md"},
		},
		{
			"no_git",
			"",
			"",
			[]string{"app/AGENTS.md"},
		},
		{
			"nested",
			"dir",
			"pkg/main.go",
			[]string{"app/AGENTS.md", "app/pkg/AGENTS.md"},
		},
		{
			"outside",
			"dir",
			"../other.go",
			nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "app")
			for _, path := range []string{"AGENTS.md", "app/AGENTS.md", "app/pkg/AGENTS.md"} {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(path), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.git == "dir" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.git == "file" {
				if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ignored"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(cwd))
			if err != nil {
				t.Fatal(err)
			}
			var files []workspace.AgentsInstructionFile
			if tt.target == "" {
				files, err = workspace.GetWorkspaceAgentsInstructions(t.Context(), work)
			} else {
				files, err = workspace.GetFileAgentsInstructions(t.Context(), work, tt.target)
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, file := range files {
				relative, _ := filepath.Rel(root, file.Path)
				got = append(got, relative)
				if file.Text != relative {
					t.Fatalf("wrong source: %+v", file)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("paths = %v, want %v", got, tt.want)
			}
		})
	}
}

// agentsHomeWorkspace 返回主目录查询错误，文件仍由真实工作区读取。
type agentsHomeWorkspace struct {
	workspace.Workspace
	homeError error
}

// GetHomeDirectory 返回测试指定的主目录查询错误。
func (w *agentsHomeWorkspace) GetHomeDirectory(context.Context) (string, error) {
	// 1. 返回预设的查询结果。
	return "", w.homeError
}

// TestGlobalAgentsInstructions 检查全局规则先于项目规则、只读取指定路径及读取失败时的降级。
func TestGlobalAgentsInstructions(t *testing.T) {
	// 1. 分别准备存在、缺失、不可读和主目录查询失败的全局规则。
	for _, tt := range []struct {
		name       string
		globalFile bool
		directory  bool
		homeError  error
	}{
		{name: "global_before_project", globalFile: true},
		{name: "missing_global"},
		{name: "unreadable_global", directory: true},
		{name: "home_unavailable", globalFile: true, homeError: os.ErrPermission},
		{name: "home_unsupported", globalFile: true, homeError: errors.ErrUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			globalPath := filepath.Join(home, ".agents", "AGENTS.md")
			projectPath := filepath.Join(project, "AGENTS.md")
			files := map[string]string{
				projectPath:                      "project rules",
				filepath.Join(home, "AGENTS.md"): "not global",
				filepath.Join(home, ".config", "openvaala", "AGENTS.md"): "not global",
				filepath.Join(home, ".claude", "CLAUDE.md"):              "not global",
			}
			if tt.globalFile {
				files[globalPath] = "global rules"
			}
			for path, text := range files {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.directory {
				if err := os.MkdirAll(globalPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(project))
			if err != nil {
				t.Fatal(err)
			}
			if tt.homeError != nil {
				work = &agentsHomeWorkspace{Workspace: work, homeError: tt.homeError}
			}
			// 2. 全局规则不可用时仍返回项目规则；读取成功时保留正文和全局标记。
			got, err := workspace.GetWorkspaceAgentsInstructions(t.Context(), work)
			if err != nil {
				t.Fatal(err)
			}
			var want []workspace.AgentsInstructionFile
			if tt.globalFile && tt.homeError == nil {
				want = append(want, workspace.AgentsInstructionFile{Path: globalPath, Text: "global rules", IsGlobal: true})
			}
			want = append(want, workspace.AgentsInstructionFile{Path: projectPath, Text: "project rules"})
			if !slices.Equal(got, want) {
				t.Fatalf("instructions = %+v, want %+v", got, want)
			}
		})
	}
}

// TestGlobalAgentsInstructionsExecutors 检查全局规则从对应工作区读取，SSH 使用远端主目录，virtual 跳过用户规则。
func TestGlobalAgentsInstructionsExecutors(t *testing.T) {
	// 1. 本机和 SSH 使用不同的主目录，虚拟工作区只准备项目规则。
	for _, kind := range workspaceKinds {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			f := newWorkspaceFixture(t, kind)
			if kind == "ssh" {
				// 测试 SFTP 服务的初始目录由测试进程的工作目录指定。
				home = t.TempDir()
				t.Chdir(home)
			}
			globalPath := filepath.Join(home, ".agents", "AGENTS.md")
			if err := os.MkdirAll(filepath.Dir(globalPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(globalPath, []byte("global "+kind), 0600); err != nil {
				t.Fatal(err)
			}
			projectPath := filepath.Join(f.root, "AGENTS.md")
			if err := f.workspace.WriteFile(t.Context(), projectPath, []byte("project "+kind), 0600, true); err != nil {
				t.Fatal(err)
			}
			// 2. 读取全部规则，验证文件来自对应环境、全局规则在前且作用域正确。
			got, err := workspace.GetWorkspaceAgentsInstructions(t.Context(), f.workspace)
			if err != nil {
				t.Fatal(err)
			}
			var want []workspace.AgentsInstructionFile
			if kind != "virtual" {
				want = append(want, workspace.AgentsInstructionFile{Path: globalPath, Text: "global " + kind, IsGlobal: true})
			}
			want = append(want, workspace.AgentsInstructionFile{Path: projectPath, Text: "project " + kind})
			if !slices.Equal(got, want) {
				t.Fatalf("instructions = %+v, want %+v", got, want)
			}
		})
	}
}
