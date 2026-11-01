package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/turn"
	"go.opentelemetry.io/otel/codes"
)

// TestSession 检查输入按顺序执行、steer 并进活动 Turn、中断后未读 steer 重投、WaitSteer、修复 running Turn、失败和停止。
func TestSession(t *testing.T) {
	// 1. 列出排队、steer、WaitSteer、中断、停止、修复和终态保存失败场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "normal_inputs_run_in_order",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				b := h.submit(t, session.SessionInputModeNormal, "B")
				c := h.submit(t, session.SessionInputModeNormal, "C")
				for _, text := range []string{"a", "b", "c"} {
					h.model.replies <- modelReply{text: text}
				}
				for _, want := range [][]defs.ItemID{{a.ID}, {b.ID}, {c.ID}} {
					finished := h.nextTurnFinished(t)
					requireFinished(t, finished, want, defs.TurnStatusCompleted)
					if finished.FinalModelCallID == "" || len(finished.Outputs) != 1 {
						t.Fatalf("finished = %+v, want final model call and one output", finished)
					}
				}
			},
		},
		{
			name: "steer_joins_active_turn",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				s := h.submit(t, session.SessionInputModeSteer, "S")
				h.model.replies <- modelReply{text: "first"}
				h.model.replies <- modelReply{text: "second"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID, s.ID}, defs.TurnStatusCompleted)
				items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
				if len(items) != 4 || items[2].ID != s.ID || items[2].TurnID != items[0].TurnID {
					t.Fatalf("log = %+v, want steer persisted in the same Turn", items)
				}
			},
		},
		{
			name: "steer_when_idle_runs_as_normal",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				s := h.submit(t, session.SessionInputModeSteer, "S")
				h.model.replies <- modelReply{text: "answer"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{s.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "steer_after_finish_boundary_queues",
			run: func(t *testing.T, h *sessionHarness) {
				mutation := &blockingTerminalMutation{
					Mutation:        h.mutation,
					terminalStarted: make(chan struct{}),
					release:         make(chan struct{}),
				}
				h.start(t, mutation, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.model.replies <- modelReply{text: "a"}
				waitClosed(t, mutation.terminalStarted, "Turn A terminal write")
				// 首轮已停止接收补充输入但还是活动 Turn，之后的 steer 排在普通消息后面。
				b := h.submit(t, session.SessionInputModeNormal, "B")
				s := h.submit(t, session.SessionInputModeSteer, "S")
				close(mutation.release)
				h.model.replies <- modelReply{text: "b"}
				h.model.replies <- modelReply{text: "s"}
				for _, want := range [][]defs.ItemID{{a.ID}, {b.ID}, {s.ID}} {
					requireFinished(t, h.nextTurnFinished(t), want, defs.TurnStatusCompleted)
				}
			},
		},
		{
			name: "unread_steer_requeued_after_interrupt",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				h.model.replies <- modelReply{hold: true}
				h.waitModelStreaming(t)
				b := h.submit(t, session.SessionInputModeNormal, "B")
				s := h.submit(t, session.SessionInputModeSteer, "S")
				status, err := h.session.GetStatus(t.Context())
				if err != nil || status.State != session.SessionStateRunning || status.PendingInputCount != 1 {
					t.Fatalf("GetStatus() = %+v, %v", status, err)
				}
				turnID, err := h.session.InterruptTurn(t.Context())
				if err != nil || turnID != status.ActiveTurnID {
					t.Fatalf("InterruptTurn() = %q, %v, want %q", turnID, err, status.ActiveTurnID)
				}
				interrupted := h.nextTurnFinished(t)
				requireFinished(t, interrupted, []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				if interrupted.TurnID != turnID || interrupted.Err == nil || interrupted.FinalModelCallID != "" {
					t.Fatalf("interrupted = %+v", interrupted)
				}
				h.model.replies <- modelReply{text: "s"}
				h.model.replies <- modelReply{text: "b"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{s.ID}, defs.TurnStatusCompleted)
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{b.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "wait_steer_wakes_on_steer",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				waited := make(chan waitSteerResult, 1)
				go func() {
					items, err := h.session.WaitSteer(t.Context())
					waited <- waitSteerResult{
						items: items,
						err:   err,
					}
				}()
				// 先确认 WaitSteer 还没返回，再提交 steer。
				select {
				case result := <-waited:
					t.Fatalf("WaitSteer() returned before steer: %+v", result)
				case <-time.After(50 * time.Millisecond):
				}
				s := h.submit(t, session.SessionInputModeSteer, "S")
				result := <-waited
				if result.err != nil || len(result.items) != 1 || result.items[0].ID != s.ID || result.items[0].TurnID != "" {
					t.Fatalf("WaitSteer() = %+v, want unread copy of %q", result, s.ID)
				}
				h.model.replies <- modelReply{text: "first"}
				h.model.replies <- modelReply{text: "second"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID, s.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "wait_steer_released_when_turn_ends",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				waited := make(chan waitSteerResult, 1)
				go func() {
					items, err := h.session.WaitSteer(t.Context())
					waited <- waitSteerResult{
						items: items,
						err:   err,
					}
				}()
				// 等一会儿再结束 Turn，检查 WaitSteer 返回空。
				time.Sleep(50 * time.Millisecond)
				h.model.replies <- modelReply{text: "a"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusCompleted)
				select {
				case result := <-waited:
					if result.err != nil || len(result.items) != 0 {
						t.Fatalf("WaitSteer() = %+v, want empty result", result)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("WaitSteer() was not released when the Turn ended")
				}
			},
		},
		{
			name: "wait_steer_returns_immediately_when_pending",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				s := h.submit(t, session.SessionInputModeSteer, "S")
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				items, err := h.session.WaitSteer(ctx)
				if err != nil || len(items) != 1 || items[0].ID != s.ID {
					t.Fatalf("WaitSteer() = %+v, %v, want %q", items, err, s.ID)
				}
				h.model.replies <- modelReply{text: "first"}
				h.model.replies <- modelReply{text: "second"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID, s.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "status_and_interrupt_while_streaming",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				input := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				h.model.replies <- modelReply{hold: true}
				h.waitModelStreaming(t)
				status, err := h.session.GetStatus(t.Context())
				if err != nil || status.State != session.SessionStateRunning {
					t.Fatalf("status = %+v, %v", status, err)
				}
				turnID, err := h.session.InterruptTurn(t.Context())
				if err != nil || turnID != status.ActiveTurnID {
					t.Fatalf("interrupt = %s, %v", turnID, err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{input.ID}, defs.TurnStatusInterrupted)
			},
		},
		{
			name: "stop_closes_events_and_drops_pending",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				h.model.replies <- modelReply{hold: true}
				h.waitModelStreaming(t)
				h.submit(t, session.SessionInputModeNormal, "B")
				h.cancel()
				var finished []*session.SessionTurnFinished
				for event := range h.received {
					if value, ok := event.(*session.SessionTurnFinished); ok {
						finished = append(finished, value)
					}
				}
				if len(finished) != 1 {
					t.Fatalf("finished turns = %+v, want only A", finished)
				}
				requireFinished(t, finished[0], []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				if err := h.waitRunStopped(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() error = %v", err)
				}
				if count := h.model.count.Load(); count != 1 {
					t.Fatalf("model requests = %d, want 1", count)
				}
				if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
					Mode: session.SessionInputModeNormal,
					Item: sessionInput("late"),
				}); err == nil {
					t.Fatal("HandleInput() after stop error = nil")
				}
				status, err := h.session.GetStatus(t.Context())
				if err != nil || status.State != session.SessionStateStopped || status.LastTurnStatus != defs.TurnStatusInterrupted {
					t.Fatalf("GetStatus() = %+v, %v", status, err)
				}
			},
		},
		{
			name: "repair_latest_running_turn",
			run: func(t *testing.T, h *sessionHarness) {
				running := defs.Turn{
					ID:             defs.NewTurnID(),
					ConversationID: h.record.ConversationID,
					SessionID:      h.record.ID,
					Status:         defs.TurnStatusRunning,
				}
				if err := h.mutation.SetTurn(t.Context(), running); err != nil {
					t.Fatal(err)
				}
				h.start(t, nil, true)
				status, err := h.session.GetStatus(t.Context())
				if err != nil || status.State != session.SessionStateIdle || status.LastTurnID != running.ID || status.LastTurnStatus != defs.TurnStatusInterrupted {
					t.Fatalf("GetStatus() = %+v, %v", status, err)
				}
				got, found, err := h.query.GetTurn(t.Context(), h.record.ID, running.ID)
				if err != nil || !found || got.Status != defs.TurnStatusInterrupted || got.Error == "" {
					t.Fatalf("GetTurn() = %+v, %v, %v", got, found, err)
				}
			},
		},
		{
			name: "interrupted_items_persisted",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				h.model.replies <- modelReply{
					hold: true,
					chunks: []string{
						`{"choices":[{"delta":{"reasoning_content":"先思考。"}}]}`,
						`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]}}]}`,
					},
				}
				// 等参数增量到了再中断，检查保存的工具调用带着已收到的参数。
				for {
					event := h.nextEvent(t)
					agentEvent, ok := event.(*session.SessionAgentEvent)
					if ok && agentEvent.Event.Type == agent.AgentEventItemDelta && agentEvent.Event.Delta != nil && agentEvent.Event.Delta.Arguments != "" {
						break
					}
				}
				if _, err := h.session.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
				if len(items) != 4 || items[1].Kind != defs.ItemReasoning || items[2].Kind != defs.ItemToolCall || items[3].Kind != defs.ItemTurnInterrupted {
					t.Fatalf("log = %+v, want input, reasoning, tool call and interrupted item", items)
				}
				if items[2].ToolCall.Status != defs.ToolCallInterrupted || items[2].ToolCall.Arguments != `{"q"` {
					t.Fatalf("interrupted tool call = %+v", items[2].ToolCall)
				}
			},
		},
		{
			name: "failed_turn_continues_session",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.model.replies <- modelReply{status: http.StatusBadRequest}
				failed := h.nextTurnFinished(t)
				requireFinished(t, failed, []defs.ItemID{a.ID}, defs.TurnStatusFailed)
				if failed.Err == nil {
					t.Fatal("failed Turn error = nil")
				}
				b := h.submit(t, session.SessionInputModeNormal, "B")
				h.model.replies <- modelReply{text: "recovered"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{b.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "unsettled_turn_stops_session",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, &failingTerminalMutation{Mutation: h.mutation}, true)
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				h.submit(t, session.SessionInputModeNormal, "B")
				h.model.replies <- modelReply{text: "cannot settle"}
				finished := h.nextTurnFinished(t)
				if !slices.Equal(finished.InputItemIDs, []defs.ItemID{a.ID}) || finished.Status != "" || !errors.Is(finished.Err, turn.ErrTurnUnsettled) {
					t.Fatalf("finished = %+v", finished)
				}
				for range h.received {
				}
				if err := h.waitRunStopped(t); !errors.Is(err, turn.ErrTurnUnsettled) {
					t.Fatalf("Run() error = %v", err)
				}
				if count := h.model.count.Load(); count != 1 {
					t.Fatalf("model requests = %d, want 1", count)
				}
			},
		},
	}
	// 2. 每个场景用独立的 Session 执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newSessionHarness(t))
		})
	}
}

