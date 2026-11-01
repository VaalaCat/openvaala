package desktop

import (
	"fmt"
	"github.com/VaalaCat/openvaala/pkg/session"
	"reflect"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// TestAgentTreeOrder 检查父节点后立即排列其后代，不混入其他分支。
func TestAgentTreeOrder(t *testing.T) {
	// 1. 输入刻意按 ID 交错，结果由真实父关系决定。
	for _, tt := range []struct {
		name  string
		infos []runtime.SessionRuntimeInfo
		want  []defs.SessionID
	}{{"interleaved", []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "A"}}, {Session: defs.Session{ID: "B"}}, {Session: defs.Session{ID: "C", ParentSessionID: "A"}}, {Session: defs.Session{ID: "D", ParentSessionID: "C"}}}, []defs.SessionID{"A", "C", "D", "B"}}, {"parent absent", []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "child", ParentSessionID: "missing"}}}, []defs.SessionID{"child"}}} {
		t.Run(tt.name, func(t *testing.T) {
			got := []defs.SessionID{}
			for _, info := range orderedSessionInfos(tt.infos) {
				got = append(got, info.Session.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("order=%v", got)
			}
		})
	}
}

// TestTurnHistoryReading 检查原生 Turn 列表按视口绘制并保留末尾滚动入口。
func TestTurnHistoryReading(t *testing.T) {
	// 1. 多页数量不全量构建，滚动到末尾后可读取最后一轮。
	for _, count := range []int{30, 120} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d := testDesktop()
			for index := range count {
				d.turnPage = append(d.turnPage, defs.Turn{ID: defs.TurnID(fmt.Sprintf("turn-%03d", index)), Status: defs.TurnStatusCompleted, Usage: defs.TokenUsage{Input: index + 1, Output: index + 2}})
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.turnsView(c) }) }, 340, 600)
			// 序号从最早一轮数起，最后一行是 #1，没有输入摘要时标题是用量。
			last := fmt.Sprintf("#1 %d / %d", count, count+1)
			if tester.HasText(last) {
				t.Fatal("entire turn history constructed outside viewport")
			}
			d.turnList.ScrollToEnd()
			tester.Frame()
			if !tester.HasText(last) {
				t.Fatalf("last turn missing: %q", tester.Texts())
			}
		})
	}
}

// TestPanelRows 检查 Agent 行的非空闲状态、上下文事实的估算与未知写法，以及 Turn 行的 token 与状态。
func TestPanelRows(t *testing.T) {
	for _, tt := range []struct {
		name  string
		build func(*Desktop)
		view  func(*Desktop, *ui.Context)
		want  []string
		deny  []string
	}{
		{"agent states", func(d *Desktop) {
			d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "a", AgentName: "default"}, State: session.SessionStateIdle}, {Session: defs.Session{ID: "b", AgentName: "worker"}, State: session.SessionStateStopped}}
		}, (*Desktop).agentsTreeView, []string{"已停止", "worker"}, []string{"空闲"}},
		{"estimated and unknown facts", func(d *Desktop) {
			d.contextUsage = &runtime.SessionContextUsage{NextRequest: runtime.ContextUsage{Tokens: 1500, Window: 0, Source: runtime.ContextUsageSourceEstimated}}
		}, (*Desktop).contextView, []string{"≈1.5K", "未知"}, nil},
		{"turn tokens", func(d *Desktop) {
			d.turnPage = []defs.Turn{{ID: "turn", Status: defs.TurnStatusFailed, Usage: defs.TokenUsage{Input: 1200, Output: 34}}}
		}, (*Desktop).turnsView, []string{"1.2K / 34"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tt.build(d)
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { tt.view(d, c) }) }, 360, 600)
			for _, want := range tt.want {
				if !tester.HasText(want) {
					t.Fatalf("missing %q: %q", want, tester.Texts())
				}
			}
			for _, deny := range tt.deny {
				if tester.HasText(deny) {
					t.Fatalf("unexpected %q: %q", deny, tester.Texts())
				}
			}
		})
	}
}
