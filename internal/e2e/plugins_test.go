package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"gopkg.in/yaml.v3"
)

// pluginsManifest 是测试插件 demo 的 manifest：SessionStart 跑 hooks/inject，demo-docs 用插件自带的假 MCP server。
const pluginsManifest = `name: demo
version: 0.1.0
description: Demo plugin of the end-to-end test
hooks:
  SessionStart:
    - hooks:
        - type: command
          script: ./hooks/inject
mcp_servers:
  - id: demo-docs
    type: stdio
    command: ${OPENVAALA_PLUGIN_ROOT}/bin/mcpserver
`

// pluginsInjectScript 是插件的 hooks/inject：按 $0 找到插件根目录，读同一插件里的 core.md，作为 SessionStart 的 additionalContext 输出。
const pluginsInjectScript = `#!/bin/bash
core=$(cat "$(dirname "$0")/../core.md")
echo '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"'"$core"'"}}'
`

// pluginsSkillBody 是插件 skill demo:greet 的正文。
const pluginsSkillBody = "DEMO_GREET_BODY: greet the user by name."

// pluginsSkillFile 是插件的 skills/greet/SKILL.md，frontmatter 里是不带前缀的名字。
const pluginsSkillFile = "---\nname: greet\ndescription: Greet the user\n---\n" + pluginsSkillBody + "\n"

const (
	// pluginsCoreMarker 是 core.md 各版本共同的开头，用来确认请求里没有、或只有一份 core.md 的内容。
	pluginsCoreMarker = "DEMO_CORE_RULE"
	// pluginsCoreV1 是第一次提交的 core.md。
	pluginsCoreV1 = pluginsCoreMarker + " v1: answer in one line"
	// pluginsCoreV2 是 update_pulls_new_commit 新提交的 core.md。
	pluginsCoreV2 = pluginsCoreMarker + " v2: answer in two lines"
)

// pluginsDocsAgentFile 配一个用插件 MCP server demo-docs 的 Agent docs。
const pluginsDocsAgentFile = "name: docs\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: default\nmcp_servers: [demo-docs]\n"

