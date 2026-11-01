package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestApplyPatch 验证补丁增删改及无效目标的处理。
func TestApplyPatch(t *testing.T) {
	// 1. 准备有效补丁、上下文不匹配、新增已存在文件、格式错误、路径越界和符号链接用例。
	tests := []*struct {
		name        string
		patch       string
		wantContent string
		wantDeleted bool
		wantAdded   bool
		wantError   bool
		errorCode   string
	}{
		{
			name: "add_update_delete",
			patch: `*** Begin Patch
*** Add File: dir/new.txt
+new
*** Update File: note.txt
@@
-old
+updated
*** Delete File: delete.txt
*** End Patch
`,
			wantContent: "updated\n",
			wantAdded:   true,
			wantDeleted: true,
		},
		{
			name: "mismatched_context_changes_nothing",
			patch: `*** Begin Patch
*** Add File: dir/new.txt
+new
*** Update File: note.txt
@@
-missing
+updated
*** End Patch
`,
			wantContent: "old\n",
			wantError:   true,
		},
		{
			name: "existing_add_changes_nothing",
			patch: `*** Begin Patch
*** Add File: note.txt
+replacement
*** End Patch
`,
			wantContent: "old\n",
			wantError:   true,
		},
		{
			name:        "malformed_patch_is_invalid_arguments",
			patch:       "*** Begin Patch\n*** Unknown: note.txt\n*** End Patch\n",
			wantContent: "old\n",
			wantError:   true,
			errorCode:   "invalid_arguments",
		},
		{
			name: "escaping_path_is_invalid_arguments",
			patch: `*** Begin Patch
*** Add File: ../escape.txt
+escape
*** End Patch
`,
			wantContent: "old\n",
			wantError:   true,
			errorCode:   "invalid_arguments",
		},
		{
			name: "symlink_target_rejected",
			patch: `*** Begin Patch
*** Update File: link.txt
@@
-old
+updated
*** End Patch
`,
			wantContent: "old\n",
			wantError:   true,
		},
	}
	// 2. 创建目标文件，执行补丁并核对实际文件和返回内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"note.txt", "delete.txt"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("old\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("note.txt", filepath.Join(root, "link.txt")); err != nil {
				t.Fatal(err)
			}
			tool, err := tools.NewApplyPatchTool(tools.NewApplyPatchToolOpt(newTestWorkspace(t, root)))
			if err != nil {
				t.Fatal(err)
			}
			if got := tool.GetDefinition().Description; got != "使用 Begin Patch 格式新增、按完整行上下文更新或删除工作目录内的普通文件；结果列出实际修改的路径。" {
				t.Fatalf("description = %q", got)
			}
			arguments, err := json.Marshal(map[string]string{"patch": tt.patch})
			if err != nil {
				t.Fatal(err)
			}
			call := &defs.ToolCall{
				CallID:    "patch",
				Name:      "apply_patch",
				Arguments: string(arguments),
			}
			result := runTool(t, tool, call)
			if (result.Error != nil) != tt.wantError {
				t.Fatalf("result = %+v", result)
			}
			if tt.errorCode != "" && result.Error.Code != tt.errorCode {
				t.Fatalf("error = %+v, want code %q", result.Error, tt.errorCode)
			}
			content, err := os.ReadFile(filepath.Join(root, "note.txt"))
			if err != nil || string(content) != tt.wantContent {
				t.Fatalf("content = %q, error = %v", content, err)
			}
			_, err = os.Stat(filepath.Join(root, "delete.txt"))
			if os.IsNotExist(err) != tt.wantDeleted {
				t.Fatalf("delete result = %v", err)
			}
			_, err = os.Stat(filepath.Join(root, "dir/new.txt"))
			if (err == nil) != tt.wantAdded {
				t.Fatalf("add result = %v", err)
			}
		})
	}
}

