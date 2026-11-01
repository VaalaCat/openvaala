package prompt_test

import (
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// TestFormatSkillInstructions 核对 Skill 目录的字段和原有顺序。
func TestFormatSkillInstructions(t *testing.T) {
	// 1. 按传入顺序生成 Skill 目录，核对名称、描述、路径和执行环境。
	for _, tt := range []struct {
		name   string
		skills []*skill.Skill
		want   string
	}{
		{name: "empty"},
		{
			name: "listed_in_given_order",
			skills: []*skill.Skill{
				{
					Name:        "zeta",
					Description: "第一项",
					Path:        "/skills/zeta/SKILL.md",
				},
				{
					Name:        "beta",
					Description: "第二项",
					Path:        "/skills/beta/SKILL.md",
					Executor:    "openvaala",
				},
			},
			want: "<openvaala-skills>\n" +
				"以下 skill 为特定任务提供说明。任务符合某项描述时，先用 read_file 完整读取它的 SKILL.md 再行动（条目带 executor 时，调用 read_file 传入同一 executor）；SKILL.md 中的相对路径以其所在目录为基准。\n" +
				"- zeta: 第一项\n  path: /skills/zeta/SKILL.md\n" +
				"- beta: 第二项\n  path: /skills/beta/SKILL.md\n  executor: openvaala\n" +
				"</openvaala-skills>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.skills)
			if got := prompt.FormatSkillInstructions(tt.skills); got != tt.want {
				t.Fatalf("FormatSkillInstructions =\n%s\nwant\n%s", got, tt.want)
			}
			if !slices.Equal(before, tt.skills) {
				t.Fatal("formatter changed skill order")
			}
		})
	}
}

// TestFormatExecutorInstructions 核对执行环境排序和默认标记。
func TestFormatExecutorInstructions(t *testing.T) {
	// 1. 生成执行环境目录，核对 ID 排序、默认标记和先读后改提示。
	for _, tt := range []struct {
		name      string
		executors []prompt.ExecutorEntry
		want      string
	}{
		{name: "empty"},
		{
			name: "single_executor",
			executors: []prompt.ExecutorEntry{{
				ID:                 "local",
				Description:        "本机",
				WorkspaceDirectory: "/work",
			}},
			want: "<openvaala-executors>\n" +
				"Executor 是工具实际运行的环境（一台机器或一个虚拟环境），各自有独立的文件系统和命令行：读写文件和运行命令只作用于所选 Executor，不同 Executor 之间的文件互不可见。可在多个 Executor 上运行的工具带有 executor 参数，用它选择执行位置；省略时使用参数说明中的默认 Executor。\n" +
				"修改文件前，先用 read_file 读取目标文件，收到结果后再修改。\n" +
				"- local（默认）: 本机\n  workspace: /work\n" +
				"</openvaala-executors>",
		},
		{
			name: "sorted_with_default",
			executors: []prompt.ExecutorEntry{
				{
					ID:                 "zeta",
					Description:        "默认环境",
					WorkspaceDirectory: "/zeta",
				},
				{
					ID:                 "alpha",
					Description:        "试验环境",
					WorkspaceDirectory: "/alpha",
				},
			},
			want: "<openvaala-executors>\n" +
				"Executor 是工具实际运行的环境（一台机器或一个虚拟环境），各自有独立的文件系统和命令行：读写文件和运行命令只作用于所选 Executor，不同 Executor 之间的文件互不可见。可在多个 Executor 上运行的工具带有 executor 参数，用它选择执行位置；省略时使用参数说明中的默认 Executor。\n" +
				"修改文件前，先用 read_file 读取目标文件，收到结果后再修改。\n" +
				"- alpha: 试验环境\n  workspace: /alpha\n" +
				"- zeta（默认）: 默认环境\n  workspace: /zeta\n" +
				"</openvaala-executors>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.executors)
			if got := prompt.FormatExecutorInstructions(tt.executors); got != tt.want {
				t.Fatalf("FormatExecutorInstructions =\n%s\nwant\n%s", got, tt.want)
			}
			if !slices.Equal(before, tt.executors) {
				t.Fatal("formatter changed executor order")
			}
		})
	}
}

// TestFormatSessionIdentity 核对 Session 身份字段。
func TestFormatSessionIdentity(t *testing.T) {
	// 1. 生成身份块，核对 Session ID、任务名和父 Session ID。
	for _, tt := range []struct {
		name    string
		session defs.Session
		want    string
	}{
		{
			name:    "root",
			session: defs.Session{ID: "root"},
			want:    "<openvaala-session>\nSessionID: root\nTaskName: \nParentSessionID: \n</openvaala-session>",
		},
		{
			name: "child_with_literal_text",
			session: defs.Session{
				ID:              "child",
				TaskName:        "100% <review>\nnext",
				ParentSessionID: "root",
			},
			want: "<openvaala-session>\nSessionID: child\nTaskName: 100% <review>\nnext\nParentSessionID: root\n</openvaala-session>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := prompt.FormatSessionIdentity(tt.session); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
