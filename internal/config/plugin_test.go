package config_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"gopkg.in/yaml.v3"
)

// badPluginManifest 是名为 bad 的插件 manifest：MCP server 写了 executors，装上后整份配置加载失败。
const badPluginManifest = "name: bad\nmcp_servers:\n  - {id: bad-docs, type: stdio, command: server, executors: [local]}\n"

// workerAgentFile 是引用 demo 插件 MCP server demo-docs 的 Agent；demo 插件不在时整份配置加载失败。
const workerAgentFile = "name: worker\ninstructions: Work.\nmcp_servers: [demo-docs]\n"

// runTestGit 在 directory 里执行 git 命令，返回去掉首尾空白的输出。
func runTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	// 1. 执行命令，失败时带上命令和 stderr 结束测试。
	command := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, arguments...)...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("git %v: %v: %s", arguments, err, exitError.Stderr)
		}
		t.Fatalf("git %v: %v", arguments, err)
	}
	return strings.TrimSpace(string(output))
}

// commitPluginRepository 往插件仓库写入文件并提交，返回 HEAD。
func commitPluginRepository(t *testing.T, repository string, files map[string]string) string {
	t.Helper()
	// 1. 仓库不存在时先 git init，再写入文件、添加并提交。
	if _, err := os.Stat(filepath.Join(repository, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(repository, 0700); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, repository, "init", "-q")
	}
	writeDefinitionFiles(t, repository, files)
	runTestGit(t, repository, "add", "-A")
	runTestGit(t, repository, "commit", "-q", "-m", "update")
	return runTestGit(t, repository, "rev-parse", "HEAD")
}

// getDemoRepositoryFiles 生成仓库里 plugins/demo 下的 demo 插件文件，core.md 的内容由 core 给出。
func getDemoRepositoryFiles(core string) map[string]string {
	// 1. 组装 demo 插件文件，再加上 core.md。
	return mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{"plugins/demo/core.md": core})
}

// newPluginConfigHome 创建插件测试用的主目录，返回主目录和其中的配置目录。
func newPluginConfigHome(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	// 1. 创建临时主目录，在 .config/openvaala 下写入 files。
	home := t.TempDir()
	configDirectory := filepath.Join(home, ".config", "openvaala")
	writeDefinitionFiles(t, configDirectory, files)
	return home, configDirectory
}