// TestSessionInputID 检查重复 InputID 只接收一次、只跑一轮，不同或空的 InputID 各跑一轮。
func TestSessionInputID(t *testing.T) {
	// 1. 准备重复、不同和空 InputID。
	tests := []struct {
		name      string
		inputIDs  [2]defs.InputID
		wantSame  bool
		wantTurns int
	}{
		{
			name:      "duplicate_input_id_accepted_once",
			inputIDs:  [2]defs.InputID{"k1", "k1"},
			wantSame:  true,
			wantTurns: 1,
		},
		{
			name:      "distinct_input_ids_queue_separately",
			inputIDs:  [2]defs.InputID{"k1", "k2"},
			wantTurns: 2,
		},
		{
			name:      "empty_input_id_not_deduplicated",
			wantTurns: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 提交两条输入，核对接收的 ID 和实际执行的轮数。
			h := newSessionHarness(t)
			h.start(t, nil, true)
			var itemIDs [2]defs.ItemID
			for index, inputID := range tt.inputIDs {
				itemID, err := h.session.HandleInput(t.Context(), &session.SessionInput{
					Mode:    session.SessionInputModeNormal,
					Item:    sessionInput("同一条输入"),
					InputID: inputID,
				})
				if err != nil {
					t.Fatal(err)
				}
				itemIDs[index] = itemID
			}
			if (itemIDs[0] == itemIDs[1]) != tt.wantSame {
				t.Fatalf("ItemIDs = %v, want same = %v", itemIDs, tt.wantSame)
			}
			for range tt.wantTurns {
				h.waitModelStarted(t)
				h.model.replies <- modelReply{text: "收到"}
				requireFinished(t, h.nextTurnFinished(t), nil, defs.TurnStatusCompleted)
			}
			select {
			case <-h.model.started:
				t.Fatalf("unexpected model request beyond %d Turns", tt.wantTurns)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestSessionInputQueues 检查用户输入先于 Agent 消息、interrupt 插队、根 Session 中断后暂停 Agent 消息、Agent 消息合批和未读 steer 重投。
func TestSessionInputQueues(t *testing.T) {
	// 1. 准备挂住模型、查询队列和暂停 Agent 消息的测试操作。
	// holdActiveTurn 让活动 Turn 停在模型流式输出中：它被取消后不会再取走给后续 Turn 准备的回复。
	holdActiveTurn := func(t *testing.T, h *sessionHarness) {
		t.Helper()
		h.waitModelStarted(t)
		h.model.replies <- modelReply{hold: true}
		h.waitModelStreaming(t)
	}
	// requireStatus 检查 Session 有没有活动 Turn、排队输入条数和预期一致。
	requireStatus := func(t *testing.T, h *sessionHarness, wantActive bool, wantPending int) {
		t.Helper()
		status, err := h.session.GetStatus(t.Context())
		if err != nil || (status.ActiveTurnID != "") != wantActive || status.PendingInputCount != wantPending {
			t.Fatalf("GetStatus() = %+v, %v; want active = %v, pending = %d", status, err, wantActive, wantPending)
		}
	}
	// pauseAgentMessages 用 InterruptTurn 中断主 Session 的活动 Turn，再提交一条 Agent 消息：它只排队、不开 Turn。
	pauseAgentMessages := func(t *testing.T, h *sessionHarness) *defs.Item {
		t.Helper()
		a := h.submit(t, session.SessionInputModeNormal, "A")
		holdActiveTurn(t, h)
		if turnID, err := h.session.InterruptTurn(t.Context()); err != nil || turnID == "" {
			t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
		}
		requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
		message := h.submitAgentMessage(t, "子任务完成")
		requireStatus(t, h, false, 1)
		if count := h.model.count.Load(); count != 1 {
			t.Fatalf("model requests = %d, want 1", count)
		}
		return message
	}
	// 2. 列出用户优先、插队、根 Session 暂停 Agent 消息、Agent 消息合批和未读 steer 重投的情况。
	tests := []struct {
		name            string
		parentSessionID defs.SessionID
		run             func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "user_before_agent",
			run: func(t *testing.T, h *sessionHarness) {
				a := h.submit(t, session.SessionInputModeNormal, "A")
				h.waitModelStarted(t)
				message := h.submitAgentMessage(t, "子任务完成")
				u := h.submit(t, session.SessionInputModeNormal, "U")
				requireStatus(t, h, true, 2)
				for _, text := range []string{"a", "u", "m"} {
					h.model.replies <- modelReply{text: text}
				}
				for _, want := range [][]defs.ItemID{{a.ID}, {u.ID}, {message.ID}} {
					requireFinished(t, h.nextTurnFinished(t), want, defs.TurnStatusCompleted)
				}
			},
		},
		{
			name: "interrupt_preempts_active_turn",
			run: func(t *testing.T, h *sessionHarness) {
				a := h.submit(t, session.SessionInputModeNormal, "A")
				holdActiveTurn(t, h)
				message := h.submitAgentMessage(t, "子任务完成")
				i := h.submit(t, session.SessionInputModeInterrupt, "I")
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				h.model.replies <- modelReply{text: "i"}
				h.model.replies <- modelReply{text: "m"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{i.ID}, defs.TurnStatusCompleted)
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{message.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "interrupt_when_idle",
			run: func(t *testing.T, h *sessionHarness) {
				i := h.submit(t, session.SessionInputModeInterrupt, "I")
				requireStatus(t, h, true, 0)
				h.model.replies <- modelReply{text: "i"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{i.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "root_interrupt_pauses_agent_messages",
			run: func(t *testing.T, h *sessionHarness) {
				message := pauseAgentMessages(t, h)
				u := h.submit(t, session.SessionInputModeNormal, "U")
				h.model.replies <- modelReply{text: "u"}
				h.model.replies <- modelReply{text: "m"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{u.ID}, defs.TurnStatusCompleted)
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{message.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name:            "child_interrupt_keeps_agent_messages",
			parentSessionID: "parent-session",
			run: func(t *testing.T, h *sessionHarness) {
				a := h.submitAgentMessage(t, "任务")
				holdActiveTurn(t, h)
				if turnID, err := h.session.InterruptTurn(t.Context()); err != nil || turnID == "" {
					t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				message := h.submitAgentMessage(t, "新指示")
				requireStatus(t, h, true, 0)
				h.model.replies <- modelReply{text: "m"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{message.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "duplicate_interrupt_input_id",
			run: func(t *testing.T, h *sessionHarness) {
				first := &session.SessionInput{
					Mode:    session.SessionInputModeInterrupt,
					Item:    sessionInput("I"),
					InputID: "prompt-1",
				}
				if itemID, err := h.session.HandleInput(t.Context(), first); err != nil || itemID != first.Item.ID {
					t.Fatalf("HandleInput() = %q, %v; want %q", itemID, err, first.Item.ID)
				}
				h.waitModelStarted(t)
				retry := &session.SessionInput{
					Mode:    session.SessionInputModeInterrupt,
					Item:    sessionInput("I"),
					InputID: "prompt-1",
				}
				if itemID, err := h.session.HandleInput(t.Context(), retry); err != nil || itemID != first.Item.ID {
					t.Fatalf("HandleInput() retry = %q, %v; want %q", itemID, err, first.Item.ID)
				}
				requireStatus(t, h, true, 0)
				h.model.replies <- modelReply{text: "i"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.Item.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "queued_agent_messages_share_one_turn",
			run: func(t *testing.T, h *sessionHarness) {
				first := pauseAgentMessages(t, h)
				second := h.submitAgentMessage(t, "子任务二完成")
				third := h.submitAgentMessage(t, "子任务三完成")
				requireStatus(t, h, false, 3)
				u := h.submit(t, session.SessionInputModeNormal, "U")
				h.model.replies <- modelReply{text: "u"}
				h.model.replies <- modelReply{text: "m"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{u.ID}, defs.TurnStatusCompleted)
				// 排队的三条 Agent 消息合成一轮：第一条是这一轮的输入，其余两条在第一次请求模型前作为补充输入读入。
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID, second.ID, third.ID}, defs.TurnStatusCompleted)
				requireStatus(t, h, false, 0)
				if count := h.model.count.Load(); count != 3 {
					t.Fatalf("model requests = %d, want 3", count)
				}
			},
		},
		{
			name: "unread_user_steers_resubmitted_together",
			run: func(t *testing.T, h *sessionHarness) {
				a := h.submit(t, session.SessionInputModeNormal, "A")
				holdActiveTurn(t, h)
				steerTests := h.submit(t, session.SessionInputModeSteer, "看下测试")
				steerDocs := h.submit(t, session.SessionInputModeSteer, "顺便改文档")
				if turnID, err := h.session.InterruptTurn(t.Context()); err != nil || turnID == "" {
					t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusInterrupted)
				// 中断时没读到的两条用户 steer 合成一批，开一轮一起处理。
				h.model.replies <- modelReply{text: "s"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{steerTests.ID, steerDocs.ID}, defs.TurnStatusCompleted)
				requireStatus(t, h, false, 0)
				if count := h.model.count.Load(); count != 2 {
					t.Fatalf("model requests = %d, want 2", count)
				}
			},
		},
		{
			name: "compact_while_agent_messages_paused",
			run: func(t *testing.T, h *sessionHarness) {
				pauseAgentMessages(t, h)
				turnID, err := h.session.CompactContext(t.Context())
				if err != nil || turnID == "" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				h.model.replies <- modelReply{text: "摘要"}
				finished := h.nextTurnFinished(t)
				requireFinished(t, finished, nil, defs.TurnStatusCompleted)
				if finished.TurnID != turnID {
					t.Fatalf("finished = %+v, want compact Turn %s", finished, turnID)
				}
			},
		},
	}
	// 3. 创建根或子 Session，执行场景并核对输入顺序和轮数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			h.record.ParentSessionID = tt.parentSessionID
			h.start(t, nil, true)
			tt.run(t, h)
		})
	}
}

// TestSessionTurnEvents 检查一轮的事件以 SessionTurnStarted 开头、SessionTurnFinished 结尾，SessionItemsSaved 和日志一致。
func TestSessionTurnEvents(t *testing.T) {
	// 1. 分别准备成功和失败的模型回复。
	tests := []struct {
		name       string
		reply      modelReply
		wantStatus defs.TurnStatus
	}{
		{
			name:       "completed_turn",
			reply:      modelReply{text: "答复"},
			wantStatus: defs.TurnStatusCompleted,
		},
		{
			name:       "failed_turn",
			reply:      modelReply{status: http.StatusBadRequest},
			wantStatus: defs.TurnStatusFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 执行一轮并收集事件，检查开始、结束和保存事件的 ID、顺序和日志。
			h := newSessionHarness(t)
			h.start(t, nil, true)
			input := h.submit(t, session.SessionInputModeNormal, "你好")
			h.model.replies <- tt.reply

			var events []session.SessionEvent
			for {
				event := h.nextEvent(t)
				if _, ok := event.(*session.SessionPendingInputsUpdated); ok {
					continue
				}
				events = append(events, event)
				if _, ok := event.(*session.SessionTurnFinished); ok {
					break
				}
			}
			started, ok := events[0].(*session.SessionTurnStarted)
			finished := events[len(events)-1].(*session.SessionTurnFinished)
			if !ok || started.TurnID != finished.TurnID || finished.Status != tt.wantStatus {
				t.Fatalf("first event = %T %+v, finished = %+v", events[0], events[0], finished)
			}
			var saved []dao.LoggedItem
			for _, event := range events {
				if itemsSaved, ok := event.(*session.SessionItemsSaved); ok {
					if itemsSaved.TurnID != started.TurnID {
						t.Fatalf("saved TurnID = %s, want %s", itemsSaved.TurnID, started.TurnID)
					}
					saved = append(saved, itemsSaved.Items...)
				}
			}
			logged := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
			if len(saved) == 0 || len(saved) != len(logged) || saved[0].Item.ID != input.ID {
				t.Fatalf("saved = %+v, log = %+v", saved, logged)
			}
			for index, item := range logged {
				if saved[index].Ordinal != storage.Ordinal(index+1) || saved[index].Item.ID != item.ID {
					t.Fatalf("saved[%d] = %d %s, want %d %s", index, saved[index].Ordinal, saved[index].Item.ID, index+1, item.ID)
				}
			}
		})
	}
}

// TestSessionsRunConcurrently 检查两个 Session 能同时请求模型并各自完成。
func TestSessionsRunConcurrently(t *testing.T) {
	// 1. 创建共用一个模型服务的两个 Session，同时提交输入。
	model := newControlledModel(t)
	first := newSessionHarnessWithModel(t, model)
	second := newSessionHarnessWithModel(t, model)
	first.start(t, nil, true)
	second.start(t, nil, true)
	a := first.submit(t, session.SessionInputModeNormal, "one")
	b := second.submit(t, session.SessionInputModeNormal, "two")
	// 2. 等两个请求都开始后再回复，检查各自输入完成。
	first.waitModelStarted(t)
	first.waitModelStarted(t)
	first.model.replies <- modelReply{text: "one"}
	first.model.replies <- modelReply{text: "two"}
	requireFinished(t, first.nextTurnFinished(t), []defs.ItemID{a.ID}, defs.TurnStatusCompleted)
	requireFinished(t, second.nextTurnFinished(t), []defs.ItemID{b.ID}, defs.TurnStatusCompleted)
}

// TestSessionRunLifecycle 检查 Run 的启动限制、Run 之前的输入会等待、无效输入被拒、停止后和空闲时的 WaitSteer。
func TestSessionRunLifecycle(t *testing.T) {
	// 1. 列出 events 为 nil、重复启动、提前输入、无效输入和 WaitSteer 场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "nil_events_rejected",
			run: func(t *testing.T, h *sessionHarness) {
				if err := h.newSession(t, nil).Run(t.Context(), nil); err == nil {
					t.Fatal("Run(nil events) error = nil")
				}
			},
		},
		{
			name: "second_run_rejected",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				// GetStatus 返回了，说明第一次 Run 已进入事件循环。
				if _, err := h.session.GetStatus(t.Context()); err != nil {
					t.Fatal(err)
				}
				events := make(chan session.SessionEvent)
				if err := h.session.Run(t.Context(), events); err == nil {
					t.Fatal("second Run() error = nil")
				}
				if _, open := <-events; open {
					t.Fatal("second Run() left events open")
				}
			},
		},
		{
			name: "input_before_run_waits",
			run: func(t *testing.T, h *sessionHarness) {
				h.session = h.newSession(t, nil)
				accepted := make(chan error, 1)
				item := sessionInput("before run")
				go func() {
					_, err := h.session.HandleInput(t.Context(), &session.SessionInput{
						Mode: session.SessionInputModeNormal,
						Item: item,
					})
					accepted <- err
				}()
				h.model.replies <- modelReply{text: "started"}
				h.run(t, true)
				if err := <-accepted; err != nil {
					t.Fatal(err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{item.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "invalid_input_rejected",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				assigned := sessionInput("assigned")
				assigned.TurnID = defs.NewTurnID()
				withContent := func(content ...*defs.Content) *defs.Item {
					item := sessionInput("content")
					item.Message.Content = content
					return item
				}
				for _, input := range []*session.SessionInput{
					nil,
					{Mode: session.SessionInputModeNormal},
					{
						Mode: session.SessionInputModeNormal,
						Item: assigned,
					},
					{
						Mode: "unknown",
						Item: sessionInput("mode"),
					},
					{
						Mode: session.SessionInputModeNormal,
						Item: withContent(),
					},
					{
						Mode: session.SessionInputModeNormal,
						Item: withContent(nil),
					},
					{
						Mode: session.SessionInputModeNormal,
						Item: withContent(&defs.Content{Kind: defs.ContentText}),
					},
					{
						Mode: session.SessionInputModeNormal,
						Item: withContent(&defs.Content{Kind: defs.ContentImage}),
					},
					{
						Mode: session.SessionInputModeNormal,
						Item: withContent(&defs.Content{Kind: "audio"}),
					},
				} {
					if _, err := h.session.HandleInput(t.Context(), input); err == nil {
						t.Fatalf("HandleInput(%+v) error = nil", input)
					}
				}
				// 被拒的输入没进收件箱，也没开始 Turn。
				status, err := h.session.GetStatus(t.Context())
				if err != nil || status.State != session.SessionStateIdle || status.LastTurnID != "" {
					t.Fatalf("status after rejected inputs = %+v, %v, want idle without a Turn", status, err)
				}
			},
		},
		{
			name: "wait_steer_after_stop",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				h.cancel()
				for range h.received {
				}
				if err := h.waitRunStopped(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() error = %v", err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				items, err := h.session.WaitSteer(ctx)
				if err != nil || items != nil {
					t.Fatalf("WaitSteer() after stop = %+v, %v, want nil, nil", items, err)
				}
			},
		},
		{
			name: "wait_steer_without_active_turn",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				items, err := h.session.WaitSteer(ctx)
				if err != nil || len(items) != 0 {
					t.Fatalf("WaitSteer() = %+v, %v", items, err)
				}
			},
		},
	}
	// 2. 每个场景用独立的 Session 执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newSessionHarness(t))
		})
	}
}

// TestSessionTelemetry 检查 Session、Turn 的日志和 span 属性，以及 Turn span 的父 span 是提交输入时的 span。
func TestSessionTelemetry(t *testing.T) {
	// 1. 准备提交方 span、失败、中断、停止和父 Session 属性场景。
	tests := []struct {
		name string
		// setup 在创建 Session 前调整夹具，返回 Session 使用的 Mutation；返回 nil 使用真实 DAO。
		setup func(h *sessionHarness) dao.Mutation
		run   func(t *testing.T, h *sessionHarness, ctx context.Context, p *telemetrytest.Pipeline)
	}{
		{
			name: "turn_span_child_of_submitter",
			run: func(t *testing.T, h *sessionHarness, ctx context.Context, p *telemetrytest.Pipeline) {
				submitCtx, submitSpan := p.TracerProvider.Tracer("test").Start(ctx, "conversation/submitInput")
				item := sessionInput("你好")
				if _, err := h.session.HandleInput(submitCtx, &session.SessionInput{
					Mode: session.SessionInputModeNormal,
					Item: item,
				}); err != nil {
					t.Fatal(err)
				}
				submitSpan.End()
				h.model.replies <- modelReply{text: "好"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{item.ID}, defs.TurnStatusCompleted)
				turnSpan := p.WaitEndedSpans(t, "invoke_agent test", 1)[0]
				if turnSpan.Parent().SpanID() != submitSpan.SpanContext().SpanID() {
					t.Fatalf("turn parent = %s, want submitter %s", turnSpan.Parent().SpanID(), submitSpan.SpanContext().SpanID())
				}
				if telemetrytest.GetSpanAttribute(turnSpan, "openvaala.session.id") != string(h.record.ID) ||
					telemetrytest.GetSpanAttribute(turnSpan, "openvaala.input.item_id") != string(item.ID) {
					t.Fatalf("turn attributes = %v", turnSpan.Attributes())
				}
				accepted := p.GetLogRecords(t, "input accepted")
				if len(accepted) != 1 || accepted[0]["level"] != "INFO" || accepted[0]["openvaala.input.item_id"] != string(item.ID) || accepted[0]["openvaala.session.id"] != string(h.record.ID) {
					t.Fatalf("input accepted logs = %v", accepted)
				}
			},
		},
		{
			name: "failed_turn_logs_error",
			run: func(t *testing.T, h *sessionHarness, _ context.Context, p *telemetrytest.Pipeline) {
				h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{status: http.StatusBadRequest}
				if finished := h.nextTurnFinished(t); finished.Status != defs.TurnStatusFailed {
					t.Fatalf("status = %q, want failed", finished.Status)
				}
				if span := p.WaitEndedSpans(t, "invoke_agent test", 1)[0]; span.Status().Code != codes.Error {
					t.Fatalf("turn span status = %v", span.Status())
				}
				if record := p.WaitLogRecords(t, "turn failed", 1)[0]; record["level"] != "ERROR" {
					t.Fatalf("turn failed log = %v", record)
				}
			},
		},
		{
			name: "interrupted_turn_is_not_error",
			run: func(t *testing.T, h *sessionHarness, _ context.Context, p *telemetrytest.Pipeline) {
				h.submit(t, session.SessionInputModeNormal, "长任务")
				h.model.replies <- modelReply{hold: true}
				h.waitModelStreaming(t)
				if _, err := h.session.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				if finished := h.nextTurnFinished(t); finished.Status != defs.TurnStatusInterrupted {
					t.Fatalf("status = %q, want interrupted", finished.Status)
				}
				if record := p.WaitLogRecords(t, "turn finished", 1)[0]; record["openvaala.turn.status"] != "interrupted" || record["level"] != "INFO" {
					t.Fatalf("turn finished log = %v", record)
				}
				if len(p.GetLogRecords(t, "turn failed")) != 0 || len(p.GetLogRecords(t, "model call failed")) != 0 {
					t.Fatalf("interrupt produced error logs: %s", p.GetLogText())
				}
			},
		},
		{
			name: "session_lifecycle_logs",
			run: func(t *testing.T, h *sessionHarness, _ context.Context, p *telemetrytest.Pipeline) {
				started := p.WaitLogRecords(t, "session started", 1)[0]
				if started["gen_ai.agent.name"] != "test" || started["openvaala.session.id"] != string(h.record.ID) {
					t.Fatalf("session started log = %v", started)
				}
				if _, found := started["openvaala.parent_session.id"]; found {
					t.Fatalf("session without parent logged parent_session.id: %v", started)
				}
				h.cancel()
				h.waitRunStopped(t)
				if stopped := p.WaitLogRecords(t, "session stopped", 1)[0]; stopped["level"] != "INFO" {
					t.Fatalf("session stopped log = %v", stopped)
				}
			},
		},
		{
			name: "queued_inputs_keep_own_submitter_span",
			run: func(t *testing.T, h *sessionHarness, ctx context.Context, p *telemetrytest.Pipeline) {
				tracer := p.TracerProvider.Tracer("test")
				submitA, spanA := tracer.Start(ctx, "conversation/submitInput")
				itemA := sessionInput("A")
				if _, err := h.session.HandleInput(submitA, &session.SessionInput{
					Mode: session.SessionInputModeNormal,
					Item: itemA,
				}); err != nil {
					t.Fatal(err)
				}
				spanA.End()
				h.model.replies <- modelReply{hold: true}
				h.waitModelStreaming(t)
				// A 还是活动 Turn：B 排队，轮到 B 开 Turn 时要以 B 自己的提交方 span 为父。
				submitB, spanB := tracer.Start(ctx, "conversation/submitInput")
				itemB := sessionInput("B")
				if _, err := h.session.HandleInput(submitB, &session.SessionInput{
					Mode: session.SessionInputModeNormal,
					Item: itemB,
				}); err != nil {
					t.Fatal(err)
				}
				spanB.End()
				if _, err := h.session.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{itemA.ID}, defs.TurnStatusInterrupted)
				h.model.replies <- modelReply{text: "b"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{itemB.ID}, defs.TurnStatusCompleted)

				wantParents := map[string]string{
					string(itemA.ID): spanA.SpanContext().SpanID().String(),
					string(itemB.ID): spanB.SpanContext().SpanID().String(),
				}
				for _, turnSpan := range p.WaitEndedSpans(t, "invoke_agent test", 2) {
					itemID := telemetrytest.GetSpanAttribute(turnSpan, "openvaala.input.item_id")
					want, found := wantParents[itemID]
					if !found || turnSpan.Parent().SpanID().String() != want {
						t.Fatalf("turn for item %q parent = %s, want %q (known items %v)", itemID, turnSpan.Parent().SpanID(), want, wantParents)
					}
					delete(wantParents, itemID)
				}
				if len(wantParents) != 0 {
					t.Fatalf("no turn span for items %v", wantParents)
				}
			},
		},
		{
			name: "unsettled_turn_logs_session_stopped_with_error",
			setup: func(h *sessionHarness) dao.Mutation {
				return &failingTerminalMutation{Mutation: h.mutation}
			},
			run: func(t *testing.T, h *sessionHarness, _ context.Context, p *telemetrytest.Pipeline) {
				h.submit(t, session.SessionInputModeNormal, "A")
				h.model.replies <- modelReply{text: "cannot settle"}
				if finished := h.nextTurnFinished(t); !errors.Is(finished.Err, turn.ErrTurnUnsettled) {
					t.Fatalf("finished = %+v, want unsettled Turn", finished)
				}
				if err := h.waitRunStopped(t); !errors.Is(err, turn.ErrTurnUnsettled) {
					t.Fatalf("Run() error = %v", err)
				}
				stopped := p.WaitLogRecords(t, "session stopped with error", 1)[0]
				if stopped["level"] != "ERROR" || stopped["openvaala.session.id"] != string(h.record.ID) {
					t.Fatalf("session stopped with error log = %v", stopped)
				}
				if len(p.GetLogRecords(t, "session stopped")) != 0 {
					t.Fatalf("abnormal stop also logged a normal stop: %s", p.GetLogText())
				}
			},
		},
		{
			name: "child_session_logs_parent_session_id",
			setup: func(h *sessionHarness) dao.Mutation {
				h.record.ParentSessionID = "parent-session"
				return nil
			},
			run: func(t *testing.T, h *sessionHarness, _ context.Context, p *telemetrytest.Pipeline) {
				started := p.WaitLogRecords(t, "session started", 1)[0]
				if started["openvaala.parent_session.id"] != "parent-session" || started["openvaala.session.id"] != string(h.record.ID) {
					t.Fatalf("session started log = %v", started)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 在测试遥测管道里运行 Session，执行场景检查。
			p := telemetrytest.NewPipeline(t)
			ctx := telemetry.ContextWithTelemetry(t.Context(), p.Telemetry)
			h := newSessionHarness(t)
			var mutation dao.Mutation
			if tt.setup != nil {
				mutation = tt.setup(h)
			}
			h.session = h.newSession(t, mutation)
			h.runWithContext(t, ctx, true)
			tt.run(t, h, ctx, p)
		})
	}
}

// TestSessionModelSelection 检查 Turn 使用的模型和强度、所选模型不在目录里时拒收输入和压缩、运行中换模型从下一轮生效。
func TestSessionModelSelection(t *testing.T) {
	// 1. 准备默认模型、指定模型、模型不在目录里、强度回退和运行中切换场景。
	removedRef := defs.ModelRef{
		ProviderID: "test",
		ModelID:    "gone",
	}
	tests := []struct {
		name                string
		selection           defs.ModelSelection // 写进 Session 记录的选择
		withoutAgentDefault bool                // 是否从模型目录里去掉 Agent 的默认模型？
		run                 func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "zero_selection_uses_agent_default",
			run: func(t *testing.T, h *sessionHarness) {
				input := h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{text: "好"}
				started := h.nextTurnStarted(t)
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{input.ID}, defs.TurnStatusCompleted)
				if body := h.nextModelBody(t); body["model"] != "session-test" || body["reasoning_effort"] != nil {
					t.Fatalf("request = %v", body)
				}
				want := defs.ModelSelection{Ref: testDefaultModelRef}
				if started.Model != want {
					t.Fatalf("started model = %+v, want %+v", started.Model, want)
				}
				h.requireTurnModel(t, started.TurnID, want)
			},
		},
		{
			name: "selected_model_and_effort",
			selection: defs.ModelSelection{
				Ref:             testOtherModelRef,
				ReasoningEffort: "high",
			},
			run: func(t *testing.T, h *sessionHarness) {
				h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{text: "好"}
				started := h.nextTurnStarted(t)
				h.nextTurnFinished(t)
				if body := h.nextModelBody(t); body["model"] != "other-upstream" || body["reasoning_effort"] != "high" {
					t.Fatalf("request = %v", body)
				}
				h.requireTurnModel(t, started.TurnID, defs.ModelSelection{
					Ref:             testOtherModelRef,
					ReasoningEffort: "high",
				})
			},
		},
		{
			name: "unsupported_effort_uses_model_default",
			selection: defs.ModelSelection{
				Ref:             testOtherModelRef,
				ReasoningEffort: "medium",
			},
			run: func(t *testing.T, h *sessionHarness) {
				h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{text: "好"}
				started := h.nextTurnStarted(t)
				h.nextTurnFinished(t)
				if body := h.nextModelBody(t); body["model"] != "other-upstream" || body["reasoning_effort"] != "low" {
					t.Fatalf("request = %v", body)
				}
				h.requireTurnModel(t, started.TurnID, defs.ModelSelection{
					Ref:             testOtherModelRef,
					ReasoningEffort: "low",
				})
			},
		},
		{
			name: "removed_model_rejects_input",
			selection: defs.ModelSelection{
				Ref:             removedRef,
				ReasoningEffort: "high",
			},
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeNormal, "model test/gone is not configured; choose another model")
				h.requireNoTurn(t)
			},
		},
		{
			name:      "removed_model_rejects_steer",
			selection: defs.ModelSelection{Ref: removedRef},
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeSteer, "model test/gone is not configured; choose another model")
				h.requireNoTurn(t)
			},
		},
		{
			name:      "removed_model_rejects_idle_only_input",
			selection: defs.ModelSelection{Ref: removedRef},
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeIdleOnly, "model test/gone is not configured; choose another model")
				h.requireNoTurn(t)
			},
		},
		{
			name:      "removed_model_rejects_compaction",
			selection: defs.ModelSelection{Ref: removedRef},
			run: func(t *testing.T, h *sessionHarness) {
				if turnID, err := h.session.CompactContext(t.Context()); turnID != "" || err == nil || err.Error() != "model test/gone is not configured; choose another model" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				h.requireNoTurn(t)
			},
		},
		{
			name:      "removed_model_then_select_configured_model",
			selection: defs.ModelSelection{Ref: removedRef},
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeNormal, "model test/gone is not configured; choose another model")
				if err := h.session.SelectModel(t.Context(), defs.ModelSelection{Ref: testOtherModelRef}); err != nil {
					t.Fatal(err)
				}
				h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{text: "好"}
				started := h.nextTurnStarted(t)
				h.nextTurnFinished(t)
				if body := h.nextModelBody(t); body["model"] != "other-upstream" {
					t.Fatalf("request = %v", body)
				}
				h.requireTurnModel(t, started.TurnID, defs.ModelSelection{
					Ref:             testOtherModelRef,
					ReasoningEffort: "low",
				})
			},
		},
		{
			name:                "zero_selection_without_agent_default_rejects_input",
			withoutAgentDefault: true,
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeNormal, "no model is configured; choose a model")
				h.requireNoTurn(t)
			},
		},
		{
			name:                "zero_selection_without_agent_default_rejects_compaction",
			withoutAgentDefault: true,
			run: func(t *testing.T, h *sessionHarness) {
				if turnID, err := h.session.CompactContext(t.Context()); turnID != "" || err == nil || err.Error() != "no model is configured; choose a model" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				h.requireNoTurn(t)
			},
		},
		{
			name:                "zero_selection_without_agent_default_then_select_configured_model",
			withoutAgentDefault: true,
			run: func(t *testing.T, h *sessionHarness) {
				h.requireInputRejected(t, session.SessionInputModeNormal, "no model is configured; choose a model")
				if err := h.session.SelectModel(t.Context(), defs.ModelSelection{Ref: testOtherModelRef}); err != nil {
					t.Fatal(err)
				}
				h.submit(t, session.SessionInputModeNormal, "你好")
				h.model.replies <- modelReply{text: "好"}
				h.nextTurnFinished(t)
				if body := h.nextModelBody(t); body["model"] != "other-upstream" {
					t.Fatalf("request = %v", body)
				}
			},
		},
		{
			name: "select_during_active_turn",
			run: func(t *testing.T, h *sessionHarness) {
				h.submit(t, session.SessionInputModeNormal, "第一轮")
				h.waitModelStarted(t)
				if err := h.session.SelectModel(t.Context(), defs.ModelSelection{Ref: testOtherModelRef}); err != nil {
					t.Fatal(err)
				}
				h.submit(t, session.SessionInputModeNormal, "第二轮")
				h.model.replies <- modelReply{text: "一"}
				h.model.replies <- modelReply{text: "二"}
				first, second := h.nextModelBody(t), h.nextModelBody(t)
				if first["model"] != "session-test" || second["model"] != "other-upstream" || second["reasoning_effort"] != "low" {
					t.Fatalf("first = %v, second = %v", first["model"], second)
				}
			},
		},
		{
			name: "select_after_stop_returns_nil",
			run: func(t *testing.T, h *sessionHarness) {
				h.cancel()
				_ = h.waitRunStopped(t)
				if err := h.session.SelectModel(t.Context(), defs.ModelSelection{Ref: testOtherModelRef}); err != nil {
					t.Fatalf("SelectModel after stop = %v, want nil", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 按场景配置模型目录和选择，再执行场景检查。
			h := newSessionHarness(t)
			h.record.Model = tt.selection
			if tt.withoutAgentDefault {
				delete(h.models, testDefaultModelRef)
			}
			h.start(t, nil, true)
			tt.run(t, h)
		})
	}
}

