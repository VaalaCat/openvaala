package conversation

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestNewConversationConfig 检查装配配置时 Executor 的顺序和目录、设置校验、Skill 来源、客户端环境变量和 SSH 工作区释放。
func TestNewConversationConfig(t *testing.T) {
	// 1. 列出 Executor 排序、目录、设置校验、Skill、客户端环境变量和工作区释放场景。
	tests := []struct {
		name string
		run  func(t *testing.T, f *environmentFixture)
	}{
		{
			name: "fills_local_directory",
			run: func(t *testing.T, f *environmentFixture) {
				c := f.create(t, defs.ExecutorSettings{ExecutorIDs: []string{"local"}})
				if got := f.getRecord(t, c).ExecutorSettings.WorkspaceDirectories["local"]; got != f.localDirectory {
					t.Fatalf("saved local directory = %q, want the local definition's %q", got, f.localDirectory)
				}
			},
		},
		{
			name: "executors_follow_definition_order",
			run: func(t *testing.T, f *environmentFixture) {
				if got := getExecutorIDs(t, f.create(t, defs.ExecutorSettings{})); !slices.Equal(got, []string{"local", "openvaala", "alpha", "extra"}) {
					t.Fatalf("executors = %v, want local, openvaala, then the configured order", got)
				}
			},
		},
		{
			name: "executor_ids_keep_definition_order",
			run: func(t *testing.T, f *environmentFixture) {
				if got := getExecutorIDs(t, f.create(t, defs.ExecutorSettings{ExecutorIDs: []string{"extra", "local"}})); !slices.Equal(got, []string{"local", "extra"}) {
					t.Fatalf("executors = %v, want local, extra", got)
				}
			},
		},
		{
			name: "default_executor_first",
			run: func(t *testing.T, f *environmentFixture) {
				if got := getExecutorIDs(t, f.create(t, defs.ExecutorSettings{DefaultExecutorID: "extra"})); !slices.Equal(got, []string{"extra", "local", "openvaala", "alpha"}) {
					t.Fatalf("executors = %v, want extra first and the rest unchanged", got)
				}
			},
		},
		{
			name: "settings_directory_overrides_definition",
			run: func(t *testing.T, f *environmentFixture) {
				directory := t.TempDir()
				c := f.create(t, defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"extra": directory, "alpha": "/ignored"}})
				for _, executor := range getRootDetails(t, c).Executors {
					want := map[string]string{"local": f.localDirectory, "openvaala": "/workspace", "alpha": "/workspace", "extra": directory}[executor.ID]
					if executor.WorkspaceDirectory != want {
						t.Fatalf("executor %s directory = %q, want %q (virtual ignores the settings)", executor.ID, executor.WorkspaceDirectory, want)
					}
				}
			},
		},
		{
			name: "unknown_executor_rejected",
			run: func(t *testing.T, f *environmentFixture) {
				_, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{ExecutorIDs: []string{"local", "missing"}}, defs.ExecutorSettings{}, f.newOpt())
				if !errors.Is(err, ErrInvalidExecutorSettings) || !strings.Contains(err.Error(), `unknown executor "missing"`) {
					t.Fatalf("CreateConversation() error = %v, want ErrInvalidExecutorSettings for missing", err)
				}
			},
		},
		{
			name: "stored_unknown_executor_warns",
			run: func(t *testing.T, f *environmentFixture) {
				pipeline := telemetrytest.NewPipeline(t)
				ctx := telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry)
				settings := defs.ExecutorSettings{ExecutorIDs: []string{"local", "missing"}}
				c, err := f.reopen(ctx, t, settings, settings)
				if err != nil {
					t.Fatalf("GetConversation() error = %v, want the stored unknown executor kept", err)
				}
				if got := getExecutorIDs(t, c); !slices.Equal(got, []string{"local"}) {
					t.Fatalf("executors = %v, want only local", got)
				}
				records := pipeline.GetLogRecords(t, "executor in conversation record not found")
				if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["openvaala.executor.id"] != "missing" {
					t.Fatalf("warn records = %v, want one for missing", records)
				}
			},
		},
		{
			name: "stored_unknown_default_falls_back",
			run: func(t *testing.T, f *environmentFixture) {
				settings := defs.ExecutorSettings{
					DefaultExecutorID: "gone",
					ExecutorIDs:       []string{"extra", "gone"},
				}
				c, err := f.reopen(t.Context(), t, settings, settings)
				if err != nil {
					t.Fatal(err)
				}
				if got := getExecutorIDs(t, c); !slices.Equal(got, []string{"extra"}) {
					t.Fatalf("executors = %v, want extra", got)
				}
			},
		},
		{
			// executor_ids 里的 Executor 全被删出配置：没有默认 Executor，对话照样能打开，Skill 只有共用的。
			name: "stored_scope_without_executors_keeps_shared_skills",
			run: func(t *testing.T, f *environmentFixture) {
				writeTestSkills(t, filepath.Join(f.localDirectory, ".agents", "skills"), "project-skill")
				f.definitions.Skills = []*skill.Skill{{
					Name:        "shared-skill",
					Description: "共用",
					Path:        "/shared/shared-skill/SKILL.md",
				}}
				settings := defs.ExecutorSettings{ExecutorIDs: []string{"gone"}}
				c, err := f.reopen(t.Context(), t, settings, settings)
				if err != nil {
					t.Fatal(err)
				}
				if got, skills := getExecutorIDs(t, c), getSkillPaths(c); len(got) != 0 || !reflect.DeepEqual(skills, map[string]string{"shared-skill": "/shared/shared-skill/SKILL.md"}) {
					t.Fatalf("executors = %v, skills = %v; want none and only the shared skill", got, skills)
				}
			},
		},
		{
			name: "default_not_available",
			run: func(t *testing.T, f *environmentFixture) {
				settings := defs.ExecutorSettings{
					DefaultExecutorID: "openvaala",
					ExecutorIDs:       []string{"local"},
				}
				_, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, settings, defs.ExecutorSettings{}, f.newOpt())
				if !errors.Is(err, ErrInvalidExecutorSettings) || !strings.Contains(err.Error(), `default executor "openvaala" is not in executor_ids`) {
					t.Fatalf("CreateConversation() error = %v, want ErrInvalidExecutorSettings for the default executor", err)
				}
			},
		},
		{
			name: "default_executor_missing",
			run: func(t *testing.T, f *environmentFixture) {
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": filepath.Join(t.TempDir(), "missing")}}
				_, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, settings, defs.ExecutorSettings{}, f.newOpt())
				if err == nil || !strings.HasPrefix(err.Error(), `executor "local": `) {
					t.Fatalf("CreateConversation() error = %v, want one starting with executor \"local\": ", err)
				}
			},
		},
		{
			name: "relative_local_directory_rejected",
			run: func(t *testing.T, f *environmentFixture) {
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "relative/project"}}
				if _, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, settings, defs.ExecutorSettings{}, f.newOpt()); err == nil {
					t.Fatal("relative local directory was accepted")
				}
			},
		},
		{
			// 项目目录 > 共用 > 主目录：same 三处都有取项目目录的，shared 只在共用和主目录里取共用的。
			name: "skill_order",
			run: func(t *testing.T, f *environmentFixture) {
				projectSkills := filepath.Join(f.localDirectory, ".agents", "skills")
				writeTestSkills(t, projectSkills, "same")
				writeTestSkills(t, filepath.Join(f.home, ".agents", "skills"), "same", "shared", "home-only")
				f.definitions.Skills = []*skill.Skill{
					{
						Name:        "same",
						Description: "共用",
						Path:        "/shared/same/SKILL.md",
					},
					{
						Name:        "shared",
						Description: "共用",
						Path:        "/shared/shared/SKILL.md",
					},
				}
				got := getSkillPaths(f.create(t, defs.ExecutorSettings{}))
				want := map[string]string{
					"same":      filepath.Join(projectSkills, "same", "SKILL.md"),
					"shared":    "/shared/shared/SKILL.md",
					"home-only": filepath.Join(f.home, ".agents", "skills", "home-only", "SKILL.md"),
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("skills = %v, want %v", got, want)
				}
			},
		},
		{
			// 项目目录和主目录的 Skill 都经默认 Executor 读取，标的是它的 ID。
			name: "skills_from_default_executor",
			run: func(t *testing.T, f *environmentFixture) {
				writeTestSkills(t, filepath.Join(f.extraDirectory, ".claude", "skills"), "project-skill")
				writeTestSkills(t, filepath.Join(f.localDirectory, ".agents", "skills"), "local-project-skill")
				writeTestSkills(t, filepath.Join(f.home, ".claude", "skills"), "home-skill")
				c := f.create(t, defs.ExecutorSettings{DefaultExecutorID: "extra"})
				got := make(map[string]string)
				for _, info := range c.GetSkills() {
					got[info.Name] = info.Executor
				}
				if want := map[string]string{"project-skill": "extra", "home-skill": "extra"}; !reflect.DeepEqual(got, want) {
					t.Fatalf("skill executors = %v, want %v", got, want)
				}
			},
		},
		{
			// builtin 没有用户主目录，以它为默认时跳过主目录的 Skill，不算失败。
			name: "builtin_default_skips_home",
			run: func(t *testing.T, f *environmentFixture) {
				writeTestSkills(t, filepath.Join(f.home, ".agents", "skills"), "home-skill")
				if got := getSkillPaths(f.create(t, defs.ExecutorSettings{DefaultExecutorID: "openvaala"})); len(got) != 0 {
					t.Fatalf("skills = %v, want none", got)
				}
			},
		},
		{
			name: "ephemeral_has_no_executors",
			run: func(t *testing.T, f *environmentFixture) {
				f.definitions.Skills = []*skill.Skill{{
					Name:        "shared",
					Description: "共用",
					Path:        "/shared/shared/SKILL.md",
				}}
				opt := f.newOpt()
				opt.Ephemeral = true
				c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
				if err != nil {
					t.Fatal(err)
				}
				if executors, skills := getRootDetails(t, c).Executors, c.GetSkills(); len(executors) != 0 || len(skills) != 0 {
					t.Fatalf("executors = %+v, skills = %+v; want none", executors, skills)
				}
			},
		},
		{
			name: "client_environment_reaches_local_commands",
			run: func(t *testing.T, f *environmentFixture) {
				t.Setenv("TOOL_ENV_MARK", "parent")
				if got := f.runEcho(t, []string{"TOOL_ENV_MARK=first"}); !strings.Contains(got, "first") || strings.Contains(got, "parent") {
					t.Fatalf("bash result = %s, want the client environment first", got)
				}
			},
		},
		{
			name: "nil_environment_inherits_process",
			run: func(t *testing.T, f *environmentFixture) {
				t.Setenv("TOOL_ENV_MARK", "parent")
				if got := f.runEcho(t, nil); !strings.Contains(got, "parent") {
					t.Fatalf("bash result = %s, want the process environment parent", got)
				}
			},
		},
		{
			name: "virtual_files_per_conversation",
			run: func(t *testing.T, f *environmentFixture) {
				f.definitions.Executors = newTestExecutors("scratch")
				first := startTestConversation(t, f.store, f.model, f.create(t, defs.ExecutorSettings{}))
				second := startTestConversation(t, f.store, f.model, f.create(t, defs.ExecutorSettings{}))
				patch := `{"patch":"*** Begin Patch\n*** Add File: a.txt\n+first\n*** End Patch"}`
				if result := f.runTool(t, first, "apply_patch", patch); result.Error != nil {
					t.Fatalf("apply_patch error = %+v", result.Error)
				}
				if result := f.runTool(t, first, "read_file", `{"path":"/workspace/scratch/a.txt"}`); result.Error != nil || !strings.Contains(string(result.Content), "first") {
					t.Fatalf("first conversation read_file = %s %+v, want its own file", result.Content, result.Error)
				}
				if result := f.runTool(t, second, "read_file", `{"path":"/workspace/scratch/a.txt"}`); result.Error == nil {
					t.Fatalf("second conversation read_file = %s, want the file missing", result.Content)
				}
			},
		},
		{
			name: "run_with_canceled_context_releases",
			run: func(t *testing.T, f *environmentFixture) {
				server := newSSHTestServer(t)
				f.definitions.Executors = []*defs.ExecutorDefinition{{
					ID:                 "remote",
					Kind:               defs.ExecutorKindSSH,
					Description:        "远端",
					WorkspaceDirectory: t.TempDir(),
					SSH: &defs.SSHExecutorDefinition{
						Address:      "test@" + server.address,
						IdentityFile: server.identityFile,
					},
				}}
				canceledCtx, cancel := context.WithCancel(t.Context())
				cancel()
				created := f.create(t, defs.ExecutorSettings{})
				if err := created.Run(canceledCtx, nil); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() of the created conversation = %v, want context.Canceled", err)
				}
				server.waitOpenConnections(t, 0)
				reopened, err := reopenTestConversation(t, created, f.newOpt())
				if err != nil {
					t.Fatal(err)
				}
				server.waitOpenConnections(t, 1)
				events := make(chan ConversationEvent)
				start := time.Now()
				if err := reopened.Run(canceledCtx, events); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
					t.Fatalf("Run() = %v after %v, want context.Canceled at once", err, time.Since(start))
				}
				if _, open := <-events; open {
					t.Fatal("events still open after Run returned")
				}
				server.waitOpenConnections(t, 0)
				if accepted := server.acceptedConnections.Load(); accepted != 2 {
					t.Fatalf("accepted SSH connections = %d, want one per assembly", accepted)
				}
			},
		},
	}
	// 2. 每个场景用独立的目录和存储执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newEnvironmentFixture(t))
		})
	}
}