// TestApplyPatchChangeStartLines 验证结果只记录更新成功文件的原始起始行号。
func TestApplyPatchChangeStartLines(t *testing.T) {
	// 1. 准备多段更新、新增和部分写入失败的补丁。
	const notes = "one\ntwo\nthree\nfour\nfive\nsix\n"
	tests := []struct {
		name          string
		patch         string
		failWritePath string
		wantOutcome   string
		wantUpdated   []string
		want          map[string][]int
	}{
		{
			name:        "two_sections_in_one_file_start_where_their_context_matches",
			patch:       "*** Begin Patch\n*** Update File: notes.txt\n@@\n two\n-three\n+3\n@@\n five\n-six\n*** End Patch\n",
			wantOutcome: "applied",
			wantUpdated: []string{"notes.txt"},
			want:        map[string][]int{"notes.txt": {2, 5}},
		},
		{
			name:        "added_file_has_no_entry",
			patch:       "*** Begin Patch\n*** Add File: new.txt\n+new\n*** End Patch\n",
			wantOutcome: "applied",
		},
		{
			// 第二个文件写入失败：只有改成功的 notes.txt 有起始行。
			name:          "partially_applied_lists_only_the_applied_files",
			patch:         "*** Begin Patch\n*** Update File: notes.txt\n@@\n-one\n+1\n*** Update File: other.txt\n@@\n-one\n+1\n*** End Patch\n",
			failWritePath: "other.txt",
			wantOutcome:   "partially_applied",
			wantUpdated:   []string{"notes.txt"},
			want:          map[string][]int{"notes.txt": {1}},
		},
	}
	// 2. 创建目标文件，执行补丁并核对返回的 outcome、updated 和起始行号。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"notes.txt", "other.txt"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(notes), 0600); err != nil {
					t.Fatal(err)
				}
			}
			toolWorkspace := &failingWriteWorkspace{
				Workspace: newTestWorkspace(t, root),
				failPath:  tt.failWritePath,
			}
			tool, err := tools.NewApplyPatchTool(tools.NewApplyPatchToolOpt(toolWorkspace))
			if err != nil {
				t.Fatal(err)
			}
			arguments, err := json.Marshal(map[string]string{"patch": tt.patch})
			if err != nil {
				t.Fatal(err)
			}
			result := runTool(t, tool, &defs.ToolCall{
				CallID:    "patch",
				Name:      "apply_patch",
				Arguments: string(arguments),
			})
			var content struct {
				Outcome          string           `json:"outcome"`
				Updated          []string         `json:"updated"`
				ChangeStartLines map[string][]int `json:"change_start_lines"`
			}
			if err := json.Unmarshal(result.Content, &content); err != nil {
				t.Fatalf("result %s: %v", result.Content, err)
			}
			if content.Outcome != tt.wantOutcome || !reflect.DeepEqual(content.Updated, tt.wantUpdated) || !reflect.DeepEqual(content.ChangeStartLines, tt.want) {
				t.Fatalf("result = %s, want outcome %s, updated %v and change_start_lines %v", result.Content, tt.wantOutcome, tt.wantUpdated, tt.want)
			}
		})
	}
}

// failingWriteWorkspace 写入路径以 failPath 结尾的文件时返回错误，其余照常写入；failPath 为空时不失败。
type failingWriteWorkspace struct {
	workspace.Workspace
	failPath string
}

// WriteFile 模拟指定路径的文件写入失败。
func (w *failingWriteWorkspace) WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode, createOnly bool) error {
	// 1. 路径匹配指定后缀时返回写入失败，未匹配时交给真实工作区写入。
	if w.failPath != "" && strings.HasSuffix(path, w.failPath) {
		return errors.New("write failed")
	}
	return w.Workspace.WriteFile(ctx, path, content, mode, createOnly)
}

// TestBuiltinToolsEditWorkflow 验证本机和虚拟工作区中的创建、读取、补丁与执行流程。
func TestBuiltinToolsEditWorkflow(t *testing.T) {
	// 1. 分别创建两类工作区和文件、命令工具。
	for _, kind := range []string{"local", "virtual"} {
		t.Run(kind, func(t *testing.T) {
			toolWorkspace := newTestWorkspace(t, t.TempDir())
			if kind == "virtual" {
				virtualWorkspace, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", nil))
				if err != nil {
					t.Fatal(err)
				}
				toolWorkspace = virtualWorkspace
			}
			readFile, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(toolWorkspace))
			if err != nil {
				t.Fatal(err)
			}
			applyPatch, err := tools.NewApplyPatchTool(tools.NewApplyPatchToolOpt(toolWorkspace))
			if err != nil {
				t.Fatal(err)
			}
			instance := newCommandToolExecutor(t, startTerminalManager(t, nil), newCommandEnvironment(t, "local", toolWorkspace, readFile, applyPatch))
			steps := []*struct {
				name      string
				toolName  string
				arguments map[string]string
				wantText  string
			}{
				{
					name:      "create_project_with_bash",
					toolName:  "bash",
					arguments: map[string]string{"command": "printf 'printf old\\n' > greet.sh; printf obsolete > old.txt; printf created"},
					wantText:  "created",
				},
				{
					name:      "inspect_script",
					toolName:  "read_file",
					arguments: map[string]string{"path": "greet.sh"},
					wantText:  "printf old",
				},
				{
					name:     "edit_project",
					toolName: "apply_patch",
					arguments: map[string]string{"patch": `*** Begin Patch
*** Update File: greet.sh
@@
-printf old
+printf 'hello from edited script'
*** Add File: README.md
+Run bash greet.sh
*** Delete File: old.txt
*** End Patch
`},
					wantText: "applied",
				},
				{
					name:      "read_changed_script",
					toolName:  "read_file",
					arguments: map[string]string{"path": "greet.sh"},
					wantText:  "printf 'hello from edited script'",
				},
				{
					name:      "read_added_file",
					toolName:  "read_file",
					arguments: map[string]string{"path": "README.md"},
					wantText:  "Run bash greet.sh",
				},
				{
					name:      "execute_changed_script_and_check_deleted_file",
					toolName:  "bash",
					arguments: map[string]string{"command": "test ! -e old.txt && bash greet.sh"},
					wantText:  "hello from edited script",
				},
			}
			// 2. 依次创建项目、读文件、应用补丁，再执行改后的脚本。
			for _, step := range steps {
				if !t.Run(step.name, func(t *testing.T) {
					arguments, err := json.Marshal(step.arguments)
					if err != nil {
						t.Fatal(err)
					}
					result := runExecutorCall(t, instance, &defs.ToolCall{
						CallID:    step.name,
						Name:      step.toolName,
						Arguments: string(arguments),
					})
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					if !strings.Contains(string(result.Content), step.wantText) {
						t.Fatalf("result=%s, want %q", result.Content, step.wantText)
					}
				}) {
					return
				}
			}
		})
	}
}

