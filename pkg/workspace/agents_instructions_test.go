package workspace_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestAgentsInstructions 验证 Git 根、文件目录及工作区外路径的规则读取。
func TestAgentsInstructions(t *testing.T) {
	// 1. 逐项创建规则和 Git 标记，读取后核对规则路径及正文。
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