// TestResolveWorkspaceDirectory 检查 ResolveWorkspaceDirectory 展开 ~、接相对路径、清理 . 和 ..，以及无效目录、Executor 和 SSH 连接失败的错误。
func TestResolveWorkspaceDirectory(t *testing.T) {
	// 1. 列出主目录展开、相对目录、软链接、无效目录和无效 Executor 场景。
	tests := []struct {
		name                                 string
		executorID, directory, baseDirectory string
		// want 是解析出的目录；wantError 非空时期望带 ErrInvalidExecutorSettings 的这条错误；wantConnectError 非空时期望
		// 不带 ErrInvalidExecutorSettings、包含这段文字的错误。{home}、{local}、{extra} 换成临时 HOME 和两个 local 种类 Executor 的根目录。
		want, wantError, wantConnectError string
	}{
		{
			name:       "tilde_is_home",
			executorID: "local",
			directory:  "~",
			want:       "{home}",
		},
		{
			name:       "tilde_slash_is_under_home",
			executorID: "local",
			directory:  "~/proj",
			want:       "{home}/proj",
		},
		{
			name:          "parent_of_base_directory",
			executorID:    "local",
			directory:     "../../proj",
			baseDirectory: "{home}/proj/sub",
			want:          "{home}/proj",
		},
		{
			name:       "relative_to_default_directory",
			executorID: "local",
			directory:  "sub",
			want:       "{local}/sub",
		},
		{
			name:          "absolute",
			executorID:    "local",
			directory:     "{home}/proj/sub",
			baseDirectory: "{local}",
			want:          "{home}/proj/sub",
		},
		{
			name:          "dot_segments_cleaned",
			executorID:    "local",
			directory:     "./sub/../sub/.",
			baseDirectory: "{home}/proj",
			want:          "{home}/proj/sub",
		},
		{
			name:          "symlink_not_resolved",
			executorID:    "local",
			directory:     "link",
			baseDirectory: "{home}",
			want:          "{home}/link",
		},
		{
			name:          "file_is_not_a_directory",
			executorID:    "local",
			directory:     "file",
			baseDirectory: "{home}/proj",
			wantError:     "invalid executor settings: not a directory: {home}/proj/file",
		},
		{
			name:          "missing_is_not_a_directory",
			executorID:    "local",
			directory:     "missing",
			baseDirectory: "{home}/proj",
			wantError:     "invalid executor settings: not a directory: {home}/proj/missing",
		},
		{
			name:       "configured_local_default_root",
			executorID: "extra",
			directory:  "sub",
			want:       "{extra}/sub",
		},
		{
			name:       "configured_local_tilde",
			executorID: "extra",
			directory:  "~/proj",
			want:       "{home}/proj",
		},
		{
			name:       "builtin_cannot_change_directory",
			executorID: "openvaala",
			directory:  "/x",
			wantError:  `invalid executor settings: executor "openvaala" does not support changing directory`,
		},
		{
			name:       "virtual_cannot_change_directory",
			executorID: "alpha",
			directory:  "/x",
			wantError:  `invalid executor settings: executor "alpha" does not support changing directory`,
		},
		{
			name:       "unknown_executor",
			executorID: "gone",
			directory:  "/x",
			wantError:  `invalid executor settings: unknown executor "gone"`,
		},
		{
			name:             "ssh_connect_failure_returned_as_is",
			executorID:       "remote",
			directory:        "~",
			wantConnectError: "connect test@127.0.0.1:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 创建目录、文件和软链接样本，替换用例里的临时路径。
			f := newEnvironmentFixture(t)
			executors := append(f.definitions.Executors, &defs.ExecutorDefinition{
				ID:                 "remote",
				Kind:               defs.ExecutorKindSSH,
				Description:        "离线",
				WorkspaceDirectory: "/project",
				SSH:                &defs.SSHExecutorDefinition{Address: "test@127.0.0.1:1"},
			})
			for _, directory := range []string{filepath.Join(f.home, "proj", "sub"), filepath.Join(f.localDirectory, "sub"), filepath.Join(f.extraDirectory, "sub")} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.home, "proj", "file"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(f.home, "proj", "sub"), filepath.Join(f.home, "link")); err != nil {
				t.Fatal(err)
			}
			replacer := strings.NewReplacer("{home}", f.home, "{local}", f.localDirectory, "{extra}", f.extraDirectory)
			directory, baseDirectory := replacer.Replace(tt.directory), replacer.Replace(tt.baseDirectory)
			// 3. 解析路径，核对得到的绝对路径、设置错误或 SSH 连接错误。
			got, err := ResolveWorkspaceDirectory(t.Context(), executors, tt.executorID, directory, baseDirectory)
			switch {
			case tt.wantError != "":
				if want := replacer.Replace(tt.wantError); !errors.Is(err, ErrInvalidExecutorSettings) || err.Error() != want {
					t.Fatalf("ResolveWorkspaceDirectory(%s, %q, %q) = %q, %v; want error %q", tt.executorID, directory, baseDirectory, got, err, want)
				}
			case tt.wantConnectError != "":
				if err == nil || errors.Is(err, ErrInvalidExecutorSettings) || !strings.Contains(err.Error(), tt.wantConnectError) {
					t.Fatalf("ResolveWorkspaceDirectory(%s) error = %v, want one containing %q", tt.executorID, err, tt.wantConnectError)
				}
			default:
				if want := replacer.Replace(tt.want); err != nil || got != want {
					t.Fatalf("ResolveWorkspaceDirectory(%s, %q, %q) = %q, %v; want %q", tt.executorID, directory, baseDirectory, got, err, want)
				}
			}
		})
	}
}