// TestSessionModelChangeAfterRestart 检查重启后换了模型时，第二轮日志里有 ModelChange，没换时没有。
func TestSessionModelChangeAfterRestart(t *testing.T) {
	// 1. 分别准备重启后沿用模型和换模型。
	tests := []struct {
		name       string
		second     defs.ModelSelection
		wantChange bool
	}{
		{name: "restart_with_same_model"},
		{
			name:       "restart_with_new_model",
			second:     defs.ModelSelection{Ref: testOtherModelRef},
			wantChange: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 完成首轮并停止，重建 Session 执行第二轮。
			h := newSessionHarness(t)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "第一轮")
			h.model.replies <- modelReply{text: "一"}
			h.nextTurnFinished(t)
			h.cancel()
			// 读完本次事件流，等收集任务结束后再重建 Session。
			for range h.received {
			}
			_ = h.waitRunStopped(t)

			h.record.Model = tt.second
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "第二轮")
			h.model.replies <- modelReply{text: "二"}
			h.nextTurnFinished(t)

			// 3. 检查日志只在模型变了时保存 ModelChange。
			page, err := h.query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: h.record.ConversationID,
				SessionID:      h.record.ID,
				Order:          storage.DirectionAsc,
				Limit:          20,
			})
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(page.Items, func(logged dao.LoggedItem) bool { return logged.Item.Kind == defs.ItemModelChange })
			if (index >= 0) != tt.wantChange {
				t.Fatalf("model change present = %v, want %v", index >= 0, tt.wantChange)
			}
			if tt.wantChange && *page.Items[index].Item.ModelChange != (defs.ModelChange{
				PreviousModel: testDefaultModelRef,
				Model:         testOtherModelRef,
			}) {
				t.Fatalf("model change = %+v", page.Items[index].Item.ModelChange)
			}
		})
	}
}

