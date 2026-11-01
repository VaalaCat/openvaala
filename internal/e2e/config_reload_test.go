package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/skill"
)

// TestConfigReload 检查 reload 配置后，已打开的空闲对话能用上新加的 Skill 和 Executor。
func TestConfigReload(t *testing.T) {
	// 1. 构建程序，列出 reload 前要写进配置目录的文件。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name string
		// files 是 reload 之前写进配置目录的文件，键相对配置目录。
		files        map[string]string
		wantSkill    string
		wantExecutor string
	}{
		{
			name: "skill_and_executor_reach_open_conversation",
			files: map[string]string{
				"skills/added/SKILL.md": "---\nname: added\ndescription: reload 之后加的技能\n---\nADDED_BODY\n",
				"executors/extra.yaml":  "id: extra\nkind: virtual\ndescription: reload 之后加的环境\n",
			},
			wantSkill:    "added",
			wantExecutor: "extra",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 打开对话，再写入新的 Skill 和 Executor 配置文件。
			f := newConversationManagementFixture(t, binary)
			observer := f.openNewConversation(t)
			conversationID, rootSessionID := observer.conversation.ID, observer.conversation.RootSessionID
			configDirectory := filepath.Dir(f.config)
			for name, content := range test.files {
				path := filepath.Join(configDirectory, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// 2. 重新加载配置，检查对话的 Skill 列表、Executor 列表和根 Session 的 Executor 里都有新加的项。
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancel()
			result, err := f.control.ReloadConfig(ctx)
			if err != nil || len(result.BusyConversationIDs) != 0 {
				t.Fatalf("ReloadConfig() = %+v, %v; want no busy conversation", result, err)
			}
			skills, err := f.control.GetSkills(ctx, &runtimeclient.GetSkillsRequest{ConversationID: conversationID})
			if err != nil || !slices.ContainsFunc(skills, func(info skill.SkillInfo) bool { return info.Name == test.wantSkill }) {
				t.Fatalf("skill/list of the open conversation = %+v, %v; want %s", skills, err, test.wantSkill)
			}
			isWantedExecutor := func(executor conversation.ExecutorDetails) bool { return executor.ID == test.wantExecutor }
			executors, err := f.control.ListExecutors(ctx)
			if err != nil || !slices.ContainsFunc(executors, isWantedExecutor) {
				t.Fatalf("executor/list = %+v, %v; want %s", executors, err, test.wantExecutor)
			}
			details, err := f.control.GetSessionDetails(ctx, conversationID, rootSessionID)
			if err != nil || !slices.ContainsFunc(details.Executors, isWantedExecutor) {
				t.Fatalf("root Session executors = %+v, %v; want %s", details.Executors, err, test.wantExecutor)
			}
		})
	}
}
