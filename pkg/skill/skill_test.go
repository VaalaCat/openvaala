package skill_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// skillFile 生成带 YAML 文件头和正文的合法 Skill 文件。
func skillFile(name, description string) string {
	// 1. 拼接名称、描述、license 和正文。
	return "---\nname: " + name + "\ndescription: " + description + "\nlicense: MIT\n---\n# " + name + "\n正文\n"
}

// TestLoadSkills 核对目录优先级、名称校验、插件前缀和符号链接。
func TestLoadSkills(t *testing.T) {
	// 1. 逐例创建 Skill 文件和链接，按目录优先级加载并比较名称、描述、来源及错误。
	for _, tt := range []struct {
		name     string
		files    map[string]string // 相对临时目录的路径 → 内容
		symlinks map[string]string // 链接位置 → 目标，均相对临时目录
		want     []string          // name|description|相对路径
		wantErr  string
	}{
		{
			name: "dedup_prefers_first_directory",
			files: map[string]string{
				"project/review/SKILL.md": skillFile("review", "项目审查"),
				"private/review/SKILL.md": skillFile("review", "私有审查"),
				"private/alpha/SKILL.md":  skillFile("alpha", "  多行\n  描述  "),
			},
			want: []string{"alpha|多行 描述|private/alpha/SKILL.md", "review|项目审查|project/review/SKILL.md"},
		},
		{
			name: "missing_directories_skipped",
		},
		{
			name:    "strict_invalid_fails",
			files:   map[string]string{"private/broken/SKILL.md": "# 没有 frontmatter\n"},
			wantErr: filepath.Join("private", "broken", "SKILL.md"),
		},
		{
			name: "external_invalid_skipped",
			files: map[string]string{
				"project/broken/SKILL.md": "---\nname: broken\n",
				"project/empty/SKILL.md":  skillFile("empty", "  "),
				"project/good/SKILL.md":   skillFile("good", "可用"),
			},
			want: []string{"good|可用|project/good/SKILL.md"},
		},
		{
			name:  "frontmatter_name_used",
			files: map[string]string{"private/foo/SKILL.md": skillFile("bar", "使用声明名称")},
			want:  []string{"bar|使用声明名称|private/foo/SKILL.md"},
		},
		{
			name:  "missing_name_uses_directory",
			files: map[string]string{"private/review/SKILL.md": "---\ndescription: 审查代码\n---\nReview.\n"},
			want:  []string{"review|审查代码|private/review/SKILL.md"},
		},
		{
			name: "nested_skills_deduplicate_by_declared_name",
			files: map[string]string{
				"plugin/group/first/SKILL.md": skillFile("review", "先出现"),
				"plugin/group/next/SKILL.md":  skillFile("review", "重复"),
			},
			want: []string{"demo:review|先出现|plugin/group/first/SKILL.md"},
		},
		{
			name: "skill_references_not_scanned_as_skills",
			files: map[string]string{
				"plugin/review/SKILL.md":                 skillFile("review", "审查"),
				"plugin/review/references/demo/SKILL.md": skillFile("demo", "示例"),
			},
			want: []string{"demo:review|审查|plugin/review/SKILL.md"},
		},
		{
			name:    "uppercase_name_is_invalid",
			files:   map[string]string{"private/Bad/SKILL.md": skillFile("Bad", "大写")},
			wantErr: "lowercase",
		},
		{
			name: "non_skill_entries_ignored",
			files: map[string]string{
				"private/notes/README.md": "不是 skill",
				"private/file.txt":        "普通文件",
			},
		},
		{
			name:  "plugin_directory_prefixes_names",
			files: map[string]string{"plugin/greet/SKILL.md": skillFile("greet", "插件问候")},
			want:  []string{"demo:greet|插件问候|plugin/greet/SKILL.md"},
		},
		{
			name: "plugin_skill_does_not_shadow_same_plain_name",
			files: map[string]string{
				"project/greet/SKILL.md": skillFile("greet", "项目问候"),
				"plugin/greet/SKILL.md":  skillFile("greet", "插件问候"),
			},
			want: []string{"demo:greet|插件问候|plugin/greet/SKILL.md", "greet|项目问候|project/greet/SKILL.md"},
		},
		{
			name:    "prefixed_frontmatter_name_is_invalid",
			files:   map[string]string{"private/demo:greet/SKILL.md": skillFile("demo:greet", "自带前缀")},
			wantErr: "lowercase",
		},
		{
			name:     "symlinked_skill_keeps_scanned_path",
			files:    map[string]string{"elsewhere/linked/SKILL.md": skillFile("linked", "链接")},
			symlinks: map[string]string{"project/linked": "elsewhere/linked"},
			want:     []string{"linked|链接|project/linked/SKILL.md"},
		},
		{
			name:     "symlinked_container_does_not_loop",
			files:    map[string]string{"project/review/SKILL.md": skillFile("review", "审查")},
			symlinks: map[string]string{"project/loop": "project"},
			want:     []string{"review|审查|project/review/SKILL.md"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for path, content := range tt.files {
				fullPath := filepath.Join(base, path)
				if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for link, target := range tt.symlinks {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(base, link)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, target), filepath.Join(base, link)); err != nil {
					t.Fatal(err)
				}
			}
			localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(base))
			if err != nil {
				t.Fatal(err)
			}
			// 优先级：project（外部）→ private（私有，严格）→ plugin（插件，名字带 demo: 前缀）→ absent（不存在）。
			skills, err := skill.LoadSkills(t.Context(), []skill.SkillDirectory{
				{
					Workspace: localWorkspace,
					Path:      filepath.Join(base, "project"),
				},
				{
					Workspace: localWorkspace,
					Path:      filepath.Join(base, "private"),
					Strict:    true,
				},
				{
					Workspace:  localWorkspace,
					Path:       filepath.Join(base, "plugin"),
					NamePrefix: "demo:",
				},
				{
					Workspace: localWorkspace,
					Path:      filepath.Join(base, "absent"),
				},
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadSkills error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, loaded := range skills {
				relative, err := filepath.Rel(base, loaded.Path)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, loaded.Name+"|"+loaded.Description+"|"+filepath.ToSlash(relative))
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("skills = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoadSkillsFromVirtualWorkspace 核对虚拟工作区中的 Skill 目录。
func TestLoadSkillsFromVirtualWorkspace(t *testing.T) {
	// 1. 将 Skill 文件放入虚拟工作区，加载后比较名称、执行环境和文件路径。
	virtualWorkspace, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", map[string]*workspace.VirtualFile{
		"/skills/demo/SKILL.md":   {Content: []byte(skillFile("demo", "虚拟 skill"))},
		"/skills/notes/README.md": {Content: []byte("不是 skill")},
	}))
	if err != nil {
		t.Fatal(err)
	}
	skills, err := skill.LoadSkills(t.Context(), []skill.SkillDirectory{
		{
			Workspace:  virtualWorkspace,
			ExecutorID: "openvaala",
			Path:       "/skills",
			Strict:     true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "demo" || skills[0].Path != "/skills/demo/SKILL.md" || skills[0].Executor != "openvaala" {
		t.Fatalf("skills = %+v", skills)
	}
}

// TestLoadSkillsLogsSkipped 核对 Skill 加载的日志和错误。
func TestLoadSkillsLogsSkipped(t *testing.T) {
	// 1. 准备缺失文件、无效名称和错误文件头样例。
	const executorID = "executor-1"
	tests := []struct {
		name        string
		files       map[string]string // 相对临时目录的路径 → 内容
		strict      bool
		wantErr     bool
		wantMessage string // 为空表示不应写出任何 skip 日志
		wantPath    string // 相对临时目录
	}{
		{
			name:        "invalid_skill_file",
			files:       map[string]string{"project/broken/SKILL.md": "---\nname: broken\n"},
			wantMessage: "skill skipped",
			wantPath:    "project/broken/SKILL.md",
		},
		{
			name:        "unreadable_skill_directory",
			files:       map[string]string{"project": "不是目录"},
			wantMessage: "skill directory skipped",
			wantPath:    "project",
		},
		{
			name: "missing_directory_not_logged",
		},
		{
			name:    "strict_invalid_skill_not_logged",
			files:   map[string]string{"project/broken/SKILL.md": "---\nname: broken\n"},
			strict:  true,
			wantErr: true,
		},
		{
			name:    "strict_unreadable_directory_not_logged",
			files:   map[string]string{"project": "不是目录"},
			strict:  true,
			wantErr: true,
		},
	}
	// 2. 逐例加载工作区中的 Skill，核对非严格目录的跳过日志和严格目录的错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for path, content := range tt.files {
				fullPath := filepath.Join(base, path)
				if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(base))
			if err != nil {
				t.Fatal(err)
			}
			p := telemetrytest.NewPipeline(t)
			ctx := telemetry.ContextWithTelemetry(t.Context(), p.Telemetry)
			skills, err := skill.LoadSkills(ctx, []skill.SkillDirectory{{
				Workspace:  localWorkspace,
				ExecutorID: executorID,
				Path:       filepath.Join(base, "project"),
				Strict:     tt.strict,
			}})
			if (err != nil) != tt.wantErr || len(skills) != 0 {
				t.Fatalf("LoadSkills() = %v, %v; want no skills, error %t", skills, err, tt.wantErr)
			}
			if tt.wantMessage == "" {
				for _, message := range []string{"skill skipped", "skill directory skipped"} {
					if records := p.GetLogRecords(t, message); len(records) != 0 {
						t.Fatalf("%q logs = %v, want none", message, records)
					}
				}
				return
			}
			records := p.GetLogRecords(t, tt.wantMessage)
			if len(records) != 1 || records[0]["path"] != filepath.Join(base, tt.wantPath) ||
				records[0]["openvaala.executor.id"] != executorID || records[0]["level"] != "WARN" {
				t.Fatalf("%s logs = %v", tt.wantMessage, records)
			}
		})
	}
}

// TestSkillGetContent 核对读到的是 Skill 文件当前的完整内容。
func TestSkillGetContent(t *testing.T) {
	// 1. 加载 Skill 后追加正文，再读取并比较完整内容，包含超过目录扫描大小上限的样例。
	for _, size := range []int{20, (1 << 20) + 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "review")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(path, []byte(skillFile("review", "test")), 0600); err != nil {
				t.Fatal(err)
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := skill.LoadSkills(t.Context(), []skill.SkillDirectory{{
				Workspace: work,
				Path:      root,
				Strict:    true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			want := skillFile("review", "test") + strings.Repeat("x", size)
			if err := os.WriteFile(path, []byte(want), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := catalog[0].GetContent(t.Context())
			if err != nil || got != want {
				t.Fatalf("content bytes %d, want %d; %v", len(got), len(want), err)
			}
		})
	}
}