// TestSessionLastTurnExecutors 检查重启后 Executor 列表变了时，第二轮日志里有 ExecutorsChange，没变时没有。
func TestSessionLastTurnExecutors(t *testing.T) {
	// 1. 准备前后相同或变化的 Executor 列表。
	first := []defs.TurnExecutor{{
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}, {
		ID:                 "openvaala",
		WorkspaceDirectory: "/workspace",
	}}
	tests := []struct {
		name       string
		second     []defs.TurnExecutor
		wantChange bool
	}{
		{
			name:   "restart_with_same_executors",
			second: first,
		},
		{
			name: "restart_with_new_executors",
			second: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			wantChange: true,
		},
	}
	// 2. 按 Executor 列表创建虚拟工作区和执行环境。
	newEnvironments := func(t *testing.T, list []defs.TurnExecutor) []executor.ExecutionEnvironment {
		var environments []executor.ExecutionEnvironment
		for _, each := range list {
			work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt(each.WorkspaceDirectory, nil))
			if err != nil {
				t.Fatal(err)
			}
			environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(each.ID, "", work))
			if err != nil {
				t.Fatal(err)
			}
			environments = append(environments, environment)
		}
		return environments
	}
	// 3. 跑完第一轮后停止，按新环境重建 Session 跑第二轮，检查保存的 Executor 变化。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			h.environments = newEnvironments(t, first)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "第一轮")
			h.model.replies <- modelReply{text: "一"}
			h.nextTurnFinished(t)
			h.cancel()
			// 读完本次事件流，等收集任务结束后再重建 Session。
			for range h.received {
			}
			_ = h.waitRunStopped(t)

			h.environments = newEnvironments(t, tt.second)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "第二轮")
			h.model.replies <- modelReply{text: "二"}
			h.nextTurnFinished(t)

			page, err := h.query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: h.record.ConversationID,
				SessionID:      h.record.ID,
				Order:          storage.DirectionAsc,
				Limit:          20,
			})
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(page.Items, func(logged dao.LoggedItem) bool { return logged.Item.Kind == defs.ItemExecutorsChange })
			if (index >= 0) != tt.wantChange {
				t.Fatalf("executors change present = %v, want %v", index >= 0, tt.wantChange)
			}
			if !tt.wantChange {
				return
			}
			change := page.Items[index].Item.ExecutorsChange
			if !slices.Equal(change.PreviousExecutors, first) || !slices.Equal(change.Executors, tt.second) {
				t.Fatalf("executors change = %+v, want %+v -> %+v", change, first, tt.second)
			}
		})
	}
}

// TestSessionCommands 检查后台命令跨 Turn 保留、中断 Turn 不停命令、父子 Session 互相看不到命令、Session 停止时结束命令进程。
func TestSessionCommands(t *testing.T) {
	// 1. 准备跨轮命令、中断 Turn、父子隔离和 Session 停止场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "command_survives_turns",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				started := h.runToolTurn(t, "call-bash", "bash", map[string]any{
					"command":      "sleep 30; echo late",
					"wait_seconds": 0,
				})
				if started.ToolError != nil || started.Status != "running" || started.CommandID == "" {
					t.Fatalf("bash result = %+v", started)
				}
				read := h.runToolTurn(t, "call-read", "read_command_output", map[string]any{
					"command_id":   started.CommandID,
					"wait_seconds": 0,
				})
				if read.ToolError != nil || read.Status != "running" {
					t.Fatalf("read_command_output result = %+v", read)
				}
				stopped := h.runToolTurn(t, "call-stop", "stop_command", map[string]any{"command_id": started.CommandID})
				if stopped.ToolError != nil || stopped.StopReason != "stopped" {
					t.Fatalf("stop_command result = %+v", stopped)
				}
			},
		},
		{
			name: "interrupt_keeps_command",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				input := h.submit(t, session.SessionInputModeNormal, "start")
				h.model.replies <- toolCallReply(t, "call-bash", "bash", map[string]any{
					"command":      "printf started; sleep 30",
					"wait_seconds": 30,
				})
				h.nextToolProgress(t)
				if turnID, err := h.session.InterruptTurn(t.Context()); err != nil || turnID == "" {
					t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{input.ID}, defs.TurnStatusInterrupted)
				// bash 在等待时被中断，照样保存 running 结果和命令编号。
				interrupted := h.getSavedCommandResult(t, "call-bash")
				if interrupted.ToolError != nil || interrupted.Status != "running" || interrupted.CommandID == "" {
					t.Fatalf("interrupted bash result = %+v", interrupted)
				}
				listed := h.runToolTurn(t, "call-list", "list_commands", map[string]any{})
				if listed.ToolError != nil || len(listed.Commands) != 1 || listed.Commands[0].CommandID != interrupted.CommandID ||
					listed.Commands[0].Status != "running" {
					t.Fatalf("list_commands result = %+v", listed)
				}
			},
		},
		{
			name: "parent_child_isolation",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				started := h.runToolTurn(t, "call-bash", "bash", map[string]any{
					"command":      "sleep 30",
					"wait_seconds": 0,
				})
				child := newSessionHarnessWithModel(t, h.model)
				child.environments = h.environments
				child.record.ParentSessionID = h.record.ID
				if err := child.mutation.SetSession(t.Context(), child.record); err != nil {
					t.Fatal(err)
				}
				child.start(t, nil, true)
				read := child.runToolTurn(t, "call-read", "read_command_output", map[string]any{
					"command_id":   started.CommandID,
					"wait_seconds": 0,
				})
				if read.ToolError == nil || read.ToolError.Code != "command_not_found" {
					t.Fatalf("child read_command_output result = %+v", read)
				}
			},
		},
		{
			name: "session_stop_stops_commands",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				started := h.runToolTurn(t, "call-bash", "bash", map[string]any{
					"command":      "echo $$; exec sleep 30",
					"wait_seconds": 1,
				})
				if started.Status != "running" {
					t.Fatalf("bash result = %+v", started)
				}
				h.cancel()
				for range h.received {
				}
				if err := h.waitRunStopped(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() error = %v", err)
				}
				requireProcessExited(t, started.Output)
			},
		},
		{
			name: "session_stop_during_first_wait",
			run: func(t *testing.T, h *sessionHarness) {
				h.start(t, nil, true)
				h.submit(t, session.SessionInputModeNormal, "start")
				h.model.replies <- toolCallReply(t, "call-bash", "bash", map[string]any{
					"command":      "echo $$; exec sleep 30",
					"wait_seconds": 30,
				})
				output := h.nextToolProgress(t)
				stopRequested := time.Now()
				h.cancel()
				for range h.received {
				}
				_ = h.waitRunStopped(t)
				if elapsed := time.Since(stopRequested); elapsed > 5*time.Second {
					t.Fatalf("Session.Run returned %v after stop, want within 5s", elapsed)
				}
				requireProcessExited(t, output)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 用本机执行环境执行场景。
			h := newSessionHarness(t)
			h.environments = newLocalSessionEnvironments(t)
			tt.run(t, h)
		})
	}
}

// TestNewSessionValidation 检查 NewSession 在缺少必填配置时返回错误。
func TestNewSessionValidation(t *testing.T) {
	// 1. 准备完整配置，再分别去掉 Session、DAO、模型客户端和 Agent。
	query, mutation := openSessionDAO(t)
	record := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: "test-conversation",
		AgentName:      "test",
	}
	client := llmkit.NewClient(llmkit.ClientOptions{})
	models := newSessionTestModels("http://127.0.0.1")
	definition := &agent.AgentDefinition{
		Name:  "test",
		Model: testDefaultModelRef,
	}
	withModels := func(opt *session.SessionOpt) *session.SessionOpt {
		opt.Models = models
		return opt
	}
	tests := []struct {
		name string
		opt  *session.SessionOpt
	}{
		{name: "nil"},
		{
			name: "empty_session",
			opt:  withModels(session.NewSessionOpt(defs.Session{}, query, mutation, client, definition)),
		},
		{
			name: "nil_query",
			opt:  withModels(session.NewSessionOpt(record, nil, mutation, client, definition)),
		},
		{
			name: "nil_mutation",
			opt:  withModels(session.NewSessionOpt(record, query, nil, client, definition)),
		},
		{
			name: "nil_client",
			opt:  withModels(session.NewSessionOpt(record, query, mutation, nil, definition)),
		},
		{
			name: "nil_agent",
			opt:  withModels(session.NewSessionOpt(record, query, mutation, client, nil)),
		},
	}
	// 2. 检查每种缺失都返回创建错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := session.NewSession(tt.opt); err == nil {
				t.Fatal("NewSession() error = nil")
			}
		})
	}
}

// TestSessionCompactionWithResponses 检查 Responses 协议下，窗口有限时先生成摘要再回答，窗口为 0 时不压缩。
func TestSessionCompactionWithResponses(t *testing.T) {
	// 1. 分别配置有限窗口和窗口为 0。
	for _, tt := range []struct {
		name                  string
		window, wantSummaries int
	}{
		{
			"configured",
			1000,
			1,
		}, {
			"disabled",
			0,
			0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 写入较长的旧历史，用 Responses 模型执行新输入。
			model := &summarySessionModel{requests: make(chan map[string]any, 4)}
			server := httptest.NewServer(model)
			defer server.Close()
			h := newSessionHarness(t)
			source := sessionInput(strings.Repeat("old history ", 400))
			source.TurnID = "old-turn"
			if _, err := h.mutation.AppendItems(t.Context(), h.record.ConversationID, h.record.ID, source); err != nil {
				t.Fatal(err)
			}
			sessionModel := newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIResponses,
				BaseURL:  server.URL,
				Model:    "session-test",
			}, tt.window)
			opt := session.NewSessionOpt(h.record, h.query, h.mutation, llmkit.NewClient(llmkit.ClientOptions{}), &agent.AgentDefinition{
				Name:         "test",
				Instructions: "system",
				Model:        sessionModel.Ref,
			})
			opt.Models = map[defs.ModelRef]*defs.ModelDefinition{sessionModel.Ref: sessionModel}
			instance, err := session.NewSession(opt)
			if err != nil {
				t.Fatal(err)
			}
			h.session = instance
			h.run(t, true)
			input := h.submit(t, session.SessionInputModeNormal, strings.Repeat("recent ", 220))
			finished := h.nextTurnFinished(t)
			requireFinished(t, finished, []defs.ItemID{input.ID}, defs.TurnStatusCompleted)
			if len(finished.Outputs) != 1+tt.wantSummaries {
				t.Fatalf("outputs = %+v", finished.Outputs)
			}
			// 3. 核对摘要请求、缓存键、最终上下文和保存的摘要覆盖到的 Item。
			var final map[string]any
			for index := 0; index < 1+tt.wantSummaries; index++ {
				req := <-model.requests
				if req["prompt_cache_key"] != string(h.record.ID) {
					t.Fatalf("cache key = %v", req["prompt_cache_key"])
				}
				if tt.wantSummaries == 1 && index == 0 && req["tool_choice"] != "none" {
					t.Fatal("summary request allowed tools")
				}
				final = req
			}
			payload, _ := json.Marshal(final["input"])
			if tt.wantSummaries > 0 && (strings.Contains(string(payload), "old history") || !strings.Contains(string(payload), "saved summary") || !strings.Contains(string(payload), "recent")) {
				t.Fatalf("final model input = %s", payload)
			}
			items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
			if len(items) != 3+tt.wantSummaries {
				t.Fatalf("saved items = %+v", items)
			}
			if tt.wantSummaries > 0 && (items[2].Kind != defs.ItemContextSummary || items[2].ContextSummary.CoveredThroughItemID != source.ID) {
				t.Fatal("summary identity changed in log")
			}
		})
	}
}

// TestSessionContextUsageRecovery 检查重开存储后的首次模型请求先恢复报告，再决定是否自动压缩。
func TestSessionContextUsageRecovery(t *testing.T) {
	// 1. 覆盖有效报告、最近报告缺失、最新响应无用量和明确模型切换。
	for _, tt := range []struct {
		name                                                string
		clearReport, inheritReport, clearUsage, changeModel bool
		wantUsageBased                                      bool
	}{
		{name: "reopened_before_new_usage", wantUsageBased: true},
		{name: "latest_turn_inherited_report", inheritReport: true, wantUsageBased: true},
		{name: "latest_turn_without_report_does_not_scan_older", clearReport: true},
		{name: "latest_without_usage_does_not_scan_older", clearUsage: true},
		{name: "changed_model_uses_estimate", changeModel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 用真实 Session 保存较长历史和低实际用量，随后停止 Session 并关闭数据库。
			root := t.TempDir()
			open := func() (storagesqlite.Database, dao.Query, dao.Mutation) {
				database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = database.Close() })
				logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
				if err != nil {
					t.Fatal(err)
				}
				query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
				if err != nil {
					t.Fatal(err)
				}
				return database, query, mutation
			}
			database, query, mutation := open()
			model := newControlledModel(t)
			h := &sessionHarness{
				model:    model,
				models:   newSessionTestModels(model.server.URL),
				query:    query,
				mutation: mutation,
				record:   defs.Session{ID: defs.NewSessionID(), ConversationID: "test-conversation", AgentName: "test"},
			}
			if err := mutation.SetSession(t.Context(), h.record); err != nil {
				t.Fatal(err)
			}
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, strings.Repeat("long prior history ", 1500))
			h.waitModelStarted(t)
			h.nextModelBody(t)
			h.model.replies <- modelReply{chunks: []string{
				`{"choices":[{"delta":{"content":"prior reply"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
			}}
			first := h.nextTurnFinished(t)
			h.cancel()
			_ = h.waitRunStopped(t)
			for range h.received {
			}
			previous, found, err := query.GetTurn(t.Context(), h.record.ID, first.TurnID)
			if err != nil || !found || previous.ContextUsage == nil || previous.ContextUsage.Usage == (defs.TokenUsage{}) {
				t.Fatalf("first saved Turn = %+v, found %v, err %v", previous, found, err)
			}
			if tt.clearReport || tt.inheritReport || tt.clearUsage {
				latest := defs.Turn{
					ID: defs.NewTurnID(), ConversationID: h.record.ConversationID, SessionID: h.record.ID,
					Status: defs.TurnStatusCompleted, Model: previous.Model,
				}
				input := sessionInput("intermediate input")
				input.TurnID = latest.ID
				saved, err := mutation.AppendItems(t.Context(), h.record.ConversationID, h.record.ID, input)
				if err != nil {
					t.Fatal(err)
				}
				latest.ItemsRange = defs.TurnItemRange{FirstOrdinal: uint64(saved[0].Ordinal), LastOrdinal: uint64(saved[0].Ordinal)}
				if tt.clearReport || tt.inheritReport {
					latest.Status = defs.TurnStatusFailed
					latest.Error = "no complete response"
					if tt.inheritReport {
						latest.ContextUsage = previous.ContextUsage
					}
				} else {
					latest.FinalModelCallID = defs.NewModelCallID()
					latest.ContextUsage = &defs.TurnContextUsage{ModelCallID: latest.FinalModelCallID, InputThroughItemID: input.ID}
				}
				if err := mutation.SetTurn(t.Context(), latest); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			query, mutation = nil, nil
			h.session = nil
			// 3. 重开相同临时存储，启用小窗口，在下一次响应阻塞期间检查首次压缩选择和测量。
			database, query, mutation = open()
			h.query, h.mutation = query, mutation
			models := newSessionTestModels(h.model.server.URL)
			for _, model := range models {
				model.ContextWindow = 1000
			}
			h.models = models
			if tt.changeModel {
				h.record.Model.Ref = testOtherModelRef
			}
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, strings.Repeat("recent ", 220))
			h.waitModelStarted(t)
			body := h.nextModelBody(t)
			isSummary := body["tool_choice"] == "none"
			if isSummary == tt.wantUsageBased {
				t.Fatalf("first request summary = %v, want restored usage %v", isSummary, tt.wantUsageBased)
			}
			if isSummary {
				h.model.replies <- modelReply{text: "short summary"}
				h.waitModelStarted(t)
				h.nextModelBody(t)
			}
			var measurement *agent_context.ContextMeasurement
			for measurement == nil {
				if event, ok := h.nextEvent(t).(*session.SessionAgentEvent); ok && event.Event.Type == agent.AgentEventContextMeasured {
					measurement = event.Event.Measurement
				}
			}
			if measurement.IsUsageBased != tt.wantUsageBased {
				t.Fatalf("first measurement = %+v", measurement)
			}
			if tt.wantUsageBased && (measurement.EstimatedTokens <= 800 || measurement.RequestTokens >= 800) {
				t.Fatalf("fixture did not distinguish compression decision: %+v", measurement)
			}
			// 4. 新调用尚未返回时，恢复没有重复累计计费。
			usage, err := dao.GetSessionUsage(t.Context(), query, h.record.ID)
			if err != nil || usage.Input != 100 {
				t.Fatalf("billed usage = %+v, %v", usage, err)
			}
			h.model.replies <- modelReply{text: "next reply"}
			finished := h.nextTurnFinished(t)
			if finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("finished = %+v", finished)
			}
			h.cancel()
			_ = h.waitRunStopped(t)
		})
	}
}

// TestSessionQueuedContextUsage 检查排队的下一轮在新响应之前已经使用上一轮报告。
func TestSessionQueuedContextUsage(t *testing.T) {
	// 1. 分别用正常完整响应和无用量完整响应结束第一轮。
	for _, tt := range []struct {
		name  string
		usage bool
	}{{"with_usage", true}, {"without_usage", false}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "first")
			h.waitModelStarted(t)
			h.submit(t, session.SessionInputModeNormal, "second queued")
			reply := modelReply{text: "first reply"}
			if tt.usage {
				reply.chunks = []string{
					`{"choices":[{"delta":{"content":"first reply"}}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
				}
			}
			h.model.replies <- reply
			first := h.nextTurnFinished(t)
			// 2. 第二轮请求保持阻塞，读取其首次测量，核对保存的本轮报告与累计消耗。
			h.waitModelStarted(t)
			for {
				event, ok := h.nextEvent(t).(*session.SessionAgentEvent)
				if !ok || event.TurnID == first.TurnID || event.Event.Type != agent.AgentEventContextMeasured {
					continue
				}
				if event.Event.Measurement.IsUsageBased != tt.usage {
					t.Fatalf("queued measurement = %+v", event.Event.Measurement)
				}
				break
			}
			record, found, err := h.query.GetTurn(t.Context(), h.record.ID, first.TurnID)
			if err != nil || !found || record.ContextUsage == nil || (record.ContextUsage.Usage != defs.TokenUsage{}) != tt.usage {
				t.Fatalf("saved Turn = %+v, %v", record, err)
			}
			h.model.replies <- modelReply{text: "second reply"}
			second := h.nextTurnFinished(t)
			if second.Status != defs.TurnStatusCompleted {
				t.Fatalf("second = %+v", second)
			}
		})
	}
}

