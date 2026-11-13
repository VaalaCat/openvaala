package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// TestConfigReload 检查 reload 配置后，已打开的空闲对话能用上新加的 Skill 和 Executor，删掉对话在用的 Executor 后还能继续。
func TestConfigReload(t *testing.T) {
	// 1. 构建程序，列出加配置和删配置的场景。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name string
		run  func(t *testing.T, f *conversationManagementFixture)
	}{
		{
			name: "skill_and_executor_reach_open_conversation",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 打开对话，再写入新的 Skill 和 Executor 配置文件。
				observer := f.openNewConversation(t)
				conversationID, rootSessionID := observer.conversation.ID, observer.conversation.RootSessionID
				f.writeConfigFiles(t, map[string]string{
					"skills/added/SKILL.md": "---\nname: added\ndescription: reload 之后加的技能\n---\nADDED_BODY\n",
					"executors/extra.yaml":  "id: extra\nkind: virtual\ndescription: reload 之后加的环境\n",
				})
				// 2. 重新加载配置，检查对话的 Skill 列表、Executor 列表和根 Session 的 Executor 里都有新加的项。
				f.reloadConfig(t)
				f.waitReloadedExecutor(t, observer.conversation, true)
				ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
				defer cancel()
				skills, err := f.control.GetSkills(ctx, &runtimeclient.GetSkillsRequest{ConversationID: conversationID})
				if err != nil || !slices.ContainsFunc(skills, func(info skill.SkillInfo) bool { return info.Name == "added" }) {
					t.Fatalf("skill/list of the open conversation = %+v, %v; want added", skills, err)
				}
				executors, err := f.control.ListExecutors(ctx)
				if err != nil || !slices.ContainsFunc(executors, isExtraExecutor) {
					t.Fatalf("executor/list = %+v, %v; want extra", executors, err)
				}
				details, err := f.control.GetSessionDetails(ctx, conversationID, rootSessionID)
				if err != nil || !slices.ContainsFunc(details.Executors, isExtraExecutor) {
					t.Fatalf("root Session executors = %+v, %v; want extra", details.Executors, err)
				}
			},
		},
		{
			name: "reload_removes_executor_in_use",
			run: func(t *testing.T, f *conversationManagementFixture) {
				// 1. 加上 extra，新建以它为默认 Executor 的对话并跑完一轮。
				f.writeConfigFiles(t, map[string]string{"executors/extra.yaml": "id: extra\nkind: virtual\ndescription: 用完要删的环境\n"})
				f.reloadConfig(t)
				record, err := f.control.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{
					AgentName:        "assistant",
					ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "extra"},
				})
				if err != nil {
					t.Fatal(err)
				}
				observer := openRuntimeObserver(t, f.config, record.ID)
				f.runTurn(t, observer, "第一轮", e2eModelReply{text: "好"})
				observer.stop()
				// 2. 删掉 extra 的配置文件后 reload，根 Session 不再有 extra。
				if err := os.Remove(filepath.Join(filepath.Dir(f.config), "executors", "extra.yaml")); err != nil {
					t.Fatal(err)
				}
				f.reloadConfig(t)
				f.waitReloadedExecutor(t, record, false)
				details, err := f.control.GetSessionDetails(t.Context(), record.ID, record.RootSessionID)
				if err != nil || slices.ContainsFunc(details.Executors, isExtraExecutor) {
					t.Fatalf("root Session executors = %+v, %v; want extra gone", details.Executors, err)
				}
				// 3. 下一轮对话正常完成。
				f.runTurn(t, openRuntimeObserver(t, f.config, record.ID), "第二轮", e2eModelReply{text: "好"})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, newConversationManagementFixture(t, binary)) })
	}
}

// isExtraExecutor 这是 reload 用例里加的 extra 吗？
func isExtraExecutor(executor conversation.ExecutorDetails) bool {
	// 1. 按 ID 判断。
	return executor.ID == "extra"
}

