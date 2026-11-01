package prompt_test

import (
	"slices"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
)

// TestFormatSpawnAgentDescription 核对委派说明、Agent 和模型目录。
func TestFormatSpawnAgentDescription(t *testing.T) {
	// 1. 生成委派说明，核对 Agent、模型、窗口和推理强度目录。
	alpha := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "alpha",
		},
		DisplayName:            "Alpha",
		ContextWindow:          1000,
		ReasoningEfforts:       []string{"low", "high"},
		DefaultReasoningEffort: "low",
		Target:                 &llmkit.Target{APIKey: "SENTINEL-DO-NOT-EXPOSE"},
	}
	beta := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "p",
			ModelID:    "beta",
		},
		DisplayName:   "Beta",
		ContextWindow: 2000,
	}
	const instructions = "将可独立执行的工作委派给一个直接子 Agent，子任务可以并行运行。" +
		"message 应提供目标、必要材料和预期交付物；默认不继承历史，需要时明确设置 fork_turns。" +
		"返回 session_id、task_name 和 accepted 表示首个任务已接收，不表示已经完成；子任务每个 Turn 结束后，结果以通知消息自动进入你的收件箱，在下一次模型请求前读入。" +
		"需要暂停等待时使用 wait_agent，它只在新消息到达时唤醒、不返回正文。"
	for _, tt := range []struct {
		name   string
		agents []prompt.AgentEntry
		models []*defs.ModelDefinition
		want   string
	}{
		{
			name: "empty",
			want: "可选 agent_name（省略时与当前 Agent 相同）：\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：",
		},
		{
			name: "reversed",
			agents: []prompt.AgentEntry{{
				Name:        "worker",
				Description: "调查",
			}, {Name: "assistant"}},
			models: []*defs.ModelDefinition{beta, alpha},
			want:   "可选 agent_name（省略时与当前 Agent 相同）：\n- assistant\n- worker：调查\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：\n- p/alpha（Alpha）：窗口 1000，reasoning_effort 可选 low|high，默认 low\n- p/beta（Beta）：窗口 2000",
		},
		{
			name:   "without_efforts",
			models: []*defs.ModelDefinition{beta},
			want:   "可选 agent_name（省略时与当前 Agent 相同）：\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：\n- p/beta（Beta）：窗口 2000",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agents, models := slices.Clone(tt.agents), slices.Clone(tt.models)
			if got := prompt.FormatSpawnAgentDescription(tt.agents, tt.models); got != instructions+tt.want {
				t.Fatalf("got %q, want %q", got, instructions+tt.want)
			}
			if !slices.Equal(agents, tt.agents) || !slices.Equal(models, tt.models) {
				t.Fatal("input order changed")
			}
		})
	}
}

// TestFormatExecutorParameterDescription 核对执行位置参数中的默认环境。
func TestFormatExecutorParameterDescription(t *testing.T) {
	// 1. 生成执行位置参数说明，核对默认环境 ID。
	for _, id := range []string{"local", "sandbox%<>"} {
		t.Run(id, func(t *testing.T) {
			want := "执行此调用的 Executor，见系统提示 <openvaala-executors>；省略时使用 " + id
			if got := prompt.FormatExecutorParameterDescription(id); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}