// TestSessionContextUsageAcrossFailedTurn 检查第一次完整响应前失败的 Turn 沿用上一轮的报告，下一轮仍然使用它。
func TestSessionContextUsageAcrossFailedTurn(t *testing.T) {
	// 1. 分别用带用量和无用量的完整响应结束第一轮。
	for _, tt := range []struct {
		name  string
		usage bool
	}{{"with_usage", true}, {"without_usage", false}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "first")
			reply := modelReply{text: "first reply"}
			if tt.usage {
				reply.chunks = []string{
					`{"choices":[{"delta":{"content":"first reply"}}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
				}
			}
			h.model.replies <- reply
			first := h.nextTurnFinished(t)
			// 2. 第二轮在第一次完整响应前失败，落盘的报告就是第一轮的报告。
			h.submit(t, session.SessionInputModeNormal, "second")
			h.model.replies <- modelReply{status: http.StatusBadRequest}
			second := h.nextTurnFinished(t)
			if second.Status != defs.TurnStatusFailed {
				t.Fatalf("second = %+v, want failed", second)
			}
			firstRecord, found, err := h.query.GetTurn(t.Context(), h.record.ID, first.TurnID)
			if err != nil || !found || firstRecord.ContextUsage == nil {
				t.Fatalf("first saved Turn = %+v, %v, %v", firstRecord, found, err)
			}
			secondRecord, found, err := h.query.GetTurn(t.Context(), h.record.ID, second.TurnID)
			if err != nil || !found || !reflect.DeepEqual(secondRecord.ContextUsage, firstRecord.ContextUsage) {
				t.Fatalf("second saved Turn = %+v, %v, %v; want the first report %+v", secondRecord, found, err, firstRecord.ContextUsage)
			}
			// 3. 第三轮第一次测量使用同一份报告。
			h.submit(t, session.SessionInputModeNormal, "third")
			for {
				event, ok := h.nextEvent(t).(*session.SessionAgentEvent)
				if !ok || event.TurnID == first.TurnID || event.TurnID == second.TurnID || event.Event.Type != agent.AgentEventContextMeasured {
					continue
				}
				if event.Event.Measurement.IsUsageBased != tt.usage {
					t.Fatalf("third measurement = %+v", event.Event.Measurement)
				}
				break
			}
			h.model.replies <- modelReply{text: "third reply"}
			if third := h.nextTurnFinished(t); third.Status != defs.TurnStatusCompleted {
				t.Fatalf("third = %+v", third)
			}
		})
	}
}

// sessionHarness 持有一个用真实 DAO 和受控模型服务的 Session，测试结束时停止它并读完事件流。
type sessionHarness struct {
	// skills 是下一次创建的 Session 可用的 Skill。
	skills []*skill.Skill
	// environments 是下一次创建的 Session 可用的执行环境。
	environments []executor.ExecutionEnvironment
	model        *controlledModel
	// models 是下一次创建的 Session 用的模型目录。
	models   map[defs.ModelRef]*defs.ModelDefinition
	query    dao.Query
	mutation dao.Mutation
	// record 是已保存的 Session 记录，下一次创建 Session 时用它。
	record defs.Session

	// hooks 是下一次创建的 Session 的 hook，nil 表示没有 hook。
	hooks hook.Hooks
	// hookStartSource 是下一次创建的 Session 第一个非压缩 Turn 触发 SessionStart 的来源。
	hookStartSource string

	session session.Session
	// events 是交给 Session.Run 的事件通道。
	events chan session.SessionEvent
	// received 是从 events 转发过来、供测试读取的事件；不收集时一开始就关闭。
	received chan session.SessionEvent
	cancel   context.CancelFunc
	// runStopped 在 Run 返回后关闭。
	runStopped chan struct{}
	// runErr 是 Run 的返回值，runStopped 关闭后才能读。
	runErr error
}

// newSessionHarness 创建带独立受控模型服务的测试夹具。
func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	// 1. 启动独立的模型服务，组装测试夹具。
	return newSessionHarnessWithModel(t, newControlledModel(t))
}

// newSessionHarnessWithModel 用指定的模型服务创建测试存储，保存 Session 记录。
func newSessionHarnessWithModel(t *testing.T, model *controlledModel) *sessionHarness {
	t.Helper()
	// 1. 创建独立存储，保存 Session 记录，返回用指定模型服务的测试夹具。
	query, mutation := openSessionDAO(t)
	record := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: "test-conversation",
		AgentName:      "test",
	}
	if err := mutation.SetSession(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	return &sessionHarness{
		model:    model,
		models:   newSessionTestModels(model.server.URL),
		query:    query,
		mutation: mutation,
		record:   record,
	}
}

// start 创建测试 Session 并运行它。
func (h *sessionHarness) start(t *testing.T, mutation dao.Mutation, collect bool) {
	t.Helper()
	// 1. 创建实例并启动，collect 为 true 时在后台转发事件。
	h.session = h.newSession(t, mutation)
	h.run(t, collect)
}

// newSession 按夹具当前的配置创建 Session 实例。
func (h *sessionHarness) newSession(t *testing.T, mutation dao.Mutation) session.Session {
	t.Helper()
	// 1. mutation 为 nil 时用真实 DAO，填入模型目录、Skill、执行环境和 hook 后创建 Session。
	if mutation == nil {
		mutation = h.mutation
	}
	opt := session.NewSessionOpt(
		h.record,
		h.query,
		mutation,
		llmkit.NewClient(llmkit.ClientOptions{}),
		&agent.AgentDefinition{
			Name:         "test",
			Instructions: "Answer briefly.",
			Model:        testDefaultModelRef,
		},
	)
	opt.Models = h.models
	opt.Skills = h.skills
	opt.Environments = h.environments
	opt.Hooks = h.hooks
	opt.HookStartSource = h.hookStartSource
	instance, err := session.NewSession(opt)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

// testDefaultModelRef、testOtherModelRef 是 Session 测试模型目录里的两个模型：默认模型不能选强度；other 支持 low、high，默认 low。
var (
	testDefaultModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "session-test",
	}
	testOtherModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "other",
	}
)

// newSessionTestModels 创建默认模型和能选强度的 other 模型组成的目录。
func newSessionTestModels(serverURL string) map[defs.ModelRef]*defs.ModelDefinition {
	// 1. 两个模型都连到 serverURL，other 的上游模型名是 other-upstream。
	return map[defs.ModelRef]*defs.ModelDefinition{
		testDefaultModelRef: {
			Ref: testDefaultModelRef,
			Target: &llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  serverURL,
				Model:    "session-test",
			},
		},
		testOtherModelRef: {
			Ref: testOtherModelRef,
			Target: &llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  serverURL,
				Model:    "other-upstream",
			},
			ReasoningEfforts:       []string{"low", "high"},
			DefaultReasoningEffort: "low",
		},
	}
}

// newTestModel 创建带连接参数和上下文窗口的测试模型。
func newTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 用 test/<上游模型名> 作为引用，填入连接和窗口；窗口为 0 时不压缩。
	return &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    target.Model,
		},
		Target:        target,
		ContextWindow: window,
	}
}

// run 用测试 ctx 运行 Session。
func (h *sessionHarness) run(t *testing.T, collect bool) {
	t.Helper()
	// 1. 用测试 ctx 启动，collect 原样传下去。
	h.runWithContext(t, t.Context(), collect)
}

// runWithContext 在后台运行 Session，按 collect 转发事件，登记测试结束时停止它。
func (h *sessionHarness) runWithContext(t *testing.T, parent context.Context, collect bool) {
	t.Helper()
	// 1. 派生运行 ctx，创建事件和停止通知通道，在后台启动 Session.Run。
	ctx, cancel := context.WithCancel(parent)
	h.cancel = cancel
	h.events = make(chan session.SessionEvent)
	h.received = make(chan session.SessionEvent, 1024)
	h.runStopped = make(chan struct{})
	go func() {
		h.runErr = h.session.Run(ctx, h.events)
		close(h.runStopped)
	}()
	// 2. collect 为 true 时把事件转发到带缓冲的 received，否则直接关闭 received。
	if collect {
		go func() {
			for event := range h.events {
				h.received <- event
			}
			close(h.received)
		}()
	} else {
		close(h.received)
	}
	// 3. 测试结束时取消 ctx、读完事件并等 Run 返回。
	t.Cleanup(func() {
		cancel()
		if !collect {
			for range h.events {
			}
		}
		for range h.received {
		}
		select {
		case <-h.runStopped:
		case <-time.After(10 * time.Second):
			t.Error("Session.Run did not stop")
		}
	})
}

// waitRunStopped 等 Session.Run 返回，返回它的错误。
func (h *sessionHarness) waitRunStopped(t *testing.T) error {
	t.Helper()
	// 1. 等 runStopped 关闭，再返回 runErr。
	waitClosed(t, h.runStopped, "Session.Run return")
	return h.runErr
}

// submit 向 Session 提交指定模式的用户文本。
func (h *sessionHarness) submit(t *testing.T, mode session.SessionInputMode, text string) *defs.Item {
	t.Helper()
	// 1. 创建文本 Item 并提交，失败时结束测试。
	item := sessionInput(text)
	if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
		Mode: mode,
		Item: item,
	}); err != nil {
		t.Fatalf("HandleInput(%q) error = %v", text, err)
	}
	return item
}

// submitAgentMessage 提交一条 normal Agent 消息。
func (h *sessionHarness) submitAgentMessage(t *testing.T, text string) *defs.Item {
	t.Helper()
	// 1. 创建发送方为 Agent child 的消息，以 normal 提交。
	item := sessionInput(text)
	item.Message.From = &defs.Address{
		Kind: defs.IdentityAgent,
		ID:   "child",
	}
	if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
		Mode: session.SessionInputModeNormal,
		Item: item,
	}); err != nil {
		t.Fatalf("HandleInput(%q) error = %v", text, err)
	}
	return item
}

// requireInputRejected 检查输入被拒收，错误文本和 wantErr 一致。
func (h *sessionHarness) requireInputRejected(t *testing.T, mode session.SessionInputMode, wantErr string) {
	t.Helper()
	// 1. 提交消息，检查返回的 ID 为空且错误文本和 wantErr 完全一致。
	itemID, err := h.session.HandleInput(t.Context(), &session.SessionInput{
		Mode: mode,
		Item: sessionInput("你好"),
	})
	if itemID != "" || err == nil || err.Error() != wantErr {
		t.Fatalf("HandleInput() = %q, %v; want error %q", itemID, err, wantErr)
	}
}

// requireNoTurn 检查没有 Turn、排队输入、事件、模型请求和日志。
func (h *sessionHarness) requireNoTurn(t *testing.T) {
	t.Helper()
	// 1. 查询执行状态，检查没有活动 Turn、历史 Turn、排队输入、事件、模型调用和日志。
	status, err := h.session.GetStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveTurnID != "" || status.LastTurnID != "" || status.PendingInputCount != 0 {
		t.Fatalf("status = %+v, want no Turn and no pending input", status)
	}
	if len(h.received) != 0 || h.model.count.Load() != 0 {
		t.Fatalf("events = %d, model calls = %d, want none", len(h.received), h.model.count.Load())
	}
	if items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID); len(items) != 0 {
		t.Fatalf("log = %+v, want empty", items)
	}
}

// nextEvent 读取下一条 Session 事件。
func (h *sessionHarness) nextEvent(t *testing.T) session.SessionEvent {
	t.Helper()
	// 1. 十秒内读一条事件，通道关闭或超时时结束测试。
	select {
	case event, open := <-h.received:
		if !open {
			t.Fatal("Session events closed")
		}
		return event
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Session event")
	}
	return nil
}

// nextTurnFinished 读取下一条 Turn 结束事件。
func (h *sessionHarness) nextTurnFinished(t *testing.T) *session.SessionTurnFinished {
	t.Helper()
	// 1. 跳过其他事件，返回下一个 Turn 结束事件。
	for {
		if finished, ok := h.nextEvent(t).(*session.SessionTurnFinished); ok {
			return finished
		}
	}
}

// nextTurnStarted 读取下一条 Turn 开始事件。
func (h *sessionHarness) nextTurnStarted(t *testing.T) *session.SessionTurnStarted {
	t.Helper()
	// 1. 跳过其他事件，返回下一个 Turn 开始事件。
	for {
		if started, ok := h.nextEvent(t).(*session.SessionTurnStarted); ok {
			return started
		}
	}
}

// nextModelBody 读取下一次模型请求正文。
func (h *sessionHarness) nextModelBody(t *testing.T) map[string]any {
	t.Helper()
	// 1. 十秒内读取请求正文，超时结束测试。
	select {
	case body := <-h.model.bodies:
		return body
	case <-time.After(10 * time.Second):
		t.Fatal("model request body did not arrive")
		return nil
	}
}

// requireTurnModel 检查保存的 Turn 记录里的模型选择。
func (h *sessionHarness) requireTurnModel(t *testing.T, turnID defs.TurnID, want defs.ModelSelection) {
	t.Helper()
	// 1. 查询 Turn 记录，检查模型和推理强度和 want 相同。
	record, found, err := h.query.GetTurn(t.Context(), h.record.ID, turnID)
	if err != nil || !found || record.Model != want {
		t.Fatalf("Turn %s = %+v, %v, %v; want model %+v", turnID, record, found, err, want)
	}
}

// waitModelStarted 等待模型收到一次请求。
func (h *sessionHarness) waitModelStarted(t *testing.T) {
	t.Helper()
	// 1. 十秒内读取模型开始信号，超时结束测试。
	select {
	case <-h.model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model request did not start")
	}
}

// waitModelStreaming 等待模型开始写出流式回复。
func (h *sessionHarness) waitModelStreaming(t *testing.T) {
	t.Helper()
	// 1. 十秒内读取模型流式输出信号，超时结束测试。
	select {
	case <-h.model.streaming:
	case <-time.After(10 * time.Second):
		t.Fatal("model response did not start streaming")
	}
}

// requireFinished 检查 Turn 结束事件的 TurnID、输入和终态，completed 时不能带错误。
func requireFinished(t *testing.T, finished *session.SessionTurnFinished, inputs []defs.ItemID, status defs.TurnStatus) {
	t.Helper()
	// 1. 检查 TurnID 非空、inputs 不为 nil 时输入一致、终态一致；completed 时不能带错误。
	if finished.TurnID == "" || (inputs != nil && !slices.Equal(finished.InputItemIDs, inputs)) || finished.Status != status {
		t.Fatalf("finished = %+v, want inputs %v and status %q", finished, inputs, status)
	}
	if status == defs.TurnStatusCompleted && finished.Err != nil {
		t.Fatalf("finished error = %v", finished.Err)
	}
}

// runToolTurn 执行一轮工具调用，返回日志里保存的调用结果。
func (h *sessionHarness) runToolTurn(t *testing.T, callID, name string, arguments map[string]any) sessionCommandResult {
	t.Helper()
	// 1. 提交输入，依次回复工具调用和最终文本，等 Turn 完成后读取保存的结果。
	input := h.submit(t, session.SessionInputModeNormal, "call "+name)
	h.model.replies <- toolCallReply(t, callID, name, arguments)
	h.model.replies <- modelReply{text: "done"}
	requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{input.ID}, defs.TurnStatusCompleted)
	return h.getSavedCommandResult(t, callID)
}

// getSavedCommandResult 从 Session 日志读取指定 CallID 的命令工具结果。
func (h *sessionHarness) getSavedCommandResult(t *testing.T, callID string) sessionCommandResult {
	t.Helper()
	// 1. 按 CallID 找到工具结果，解码正文并带上工具错误，没找到时结束测试。
	for _, item := range readSessionLog(t, h.query, h.record.ConversationID, h.record.ID) {
		if item.Kind != defs.ItemToolResult || item.ToolResult.CallID != callID {
			continue
		}
		var result sessionCommandResult
		if err := json.Unmarshal(item.ToolResult.Content, &result); err != nil {
			t.Fatalf("result %s: %v", item.ToolResult.Content, err)
		}
		result.ToolError = item.ToolResult.Error
		return result
	}
	t.Fatalf("no saved result for %s", callID)
	return sessionCommandResult{}
}

// nextToolProgress 读取下一条工具进度事件里的文本。
func (h *sessionHarness) nextToolProgress(t *testing.T) string {
	t.Helper()
	// 1. 跳过其他事件，解码下一条工具进度的文本。
	for {
		agentEvent, ok := h.nextEvent(t).(*session.SessionAgentEvent)
		if !ok || agentEvent.Event.Type != agent.AgentEventToolProgress {
			continue
		}
		var text string
		if err := json.Unmarshal(agentEvent.Event.ToolProgress.Content, &text); err != nil {
			t.Fatalf("progress %s: %v", agentEvent.Event.ToolProgress.Content, err)
		}
		return text
	}
}

// sessionCommandResult 是命令工具结果里测试关心的字段；ToolError 是这次调用的工具错误。
type sessionCommandResult struct {
	CommandID  string `json:"command_id"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason"`
	Output     string `json:"output"`
	Commands   []struct {
		CommandID string `json:"command_id"`
		Status    string `json:"status"`
	} `json:"commands"`
	ToolError *defs.ToolError `json:"-"`
}

// toolCallReply 创建一次发出指定工具调用的模型回复。
func toolCallReply(t *testing.T, callID, name string, arguments map[string]any) modelReply {
	t.Helper()
	// 1. 编码参数和工具调用增量，后面跟上 finish_reason 为 tool_calls 的结束增量。
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	toolCall := map[string]any{
		"index": 0,
		"id":    callID,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": string(encodedArguments),
		},
	}
	chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{toolCall}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return modelReply{chunks: []string{string(chunk), `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`}}
}

// newLocalSessionEnvironments 创建以本机临时目录为工作区的 local 执行环境。
func newLocalSessionEnvironments(t *testing.T) []executor.ExecutionEnvironment {
	t.Helper()
	// 1. 创建临时的本机工作区，装成唯一的 local 执行环境。
	work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("local", "", work))
	if err != nil {
		t.Fatal(err)
	}
	return []executor.ExecutionEnvironment{environment}
}