// TestGetWorkspaceDirectories 检查目录候选的路径解析、直接目录筛选和 Executor 错误。
func TestGetWorkspaceDirectories(t *testing.T) {
	// 1. 列出默认目录、相对目录、主目录、链接和无效输入。
	for _, tt := range []struct {
		name, executorID, directory, baseDirectory, wantDirectory string
		wantDirectories                                           []string
		wantInvalid                                               bool
	}{
		{name: "default_directory", executorID: "local", wantDirectory: "{local}", wantDirectories: []string{"alpha", "zeta"}},
		{name: "base_relative_directory", executorID: "local", directory: "alpha/../zeta", baseDirectory: "{local}", wantDirectory: "{local}/zeta", wantDirectories: []string{}},
		{name: "absolute_directory", executorID: "local", directory: "{local}/alpha", wantDirectory: "{local}/alpha", wantDirectories: []string{"nested"}},
		{name: "home_directory", executorID: "local", directory: "~", wantDirectory: "{home}", wantDirectories: []string{"project"}},
		{name: "home_relative_directory", executorID: "local", directory: "~/project", wantDirectory: "{home}/project", wantDirectories: []string{}},
		{name: "typed_symlink_directory", executorID: "local", directory: "link", wantDirectory: "{local}/link", wantDirectories: []string{"nested"}},
		{name: "missing_directory", executorID: "local", directory: "missing", wantInvalid: true},
		{name: "file_not_directory", executorID: "local", directory: "file", wantInvalid: true},
		{name: "virtual_rejected", executorID: "alpha", directory: "/workspace", wantInvalid: true},
		{name: "unknown_executor", executorID: "missing", wantInvalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 创建两层目录、文件和目录链接，替换临时路径。
			f := newEnvironmentFixture(t)
			for _, directory := range []string{filepath.Join(f.localDirectory, "alpha", "nested"), filepath.Join(f.localDirectory, "zeta"), filepath.Join(f.home, "project")} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.localDirectory, "file"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(f.localDirectory, "alpha"), filepath.Join(f.localDirectory, "link")); err != nil {
				t.Fatal(err)
			}
			replacer := strings.NewReplacer("{local}", f.localDirectory, "{home}", f.home)
			// 3. 核对绝对目录和排序后的普通直接子目录；错误沿用现有设置错误。
			got, err := GetWorkspaceDirectories(t.Context(), f.definitions.Executors, tt.executorID, replacer.Replace(tt.directory), replacer.Replace(tt.baseDirectory))
			if tt.wantInvalid {
				if !errors.Is(err, ErrInvalidExecutorSettings) {
					t.Fatalf("GetWorkspaceDirectories = %+v, %v; want ErrInvalidExecutorSettings", got, err)
				}
				return
			}
			if err != nil || got.Directory != replacer.Replace(tt.wantDirectory) || !reflect.DeepEqual(got.Directories, tt.wantDirectories) {
				t.Fatalf("GetWorkspaceDirectories = %+v, %v; want %q, %v", got, err, replacer.Replace(tt.wantDirectory), tt.wantDirectories)
			}
		})
	}
}