// TestPluginsEndToEnd 检查插件安装、启停、更新、卸载和加载到对话的内容。
func TestPluginsEndToEnd(t *testing.T) {
	// 1. 构建模型与 MCP 服务，创建临时配置和插件 Git 仓库。
	binary := buildOpenVaala(t)
	model := newE2EModel(t)
	configPath := writeE2EConfig(t, model.server.URL)
	f := &pluginsFixture{
		mcp: &mcpFixture{
			binary:     binary,
			config:     configPath,
			model:      model,
			fakeServer: buildFakeMCPServer(t),
		},
		home:       filepath.Dir(configPath),
		repository: t.TempDir(),
	}
	f.writeRepository(t)

	// 2. 启动 serve 并登记清理（控制 Client 和 app-server 先退出，serve 最后停止），连上控制 Client 和 stdio app-server。
	startE2EServe(t, binary, configPath)
	stopServeOnCleanup(t, binary, configPath)
	f.mcp.control, _ = newE2ERuntimeClient(t, configPath)
	go func() {
		for range f.mcp.control.Events() {
		}
	}()
	f.wire = startAppServer(t, binary, configPath, "stdio")
	f.wire.call(t, "initialize", codexClientInfo, nil)

	// 3. 按顺序运行各用例，共用同一个 serve 和插件仓库，后面的用例接着前面的安装状态。
	for _, test := range []struct {
		name string
		run  func(t *testing.T, f *pluginsFixture)
	}{
		{
			name: "install_from_git",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 从 file:// 安装插件，检查服务重新加载和安装记录的提交号。
				output, err := f.runPlugin(t, "install", "file://"+f.repository, "--path", "plugins/demo")
				if err != nil || !strings.Contains(output, "installed plugin demo") || !strings.Contains(output, "configuration reloaded") {
					t.Fatalf("plugin install = %q, error %v, want demo installed and the serve reloaded", output, err)
				}
				record := f.getInstallRecord(t)
				if head := f.runGit(t, "rev-parse", "HEAD"); record.Commit != head || record.Source != "file://"+f.repository || record.Path != "plugins/demo" {
					t.Errorf("install record = %+v, want source file://%s, path plugins/demo and commit %s", record, f.repository, head)
				}
			},
		},
		{
			name: "new_thread_uses_plugin",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 新建对话，检查首版插件内容。
				f.assertNewConversation(t, pluginsCoreV1)
			},
		},
		{
			name: "disable_removes_plugin",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 禁用插件，检查新对话不再加载插件内容。
				output, err := f.runPlugin(t, "disable", "demo")
				if err != nil || !strings.Contains(output, "disabled plugin demo") || !strings.Contains(output, "configuration reloaded") {
					t.Fatalf("plugin disable = %q, error %v, want demo disabled and the serve reloaded", output, err)
				}
				f.assertNewConversation(t, "")
			},
		},
		{
			name: "disable_rejected_when_agent_uses_mcp",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 启用插件并新增引用插件 MCP 的 Agent，检查禁用被拒且配置文件不变。
				if output, err := f.runPlugin(t, "enable", "demo"); err != nil || !strings.Contains(output, "configuration reloaded") {
					t.Fatalf("plugin enable = %q, error %v, want the serve reloaded", output, err)
				}
				f.mcp.writeFile(t, "agents/docs.yaml", pluginsDocsAgentFile)
				f.mcp.reload(t)
				before, err := os.ReadFile(f.mcp.config)
				if err != nil {
					t.Fatal(err)
				}
				output, err := f.runPlugin(t, "disable", "demo")
				if err == nil || !strings.Contains(output, `mcp server "demo-docs" is not configured`) {
					t.Errorf("plugin disable = %q, error %v, want it to fail because agent docs uses demo-docs", output, err)
				}
				if after, err := os.ReadFile(f.mcp.config); err != nil || string(after) != string(before) {
					t.Errorf("config.yaml after the rejected disable = %q (error %v), want it unchanged %q", after, err, before)
				}
				if err := os.Remove(filepath.Join(f.home, "agents", "docs.yaml")); err != nil {
					t.Fatal(err)
				}
				f.mcp.reload(t)
			},
		},
		{
			name: "update_pulls_new_commit",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 打开对话并记录插件 MCP 进程，提交新版 core.md 后更新插件。
				observer := f.mcp.openConversation(t, "assistant")
				f.mcp.waitMCPServer(t, observer.conversation.ID, "demo-docs", mcpclient.ConnectionStateConnected)
				previousPID := f.getMCPServerPID(t, observer)

				f.writeRepositoryFile(t, "core.md", pluginsCoreV2, 0600)
				f.commitRepository(t, "update core")
				output, err := f.runPlugin(t, "update", "demo")
				if err != nil || !strings.Contains(output, "updated plugin demo") || !strings.Contains(output, "configuration reloaded") {
					t.Fatalf("plugin update = %q, error %v, want demo updated and the serve reloaded", output, err)
				}
				if record, head := f.getInstallRecord(t), f.runGit(t, "rev-parse", "HEAD"); record.Commit != head {
					t.Errorf("install record commit = %s, want the new HEAD %s", record.Commit, head)
				}

				// 2. 检查 demo-docs 换了新进程，等旧进程退出后检查新对话收到新版内容。
				f.mcp.waitMCPServer(t, observer.conversation.ID, "demo-docs", mcpclient.ConnectionStateConnected)
				if pid := f.getMCPServerPID(t, observer); pid == previousPID {
					t.Errorf("demo-docs pid after the update = %d, want a restarted process instead of %d", pid, previousPID)
				}
				waitProcessGone(t, previousPID)
				f.assertNewConversation(t, pluginsCoreV2)
			},
		},
		{
			name: "uninstall",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 卸载插件，检查服务重新加载且插件目录删除。
				output, err := f.runPlugin(t, "uninstall", "demo")
				if err != nil || !strings.Contains(output, "uninstalled plugin demo") || !strings.Contains(output, "configuration reloaded") {
					t.Fatalf("plugin uninstall = %q, error %v, want demo uninstalled and the serve reloaded", output, err)
				}
				if _, err := os.Lstat(filepath.Join(f.home, "plugins", "demo")); !os.IsNotExist(err) {
					t.Errorf("stat plugins/demo after uninstall = %v, want it removed", err)
				}
			},
		},
		{
			name: "install_errors",
			run: func(t *testing.T, f *pluginsFixture) {
				// 1. 重新安装 demo，分别检查缺少 manifest 和重复安装的错误。
				if output, err := f.runPlugin(t, "install", "file://"+f.repository, "--path", "plugins/demo"); err != nil {
					t.Fatalf("plugin install = %q, error %v", output, err)
				}
				for _, test := range []struct {
					name, path, wantError string
				}{
					{
						name:      "missing_manifest",
						path:      "plugins/none",
						wantError: "plugin not found at plugins/none; use --path to point at the plugin directory",
					},
					{
						name:      "already_installed",
						path:      "plugins/demo",
						wantError: `plugin "demo" is already installed; use openvaala plugin update demo`,
					},
				} {
					t.Run(test.name, func(t *testing.T) {
						output, err := f.runPlugin(t, "install", "file://"+f.repository, "--path", test.path)
						if err == nil || !strings.Contains(output, test.wantError) {
							t.Errorf("plugin install --path %s = %q, error %v, want the error %q", test.path, output, err, test.wantError)
						}
					})
				}
				entries, err := os.ReadDir(filepath.Join(f.home, "plugins"))
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				if !slices.Equal(names, []string{"demo"}) {
					t.Errorf("plugins directory = %v, want only demo", names)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, f) })
	}
}