// requireProcessExited 检查输出里的进程号对应的进程已经退出。
func requireProcessExited(t *testing.T, output string) {
	t.Helper()
	// 1. 解析进程号，检查发信号 0 返回 ESRCH。
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		t.Fatalf("output %q is not a process ID", output)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process %d still exists: kill(0) = %v", pid, err)
	}
}

// waitClosed 等信号通道关闭。
func waitClosed(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	// 1. 十秒内等通道关闭，超时结束测试。
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// waitSteerResult 是后台 WaitSteer 调用返回的补充输入和错误。
type waitSteerResult struct {
	items []*defs.Item
	err   error
}

// openSessionDAO 在临时目录创建 SQLite 记录和文件日志存储，返回 DAO。
func openSessionDAO(t *testing.T) (dao.Query, dao.Mutation) {
	t.Helper()
	// 1. 打开 SQLite 和文件日志，创建 DAO，登记测试结束时关闭数据库。
	root := t.TempDir()
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	return query, mutation
}

// readSessionLog 按时间正序读取 Session 日志的 Item。
func readSessionLog(t *testing.T, query dao.Query, conversationID defs.ConversationID, sessionID defs.SessionID) []*defs.Item {
	t.Helper()
	// 1. 正序查询日志，取出各条 Item，失败时结束测试。
	page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Order:          storage.DirectionAsc,
		Limit:          100,
	})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]*defs.Item, len(page.Items))
	for index, logged := range page.Items {
		items[index] = logged.Item
	}
	return items
}

// sessionInput 创建用户文本输入 Item。
func sessionInput(text string) *defs.Item {
	// 1. 分配 ItemID，填写用户发送方和文本。
	return &defs.Item{
		ID:   defs.NewItemID(),
		Kind: defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityUser,
				ID:   "user",
			},
			Content: []*defs.Content{
				{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				},
			},
		},
	}
}

// modelReply 是测试给受控模型服务的一次回复。
type modelReply struct {
	// text 是不 hold、也没给 chunks 时回复的文本。
	text string
	// status 非 0 时直接回这个 HTTP 错误码。
	status int
	// hold 是否写出 chunks 后挂住，直到请求取消？没给 chunks 时写一段 partial 文本。
	hold bool
	// chunks 是要依次写出的 SSE data；不 hold 时写完再写 [DONE]。
	chunks []string
}

// controlledModel 是按测试给的回复应答的模型 HTTP 服务。
type controlledModel struct {
	server *httptest.Server
	// started 每收到一次请求就送进这次请求的序号。
	started chan int
	// streaming 在 hold 回复写出增量后送进这次请求的序号。
	streaming chan int
	// replies 是测试按顺序给出的回复。
	replies chan modelReply
	// bodies 按到达顺序保存每次请求解码后的请求体。
	bodies chan map[string]any
	// count 是已收到的请求数。
	count atomic.Int64
}

// newControlledModel 启动按测试给的回复应答的模型 HTTP 服务，测试结束时关闭。
func newControlledModel(t *testing.T) *controlledModel {
	t.Helper()
	// 1. 创建请求信号、请求体和回复队列，启动 HTTP 服务。
	model := &controlledModel{
		started:   make(chan int, 128),
		streaming: make(chan int, 128),
		replies:   make(chan modelReply, 128),
		bodies:    make(chan map[string]any, 128),
	}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解码并记下请求，通知已收到，等测试的回复或请求取消。
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model.bodies <- body
		request := int(model.count.Add(1))
		model.started <- request
		var reply modelReply
		select {
		case reply = <-model.replies:
		case <-r.Context().Done():
			return
		}
		// 2. 发送错误或流式增量；hold 时通知已开始流式输出并挂到请求取消，否则写结束标记。
		if reply.status != 0 {
			http.Error(w, "injected model failure", reply.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if reply.hold {
			chunks := reply.chunks
			if len(chunks) == 0 {
				chunks = []string{`{"choices":[{"delta":{"content":"partial"}}]}`}
			}
			for _, chunk := range chunks {
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			}
			w.(http.Flusher).Flush()
			model.streaming <- request
			<-r.Context().Done()
			return
		}
		if len(reply.chunks) > 0 {
			for _, chunk := range reply.chunks {
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", reply.text))
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	// 2. 登记测试结束时关闭模型服务。
	t.Cleanup(model.server.Close)
	return model
}

// blockingTerminalMutation 在第一次写 Turn 终态时通知测试并等放行。
type blockingTerminalMutation struct {
	dao.Mutation
	// terminalStarted 在第一次写 Turn 终态时关闭。
	terminalStarted chan struct{}
	// release 由测试关闭，放行这次终态写入。
	release chan struct{}
	once    sync.Once
}

// SetTurn 第一次写 Turn 终态时通知测试并等放行，再交给 DAO 保存。
func (mutation *blockingTerminalMutation) SetTurn(ctx context.Context, value defs.Turn) error {
	// 1. 第一次写不是 running 的状态时关闭 terminalStarted，等 release 关闭；然后交给 DAO。
	if value.Status != defs.TurnStatusRunning {
		first := false
		mutation.once.Do(func() {
			first = true
			close(mutation.terminalStarted)
		})
		if first {
			<-mutation.release
		}
	}
	return mutation.Mutation.SetTurn(ctx, value)
}

// failingTerminalMutation 让所有 Turn 终态写入失败，running 记录照常保存。
type failingTerminalMutation struct {
	dao.Mutation
}

// SetTurn 写终态时返回注入错误。
func (mutation *failingTerminalMutation) SetTurn(ctx context.Context, value defs.Turn) error {
	// 1. 不是 running 时返回注入错误，否则交给 DAO。
	if value.Status != defs.TurnStatusRunning {
		return errors.New("injected terminal Turn persistence failure")
	}
	return mutation.Mutation.SetTurn(ctx, value)
}

// summarySessionModel 是用 Responses 协议应答的测试模型服务：摘要请求回 saved summary，其他请求回 answer；收到的请求按顺序放进 requests。
type summarySessionModel struct{ requests chan map[string]any }

// ServeHTTP 应答一次 Responses 请求。
func (m *summarySessionModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. 解码并记下请求，tool_choice 为 none 时回复摘要，否则回复 answer。
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	m.requests <- request
	text := "answer"
	if request["tool_choice"] == "none" {
		text = "saved summary"
	}
	// 2. 写出文本增量和 completed 事件。
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":%q}\n\n", text)
	fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
}

// TestSessionSkillInputGroups 检查 Skill 正文和它的输入同组：排队、steer、中断重投后仍在同一轮保存，且各只保存一次。
func TestSessionSkillInputGroups(t *testing.T) {
	// 1. 列出普通输入、排队、steer 和未读 steer 重投场景；busy 表示提交前已有活动 Turn，fallback 表示 steer 读入前就中断活动 Turn。
	for _, tt := range []struct {
		name     string
		mode     session.SessionInputMode
		busy     bool
		fallback bool
	}{
		{
			"normal",
			session.SessionInputModeNormal,
			false,
			false,
		},
		{
			"queued",
			session.SessionInputModeNormal,
			true,
			false,
		},
		{
			"steer",
			session.SessionInputModeSteer,
			true,
			false,
		},
		{
			"unread_steer_fallback",
			session.SessionInputModeSteer,
			true,
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 加载 Skill，按场景先开首轮，再提交带斜杠指令的输入。
			h := newSessionHarness(t)
			root := loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			var first *defs.Item
			if tt.busy {
				first = h.submit(t, session.SessionInputModeNormal, "hold")
				h.waitModelStarted(t)
				if tt.fallback {
					h.model.replies <- modelReply{hold: true}
					h.waitModelStreaming(t)
				}
			}
			input := sessionInput("请用 /review /tests /skill:review 检查")
			request := &session.SessionInput{
				Mode:    tt.mode,
				Item:    input,
				InputID: "skill-request",
			}
			id, err := h.session.HandleInput(t.Context(), request)
			if err != nil || id != input.ID {
				t.Fatalf("accept = %s, %v", id, err)
			}
			// 3. 删掉正文文件：重复 InputID 仍返回原 ID，新输入重新读取正文时失败。
			for _, name := range []string{"review", "tests"} {
				if err := os.Remove(filepath.Join(root, name, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			id, err = h.session.HandleInput(t.Context(), request)
			if err != nil || id != input.ID {
				t.Fatalf("replay = %s, %v", id, err)
			}
			if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode:    tt.mode,
				Item:    sessionInput("/review"),
				InputID: "new-request",
			}); err == nil {
				t.Fatal("new submission did not reread the deleted file")
			}
			// 4. 按场景中断或完成 Turn，检查 Skill 和原消息在同一轮保存，且各只保存一次。
			if tt.fallback {
				if _, err := h.session.InterruptTurn(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID}, defs.TurnStatusInterrupted)
			}
			h.model.replies <- modelReply{text: "done"}
			if tt.busy && !tt.fallback {
				h.model.replies <- modelReply{text: "done"}
			}
			finished := h.nextTurnFinished(t)
			if tt.busy && tt.mode == session.SessionInputModeNormal {
				requireFinished(t, finished, []defs.ItemID{first.ID}, defs.TurnStatusCompleted)
				finished = h.nextTurnFinished(t)
			}
			wantIDs := []defs.ItemID{input.ID}
			if tt.busy && !tt.fallback && tt.mode == session.SessionInputModeSteer {
				wantIDs = []defs.ItemID{first.ID, input.ID}
			}
			requireFinished(t, finished, wantIDs, defs.TurnStatusCompleted)
			var names []string
			for _, item := range readSessionLog(t, h.query, h.record.ConversationID, h.record.ID) {
				if item.SkillInstruction != nil {
					if item.SkillInstruction.InputItemID != input.ID || item.TurnID != finished.TurnID {
						t.Fatalf("instruction split from input: %+v", item)
					}
					names = append(names, item.SkillInstruction.Name)
				}
			}
			if !slices.Equal(names, []string{"review", "tests"}) {
				t.Fatalf("skills = %v", names)
			}
		})
	}
}

// loadSessionTestSkills 写入并加载普通 Skill 和带插件前缀的 Skill，返回普通 Skill 的根目录。
func loadSessionTestSkills(t *testing.T, h *sessionHarness) string {
	t.Helper()
	// 1. 写入 review、tests 和插件 greet 的 Skill 文件。
	root := t.TempDir()
	for _, name := range []string{"review", "tests"} {
		writeSessionTestSkill(t, root, name)
	}
	pluginRoot := t.TempDir()
	writeSessionTestSkill(t, pluginRoot, "greet")
	// 2. 创建两个本机工作区，加载普通目录和带 demo: 前缀的插件目录。
	work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	pluginWork, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(pluginRoot))
	if err != nil {
		t.Fatal(err)
	}
	h.skills, err = skill.LoadSkills(t.Context(), []skill.SkillDirectory{
		{
			Workspace:  work,
			Path:       root,
			ExecutorID: "local",
			Strict:     true,
		},
		{
			Workspace:  pluginWork,
			Path:       pluginRoot,
			ExecutorID: "local",
			NamePrefix: "demo:",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeSessionTestSkill 在指定目录写入 Skill 名称、描述和正文。
func writeSessionTestSkill(t *testing.T, root, name string) {
	t.Helper()
	// 1. 创建 Skill 目录并写入 SKILL.md。
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: test\n---\n"+name+" instructions"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestSessionSkillReferences 检查用户斜杠指令的匹配、去重，以及 /skill: 指向不存在的 Skill 时拒收输入。
func TestSessionSkillReferences(t *testing.T) {
	// 1. 准备完整指令、重复指令、带标点、插件前缀和 /skill: 指向不存在的名字。
	for _, tt := range []struct {
		text      string
		want      []string
		wantError bool
	}{
		{
			"请用 /review /tests /review",
			[]string{"review", "tests"},
			false,
		},
		{
			"/review /skill:review",
			[]string{"review"},
			false,
		},
		{
			"/missing /review/file /review, /skill:review, \\/review",
			nil,
			false,
		},
		{
			"/demo:greet /skill:demo:greet",
			[]string{"demo:greet"},
			false,
		},
		{
			"/skill:missing",
			nil,
			true,
		},
		{
			"/skill:demo:missing",
			nil,
			true,
		},
	} {
		t.Run(tt.text, func(t *testing.T) {
			// 2. 提交用户消息，检查拒收错误或保存的 Skill 顺序。
			h := newSessionHarness(t)
			loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			input := sessionInput(tt.text)
			_, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode: session.SessionInputModeNormal,
				Item: input,
			})
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("skill %q is not available", strings.TrimPrefix(tt.text, "/skill:"))) {
					t.Fatalf("error=%v", err)
				}
				if got := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID); len(got) != 0 {
					t.Fatalf("rejected log=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h.model.replies <- modelReply{text: "done"}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{input.ID}, defs.TurnStatusCompleted)
			var got []string
			for _, item := range readSessionLog(t, h.query, h.record.ConversationID, h.record.ID) {
				if item.SkillInstruction != nil {
					got = append(got, item.SkillInstruction.Name)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("skills=%v want=%v", got, tt.want)
			}
		})
	}
}

// TestSessionSkillReadFailure 检查 Skill 正文读取失败时整条输入被拒、什么都不保存，恢复后用同一 InputID 重试成功。
func TestSessionSkillReadFailure(t *testing.T) {
	// 1. 分别删掉 review 或 tests 的正文，再提交同时引用两者的消息。
	for _, failedName := range []string{"review", "tests"} {
		t.Run(failedName, func(t *testing.T) {
			h := newSessionHarness(t)
			root := loadSessionTestSkills(t, h)
			path := filepath.Join(root, failedName, "SKILL.md")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			h.start(t, nil, true)
			request := &session.SessionInput{
				Mode:    session.SessionInputModeNormal,
				Item:    sessionInput("/review /tests"),
				InputID: "retry",
			}
			if _, err := h.session.HandleInput(t.Context(), request); err == nil {
				t.Fatal("missing Skill accepted")
			}
			if got := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID); len(got) != 0 {
				t.Fatalf("partial group persisted=%v", got)
			}
			// 2. 恢复文件后用相同的 InputID 重试，核对保存的完整消息组。
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			id, err := h.session.HandleInput(t.Context(), request)
			if err != nil || id != request.Item.ID {
				t.Fatalf("retry=%s %v", id, err)
			}
			h.model.replies <- modelReply{text: "done"}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{request.Item.ID}, defs.TurnStatusCompleted)
			items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
			if len(items) != 4 || items[0].ID != id || items[1].Kind != defs.ItemSkillInstruction || items[2].Kind != defs.ItemSkillInstruction {
				t.Fatalf("accepted log=%+v", items)
			}
		})
	}
}

// TestSessionContextSkillReferences 检查附件（IsContext）文本里的斜杠指令不选 Skill，附件照样交给模型。
func TestSessionContextSkillReferences(t *testing.T) {
	// 1. 准备正文和附件里不同的 Skill 指令。
	for _, tt := range []struct {
		name, direct, attached string
		wantSkills             int
	}{
		{
			"attachment_only",
			"check this file",
			"/review /skill:missing",
			0,
		},
		{
			"mixed_input",
			"/review",
			"/tests /skill:missing",
			1,
		},
		{
			"direct_input",
			"/review /tests",
			"file content",
			2,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 附件标成 IsContext 后提交，检查模型收到附件，且只有正文里的指令生成 Skill Item。
			h := newSessionHarness(t)
			loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			item := sessionInput(tt.direct)
			item.Message.Content = append(item.Message.Content, &defs.Content{
				Kind: defs.ContentText,
				Text: &defs.TextContent{
					Text:      tt.attached,
					IsContext: true,
				},
			})
			if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode: session.SessionInputModeIdleOnly,
				Item: item,
			}); err != nil {
				t.Fatal(err)
			}
			request := <-h.model.bodies
			body, _ := json.Marshal(request)
			if !strings.Contains(string(body), tt.attached) {
				t.Fatalf("model lacks attachment: %s", body)
			}
			h.model.replies <- modelReply{text: "done"}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{item.ID}, defs.TurnStatusCompleted)
			count := 0
			for _, logged := range readSessionLog(t, h.query, h.record.ConversationID, h.record.ID) {
				if logged.Kind == defs.ItemSkillInstruction {
					count++
				}
			}
			if count != tt.wantSkills {
				t.Fatalf("skills = %d, want %d", count, tt.wantSkills)
			}
		})
	}
}