// TestGetWorkspaceDirectoriesSSH 检查一次目录查询只使用一条临时 SSH 连接，并保留其他工作区的连接。
func TestGetWorkspaceDirectoriesSSH(t *testing.T) {
	// 1. 列出相对、绝对和主目录展开场景。
	for _, directory := range []string{"", "~", "absolute"} {
		t.Run(cmp.Or(directory, "relative"), func(t *testing.T) {
			server := newSSHTestServer(t)
			root := t.TempDir()
			if directory == "~" {
				var err error
				root, err = os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				for _, name := range []string{"alpha", "zeta"} {
					if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			}
			definition := &defs.ExecutorDefinition{ID: "remote", Kind: defs.ExecutorKindSSH, WorkspaceDirectory: root,
				SSH: &defs.SSHExecutorDefinition{Address: "test@" + server.address, IdentityFile: server.identityFile}}
			// 2. 先打开另一个工作区，再发起目录查询。
			conversationWorkspace, err := NewExecutorWorkspace(t.Context(), definition, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeWorkspace(t.Context(), conversationWorkspace)
			if _, err := conversationWorkspace.GetMetadata(t.Context(), root, true); err != nil {
				t.Fatal(err)
			}
			queryDirectory := directory
			if directory == "absolute" {
				queryDirectory = root
			}
			for request := 1; request <= 2; request++ {
				got, err := GetWorkspaceDirectories(t.Context(), []*defs.ExecutorDefinition{definition}, "remote", queryDirectory, "")
				if err != nil || got.Directory != root {
					t.Fatalf("GetWorkspaceDirectories = %+v, %v; want %q", got, err, root)
				}
				if directory != "~" && !reflect.DeepEqual(got.Directories, []string{"alpha", "zeta"}) {
					t.Fatalf("directories = %v, want alpha and zeta without symlink", got.Directories)
				}
				// 3. 每次查询只新建一条连接，返回时已关闭；原工作区仍可读取。
				server.waitOpenConnections(t, 1)
				if got := server.acceptedConnections.Load(); got != int32(request+1) {
					t.Fatalf("accepted connections = %d, want %d", got, request+1)
				}
				if _, err := conversationWorkspace.GetMetadata(t.Context(), root, true); err != nil {
					t.Fatalf("existing workspace stopped: %v", err)
				}
			}
		})
	}
}

// TestGetWorkspaceDirectoriesSSHUnavailable 检查连接被拒及服务端期限会结束请求并关闭连接。
func TestGetWorkspaceDirectoriesSSHUnavailable(t *testing.T) {
	// 1. 分别拒绝连接或在握手阶段保持静默，调用方不设置额外期限。
	for _, refused := range []bool{true, false} {
		t.Run(fmt.Sprintf("connection_refused_%t", refused), func(t *testing.T) {
			server := newSSHTestServer(t)
			server.stallHandshake.Store(true)
			address := server.address
			if refused {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address = listener.Addr().String()
				_ = listener.Close()
			}
			definition := &defs.ExecutorDefinition{ID: "remote", Kind: defs.ExecutorKindSSH, WorkspaceDirectory: t.TempDir(),
				SSH: &defs.SSHExecutorDefinition{Address: "test@" + address, IdentityFile: server.identityFile}}
			started := time.Now()
			_, err := GetWorkspaceDirectories(t.Context(), []*defs.ExecutorDefinition{definition}, "remote", "", "")
			// 2. 连接错误原样返回，静默连接由服务端十秒期限结束，函数返回时连接已关闭。
			if err == nil || errors.Is(err, ErrInvalidExecutorSettings) {
				t.Fatalf("GetWorkspaceDirectories error = %v, want an access error", err)
			}
			if !refused && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 12*time.Second) {
				t.Fatalf("GetWorkspaceDirectories error = %v after %v, want server deadline", err, time.Since(started))
			}
			server.waitOpenConnections(t, 0)
		})
	}
}

// TestGetWorkspaceDirectoriesSSHCancellation 检查连接、路径解析、读取和收尾无响应时取消会关闭连接并等待请求退出。
func TestGetWorkspaceDirectoriesSSHCancellation(t *testing.T) {
	// 1. 在各 SSH 或 SFTP 阶段暂停服务端响应。
	for _, stage := range []struct {
		name      string
		packet    uint32
		directory string
	}{
		{name: "handshake"},
		{name: "sftp_init", packet: 1},
		{name: "getwd", packet: 16, directory: "~"},
		{name: "stat", packet: 17},
		{name: "opendir", packet: 11},
		{name: "readdir", packet: 12},
		{name: "close_handle", packet: 4},
	} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline_%t", stage.name, deadline), func(t *testing.T) {
				server := newSSHTestServer(t)
				server.stallPacket.Store(stage.packet)
				server.stallHandshake.Store(stage.name == "handshake")
				definition := &defs.ExecutorDefinition{ID: "remote", Kind: defs.ExecutorKindSSH, WorkspaceDirectory: t.TempDir(),
					SSH: &defs.SSHExecutorDefinition{Address: "test@" + server.address, IdentityFile: server.identityFile}}
				ctx, cancel := context.WithCancel(t.Context())
				wantError := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
					wantError = context.DeadlineExceeded
				}
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					_, err := GetWorkspaceDirectories(ctx, []*defs.ExecutorDefinition{definition}, "remote", stage.directory, "")
					finished <- err
				}()
				select {
				case <-server.stalled:
				case <-time.After(3 * time.Second):
					t.Fatal("request did not reach the paused stage")
				}
				// 2. 取消或等请求到期，核对函数已经返回且服务端连接已关闭。
				if !deadline {
					cancel()
				}
				select {
				case err := <-finished:
					if !errors.Is(err, wantError) {
						t.Fatalf("GetWorkspaceDirectories error = %v, want %v", err, wantError)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("directory request did not finish after cancellation")
				}
				server.waitOpenConnections(t, 0)
				// 3. 恢复服务后再次查询能正常完成，没有留住原请求的连接。
				server.stallPacket.Store(0)
				server.stallHandshake.Store(false)
				if _, err := GetWorkspaceDirectories(t.Context(), []*defs.ExecutorDefinition{definition}, "remote", "", ""); err != nil {
					t.Fatalf("next directory request: %v", err)
				}
				server.waitOpenConnections(t, 0)
			})
		}
	}
}