// readPluginInstallRecord 读取插件目录里的安装记录。
func readPluginInstallRecord(t *testing.T, pluginDirectory string) config.PluginInstallRecord {
	t.Helper()
	// 1. 读取并解析安装记录，失败时结束测试。
	content, err := os.ReadFile(filepath.Join(pluginDirectory, ".openvaala-install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var record config.PluginInstallRecord
	if err := yaml.Unmarshal(content, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// getPluginEntryNames 按名字顺序列出 plugins 目录里的条目名，包括隐藏的临时目录。
func getPluginEntryNames(t *testing.T, pluginsDirectory string) []string {
	t.Helper()
	// 1. 读取目录，目录不存在时返回空列表。
	entries, err := os.ReadDir(pluginsDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// readTestFile 读取文件文本，文件不存在时返回空串。
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	// 1. 读取文件，其他读取错误时结束测试。
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(content)
}

// TestSkillPluginCompatibility 检查原版 Codex 和 Claude 插件的安装、技能读取、禁用与更新，外部 hook 和 MCP 配置不加载。
func TestSkillPluginCompatibility(t *testing.T) {
	// 1. 按两种 manifest 及其路径写法准备原版插件，包含默认目录、自定义目录和技能附属文件。
	for _, tt := range []struct {
		name, manifestPath, manifest string
		otherManifests               map[string]string
		extraFiles                   map[string]string
		wantSkills                   []string
		wantErr                      string
	}{
		{
			name: "codex_string", manifestPath: ".codex-plugin/plugin.json",
			manifest:   `{"name":"demo","version":"1.0.0","skills":"./extra/","hooks":false,"mcpServers":42}`,
			wantSkills: []string{"demo:base", "demo:guide"},
		},
		{
			name: "codex_single_skill", manifestPath: ".codex-plugin/plugin.json",
			manifest:   `{"name":"demo","version":"1.0.0","skills":"./extra/guide/"}`,
			wantSkills: []string{"demo:base", "demo:guide"},
		},
		{
			name: "codex_nested_missing_name", manifestPath: ".codex-plugin/plugin.json",
			manifest: `{"name":"demo","version":"1.0.0"}`,
			extraFiles: map[string]string{
				"skills/group/review/SKILL.md": "---\ndescription: Review code.\n---\nReview body.\n",
			},
			wantSkills: []string{"demo:base", "demo:review"},
		},
		{
			name: "claude_default", manifestPath: ".claude-plugin/plugin.json",
			manifest:   `{"name":"demo","version":"1.0.0","hooks":"./missing-hooks.json","mcpServers":"./missing-mcp.json"}`,
			wantSkills: []string{"demo:base"},
		},
		{
			name: "claude_array_and_single_skill", manifestPath: ".claude-plugin/plugin.json",
			manifest:   `{"name":"demo","version":"1.0.0","skills":["./extra/guide/","./skills/","./extra/"]}`,
			wantSkills: []string{"demo:base", "demo:guide"},
		},
		{
			name: "codex_precedes_claude", manifestPath: ".codex-plugin/plugin.json",
			manifest:       `{"name":"demo","version":"1.0.0","skills":["./extra/"]}`,
			otherManifests: map[string]string{".claude-plugin/plugin.json": "invalid json"},
			wantSkills:     []string{"demo:base", "demo:guide"},
		},
		{
			name: "native_precedes_external", manifestPath: ".openvaala-plugin/plugin.yaml",
			manifest:       "name: demo\nversion: 1.0.0\nskills: ./extra/\n",
			otherManifests: map[string]string{".codex-plugin/plugin.json": "invalid json"},
			wantSkills:     []string{"demo:guide"},
		},
		{
			name: "invalid_json_does_not_fall_back", manifestPath: ".codex-plugin/plugin.json",
			manifest:       "invalid json",
			otherManifests: map[string]string{".claude-plugin/plugin.json": `{"name":"demo"}`},
			wantErr:        "invalid JSON",
		},
		{
			name: "invalid_skill_paths", manifestPath: ".claude-plugin/plugin.json",
			manifest: `{"name":"demo","skills":42}`, wantErr: "skills must be a path or an array of paths",
		},
		{
			name: "path_outside_plugin", manifestPath: ".codex-plugin/plugin.json",
			manifest: `{"name":"demo","skills":["../outside"]}`, wantErr: "plugin skills directory must be inside the plugin directory",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 安装后通过公开配置入口读取技能及完整正文，失败安装不能留下插件。
			source := t.TempDir()
			files := map[string]string{
				tt.manifestPath:                    tt.manifest,
				"skills/base/SKILL.md":             "---\nname: base\ndescription: Base skill.\n---\nBase body.\n",
				"extra/guide/SKILL.md":             "---\nname: guide\ndescription: Guide skill.\n---\nRead references/detail.md.\n",
				"extra/guide/references/detail.md": "Complete reference.\n",
				"hooks/hooks.json":                 "invalid hook configuration",
				".mcp.json":                        "invalid MCP configuration",
			}
			for path, content := range tt.otherManifests {
				files[path] = content
			}
			for path, content := range tt.extraFiles {
				files[path] = content
			}
			writeDefinitionFiles(t, source, files)
			home, directory := newPluginConfigHome(t, map[string]string{"config.yaml": "{}\n"})
			opt := &config.LoadConfigOpt{Home: home}
			name, _, err := config.InstallPlugin(t.Context(), opt, &config.InstallPluginRequest{Source: source})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("InstallPlugin() error = %v, want %q", err, tt.wantErr)
				}
				if entries := getPluginEntryNames(t, filepath.Join(directory, "plugins")); len(entries) != 0 {
					t.Fatalf("failed installation left entries: %v", entries)
				}
				return
			}
			if err != nil || name != "demo" {
				t.Fatalf("InstallPlugin() = %q, %v", name, err)
			}
			cfg, err := config.LoadConfig(opt)
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := cfg.GetRuntimeDefinitions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, loaded := range definitions.Skills {
				if !strings.HasPrefix(loaded.Name, "demo:") {
					continue
				}
				names = append(names, loaded.Name)
				content, err := loaded.GetContent(t.Context())
				if err != nil || !strings.Contains(content, "---") {
					t.Fatalf("read skill %s: %q, %v", loaded.Name, content, err)
				}
			}
			if !slices.Equal(names, tt.wantSkills) || len(definitions.Hooks) != 0 || len(definitions.MCPServers) != 0 {
				t.Fatalf("skills = %v, hooks = %d, MCP servers = %d", names, len(definitions.Hooks), len(definitions.MCPServers))
			}
			if got := readTestFile(t, filepath.Join(directory, "plugins/demo", tt.manifestPath)); got != tt.manifest {
				t.Fatalf("manifest changed: %q", got)
			}
			// 3. 列表、禁用和更新共用同一种格式；更新读取源文件的新版本，保留附属文件。
			entries, err := config.ListPlugins(opt)
			if err != nil || len(entries) != 1 || entries[0].Error != "" || entries[0].Version != "1.0.0" {
				t.Fatalf("ListPlugins() = %+v, %v", entries, err)
			}
			if _, err := config.SetPluginDisabled(opt, "demo", true); err != nil {
				t.Fatal(err)
			}
			cfg, err = config.LoadConfig(opt)
			if err != nil {
				t.Fatal(err)
			}
			definitions, err = cfg.GetRuntimeDefinitions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, loaded := range definitions.Skills {
				if strings.HasPrefix(loaded.Name, "demo:") {
					t.Fatalf("disabled skill loaded: %s", loaded.Name)
				}
			}
			if _, err := config.SetPluginDisabled(opt, "demo", false); err != nil {
				t.Fatal(err)
			}
			writeDefinitionFiles(t, source, map[string]string{tt.manifestPath: strings.ReplaceAll(tt.manifest, "1.0.0", "2.0.0")})
			if _, err := config.UpdatePlugins(t.Context(), opt, []string{"demo"}); err != nil {
				t.Fatal(err)
			}
			entries, err = config.ListPlugins(opt)
			if err != nil || len(entries) != 1 || entries[0].Version != "2.0.0" {
				t.Fatalf("updated plugins = %+v, %v", entries, err)
			}
			if got := readTestFile(t, filepath.Join(directory, "plugins/demo/extra/guide/references/detail.md")); got != "Complete reference.\n" {
				t.Fatalf("skill reference = %q", got)
			}
		})
	}
}

// TestInstallPlugin 验证从 git 地址或本机目录安装插件：拷贝内容、写安装记录、处理目录链接；参数错误或装后配置加载失败时不留下目录。
func TestInstallPlugin(t *testing.T) {
	// 1. 创建多版本仓库，core.md 依次提交为 v1、v2、v3，并给 v2 打 tag；bad 插件安装后配置加载失败。
	repository := filepath.Join(t.TempDir(), "repo")
	commitPluginRepository(t, repository, mergeFiles(getDemoRepositoryFiles("v1\n"), map[string]string{
		"plugins/bad/.openvaala-plugin/plugin.yaml": badPluginManifest,
	}))
	tagCommit := commitPluginRepository(t, repository, map[string]string{"plugins/demo/core.md": "v2\n"})
	runTestGit(t, repository, "tag", "v2")
	headCommit := commitPluginRepository(t, repository, map[string]string{"plugins/demo/core.md": "v3\n"})
	// 2. 创建根目录本身就是插件的 Git 仓库。
	rootRepository := filepath.Join(t.TempDir(), "root-tool")
	rootCommit := commitPluginRepository(t, rootRepository, map[string]string{
		".openvaala-plugin/plugin.yaml": "name: root-tool\n",
		"core.md":                       "root\n",
	})
	// 3. 创建带目录链接的插件仓库，分别链接仓库内 shared-skills、仓库外目录和上级目录。
	linkedRepository := filepath.Join(t.TempDir(), "linked")
	writeDefinitionFiles(t, linkedRepository, mergeFiles(getPluginFiles("plugins/demo", "name: demo\n", map[string]string{"core.md": "linked\n"}), map[string]string{
		"shared-skills/greet/SKILL.md": "---\nname: greet\ndescription: Greet the user.\n---\nSay hello.\n",
	}))
	outsideDirectory := t.TempDir()
	writeDefinitionFiles(t, outsideDirectory, map[string]string{"notes.md": "outside\n"})
	for link, target := range map[string]string{"skills": "../../shared-skills", "outside": outsideDirectory, "loop": ".."} {
		if err := os.Symlink(target, filepath.Join(linkedRepository, "plugins", "demo", link)); err != nil {
			t.Fatal(err)
		}
	}
	linkedCommit := commitPluginRepository(t, linkedRepository, nil)
	// 4. 创建本机 readonly 插件，并将 bin 子目录设为只读。
	readOnlyRepository := filepath.Join(t.TempDir(), "readonly")
	writeDefinitionFiles(t, readOnlyRepository, getPluginFiles("", "name: readonly\n", map[string]string{"core.md": "readonly\n", "bin/tool": "#!/bin/sh\n"}))
	if err := os.Chmod(filepath.Join(readOnlyRepository, "bin"), 0o555); err != nil {
		t.Fatal(err)
	}
	// 5. 登记清理任务，在测试结束时恢复 bin 的写权限。
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(readOnlyRepository, "bin"), 0o700) })

	tests := []struct {
		name    string
		request config.InstallPluginRequest
		// isFreshHome 为 true 时配置目录不存在：没有 config.yaml，也没有 plugins/。
		isFreshHome bool
		// configFile 是 config.yaml 的内容，为空时是 {}。
		configFile string
		// isInstalledBefore 为 true 时先用同一个请求装一次。
		isInstalledBefore bool
		// isWithoutGit 为 true 时 PATH 里找不到 git。
		isWithoutGit bool
		wantErr      string
		wantName     string
		// wantDisabled 为 true 时插件写在 plugins.disabled 里，装上后加载出的插件里没有它。
		wantDisabled bool
		// wantRecord 不比较 InstalledAt，只要求它不是零值。
		wantRecord config.PluginInstallRecord
		wantCore   string
		// wantPaths 是装好的插件里必须是真实文件或目录（不是符号链接）的条目，wantMissingPaths 是必须不存在的条目；都相对插件目录。
		wantPaths        []string
		wantMissingPaths []string
		// isUninstalledAfter 为 true 时最后卸载插件，plugins 目录里不能留下任何条目，包括隐藏的临时目录。
		isUninstalledAfter bool
		// wantEntries 是结束后 plugins 目录下的全部条目，安装用的临时目录不能留下。
		wantEntries []string
	}{
		{
			name: "git_with_path",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + repository,
				Path:   "plugins/demo",
				Commit: headCommit,
			},
			wantCore:    "v3\n",
			wantEntries: []string{"demo"},
		},
		{
			name: "git_with_ref",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
				Ref:    "v2",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + repository,
				Ref:    "v2",
				Path:   "plugins/demo",
				Commit: tagCommit,
			},
			wantCore:    "v2\n",
			wantEntries: []string{"demo"},
		},
		{
			name: "path_is_normalized",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "./plugins/demo/",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + repository,
				Path:   "plugins/demo",
				Commit: headCommit,
			},
			wantCore:    "v3\n",
			wantEntries: []string{"demo"},
		},
		{
			name:     "git_repository_root_skips_git_directory",
			request:  config.InstallPluginRequest{Source: "file://" + rootRepository},
			wantName: "root-tool",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + rootRepository,
				Commit: rootCommit,
			},
			wantCore:    "root\n",
			wantEntries: []string{"root-tool"},
		},
		{
			name: "local_directory",
			request: config.InstallPluginRequest{
				Source: repository,
				Path:   "plugins/demo",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: repository,
				Path:   "plugins/demo",
			},
			wantCore:    "v3\n",
			wantEntries: []string{"demo"},
		},
		{
			name: "fresh_home",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			},
			isFreshHome: true,
			wantName:    "demo",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + repository,
				Path:   "plugins/demo",
				Commit: headCommit,
			},
			wantCore:    "v3\n",
			wantEntries: []string{"demo"},
		},
		{
			name: "git_copies_directory_links_inside_repository",
			request: config.InstallPluginRequest{
				Source: "file://" + linkedRepository,
				Path:   "plugins/demo",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + linkedRepository,
				Path:   "plugins/demo",
				Commit: linkedCommit,
			},
			wantCore:         "linked\n",
			wantPaths:        []string{"skills", "skills/greet/SKILL.md"},
			wantMissingPaths: []string{"outside", "loop"},
			wantEntries:      []string{"demo"},
		},
		{
			name: "local_directory_copies_directory_links_inside_repository",
			request: config.InstallPluginRequest{
				Source: linkedRepository,
				Path:   "plugins/demo",
			},
			wantName: "demo",
			wantRecord: config.PluginInstallRecord{
				Source: linkedRepository,
				Path:   "plugins/demo",
			},
			wantCore:         "linked\n",
			wantPaths:        []string{"skills", "skills/greet/SKILL.md"},
			wantMissingPaths: []string{"outside", "loop"},
			wantEntries:      []string{"demo"},
		},
		{
			// 本机来源的仓库就是给出的目录：skills 指向它外面的 shared-skills，不拷。
			name:             "local_plugin_directory_skips_links_outside_it",
			request:          config.InstallPluginRequest{Source: filepath.Join(linkedRepository, "plugins", "demo")},
			wantName:         "demo",
			wantRecord:       config.PluginInstallRecord{Source: filepath.Join(linkedRepository, "plugins", "demo")},
			wantCore:         "linked\n",
			wantMissingPaths: []string{"skills", "outside", "loop"},
			wantEntries:      []string{"demo"},
		},
		{
			name:               "local_directory_with_read_only_directory",
			request:            config.InstallPluginRequest{Source: readOnlyRepository},
			wantName:           "readonly",
			wantRecord:         config.PluginInstallRecord{Source: readOnlyRepository},
			wantCore:           "readonly\n",
			wantPaths:          []string{"bin/tool"},
			isUninstalledAfter: true,
			wantEntries:        []string{"readonly"},
		},
		{
			name: "disabled_plugin_installs_and_stays_disabled",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			},
			configFile:   "plugins:\n  disabled: [demo]\n",
			wantName:     "demo",
			wantDisabled: true,
			wantRecord: config.PluginInstallRecord{
				Source: "file://" + repository,
				Path:   "plugins/demo",
				Commit: headCommit,
			},
			wantCore:    "v3\n",
			wantEntries: []string{"demo"},
		},
		{
			// 禁用的插件加载时不读，安装时仍要按加载的规则检查它。
			name: "disabled_plugin_is_still_checked",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/bad",
			},
			configFile:  "plugins:\n  disabled: [bad]\n",
			wantErr:     "plugin mcp server cannot set executors",
			wantEntries: []string{},
		},
		{
			name: "local_directory_with_ref",
			request: config.InstallPluginRequest{
				Source: repository,
				Path:   "plugins/demo",
				Ref:    "v2",
			},
			wantErr:     "--ref requires a git source",
			wantEntries: []string{},
		},
		{
			name: "path_outside_repository",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "../x",
			},
			wantErr:     "--path must be a relative path inside the repository",
			wantEntries: []string{},
		},
		{
			name: "manifest_missing",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/none",
			},
			wantErr:     "plugin not found at plugins/none; use --path to point at the plugin directory",
			wantEntries: []string{},
		},
		{
			name:        "git_clone_fails_with_stderr",
			request:     config.InstallPluginRequest{Source: "file://" + filepath.Join(repository, "missing")},
			wantErr:     "git clone: exit status 128: fatal:",
			wantEntries: []string{},
		},
		{
			name: "git_missing",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			},
			isWithoutGit: true,
			wantErr:      "git is required to install plugins from git: ",
			wantEntries:  []string{},
		},
		{
			name: "already_installed",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			},
			isInstalledBefore: true,
			wantErr:           `plugin "demo" is already installed; use openvaala plugin update demo`,
			wantEntries:       []string{"demo"},
		},
		{
			name: "config_fails_to_load_after_install",
			request: config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/bad",
			},
			wantErr:     "plugin mcp server cannot set executors",
			wantEntries: []string{},
		},
	}
	// 6. 逐个安装，核对 plugins 目录、错误、返回值、安装记录、插件内容和加载出的 skill；需要时再卸载。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			opt := &config.LoadConfigOpt{Home: home}
			pluginsDirectory := filepath.Join(home, ".config", "openvaala", "plugins")
			if !tt.isFreshHome {
				writeDefinitionFiles(t, filepath.Join(home, ".config", "openvaala"), map[string]string{"config.yaml": cmp.Or(tt.configFile, "{}\n")})
				if err := os.MkdirAll(pluginsDirectory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.isInstalledBefore {
				if _, _, err := config.InstallPlugin(context.Background(), opt, &tt.request); err != nil {
					t.Fatal(err)
				}
			}
			if tt.isWithoutGit {
				t.Setenv("PATH", t.TempDir())
			}

			name, isDisabled, err := config.InstallPlugin(context.Background(), opt, &tt.request)
			if entries := getPluginEntryNames(t, pluginsDirectory); !slices.Equal(entries, tt.wantEntries) {
				t.Fatalf("plugins directory has %v, want %v", entries, tt.wantEntries)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("InstallPlugin() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name != tt.wantName || isDisabled != tt.wantDisabled {
				t.Fatalf("InstallPlugin() = %q, %v; want %q, %v", name, isDisabled, tt.wantName, tt.wantDisabled)
			}
			pluginDirectory := filepath.Join(pluginsDirectory, name)
			record := readPluginInstallRecord(t, pluginDirectory)
			if record.InstalledAt.IsZero() {
				t.Fatal("install record has no installed_at")
			}
			record.InstalledAt = time.Time{}
			if record != tt.wantRecord {
				t.Fatalf("install record = %+v, want %+v", record, tt.wantRecord)
			}
			if core := readTestFile(t, filepath.Join(pluginDirectory, "core.md")); core != tt.wantCore {
				t.Fatalf("core.md = %q, want %q", core, tt.wantCore)
			}
			for _, path := range append([]string{".git"}, tt.wantMissingPaths...) {
				if _, err := os.Lstat(filepath.Join(pluginDirectory, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("plugin directory has %s: %v", path, err)
				}
			}
			for _, path := range tt.wantPaths {
				if info, err := os.Lstat(filepath.Join(pluginDirectory, path)); err != nil || info.Mode()&fs.ModeSymlink != 0 {
					t.Fatalf("%s = %v, %v; want a regular file or directory", path, info, err)
				}
			}
			cfg, err := config.LoadConfig(opt)
			if err != nil {
				t.Fatal(err)
			}
			// 1. 核对启用插件的 skill 带插件名前缀，禁用的插件不加载。
			definitions, err := cfg.GetRuntimeDefinitions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var gotSkills []string
			for _, loaded := range definitions.Skills {
				if strings.HasPrefix(loaded.Name, name+":") {
					gotSkills = append(gotSkills, loaded.Path)
				}
			}
			var wantSkills []string
			if !tt.wantDisabled {
				if wantSkills, err = filepath.Glob(filepath.Join(pluginDirectory, "skills", "*", "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(gotSkills, wantSkills) {
				t.Fatalf("plugin skills = %v, want %v", gotSkills, wantSkills)
			}
			if tt.wantDisabled || !tt.isUninstalledAfter {
				return
			}
			if err := config.UninstallPlugin(opt, name); err != nil {
				t.Fatal(err)
			}
			if entries := getPluginEntryNames(t, pluginsDirectory); len(entries) != 0 {
				t.Fatalf("plugins directory has %v after uninstall, want nothing", entries)
			}
		})
	}
}

// TestUpdatePlugins 验证按安装记录更新插件：有新提交时换成新版本；手动放入、没安装、改了名、来源不在或新版本让配置加载失败时保留旧版本并报错。
func TestUpdatePlugins(t *testing.T) {
	// 1. 准备上游新提交、改过的安装记录和要更新的插件名。
	tests := []struct {
		name string
		// repositoryFiles 非 nil 时在安装之后、更新之前提交进仓库（相对仓库根目录）。
		repositoryFiles map[string]string
		// installRecord 非空时在更新前替换 demo 的安装记录，其中的 {repository} 换成仓库目录。
		installRecord string
		// names 是传给 UpdatePlugins 的插件名；plugins 目录里有装好的 demo 和手动放入的 manual。
		names       []string
		wantUpdated []string
		wantErr     string
		// wantCore 是更新后 demo 的 core.md；isNewVersion 为 true 时 demo 的记录指向新提交、安装时间变大，否则记录不变。
		wantCore     string
		isNewVersion bool
	}{
		{
			name:            "new_commit",
			repositoryFiles: map[string]string{"plugins/demo/core.md": "v2\n"},
			names:           []string{"demo"},
			wantUpdated:     []string{"demo"},
			wantCore:        "v2\n",
			isNewVersion:    true,
		},
		{
			name:            "without_names_updates_recorded_plugins_only",
			repositoryFiles: map[string]string{"plugins/demo/core.md": "v2\n"},
			wantUpdated:     []string{"demo"},
			wantCore:        "v2\n",
			isNewVersion:    true,
		},
		{
			name:     "manual_plugin",
			names:    []string{"manual"},
			wantErr:  `plugin "manual": not installed by openvaala plugin install`,
			wantCore: "v1\n",
		},
		{
			name:     "missing_plugin",
			names:    []string{"missing"},
			wantErr:  `plugin "missing": not installed`,
			wantCore: "v1\n",
		},
		{
			name: "new_version_renames_plugin",
			repositoryFiles: map[string]string{
				"plugins/demo/.openvaala-plugin/plugin.yaml": "name: other\n",
				"plugins/demo/core.md":                       "v2\n",
			},
			names:    []string{"demo"},
			wantErr:  `plugin "demo": the new version is named "other"`,
			wantCore: "v1\n",
		},
		{
			// 安装记录里没有 commit 的是本机目录来源。
			name:          "local_source_directory_removed",
			installRecord: "source: /missing/openvaala-plugin-source\npath: plugins/demo\ninstalled_at: 2026-10-23T10:00:00Z\n",
			names:         []string{"demo"},
			wantErr:       `plugin "demo": plugin source directory /missing/openvaala-plugin-source does not exist`,
			wantCore:      "v1\n",
		},
		{
			name:          "recorded_path_outside_repository",
			installRecord: "source: file://{repository}\npath: ../x\ncommit: abc123\ninstalled_at: 2026-10-23T10:00:00Z\n",
			names:         []string{"demo"},
			wantErr:       `plugin "demo": --path must be a relative path inside the repository`,
			wantCore:      "v1\n",
		},
		{
			name: "new_version_breaks_config",
			repositoryFiles: map[string]string{
				"plugins/demo/.openvaala-plugin/plugin.yaml": "name: demo\nmcp_servers:\n  - {id: demo-docs, type: stdio, command: server, executors: [local]}\n",
				"plugins/demo/core.md":                       "v2\n",
			},
			names:    []string{"demo"},
			wantErr:  "plugin mcp server cannot set executors",
			wantCore: "v1\n",
		},
	}
	// 2. 装好 demo、放好 manual，按用例改仓库或安装记录后更新，核对返回名单、错误、core.md、安装记录和配置能否加载。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repository := filepath.Join(t.TempDir(), "repo")
			commitPluginRepository(t, repository, getDemoRepositoryFiles("v1\n"))
			home, configDirectory := newPluginConfigHome(t, getPluginFiles("plugins/manual", "name: manual\n", nil))
			opt := &config.LoadConfigOpt{Home: home}
			if _, _, err := config.InstallPlugin(context.Background(), opt, &config.InstallPluginRequest{
				Source: "file://" + repository,
				Path:   "plugins/demo",
			}); err != nil {
				t.Fatal(err)
			}
			pluginsDirectory := filepath.Join(configDirectory, "plugins")
			demoDirectory := filepath.Join(pluginsDirectory, "demo")
			if tt.installRecord != "" {
				writeDefinitionFiles(t, demoDirectory, map[string]string{".openvaala-install.yaml": strings.ReplaceAll(tt.installRecord, "{repository}", repository)})
			}
			recordBefore := readPluginInstallRecord(t, demoDirectory)
			headCommit := recordBefore.Commit
			if tt.repositoryFiles != nil {
				headCommit = commitPluginRepository(t, repository, tt.repositoryFiles)
			}

			updated, err := config.UpdatePlugins(context.Background(), opt, tt.names)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			// 每个插件的错误都以 plugin "<名字>": 开头。
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.HasPrefix(err.Error(), fmt.Sprintf("plugin %q: ", tt.names[0]))) {
				t.Fatalf("UpdatePlugins() error = %v, want %q", err, tt.wantErr)
			}
			if !slices.Equal(updated, tt.wantUpdated) {
				t.Fatalf("UpdatePlugins() = %v, want %v", updated, tt.wantUpdated)
			}
			if entries := getPluginEntryNames(t, pluginsDirectory); !slices.Equal(entries, []string{"demo", "manual"}) {
				t.Fatalf("plugins directory has %v, want demo and manual only", entries)
			}
			if core := readTestFile(t, filepath.Join(demoDirectory, "core.md")); core != tt.wantCore {
				t.Fatalf("demo core.md = %q, want %q", core, tt.wantCore)
			}
			record := readPluginInstallRecord(t, demoDirectory)
			if tt.isNewVersion {
				if record.Commit != headCommit || !record.InstalledAt.After(recordBefore.InstalledAt) {
					t.Fatalf("demo install record = %+v, want commit %s installed after %v", record, headCommit, recordBefore.InstalledAt)
				}
			} else if !reflect.DeepEqual(record, recordBefore) {
				t.Fatalf("demo install record = %+v, want unchanged %+v", record, recordBefore)
			}
			if _, err := os.Stat(filepath.Join(pluginsDirectory, "manual", ".openvaala-plugin", "plugin.yaml")); err != nil {
				t.Fatalf("manual plugin changed: %v", err)
			}
			if _, err := config.LoadConfig(opt); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestUninstallPlugin 验证卸载插件：删除插件目录（符号链接只删链接）并去掉 disabled 里的名字；卸载后配置加载失败时恢复目录和 config.yaml。
func TestUninstallPlugin(t *testing.T) {
	// 1. 准备插件目录、链接和不存在的名字。
	tests := []struct {
		name string
		// files 相对配置目录；symlinks 是相对配置目录的符号链接到它指向的目录（也相对配置目录）。
		files    map[string]string
		symlinks map[string]string
		// uninstallName 是要卸载的插件名。
		uninstallName string
		wantErr       string
		// wantConfig 是结束后 config.yaml 的内容；出错时就是原内容。
		wantConfig string
		// removedPaths 结束后必须不存在（不跟随符号链接），keptPaths 必须还在；都相对配置目录。
		removedPaths []string
		keptPaths    []string
	}{
		{
			name: "removes_directory_and_disabled_name",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml": "# settings\nplugins:\n  disabled: [demo, other]\n",
			}),
			uninstallName: "demo",
			wantConfig:    "# settings\nplugins:\n  disabled: [other]\n",
			removedPaths:  []string{"plugins/demo"},
		},
		{
			name:          "symlink_removes_only_the_link",
			files:         mergeFiles(getPluginFiles("code/linked", "name: linked\n", nil), map[string]string{"config.yaml": "{}\n"}),
			symlinks:      map[string]string{"plugins/linked": "code/linked"},
			uninstallName: "linked",
			wantConfig:    "{}\n",
			removedPaths:  []string{"plugins/linked"},
			keptPaths:     []string{"code/linked/.openvaala-plugin/plugin.yaml"},
		},
		{
			name: "agent_uses_mcp_server",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml":        "# settings\n{}\n",
				"agents/worker.yaml": workerAgentFile,
			}),
			uninstallName: "demo",
			wantErr:       `mcp server "demo-docs" is not configured`,
			wantConfig:    "# settings\n{}\n",
			keptPaths:     []string{"plugins/demo/.openvaala-plugin/plugin.yaml", "plugins/demo/hooks/start.sh"},
		},
		{
			// 配置本来就加载不了，卸载后仍然不行：已经去掉的 disabled 项也要恢复。
			name: "agent_uses_mcp_server_of_disabled_plugin",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml":        "# settings\nplugins:\n  disabled: [demo]\n",
				"agents/worker.yaml": workerAgentFile,
			}),
			uninstallName: "demo",
			wantErr:       `mcp server "demo-docs" is not configured`,
			wantConfig:    "# settings\nplugins:\n  disabled: [demo]\n",
			keptPaths:     []string{"plugins/demo/.openvaala-plugin/plugin.yaml"},
		},
		{
			name:          "not_installed",
			files:         map[string]string{"config.yaml": "{}\n", "plugins/demo/README.md": "x\n"},
			uninstallName: "x",
			wantErr:       `plugin "x" is not installed`,
			wantConfig:    "{}\n",
			keptPaths:     []string{"plugins/demo/README.md"},
		},
	}
	// 2. 布置文件和链接后卸载，核对错误、config.yaml、删掉和留下的路径，以及没有残留临时目录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, configDirectory := newPluginConfigHome(t, tt.files)
			for link, target := range tt.symlinks {
				linkPath := filepath.Join(configDirectory, link)
				if err := os.MkdirAll(filepath.Dir(linkPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(configDirectory, target), linkPath); err != nil {
					t.Fatal(err)
				}
			}
			opt := &config.LoadConfigOpt{Home: home}

			err := config.UninstallPlugin(opt, tt.uninstallName)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("UninstallPlugin() error = %v, want %q", err, tt.wantErr)
			}
			if content := readTestFile(t, filepath.Join(configDirectory, "config.yaml")); content != tt.wantConfig {
				t.Fatalf("config.yaml = %q, want %q", content, tt.wantConfig)
			}
			for _, path := range tt.removedPaths {
				if _, err := os.Lstat(filepath.Join(configDirectory, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s still exists: %v", path, err)
				}
			}
			for _, path := range tt.keptPaths {
				if _, err := os.Stat(filepath.Join(configDirectory, path)); err != nil {
					t.Fatalf("%s is gone: %v", path, err)
				}
			}
			for _, name := range getPluginEntryNames(t, filepath.Join(configDirectory, "plugins")) {
				if strings.HasPrefix(name, ".") {
					t.Fatalf("plugins directory keeps temporary entry %s", name)
				}
			}
			if tt.wantErr == "" {
				if _, err := config.LoadConfig(opt); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestSetPluginDisabled 验证修改插件禁用状态：只改 plugins.disabled 并保留注释，配置加载失败时恢复原文件。
func TestSetPluginDisabled(t *testing.T) {
	// 1. 准备插件、配置文件和要做的禁用或启用。
	tests := []struct {
		name string
		// files 相对配置目录，没写 config.yaml 时配置文件不存在。
		files      map[string]string
		pluginName string
		isDisabled bool
		// isBrokenBefore 为 true 时修改之前整份配置加载失败。
		isBrokenBefore bool
		wantChanged    bool
		wantErr        string
		// wantConfig 是结束后 config.yaml 的内容；出错时就是原内容。
		wantConfig string
		// wantPlugins 是修改成功后加载出的插件名。
		wantPlugins []string
	}{
		{
			name: "disable_keeps_comments",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml": "# settings\nplugins:\n  # plugins turned off\n  disabled: [other] # keep this list short\n",
			}),
			pluginName:  "demo",
			isDisabled:  true,
			wantChanged: true,
			wantConfig:  "# settings\nplugins:\n  # plugins turned off\n  disabled: [other, demo] # keep this list short\n",
			wantPlugins: []string{},
		},
		{
			name: "already_disabled",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml": "plugins:\n  disabled: [ demo ]\n",
			}),
			pluginName:  "demo",
			isDisabled:  true,
			wantConfig:  "plugins:\n  disabled: [ demo ]\n",
			wantPlugins: []string{},
		},
		{
			name: "enable_removes_name",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml": "# settings\nplugins:\n  disabled:\n    - demo\n    - other\n",
			}),
			pluginName:  "demo",
			wantChanged: true,
			wantConfig:  "# settings\nplugins:\n  disabled:\n    - other\n",
			wantPlugins: []string{"demo"},
		},
		{
			name:        "creates_missing_config",
			files:       getDemoPluginFiles("plugins/demo"),
			pluginName:  "demo",
			isDisabled:  true,
			wantChanged: true,
			wantConfig:  "plugins:\n  disabled:\n    - demo\n",
			wantPlugins: []string{},
		},
		{
			name: "broken_plugin_can_be_disabled",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), getPluginFiles("plugins/broken", "name: other\n", nil), map[string]string{
				"config.yaml": "{}\n",
			}),
			pluginName:     "broken",
			isDisabled:     true,
			isBrokenBefore: true,
			wantChanged:    true,
			wantConfig:     "plugins:\n  disabled:\n    - broken\n",
			wantPlugins:    []string{"demo"},
		},
		{
			name: "agent_uses_mcp_server",
			files: mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{
				"config.yaml":        "# settings\n{}\n",
				"agents/worker.yaml": workerAgentFile,
			}),
			pluginName: "demo",
			isDisabled: true,
			wantErr:    `mcp server "demo-docs" is not configured`,
			wantConfig: "# settings\n{}\n",
		},
		{
			name:       "not_installed",
			files:      map[string]string{"config.yaml": "{}\n"},
			pluginName: "x",
			isDisabled: true,
			wantErr:    `plugin "x" is not installed`,
			wantConfig: "{}\n",
		},
		{
			// / 拼出来的是 plugins 目录本身，不是插件。
			name:       "plugins_directory_is_not_a_plugin",
			files:      mergeFiles(getDemoPluginFiles("plugins/demo"), map[string]string{"config.yaml": "{}\n"}),
			pluginName: "/",
			isDisabled: true,
			wantErr:    `plugin "/" is not installed`,
			wantConfig: "{}\n",
		},
	}
	// 2. 修改禁用状态，核对有没有变化、错误、config.yaml 和加载出的插件。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, configDirectory := newPluginConfigHome(t, tt.files)
			opt := &config.LoadConfigOpt{Home: home}
			if _, err := config.LoadConfig(opt); (err != nil) != tt.isBrokenBefore {
				t.Fatalf("LoadConfig() before = %v, want broken %v", err, tt.isBrokenBefore)
			}

			isChanged, err := config.SetPluginDisabled(opt, tt.pluginName, tt.isDisabled)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("SetPluginDisabled() error = %v, want %q", err, tt.wantErr)
			}
			if isChanged != tt.wantChanged {
				t.Fatalf("SetPluginDisabled() = %v, want %v", isChanged, tt.wantChanged)
			}
			if content := readTestFile(t, filepath.Join(configDirectory, "config.yaml")); content != tt.wantConfig {
				t.Fatalf("config.yaml = %q, want %q", content, tt.wantConfig)
			}
			if tt.wantErr != "" {
				return
			}
			cfg, err := config.LoadConfig(opt)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, plugin := range getLoadedPlugins(t, cfg) {
				names = append(names, plugin.Name)
			}
			if !slices.Equal(names, tt.wantPlugins) {
				t.Fatalf("loaded plugins = %v, want %v", names, tt.wantPlugins)
			}
		})
	}
}