// TestSessionIdleOnly 检查 idle_only 输入忙时在读 Skill 前就被拒、忙时重复 InputID 返回原 ID、中断后恢复空闲。
func TestSessionIdleOnly(t *testing.T) {
	// 1. 分别检查忙时拒收、立即中断和忙时重复 InputID。
	for _, scenario := range []string{"busy_before_skill_read", "cancel_immediately", "deduplicates_while_busy"} {
		t.Run(scenario, func(t *testing.T) {
			// 2. 提交 idle_only 输入，按场景重试或再提交带 Skill 的输入，检查结果。
			h := newSessionHarness(t)
			root := loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			first := &session.SessionInput{
				Mode:    session.SessionInputModeIdleOnly,
				Item:    sessionInput("first"),
				InputID: "first",
			}
			if _, err := h.session.HandleInput(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			if scenario == "deduplicates_while_busy" {
				if id, err := h.session.HandleInput(t.Context(), first); err != nil || id != first.Item.ID {
					t.Fatalf("duplicate = %s, %v", id, err)
				}
			}
			if scenario == "busy_before_skill_read" {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
					Mode: session.SessionInputModeIdleOnly,
					Item: sessionInput("/review"),
				}); !errors.Is(err, session.ErrSessionBusy) {
					t.Fatalf("busy = %v", err)
				}
			}
			// 3. 中断活动 Turn，检查恢复空闲并能接收下一条输入。
			if id, err := h.session.InterruptTurn(t.Context()); err != nil || id == "" {
				t.Fatalf("interrupt = %s, %v", id, err)
			}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.Item.ID}, defs.TurnStatusInterrupted)
			status, err := h.session.GetStatus(t.Context())
			if err != nil || status.State != session.SessionStateIdle {
				t.Fatalf("status = %+v, %v", status, err)
			}
			next := h.submit(t, session.SessionInputModeIdleOnly, "continue")
			h.model.replies <- modelReply{text: "done"}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{next.ID}, defs.TurnStatusCompleted)
		})
	}
}

// TestSessionCompactContext 检查手动压缩：没历史时不请求模型、忙时返回 ErrSessionBusy、只压缩的 Turn 不写模型变更。
func TestSessionCompactContext(t *testing.T) {
	// 1. 准备检查忙的辅助函数，以及空历史、有活动 Turn、有排队输入和换模型场景。
	requireBusy := func(t *testing.T, h *sessionHarness) {
		t.Helper()
		if turnID, err := h.session.CompactContext(t.Context()); !errors.Is(err, session.ErrSessionBusy) || turnID != "" {
			t.Fatalf("CompactContext() = %q, %v; want ErrSessionBusy", turnID, err)
		}
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *sessionHarness)
	}{
		{
			name: "idle_without_history_completes_without_model_call",
			run: func(t *testing.T, h *sessionHarness) {
				turnID, err := h.session.CompactContext(t.Context())
				if err != nil || turnID == "" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				if started := h.nextTurnStarted(t); started.TurnID != turnID || started.Model.Ref != testDefaultModelRef {
					t.Fatalf("started = %+v, want Turn %s", started, turnID)
				}
				finished := h.nextTurnFinished(t)
				requireFinished(t, finished, nil, defs.TurnStatusCompleted)
				if finished.TurnID != turnID || len(finished.InputItemIDs) != 0 || len(finished.Outputs) != 0 {
					t.Fatalf("finished = %+v, want no input and no output for Turn %s", finished, turnID)
				}
				if count := h.model.count.Load(); count != 0 {
					t.Fatalf("model calls = %d, want 0", count)
				}
				record, found, err := h.query.GetTurn(t.Context(), h.record.ID, turnID)
				if err != nil || !found || record.Status != defs.TurnStatusCompleted {
					t.Fatalf("Turn record = %+v, %v, %v", record, found, err)
				}
				if items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID); len(items) != 0 {
					t.Fatalf("log = %+v, want empty", items)
				}
			},
		},
		{
			name: "active_turn_is_busy",
			run: func(t *testing.T, h *sessionHarness) {
				first := h.submit(t, session.SessionInputModeNormal, "第一轮")
				h.waitModelStarted(t)
				requireBusy(t, h)
				h.model.replies <- modelReply{text: "一"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "queued_input_is_busy",
			run: func(t *testing.T, h *sessionHarness) {
				first := h.submit(t, session.SessionInputModeNormal, "第一轮")
				h.waitModelStarted(t)
				second := h.submit(t, session.SessionInputModeNormal, "第二轮")
				requireBusy(t, h)
				h.model.replies <- modelReply{text: "一"}
				h.model.replies <- modelReply{text: "二"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID}, defs.TurnStatusCompleted)
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{second.ID}, defs.TurnStatusCompleted)
			},
		},
		{
			name: "does_not_update_previous_turn_model",
			run: func(t *testing.T, h *sessionHarness) {
				h.submit(t, session.SessionInputModeNormal, "第一轮")
				h.model.replies <- modelReply{text: "一"}
				h.nextTurnFinished(t)
				if err := h.session.SelectModel(t.Context(), defs.ModelSelection{Ref: testOtherModelRef}); err != nil {
					t.Fatal(err)
				}
				turnID, err := h.session.CompactContext(t.Context())
				if err != nil || turnID == "" {
					t.Fatalf("CompactContext() = %q, %v", turnID, err)
				}
				h.model.replies <- modelReply{text: "摘要"}
				if started := h.nextTurnStarted(t); started.TurnID != turnID || started.Model.Ref != testOtherModelRef {
					t.Fatalf("started = %+v, want Turn %s on the selected model", started, turnID)
				}
				finished := h.nextTurnFinished(t)
				requireFinished(t, finished, nil, defs.TurnStatusCompleted)
				if len(finished.InputItemIDs) != 0 || len(finished.Outputs) != 1 || finished.Outputs[0].Kind != defs.ItemContextSummary {
					t.Fatalf("finished = %+v, want only a context summary output", finished)
				}

				// 只压缩的 Turn 不写模型变更；下一个普通 Turn 在输入前记下两个普通 Turn 之间的模型变化。
				second := h.submit(t, session.SessionInputModeNormal, "第二轮")
				h.model.replies <- modelReply{text: "二"}
				requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{second.ID}, defs.TurnStatusCompleted)
				items := readSessionLog(t, h.query, h.record.ConversationID, h.record.ID)
				var kinds []defs.ItemKind
				for _, item := range items {
					kinds = append(kinds, item.Kind)
				}
				want := []defs.ItemKind{defs.ItemMessage, defs.ItemMessage, defs.ItemContextSummary, defs.ItemModelChange, defs.ItemMessage, defs.ItemMessage}
				if !slices.Equal(kinds, want) {
					t.Fatalf("log kinds = %v, want %v", kinds, want)
				}
				if change := items[3].ModelChange; *change != (defs.ModelChange{
					PreviousModel: testDefaultModelRef,
					Model:         testOtherModelRef,
				}) {
					t.Fatalf("model change = %+v", change)
				}
			},
		},
	}
	// 2. 每个场景启动独立的 Session 执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			h.start(t, nil, true)
			tt.run(t, h)
		})
	}
}

// sessionHooksStub 是 hook.Hooks 替身：记下收到的全部事件；SessionStart 回一条已完成的运行记录（先发 running 再发最终记录），其他事件不回任何决定。
type sessionHooksStub struct {
	mutex  sync.Mutex
	events []*hook.HookEvent
}

// RunEvent 记下 hook 事件；SessionStart 时先后发出 running 和 completed 运行记录。
func (stub *sessionHooksStub) RunEvent(_ context.Context, request *hook.HookEventRequest, runUpdates chan<- *defs.HookRun) *hook.HookOutcome {
	// 1. 记下收到的事件，不是 SessionStart 时返回空结果。
	stub.mutex.Lock()
	stub.events = append(stub.events, request.Event)
	stub.mutex.Unlock()
	if request.Event.Name != defs.HookEventSessionStart {
		return &hook.HookOutcome{}
	}
	// 2. SessionStart 依次发送同一 ID 的 running 和 completed 记录，返回 completed 记录。
	run := &defs.HookRun{
		ID:         defs.NewHookRunID(),
		Event:      request.Event.Name,
		ExecutorID: request.ExecutorID,
		Status:     defs.HookRunCompleted,
	}
	running := *run
	running.Status = defs.HookRunRunning
	runUpdates <- &running
	runUpdates <- run
	return &hook.HookOutcome{SavedRuns: []*defs.HookRun{run}}
}

// GetHookInfos 返回测试 hook 信息。
func (*sessionHooksStub) GetHookInfos() []*hook.HookInfo {
	// 1. 返回空列表。
	return nil
}

// getEvents 返回收到的全部事件的副本。
func (stub *sessionHooksStub) getEvents() []*hook.HookEvent {
	// 1. 加锁读取并复制已收到的事件。
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	return slices.Clone(stub.events)
}