// TestGetPatchFileChanges 验证补丁解析的文件顺序、行数和内容。
func TestGetPatchFileChanges(t *testing.T) {
	// 1. 准备增删改、多段更新及无效补丁用例，逐项核对解析结果。
	tests := []struct {
		name    string
		patch   string
		want    []tools.PatchFileChange
		wantErr bool
	}{
		{
			name:  "add",
			patch: "*** Begin Patch\n*** Add File: a.txt\n+one\n+two\n*** End Patch",
			want: []tools.PatchFileChange{{
				Path:       "a.txt",
				Kind:       tools.PatchFileAdd,
				AddedLines: 2,
				Content:    "one\ntwo\n",
			}},
		},
		{
			name:  "update",
			patch: "*** Begin Patch\n*** Update File: b.txt\n@@\n keep\n-old\n+new\n*** End Patch",
			want: []tools.PatchFileChange{{
				Path:         "b.txt",
				Kind:         tools.PatchFileUpdate,
				AddedLines:   1,
				DeletedLines: 1,
				Sections: []tools.PatchSection{{
					Header: "@@",
					Lines:  []string{" keep", "-old", "+new"},
				}},
			}},
		},
		{
			name:  "delete",
			patch: "*** Begin Patch\n*** Delete File: c.txt\n*** End Patch",
			want: []tools.PatchFileChange{{
				Path: "c.txt",
				Kind: tools.PatchFileDelete,
			}},
		},
		{
			name:  "add_file_counts_content_lines",
			patch: "*** Begin Patch\n*** Add File: docs/a.md\n+第一行\n+第二行\n*** End Patch\n",
			want: []tools.PatchFileChange{{
				Path:       "docs/a.md",
				Kind:       tools.PatchFileAdd,
				AddedLines: 2,
				Content:    "第一行\n第二行\n",
			}},
		},
		{
			name: "update_keeps_every_section_in_order",
			patch: "*** Begin Patch\n*** Update File: ./internal/tui/tui.go\n@@\n context\n-old one\n-old two\n+new one\n context2\n" +
				"@@ func main\n-x\n+y\n+z\n*** End Patch\n",
			want: []tools.PatchFileChange{{
				Path:         "internal/tui/tui.go",
				Kind:         tools.PatchFileUpdate,
				AddedLines:   3,
				DeletedLines: 3,
				Sections: []tools.PatchSection{
					{
						Header: "@@",
						Lines:  []string{" context", "-old one", "-old two", "+new one", " context2"},
					},
					{
						Header: "@@ func main",
						Lines:  []string{"-x", "+y", "+z"},
					},
				},
			}},
		},
		{
			name:  "multiple_files_keep_patch_order",
			patch: "*** Begin Patch\n*** Delete File: b.txt\n*** Add File: a.txt\n+x\n*** Update File: c.txt\n@@\n-1\n+2\n*** End Patch\n",
			want: []tools.PatchFileChange{
				{
					Path: "b.txt",
					Kind: tools.PatchFileDelete,
				},
				{
					Path:       "a.txt",
					Kind:       tools.PatchFileAdd,
					AddedLines: 1,
					Content:    "x\n",
				},
				{
					Path:         "c.txt",
					Kind:         tools.PatchFileUpdate,
					AddedLines:   1,
					DeletedLines: 1,
					Sections: []tools.PatchSection{{
						Header: "@@",
						Lines:  []string{"-1", "+2"},
					}},
				},
			},
		},
		{
			name:    "not_a_patch",
			patch:   "hello",
			wantErr: true,
		},
		{
			name:    "empty",
			patch:   "",
			wantErr: true,
		},
		{
			name:    "absolute_path_rejected",
			patch:   "*** Begin Patch\n*** Delete File: /etc/passwd\n*** End Patch\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tools.GetPatchFileChanges(tt.patch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("changes = %+v, want %+v", got, tt.want)
			}
		})
	}
}