// environmentFixture 是装配测试的存储、受控模型和目录：临时 HOME，local 和 extra 两个 local 种类 Executor 各有自己的临时根目录，
// 定义里的 Executor 依次是 local、openvaala（builtin，virtual）、alpha（virtual）、extra。
type environmentFixture struct {
	store                                *conversationStore
	model                                *conversationModel
	home, localDirectory, extraDirectory string
	definitions                          *ConversationDefinitions
}

// newEnvironmentFixture 创建环境测试的目录、模型、存储和 Executor 定义。
func newEnvironmentFixture(t *testing.T) *environmentFixture {
	t.Helper()
	// 1. 创建临时 HOME 和两个 local 根目录，配置 local、内置 virtual、alpha 和 extra。
	f := &environmentFixture{
		store:          openConversationDAO(t),
		model:          newConversationModel(t),
		home:           t.TempDir(),
		localDirectory: t.TempDir(),
		extraDirectory: t.TempDir(),
	}
	t.Setenv("HOME", f.home)
	f.definitions = conversationDefinitions(f.model)
	f.definitions.Executors = []*defs.ExecutorDefinition{
		{
			ID:                 "local",
			Kind:               defs.ExecutorKindLocal,
			Description:        "本机",
			WorkspaceDirectory: f.localDirectory,
		},
		{
			ID:                 "openvaala",
			Kind:               defs.ExecutorKindVirtual,
			Description:        "内置",
			WorkspaceDirectory: "/workspace",
		},
		{
			ID:                 "alpha",
			Kind:               defs.ExecutorKindVirtual,
			Description:        "虚拟目录",
			WorkspaceDirectory: "/workspace",
		},
		{
			ID:                 "extra",
			Kind:               defs.ExecutorKindLocal,
			Description:        "试验目录",
			WorkspaceDirectory: f.extraDirectory,
		},
	}
	return f
}