// TestSessionHookStartSource 检查 SessionStart 只在第一个普通 Turn 触发、只压缩的 Turn 不占用来源、来源为空时不触发，运行记录按顺序转发。
func TestSessionHookStartSource(t *testing.T) {
	// 1. 准备 startup、先压缩再 resume 和空来源场景。
	tests := []struct {
		name        string
		startSource string
		// isCompactFirst 是否在两个普通 Turn 之前先跑一个只压缩的 Turn？
		isCompactFirst bool
		// wantSource 是第一个普通 Turn 的 SessionStart 来源；为空表示不触发。
		wantSource string
	}{
		{
			name:        "first_normal_turn_takes_source",
			startSource: "startup",
			wantSource:  "startup",
		},
		{
			name:           "compact_only_turn_does_not_take_source",
			startSource:    "resume",
			isCompactFirst: true,
			wantSource:     "resume",
		},
		{name: "empty_source_never_triggers_session_start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 创建带 hook 的执行环境，按场景先压缩，再执行两个普通 Turn。
			work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace", nil))
			if err != nil {
				t.Fatal(err)
			}
			hooks := &sessionHooksStub{}
			h := newSessionHarness(t)
			environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("local", "", work))
			if err != nil {
				t.Fatal(err)
			}
			h.environments = []executor.ExecutionEnvironment{environment}
			h.hooks = hooks
			h.hookStartSource = tt.startSource
			h.start(t, nil, true)

			if tt.isCompactFirst {
				if _, err := h.session.CompactContext(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireFinished(t, h.nextTurnFinished(t), nil, defs.TurnStatusCompleted)
			}
			var normalTurnIDs []defs.TurnID
			var hookRunEvents []*session.SessionHookRunEvent
			for _, text := range []string{"第一轮", "第二轮"} {
				input := h.submit(t, session.SessionInputModeNormal, text)
				h.model.replies <- modelReply{text: "好"}
				for {
					event := h.nextEvent(t)
					if hookRunEvent, ok := event.(*session.SessionHookRunEvent); ok {
						hookRunEvents = append(hookRunEvents, hookRunEvent)
					}
					if finished, ok := event.(*session.SessionTurnFinished); ok {
						requireFinished(t, finished, []defs.ItemID{input.ID}, defs.TurnStatusCompleted)
						normalTurnIDs = append(normalTurnIDs, finished.TurnID)
						break
					}
				}
			}

			// 3. 检查只在第一个普通 Turn 触发 SessionStart，并按顺序转发运行记录。
			var gotStarts []*hook.HookEvent
			for _, event := range hooks.getEvents() {
				if event.Name == defs.HookEventSessionStart {
					gotStarts = append(gotStarts, event)
				}
			}
			if tt.wantSource == "" {
				if len(gotStarts) != 0 || len(hookRunEvents) != 0 {
					t.Fatalf("SessionStart events = %+v, hook run events = %+v, want none", gotStarts, hookRunEvents)
				}
				return
			}
			if len(gotStarts) != 1 || gotStarts[0].Source != tt.wantSource || gotStarts[0].TurnID != normalTurnIDs[0] {
				t.Fatalf("SessionStart events = %+v, want one with source %q in Turn %s", gotStarts, tt.wantSource, normalTurnIDs[0])
			}
			if len(hookRunEvents) != 2 || hookRunEvents[0].TurnID != normalTurnIDs[0] || hookRunEvents[0].Run.Status != defs.HookRunRunning ||
				hookRunEvents[1].TurnID != normalTurnIDs[0] || hookRunEvents[1].Run.Status != defs.HookRunCompleted || hookRunEvents[0].Run.ID != hookRunEvents[1].Run.ID {
				t.Fatalf("hook run events = %+v, want running then completed of one run in Turn %s", hookRunEvents, normalTurnIDs[0])
			}
		})
	}
}

// TestSessionRecoversTurnItemsRange 检查修复没结束的 Turn 时补全它的 Item 范围，下一轮从日志末尾接着记。
func TestSessionRecoversTurnItemsRange(t *testing.T) {
	// 1. 准备记录里有起始序号、没有起始序号和没有 Item 的 running Turn。
	for _, tt := range []struct {
		name       string
		count      int
		savedFirst bool
	}{
		{
			"saved_start",
			3,
			true,
		}, {
			"missing_start",
			300,
			false,
		}, {
			"no_items",
			0,
			false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			previous := sessionInput("previous")
			previous.TurnID = "previous"
			if _, err := h.mutation.AppendItems(t.Context(), h.record.ConversationID, h.record.ID, previous); err != nil {
				t.Fatal(err)
			}
			running := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: h.record.ConversationID,
				SessionID:      h.record.ID,
				Status:         defs.TurnStatusRunning,
			}
			for range tt.count {
				item := sessionInput("unfinished")
				item.TurnID = running.ID
				if _, err := h.mutation.AppendItems(t.Context(), h.record.ConversationID, h.record.ID, item); err != nil {
					t.Fatal(err)
				}
			}
			if tt.savedFirst {
				running.ItemsRange.FirstOrdinal = 2
			}
			if err := h.mutation.SetTurn(t.Context(), running); err != nil {
				t.Fatal(err)
			}
			// 2. 启动 Session 触发修复，检查 interrupted 状态和恢复后的 Item 范围。
			h.start(t, nil, true)
			if _, err := h.session.GetStatus(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _, err := h.query.GetTurn(t.Context(), h.record.ID, running.ID)
			want := defs.TurnItemRange{}
			if tt.count > 0 {
				want = defs.TurnItemRange{
					FirstOrdinal: 2,
					LastOrdinal:  uint64(tt.count + 1),
				}
			}
			if err != nil || got.Status != defs.TurnStatusInterrupted || got.ItemsRange != want {
				t.Fatalf("recovered = %+v, %v, want %+v", got, err, want)
			}
			// 3. 执行下一轮，检查它的起始序号紧接在日志末尾之后。
			h.submit(t, session.SessionInputModeNormal, "next")
			h.model.replies <- modelReply{text: "answer"}
			finished := h.nextTurnFinished(t)
			next, _, err := h.query.GetTurn(t.Context(), h.record.ID, finished.TurnID)
			if err != nil || next.ItemsRange.FirstOrdinal != uint64(tt.count+2) {
				t.Fatalf("next = %+v, %v", next, err)
			}
		})
	}
}

// recoveryItemQuery 读 Item 日志时一律返回注入错误，让修复读不到日志。
type recoveryItemQuery struct{ dao.Query }

// ListItems 返回注入的日志读取错误。
func (q *recoveryItemQuery) ListItems(context.Context, dao.ListItemsRequest) (storage.Page[dao.LoggedItem, storage.Ordinal], error) {
	// 1. 返回 recovery log offline。
	return storage.Page[dao.LoggedItem, storage.Ordinal]{}, errors.New("recovery log offline")
}

// TestSessionRecoveryFailureStopsBeforeNextTurn 检查修复最近 Turn 失败时 Run 返回错误，之后的输入被拒、不请求模型。
func TestSessionRecoveryFailureStopsBeforeNextTurn(t *testing.T) {
	// 1. 准备日志读取失败、终态保存失败和记录有起始序号但日志尾部不是它的场景。
	for _, tt := range []struct {
		name                                   string
		readFailure, writeFailure, missingTail bool
	}{
		{
			name:        "log_read",
			readFailure: true,
		}, {
			name:         "terminal_save",
			writeFailure: true,
		}, {
			name:        "declared_start_without_matching_tail",
			missingTail: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionHarness(t)
			running := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: h.record.ConversationID,
				SessionID:      h.record.ID,
				Status:         defs.TurnStatusRunning,
			}
			if tt.missingTail {
				running.ItemsRange.FirstOrdinal = 1
			}
			if err := h.mutation.SetTurn(t.Context(), running); err != nil {
				t.Fatal(err)
			}
			if tt.readFailure {
				h.query = &recoveryItemQuery{Query: h.query}
			}
			var mutation dao.Mutation
			if tt.writeFailure {
				mutation = &failingTerminalMutation{Mutation: h.mutation}
			}
			// 2. 启动 Session，检查修复报错、之后的输入被拒且没请求模型。
			h.start(t, mutation, true)
			if err := h.waitRunStopped(t); err == nil {
				t.Fatal("recovery failure accepted")
			}
			if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode: session.SessionInputModeNormal,
				Item: sessionInput("next"),
			}); err == nil {
				t.Fatal("input accepted after failed recovery")
			}
			if h.model.count.Load() != 0 {
				t.Fatal("model started after failed recovery")
			}
		})
	}
}

// TestSessionPendingInputOperations 检查完整用户输入组的移动、取回、消费与打断同批处理。
func TestSessionPendingInputOperations(t *testing.T) {
	// 1. 按场景运行公开队列操作。
	for _, test := range []struct {
		name string
		run  func(*testing.T, *sessionHarness)
	}{
		{"withdraw_normal_and_steer", func(t *testing.T, h *sessionHarness) {
			root := loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			h.submit(t, session.SessionInputModeNormal, "first")
			h.waitModelStarted(t)
			turnID := h.nextTurnStarted(t).TurnID
			for _, isSteer := range []bool{false, true} {
				input := sessionInput("/review\n  改小  一点")
				input.Message.Content = append(input.Message.Content,
					&defs.Content{
						Kind: defs.ContentText,
						Text: &defs.TextContent{
							Text:      "不进预览",
							IsContext: true,
						},
					},
					&defs.Content{
						Kind: defs.ContentImage,
						Image: &defs.ImageContent{
							DataURL: "data:image/png;base64,a",
							Alt:     "图片",
						},
					})
				if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
					Mode: session.SessionInputModeNormal,
					Item: input,
				}); err != nil {
					t.Fatal(err)
				}
				if isSteer {
					if err := os.Remove(filepath.Join(root, "review", "SKILL.md")); err != nil {
						t.Fatal(err)
					}
					if err := h.session.SteerInput(t.Context(), input.ID, turnID); err != nil {
						t.Fatal(err)
					}
				}
				pending := h.waitPendingInputs(t, func(p session.SessionPendingInputs) bool { return len(p.Inputs) == 1 && p.Inputs[0].IsSteer == isSteer })
				if pending.SteerTurnID != turnID || pending.Inputs[0].Preview != "/review 改小 一点" || pending.Inputs[0].ImageCount != 1 {
					t.Fatalf("pending = %+v", pending)
				}
				items, err := h.session.WithdrawInput(t.Context(), input.ID)
				if err != nil || len(items) != 2 || items[0].ID != input.ID || items[1].SkillInstruction.InputItemID != input.ID {
					t.Fatalf("withdraw = %+v, %v", items, err)
				}
				if items[0].Message.Content[0].Text.Text != "/review\n  改小  一点" || items[0].Message.Content[1].Text.IsContext != true || items[0].Message.Content[2].Image.Alt != "图片" {
					t.Fatalf("content changed: %+v", items[0].Message.Content)
				}
				h.waitPendingInputs(t, func(p session.SessionPendingInputs) bool { return len(p.Inputs) == 0 })
				if _, err := h.session.WithdrawInput(t.Context(), input.ID); err == nil {
					t.Fatal("withdrawn input accepted again")
				}
			}
		}},
		{"steer_consumed_before_save", func(t *testing.T, h *sessionHarness) {
			input := sessionInput("consume me")
			mutation := &blockingInputMutation{
				Mutation: h.mutation,
				itemID:   input.ID,
				started:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			defer close(mutation.release)
			h.start(t, mutation, true)
			h.submit(t, session.SessionInputModeNormal, "first")
			h.waitModelStarted(t)
			turnID := h.nextTurnStarted(t).TurnID
			if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode: session.SessionInputModeNormal,
				Item: input,
			}); err != nil {
				t.Fatal(err)
			}
			if err := h.session.SteerInput(t.Context(), input.ID, turnID); err != nil {
				t.Fatal(err)
			}
			h.waitPendingInputs(t, func(p session.SessionPendingInputs) bool { return len(p.Inputs) == 1 && p.Inputs[0].IsSteer })
			h.model.replies <- modelReply{text: "first reply"}
			waitClosed(t, mutation.started, "steer save")
			h.waitPendingInputs(t, func(p session.SessionPendingInputs) bool { return len(p.Inputs) == 0 })
			if _, err := h.session.WithdrawInput(t.Context(), input.ID); err == nil {
				t.Fatal("consumed input was withdrawn")
			}
		}},
		{"successive_priority_while_cancel_is_unfinished", func(t *testing.T, h *sessionHarness) {
			mutation := &blockingTerminalMutation{
				Mutation:        h.mutation,
				terminalStarted: make(chan struct{}),
				release:         make(chan struct{}),
			}
			defer func() {
				select {
				case <-mutation.release:
				default:
					close(mutation.release)
				}
			}()
			h.start(t, mutation, true)
			first := h.submit(t, session.SessionInputModeNormal, "first")
			h.waitModelStarted(t)
			h.model.replies <- modelReply{hold: true}
			h.waitModelStreaming(t)
			turnID := h.nextTurnStarted(t).TurnID
			priorityOne := h.submit(t, session.SessionInputModeNormal, "priority one")
			priorityTwo := h.submit(t, session.SessionInputModeNormal, "priority two")
			ordinary := h.submit(t, session.SessionInputModeNormal, "ordinary")
			unread := h.submit(t, session.SessionInputModeSteer, "unread")
			if err := h.session.InterruptTurnWithInput(t.Context(), turnID, priorityOne.ID); err != nil {
				t.Fatal(err)
			}
			waitClosed(t, mutation.terminalStarted, "canceled Turn terminal save")
			if err := h.session.InterruptTurnWithInput(t.Context(), turnID, priorityTwo.ID); err != nil {
				t.Fatal(err)
			}
			if h.model.count.Load() != 1 {
				t.Fatal("new Turn started before the canceled Turn finished")
			}
			close(mutation.release)
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID}, defs.TurnStatusInterrupted)
			for range 3 {
				h.model.replies <- modelReply{text: "done"}
			}
			for _, want := range [][]defs.ItemID{{priorityTwo.ID}, {priorityOne.ID, unread.ID}, {ordinary.ID}} {
				requireFinished(t, h.nextTurnFinished(t), want, defs.TurnStatusCompleted)
			}
		}},
		{"priority_preserves_mixed_unread_groups", func(t *testing.T, h *sessionHarness) {
			loadSessionTestSkills(t, h)
			h.start(t, nil, true)
			first := h.submit(t, session.SessionInputModeNormal, "first")
			h.waitModelStarted(t)
			h.model.replies <- modelReply{hold: true}
			h.waitModelStreaming(t)
			turnID := h.nextTurnStarted(t).TurnID
			ordinary := h.submit(t, session.SessionInputModeNormal, "ordinary")
			priority := h.submit(t, session.SessionInputModeNormal, "/review priority")
			steer := h.submit(t, session.SessionInputModeSteer, "user steer")
			agentSteer := sessionInput("agent steer")
			agentSteer.Message.From = &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "child",
			}
			if _, err := h.session.HandleInput(t.Context(), &session.SessionInput{
				Mode: session.SessionInputModeSteer,
				Item: agentSteer,
			}); err != nil {
				t.Fatal(err)
			}
			agentQueued := h.submitAgentMessage(t, "agent queued")
			if err := h.session.SteerInput(t.Context(), agentQueued.ID, turnID); err == nil {
				t.Fatal("agent input was operable")
			}
			if err := h.session.InterruptTurnWithInput(t.Context(), "wrong-turn", priority.ID); err == nil {
				t.Fatal("wrong turn accepted")
			}
			if err := h.session.InterruptTurnWithInput(t.Context(), turnID, priority.ID); err != nil {
				t.Fatal(err)
			}
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{first.ID}, defs.TurnStatusInterrupted)
			for range 3 {
				h.model.replies <- modelReply{text: "done"}
			}
			finished := h.nextTurnFinished(t)
			requireFinished(t, finished, []defs.ItemID{priority.ID, steer.ID, agentSteer.ID}, defs.TurnStatusCompleted)
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{ordinary.ID}, defs.TurnStatusCompleted)
			requireFinished(t, h.nextTurnFinished(t), []defs.ItemID{agentQueued.ID}, defs.TurnStatusCompleted)
			for _, item := range readSessionLog(t, h.query, h.record.ConversationID, h.record.ID) {
				if item.SkillInstruction != nil && item.TurnID != finished.TurnID {
					t.Fatal("skill separated from priority input")
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, newSessionHarness(t)) })
	}
}

// waitPendingInputs 持续消费 Session 事件，直到待处理列表符合条件。
func (h *sessionHarness) waitPendingInputs(t *testing.T, matches func(session.SessionPendingInputs) bool) session.SessionPendingInputs {
	t.Helper()
	// 1. 读取新列表并检查条件。
	for {
		if event, ok := h.nextEvent(t).(*session.SessionPendingInputsUpdated); ok && matches(event.PendingInputs) {
			return event.PendingInputs
		}
	}
}

// blockingInputMutation 在指定输入保存前暂停。
type blockingInputMutation struct {
	dao.Mutation
	itemID  defs.ItemID
	started chan struct{}
	release chan struct{}
}

// AppendItems 暂停指定输入后交给真实 DAO 保存。
func (m *blockingInputMutation) AppendItems(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 指定输入出现时通知测试并等待放行。
	for _, item := range items {
		if item.ID == m.itemID {
			close(m.started)
			<-m.release
		}
	}
	// 2. 保存原输入。
	return m.Mutation.AppendItems(ctx, conversationID, sessionID, items...)
}