// pluginsFixture 是 TestPluginsEndToEnd 各用例共用的环境：临时 HOME 上的 serve、插件来源的 git 仓库和一条 stdio app-server 连接。
type pluginsFixture struct {
	// mcp 带着 openvaala 二进制、配置路径、假模型、假 MCP server 和控制 Client，用来新建对话、等 MCP 连接状态和 reload。
	mcp *mcpFixture
	// home 是临时 HOME，也是配置目录；插件装在 home/plugins。
	home string
	// repository 是插件来源的 git 仓库，插件在其中的 plugins/demo。
	repository string
	wire       *codexWire
}

// writeRepository 初始化并提交带 Skill、hook 和 MCP 服务的测试插件仓库。
func (f *pluginsFixture) writeRepository(t *testing.T) {
	t.Helper()
	// 1. 读取假 MCP 程序，初始化仓库，写入各插件文件并提交。
	fakeServer, err := os.ReadFile(f.mcp.fakeServer)
	if err != nil {
		t.Fatal(err)
	}
	f.runGit(t, "init", "--quiet")
	for _, file := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{
			name:    ".openvaala-plugin/plugin.yaml",
			content: pluginsManifest,
			mode:    0600,
		},
		{
			name:    "skills/greet/SKILL.md",
			content: pluginsSkillFile,
			mode:    0600,
		},
		{
			name:    "core.md",
			content: pluginsCoreV1,
			mode:    0600,
		},
		{
			name:    "hooks/inject",
			content: pluginsInjectScript,
			mode:    0700,
		},
		{
			name:    "bin/mcpserver",
			content: string(fakeServer),
			mode:    0700,
		},
	} {
		f.writeRepositoryFile(t, file.name, file.content, file.mode)
	}
	f.commitRepository(t, "add demo plugin")
}

