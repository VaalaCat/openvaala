package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
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
				f.reloadWhenIdle(t)
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
				f.reloadWhenIdle(t)
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
				f.reloadWhenIdle(t)
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

// reloadWhenIdle 重新加载配置，有对话在忙时重试，直到全部对话都换上新配置。
func (f *conversationManagementFixture) reloadWhenIdle(t *testing.T) {
	t.Helper()
	// 1. 把忙碌的对话当作 ErrSessionBusy 交给 callWhenIdle 重试。
	callWhenIdle(t, func(ctx context.Context) error {
		result, err := f.control.ReloadConfig(ctx)
		if err == nil && len(result.BusyConversationIDs) != 0 {
			return session.ErrSessionBusy
		}
		return err
	})
}