// newOpt 返回用 fixture 当前定义的对话依赖。
func (f *environmentFixture) newOpt() *ConversationOpt {
	// 1. 用存储、模型和 fixture 当前的定义生成配置。
	opt := conversationOpt(f.store, f.model)
	opt.Definitions = f.definitions
	return opt
}

// create 按指定设置创建测试对话。
func (f *environmentFixture) create(t *testing.T, settings defs.ExecutorSettings) Conversation {
	t.Helper()
	// 1. 用指定设置创建对话，失败时结束测试。
	c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, settings, defs.ExecutorSettings{}, f.newOpt())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reopen 新建一个测试对话，把记录里的 Executor 设置换成 settings 后重新打开。
func (f *environmentFixture) reopen(ctx context.Context, t *testing.T, settings, storedSettings defs.ExecutorSettings) (Conversation, error) {
	t.Helper()
	// 1. 用默认设置创建对话，读取记录并换入 settings，再以 storedSettings 为已存设置重新打开。
	record := f.getRecord(t, f.create(t, defs.ExecutorSettings{}))
	record.ExecutorSettings = settings
	return GetConversation(ctx, record, storedSettings, f.newOpt())
}

// getRecord 读出对话保存的记录。
func (f *environmentFixture) getRecord(t *testing.T, c Conversation) defs.Conversation {
	t.Helper()
	// 1. 按对话 ID 查询记录，缺失或查询失败时结束测试。
	record, found, err := f.store.query.GetConversation(t.Context(), c.GetID())
	if err != nil || !found {
		t.Fatalf("GetConversation(%s) = %v, %v", c.GetID(), found, err)
	}
	return record
}