// writeConfigFiles 把 files 写进配置目录，键相对配置目录。
func (f *conversationManagementFixture) writeConfigFiles(t *testing.T, files map[string]string) {
	t.Helper()
	// 1. 逐个建好父目录再写入。
	configDirectory := filepath.Dir(f.config)
	for name, content := range files {
		path := filepath.Join(configDirectory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// reloadConfig 只重新加载一次配置，忙碌对话由服务自动应用。
func (f *conversationManagementFixture) reloadConfig(t *testing.T) {
	t.Helper()
	// 1. 提交一次请求，配置的实际应用由调用场景观察。
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	if _, err := f.control.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
}

// waitReloadedExecutor 等待根 Session 使用的 Executor 列表出现预期变更。
func (f *conversationManagementFixture) waitReloadedExecutor(t *testing.T, record defs.Conversation, wantExtra bool) {
	t.Helper()
	// 1. 只查询实际配置，不重新发送 reload。
	ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
	defer cancel()
	for {
		details, err := f.control.GetSessionDetails(ctx, record.ID, record.RootSessionID)
		if err == nil && slices.ContainsFunc(details.Executors, isExtraExecutor) == wantExtra {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for extra executor = %v: %v", wantExtra, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestConfigReloadDeferred 检查一次真实 reload 在 Turn 或后台命令结束后自动应用，并恢复客户端订阅。
func TestConfigReloadDeferred(t *testing.T) {
	// 1. Turn 场景走 CLI，命令场景走 RPC；两者只提交一次 reload。
	binary := buildOpenVaala(t)
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background_%v", background), func(t *testing.T) {
			f := newConversationManagementFixture(t, binary)
			observer := f.openNewConversation(t)
			observer.startTurn(t, "keep working")
			request := f.model.next(t)
			release := filepath.Join(observer.conversation.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID], "release-command")
			if background {
				command := fmt.Sprintf("while [ ! -f '%s' ]; do sleep 0.05; done", release)
				request.reply <- e2eModelReply{tool: "bash", arguments: fmt.Sprintf(`{"command":%q,"wait_seconds":0}`, command)}
				f.model.next(t).reply <- e2eModelReply{text: "command running"}
				readTurnFinished(t, observer, defs.TurnStatusCompleted)
			}
			f.writeConfigFiles(t, map[string]string{
				"skills/added/SKILL.md": "---\nname: added\ndescription: automatically added skill\n---\nADDED_BODY\n",
				"executors/extra.yaml":  "id: extra\nkind: virtual\ndescription: automatically added executor\n",
			})
			ctx, cancel := context.WithTimeout(t.Context(), e2eTimeout)
			defer cancel()
			if background {
				result, err := f.control.ReloadConfig(ctx)
				if err != nil || !slices.Equal(result.PendingConversationIDs, []defs.ConversationID{observer.conversation.ID}) {
					t.Fatalf("ReloadConfig() = %+v, %v", result, err)
				}
			} else {
				command := exec.CommandContext(ctx, binary, "--config-file", f.config, "serve", "reload")
				command.Env = getE2EEnvironment(filepath.Dir(f.config))
				output, err := command.CombinedOutput()
				if err != nil || !strings.Contains(string(output), "automatically when idle") || !strings.Contains(string(output), string(observer.conversation.ID)) {
					t.Fatalf("serve reload: %v\n%s", err, output)
				}
			}
			// 2. 经过一次定时检查仍不换环境，原任务自然完成后才切换。
			time.Sleep(1100 * time.Millisecond)
			details, err := f.control.GetSessionDetails(ctx, observer.conversation.ID, observer.conversation.RootSessionID)
			if err != nil || slices.ContainsFunc(details.Executors, isExtraExecutor) {
				t.Fatalf("configuration changed while busy: %+v, %v", details, err)
			}
			if background {
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				request.reply <- e2eModelReply{text: "old turn completed"}
				readTurnFinished(t, observer, defs.TurnStatusCompleted)
			}
			for {
				if synced, ok := observer.next(t).(*runtimeclient.SessionSynced); ok {
					if len(synced.Items) != 0 {
						t.Fatalf("resync repeated delivered items: %+v", synced.Items)
					}
					break
				}
			}
			f.waitReloadedExecutor(t, observer.conversation, true)
			// 3. 沿用同一个客户端继续发消息，新 Skill 进入下一轮请求。
			next, _ := f.runTurn(t, observer, "after automatic reload", e2eModelReply{text: "new configuration"})
			if !strings.Contains(fmt.Sprint(next.body), "automatically added skill") {
				t.Fatal("new skill did not reach model")
			}
		})
	}
}