// TestListPlugins 验证列出插件目录：带上版本、禁用状态和安装记录，不是插件的目录带错误，隐藏条目和普通文件不列。
func TestListPlugins(t *testing.T) {
	// 1. 准备装好的、手动放入的、禁用的和不是插件的条目。
	tests := []struct {
		name  string
		files map[string]string
		want  []config.PluginListEntry
	}{
		{
			name: "installed_manual_disabled_and_not_a_plugin",
			files: mergeFiles(
				getPluginFiles("plugins/demo", "name: demo\nversion: 0.1.0\n", map[string]string{
					".openvaala-install.yaml": "source: file:///repo\npath: plugins/demo\ncommit: abc123\ninstalled_at: 2026-10-23T10:00:00Z\n",
				}),
				getPluginFiles("plugins/manual", "name: manual\n", nil),
				getPluginFiles("plugins/off", "name: off\nversion: 2.0.0\n", nil),
				// 名字以 . 开头的条目和普通文件不列出。
				getPluginFiles("plugins/.install-x", "name: x\n", nil),
				map[string]string{
					"config.yaml":             "plugins:\n  disabled: [off]\n",
					"plugins/empty/README.md": "not a plugin\n",
					"plugins/notes.txt":       "not a plugin\n",
				},
			),
			want: []config.PluginListEntry{
				{
					Name:    "demo",
					Version: "0.1.0",
					Record: &config.PluginInstallRecord{
						Source:      "file:///repo",
						Path:        "plugins/demo",
						Commit:      "abc123",
						InstalledAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC),
					},
				},
				{
					Name:  "empty",
					Error: "not a plugin: missing supported plugin manifest",
				},
				{Name: "manual"},
				{
					Name:       "off",
					Version:    "2.0.0",
					IsDisabled: true,
				},
			},
		},
		{
			name:  "no_plugins_directory",
			files: map[string]string{"config.yaml": "{}\n"},
		},
	}
	// 2. 列出插件，和期望逐项比较。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, _ := newPluginConfigHome(t, tt.files)
			entries, err := config.ListPlugins(&config.LoadConfigOpt{Home: home})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(entries, tt.want) {
				t.Fatalf("ListPlugins() = %+v, want %+v", entries, tt.want)
			}
		})
	}
}