// runEcho 用带指定客户端环境变量的对话跑一次 bash，返回 TOOL_ENV_MARK 的输出。
func (f *environmentFixture) runEcho(t *testing.T, environment []string) string {
	t.Helper()
	// 1. 只留 local，创建带指定客户端环境变量的对话。
	f.definitions.Executors = f.definitions.Executors[:1]
	opt := f.newOpt()
	opt.Environment = environment
	c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 调用 bash 输出 TOOL_ENV_MARK，返回工具正文。
	result := f.runTool(t, startTestConversation(t, f.store, f.model, c), "bash", `{"command":"echo $TOOL_ENV_MARK"}`)
	return string(result.Content)
}

// runTool 让根 Session 调用工具并返回结果。
func (f *environmentFixture) runTool(t *testing.T, h *conversationHarness, name, arguments string) defs.ToolResult {
	t.Helper()
	// 1. 向根 Session 提交输入，让模型发出指定的工具调用。
	itemID := h.input(t, session.SessionInputModeNormal, "run "+name)
	receiveSessionRequest(t, f.model, h.c.GetRootSessionID()).reply <- modelResponse{calls: []llmkit.ToolCall{{
		ID:        "call-" + name,
		Name:      name,
		Arguments: arguments,
	}}}
	// 2. 读取工具结果，回复 done 并等待本轮结束。
	request := receiveSessionRequest(t, f.model, h.c.GetRootSessionID())
	result := requestToolResult(t, request, "call-"+name)
	request.reply <- modelResponse{text: "done"}
	h.waitTurnFinished(t, itemID)
	return result
}

// getRootDetails 返回对话根 Session 的详情。
func getRootDetails(t *testing.T, c Conversation) SessionDetails {
	t.Helper()
	// 1. 查询根 Session 详情，失败时结束测试。
	details, err := c.GetSessionDetails(t.Context(), c.GetRootSessionID())
	if err != nil {
		t.Fatal(err)
	}
	return details
}

// getExecutorIDs 按顺序返回对话根 Session 详情里的 Executor ID。
func getExecutorIDs(t *testing.T, c Conversation) []string {
	t.Helper()
	// 1. 按详情顺序提取 Executor ID。
	var ids []string
	for _, executor := range getRootDetails(t, c).Executors {
		ids = append(ids, executor.ID)
	}
	return ids
}

// getSkillPaths 提取对话可用 Skill 的路径。
func getSkillPaths(c Conversation) map[string]string {
	// 1. 以 Skill 名为键，记下 SKILL.md 路径。
	paths := make(map[string]string)
	for _, info := range c.GetSkills() {
		paths[info.Name] = info.Path
	}
	return paths
}