// writeRepositoryFile 写入测试仓库内的插件文件。
func (f *pluginsFixture) writeRepositoryFile(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	// 1. 创建 plugins/demo 下的父目录，按指定权限写入文件。
	path := filepath.Join(f.repository, "plugins", "demo", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// commitRepository 将测试仓库全部改动提交一次。
func (f *pluginsFixture) commitRepository(t *testing.T, message string) {
	t.Helper()
	// 1. 暂存全部改动，使用测试作者信息创建提交。
	f.runGit(t, "add", "-A")
	f.runGit(t, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", message)
}

// runGit 在插件仓库里以临时 HOME 运行 git，返回去掉首尾空白的输出。
func (f *pluginsFixture) runGit(t *testing.T, arguments ...string) string {
	t.Helper()
	// 1. 执行命令，失败时结束测试，成功时返回合并输出。
	command := exec.Command("git", arguments...)
	command.Dir, command.Env = f.repository, getE2EEnvironment(f.home)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

// runPlugin 运行插件命令并返回合并输出和错误。
func (f *pluginsFixture) runPlugin(t *testing.T, arguments ...string) (string, error) {
	t.Helper()
	// 1. 设置超时和临时 HOME，执行 openvaala plugin 子命令。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, f.mcp.binary, append([]string{"--config-file", f.mcp.config, "plugin"}, arguments...)...)
	command.Env = getE2EEnvironment(f.home)
	output, err := command.CombinedOutput()
	return string(output), err
}

// getInstallRecord 读取 demo 插件的安装记录。
func (f *pluginsFixture) getInstallRecord(t *testing.T) config.PluginInstallRecord {
	t.Helper()
	// 1. 读取 .openvaala-install.yaml 并解码。
	payload, err := os.ReadFile(filepath.Join(f.home, "plugins", "demo", ".openvaala-install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var record config.PluginInstallRecord
	if err := yaml.Unmarshal(payload, &record); err != nil {
		t.Fatalf("install record %s: %v", payload, err)
	}
	return record
}

// assertNewConversation 检查新对话里的插件上下文、Skill、MCP 工具和 hook 列表；core 为空表示插件未启用。
func (f *pluginsFixture) assertNewConversation(t *testing.T, core string) {
	t.Helper()
	// 1. 新建对话并发送 /demo:greet，收集模型消息后完成 Turn。
	observer := f.mcp.openConversation(t, "assistant")
	observer.startTurn(t, "/demo:greet hi")
	request := f.mcp.model.next(t)
	var text strings.Builder
	for _, message := range getModelMessageTexts(request.body) {
		text.WriteString(message.text + "\n")
	}
	messages := text.String()
	request.reply <- e2eModelReply{text: "hi"}
	f.mcp.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")

	// 2. 按插件是否启用，检查 core.md 内容出现一份或没有，并比较 Skill、MCP 工具和 hook 列表。
	isPluginEnabled := core != ""
	wantCoreCount := 0
	if isPluginEnabled {
		wantCoreCount = 1
	}
	if strings.Count(messages, pluginsCoreMarker) != wantCoreCount || !strings.Contains(messages, core) {
		t.Errorf("first model request messages = %q, want %d core.md content from the plugin's SessionStart hook: %q", messages, wantCoreCount, core)
	}
	if strings.Contains(messages, pluginsSkillBody) != isPluginEnabled {
		t.Errorf("first model request messages = %q, want the demo:greet body: %v", messages, isPluginEnabled)
	}
	if names := getModelToolNames(request.body); slices.Contains(names, "mcp__demo-docs__echo") != isPluginEnabled {
		t.Errorf("first model request tools = %v, want mcp__demo-docs__echo: %v", names, isPluginEnabled)
	}
	hooks := f.listHooks(t)
	isPluginHookListed := len(hooks) == 1 && hooks[0].Key == "plugin:demo:SessionStart:0" && hooks[0].EventName == "sessionStart" &&
		hooks[0].Source == "plugin" && hooks[0].PluginID != nil && *hooks[0].PluginID == "demo"
	if isPluginEnabled && !isPluginHookListed || !isPluginEnabled && len(hooks) != 0 {
		t.Errorf("hooks/list = %+v, want only the SessionStart hook of plugin demo: %v", hooks, isPluginEnabled)
	}
}

// listHooks 读取临时 HOME 目录的 hook 列表。
func (f *pluginsFixture) listHooks(t *testing.T) []hooksWireListedHook {
	t.Helper()
	// 1. 调用 hooks/list，要求恰好返回一组目录结果并取出 Hooks。
	var listed struct {
		Data []struct {
			Hooks []hooksWireListedHook `json:"hooks"`
		} `json:"data"`
	}
	f.wire.call(t, "hooks/list", map[string]any{"cwds": []string{f.home}}, &listed)
	if len(listed.Data) != 1 {
		t.Fatalf("hooks/list = %+v, want one entry for %s", listed, f.home)
	}
	return listed.Data[0].Hooks
}

// getMCPServerPID 调用插件 MCP 的 pid 工具并返回进程号。
func (f *pluginsFixture) getMCPServerPID(t *testing.T, observer *runtimeObserver) int {
	t.Helper()
	// 1. 提交一轮请求，调用 demo-docs 的 pid，解析结果后完成 Turn。
	observer.startTurn(t, "which process serves demo-docs")
	f.mcp.model.next(t).reply <- e2eModelReply{
		tool:      "mcp__demo-docs__pid",
		arguments: `{}`,
	}
	request := f.mcp.model.next(t)
	pid, err := strconv.Atoi(getMCPToolResultText(t, request.body))
	if err != nil {
		t.Fatalf("pid tool result: %v", err)
	}
	request.reply <- e2eModelReply{text: "done"}
	f.mcp.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
	return pid
}