// writeTestSkills 在 directory 下写入测试 Skill 文件。
func writeTestSkills(t *testing.T, directory string, names ...string) {
	t.Helper()
	// 1. 在 directory 下逐个创建 <name>/SKILL.md，写入名称和「<name> 技能」描述。
	for _, name := range names {
		path := filepath.Join(directory, name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: "+name+" 技能\n---\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// sshTestServer 是测试进程内只带 SFTP 子系统的 SSH 服务端：只接受 identityFile 对应的公钥，SFTP 直接访问本机文件系统；
// 记录接受过的连接数和现在开着的连接数。测试结束时关闭监听和全部连接，等服务任务退出。
type sshTestServer struct {
	address, identityFile string
	acceptedConnections   atomic.Int32
	openConnections       atomic.Int32
	connections           syncmap.SyncMap[net.Conn, struct{}]
	stallHandshake        atomic.Bool
	stallPacket           atomic.Uint32
	stalled               chan struct{}
}

// newSSHTestServer 启动只提供 SFTP 的测试 SSH 服务，测试结束时关闭。
func newSSHTestServer(t *testing.T) *sshTestServer {
	t.Helper()
	// 1. 清掉 SSH agent 环境变量，生成服务端和客户端密钥，保存客户端私钥。
	t.Setenv("SSH_AUTH_SOCK", "")
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPublicKey, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(identityFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	authorizedKey, err := ssh.NewPublicKey(clientPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 配置公钥认证并启动本地监听。
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown public key")
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &sshTestServer{
		address:      listener.Addr().String(),
		identityFile: identityFile,
		stalled:      make(chan struct{}, 1),
	}
	// 3. 接收连接，记下连接数，为每个连接启动握手和 SFTP 处理。
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			server.acceptedConnections.Add(1)
			server.openConnections.Add(1)
			server.connections.Store(connection, struct{}{})
			tasks.Go(func() {
				defer server.openConnections.Add(-1)
				defer server.connections.Delete(connection)
				serveSSHTestConnection(connection, config, &tasks, server)
			})
		}
	})
	// 4. 测试结束时关闭监听和全部连接，等全部任务退出。
	t.Cleanup(func() {
		_ = listener.Close()
		server.connections.Range(func(connection net.Conn, _ struct{}) bool {
			_ = connection.Close()
			return true
		})
		tasks.Wait()
	})
	return server
}

// serveSSHTestConnection 处理 SSH 握手和 session 通道中的 SFTP 请求。
func serveSSHTestConnection(connection net.Conn, config *ssh.ServerConfig, tasks *sync.WaitGroup, fixture *sshTestServer) {
	// 1. 完成 SSH 握手，失败时关闭连接。
	if fixture.stallHandshake.Load() {
		fixture.stalled <- struct{}{}
		_, _ = io.Copy(io.Discard, connection)
		_ = connection.Close()
		return
	}
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		_ = connection.Close()
		return
	}
	defer serverConnection.Close()
	connectionEnded := make(chan struct{})
	tasks.Go(func() {
		_ = serverConnection.Wait()
		close(connectionEnded)
	})
	// 2. 丢弃全局请求，只接受 session 通道，为子系统请求启动 SFTP 服务。
	tasks.Go(func() { ssh.DiscardRequests(requests) })
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		tasks.Go(func() {
			for request := range channelRequests {
				if request.Type != "subsystem" {
					_ = request.Reply(false, nil)
					continue
				}
				_ = request.Reply(true, nil)
				tasks.Go(func() {
					input := &directorySFTPTestChannel{Channel: channel, fixture: fixture, connectionEnded: connectionEnded}
					if server, err := sftp.NewServer(input); err == nil {
						_ = server.Serve()
					}
					_ = channel.Close()
				})
			}
		})
	}
}

// directorySFTPTestChannel 在指定 SFTP 请求上停止读取，直到客户端关闭整条连接。
type directorySFTPTestChannel struct {
	ssh.Channel
	fixture         *sshTestServer
	connectionEnded <-chan struct{}
	pending         []byte
}

// Read 读取完整 SFTP 包，在被暂停的阶段等连接结束后退出。
func (c *directorySFTPTestChannel) Read(buffer []byte) (int, error) {
	// 1. 读入一个完整包，保留长度头供 SFTP 服务端继续解析。
	if len(c.pending) == 0 {
		header := make([]byte, 4)
		if _, err := io.ReadFull(c.Channel, header); err != nil {
			return 0, err
		}
		packet := make([]byte, binary.BigEndian.Uint32(header))
		if _, err := io.ReadFull(c.Channel, packet); err != nil {
			return 0, err
		}
		if uint32(packet[0]) == c.fixture.stallPacket.Load() {
			c.fixture.stalled <- struct{}{}
			<-c.connectionEnded
			return 0, io.EOF
		}
		c.pending = append(header, packet...)
	}
	// 2. 返回调用方这次可容纳的数据，下一次继续返回剩余部分。
	n := copy(buffer, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// waitOpenConnections 等 SSH 服务端开着的连接数变成 want。
func (s *sshTestServer) waitOpenConnections(t *testing.T, want int32) {
	t.Helper()
	// 1. 最多等五秒，轮询开着的连接数直到等于 want。
	deadline := time.Now().Add(5 * time.Second)
	for s.openConnections.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open SSH connections = %d, want %d", s.openConnections.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
