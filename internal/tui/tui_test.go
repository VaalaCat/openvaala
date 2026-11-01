package tui

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/skill"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/charmbracelet/x/ansi"
)

// updateGolden 是否把本次画面写回 testdata 里的 golden 文件？用 go test ./internal/tui -update 打开。
var updateGolden = flag.Bool("update", false, "rewrite golden files in testdata")

// testExecutors 是单元测试里 serve 的 Executor 列表，按 executor/list 的顺序。
var testExecutors = []conversation.ExecutorDetails{
	{
		ID:                 "local",
		Description:        "Agent 本机当前目录",
		WorkspaceDirectory: "/srv/serve",
	},
	{
		ID:                 "openvaala",
		Description:        "内置内存虚拟环境",
		WorkspaceDirectory: "/workspace",
	},
	{
		ID:                 "gpu",
		Description:        "GPU 训练机",
		WorkspaceDirectory: "/data/proj",
	},
}

const (
	// tuiTestWorkspaceDirectory 是单元测试里 TUI 的 cwd，连上本机 serve 时是草稿设置的 local 目录。
	tuiTestWorkspaceDirectory = "/work/project"
	// tuiTestLocalServerID 是单元测试里 TUI 认作本机 serve 的 server_id。
	tuiTestLocalServerID = "local-server"
)

// TestNewTUI 检查界面构造参数和 Markdown 样式。
func TestNewTUI(t *testing.T) {
	// 1. 建立有效选项，逐项传入缺失参数和样式并核对错误。
	client := newFakeClient()
	valid := func() *TUIOpt {
		opt := NewTUIOpt(client, "assistant", tuiTestWorkspaceDirectory, strings.NewReader(""), io.Discard)
		opt.MarkdownStyle = markdownStyleNoTTY
		return opt
	}
	tests := []struct {
		name    string
		opt     func() *TUIOpt
		wantErr bool
	}{
		{
			name: "valid",
			opt:  valid,
		},
		{
			name: "auto_style",
			opt:  func() *TUIOpt { opt := valid(); opt.MarkdownStyle = markdownStyleAuto; return opt },
		},
		{
			name:    "nil_opt",
			opt:     func() *TUIOpt { return nil },
			wantErr: true,
		},
		{
			name:    "missing_client",
			opt:     func() *TUIOpt { opt := valid(); opt.Client = nil; return opt },
			wantErr: true,
		},
		{
			name:    "missing_agent",
			opt:     func() *TUIOpt { opt := valid(); opt.AgentName = ""; return opt },
			wantErr: true,
		},
		{
			name:    "unknown_style",
			opt:     func() *TUIOpt { opt := valid(); opt.MarkdownStyle = "nope"; return opt },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTUI(tt.opt())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestGetLiveLayout 检查不同终端高度下各区域的行数。
func TestGetLiveLayout(t *testing.T) {
	// 1. 列出输入框、空行、页脚和正文的高度组合并核对分配结果。
	tests := []struct {
		name   string
		height int
		wanted liveLayout
		want   liveLayout
	}{
		{
			name:   "height_1_only_composer",
			height: 1,
			wanted: liveLayout{
				body:     5,
				status:   1,
				composer: 3,
				footer:   1,
			},
			want: liveLayout{composer: 1},
		},
		{
			name:   "height_4_composer_and_footer",
			height: 4,
			wanted: liveLayout{
				body:     5,
				status:   1,
				queue:    2,
				composer: 3,
				footer:   1,
			},
			want: liveLayout{
				composer: 1,
				footer:   1,
			},
		},
		{
			name:   "enough_room",
			height: 40,
			wanted: liveLayout{
				body:     5,
				status:   1,
				queue:    2,
				popup:    3,
				composer: 2,
				footer:   1,
			},
			want: liveLayout{
				body:     5,
				status:   1,
				queue:    2,
				popup:    3,
				composer: 2,
				footer:   1,
			},
		},
		{
			name:   "composer_capped_at_10",
			height: 40,
			wanted: liveLayout{
				composer: 15,
				footer:   1,
			},
			want: liveLayout{
				composer: 10,
				footer:   1,
			},
		},
		{
			name:   "composer_capped_at_third",
			height: 12,
			wanted: liveLayout{
				composer: 9,
				footer:   1,
			},
			want: liveLayout{
				composer: 4,
				footer:   1,
			},
		},
		// 剩余 6 行：状态行 1、排队 2、弹窗 3，面板或尾部被压到 0。
		{
			name:   "squeeze_body_then_popup",
			height: 10,
			wanted: liveLayout{
				body:     20,
				status:   1,
				queue:    2,
				popup:    8,
				composer: 3,
				footer:   1,
			},
			want: liveLayout{
				status:   1,
				queue:    2,
				popup:    3,
				composer: 3,
				footer:   1,
			},
		},
		{
			name:   "empty_composer_takes_one_line",
			height: 24,
			wanted: liveLayout{
				composer: 0,
				footer:   1,
			},
			want: liveLayout{
				composer: 1,
				footer:   1,
			},
		},
		// 输入块上下的空行计入输入框的预算，先于其他部分分配；内容行数的上限不变。
		{
			name:   "composer_padding_counts_in_budget",
			height: 10,
			wanted: liveLayout{
				body:            20,
				status:          1,
				queue:           2,
				popup:           8,
				composer:        3,
				composerPadding: 1,
				footer:          1,
			},
			want: liveLayout{
				status:          1,
				queue:           2,
				popup:           1,
				composer:        3,
				composerPadding: 1,
				footer:          1,
			},
		},
		{
			name:   "composer_padding_beside_capped_content",
			height: 12,
			wanted: liveLayout{
				composer:        9,
				composerPadding: 1,
				footer:          1,
			},
			want: liveLayout{
				composer:        4,
				composerPadding: 1,
				footer:          1,
			},
		},
		{
			name:   "composer_padding_kept_at_height_5",
			height: 5,
			wanted: liveLayout{
				body:            3,
				composer:        2,
				composerPadding: 1,
				footer:          1,
			},
			want: liveLayout{
				body:            1,
				composer:        1,
				composerPadding: 1,
				footer:          1,
			},
		},
		{
			name:   "composer_padding_dropped_below_height_5",
			height: 4,
			wanted: liveLayout{
				composer:        2,
				composerPadding: 1,
				footer:          1,
			},
			want: liveLayout{
				composer: 1,
				footer:   1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getLiveLayout(tt.height, tt.wanted); got != tt.want {
				t.Fatalf("layout = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestComposerBlock 检查输入块背景、上下空行和光标位置。
func TestComposerBlock(t *testing.T) {
	// 1. 按终端高度和背景色绘制输入框，核对内容行与页脚。
	tinted := lipgloss.Color("#1e1e1e")
	tests := []struct {
		name       string
		height     int
		background color.Color
		// wantBlock 输入框上下是否各有一行铺满宽度的 surface 空行？
		wantBlock bool
	}{
		{
			name:       "background_adds_block",
			height:     24,
			background: tinted,
			wantBlock:  true,
		},
		{
			name:       "background_block_at_height_5",
			height:     5,
			background: tinted,
			wantBlock:  true,
		},
		{
			name:       "background_no_block_below_height_5",
			height:     4,
			background: tinted,
		},
		{
			name:   "no_background_no_block",
			height: 24,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := NewTUIOpt(newFakeClient(), "assistant", tuiTestWorkspaceDirectory, strings.NewReader(""), io.Discard)
			opt.MarkdownStyle = "dark"
			m := newTUIModel(t.Context(), opt, tt.background)
			m.update(tea.WindowSizeMsg{
				Width:  40,
				Height: tt.height,
			})
			m.composer.focus()
			m.composer.setValue("你好")
			view := m.View()
			lines := strings.Split(view.Content, "\n")
			composerRow := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(ansi.Strip(line), "› 你好") })
			footer := lines[len(lines)-1]
			if composerRow < 0 || !strings.Contains(ansi.Strip(footer), "默认模型") || strings.Contains(footer, "48;") {
				t.Fatalf("view = %q", lines)
			}
			// 光标在输入框的第一行，随输入块的上方空行一起下移。
			if view.Cursor == nil || view.Cursor.Y != composerRow {
				t.Fatalf("cursor = %+v, composer row %d in %q", view.Cursor, composerRow, lines)
			}
			if !tt.wantBlock {
				if composerRow != len(lines)-2 {
					t.Fatalf("composer row %d, want directly above the footer in %q", composerRow, lines)
				}
				return
			}
			if composerRow != len(lines)-3 || composerRow < 1 {
				t.Fatalf("composer row %d, want a padding row above and below in %q", composerRow, lines)
			}
			for _, padding := range []string{lines[composerRow-1], lines[composerRow+1]} {
				if ansi.Strip(padding) != strings.Repeat(" ", 40) {
					t.Fatalf("padding row = %q", padding)
				}
				requireBackgroundFilled(t, padding, m.styles.surface)
			}
			if len(lines) > tt.height {
				t.Fatalf("view has %d lines, terminal height %d", len(lines), tt.height)
			}
		})
	}
}

// TestTUIModel 检查界面输入、对话事件和实时内容。
func TestTUIModel(t *testing.T) {
	// 1. 逐场景驱动界面，核对请求顺序、草稿、状态和显示内容。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "launch_is_draft_without_calls",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 等待启动 Skills 请求，核对未创建对话的草稿画面。
				// 启动时只拉草稿 Skills（不记进 calls）；模型目录等连上之后再拉。
				h.waitFor("startup skills requested", func() bool { return len(h.client.getSkillRequests()) == 1 })
				if calls := h.client.getCalls(""); len(calls) != 0 {
					t.Fatalf("calls after launch = %v", calls)
				}
				requireGolden(t, "draft_80x24", h.view())
			},
		},
		{
			name: "first_enter_creates_opens_submits_without_local_title",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 发送首条输入，核对创建、打开、提交顺序以及服务端更新前的空标题。
				h.typeText("你好")
				h.press("enter")
				h.waitFor("first input submitted", func() bool { return len(h.client.getCalls("SubmitInput")) == 1 })
				// SubmitInput 的结果消息还要由 harness 处理后，草稿打开流程才结束。
				h.waitFor("draft opening finished", func() bool { return !h.model.isOpeningDraft })
				want := []string{"CreateConversation assistant", "OpenConversation c1", "SubmitInput c1 normal 你好"}
				if calls := h.client.getCalls("CreateConversation", "OpenConversation", "SubmitInput"); !slices.Equal(calls, want) {
					t.Fatalf("calls = %v, want %v", calls, want)
				}
				if h.model.conversation.Title != "" {
					t.Fatalf("title = %q, want the Runtime to supply it", h.model.conversation.Title)
				}
				if printed := h.getPrinted(); !strings.Contains(printed, "── 新对话 · c1") {
					t.Fatalf("printed = %q, want the untitled header before the Runtime update", printed)
				}
				if h.model.isOpeningDraft || h.model.composer.getValue() != "" {
					t.Fatalf("opening = %v, composer = %q", h.model.isOpeningDraft, h.model.composer.getValue())
				}
			},
		},
		{
			// 连上本机 serve 后，新建和草稿 Skills 带 local 目录（启动时那次还不知道连上的是谁，连上后重新拉取），
			// 打开不带目录，已打开对话的 Skills 按对话查。
			name: "local_server_create_and_draft_skills_carry_directory",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 连接本机服务并发送首条输入，检查创建和草稿 Skills 带上 local 目录。
				h.waitFor("startup skills requested", func() bool { return len(h.client.getSkillRequests()) == 1 })
				h.emit(&runtimeclient.ConnectionChanged{
					State:    runtimeclient.ConnectionConnected,
					ServerID: tuiTestLocalServerID,
				})
				h.waitFor("draft skills requested again", func() bool { return len(h.client.getSkillRequests()) == 2 })
				h.typeText("你好")
				h.press("enter")
				var created []runtimeclient.CreateConversationRequest
				var opened []runtimeclient.OpenConversationRequest
				h.waitFor("draft opened and skills listed", func() bool {
					h.client.set(func(c *fakeClient) { created, opened = c.createRequests, c.openRequests })
					return len(h.client.getCalls("SubmitInput")) == 1 && !h.model.isOpeningDraft && len(h.client.getSkillRequests()) == 3
				})
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: tuiTestWorkspaceDirectory}}
				wantCreated := []runtimeclient.CreateConversationRequest{{
					AgentName:        "assistant",
					ExecutorSettings: settings,
				}}
				wantOpened := []runtimeclient.OpenConversationRequest{{ConversationID: "c1"}}
				wantSkills := []runtimeclient.GetSkillsRequest{
					{AgentName: "assistant"},
					{
						AgentName:        "assistant",
						ExecutorSettings: settings,
					},
					{
						ConversationID: "c1",
						AgentName:      "assistant",
					},
				}
				if skills := h.client.getSkillRequests(); !reflect.DeepEqual(created, wantCreated) || !reflect.DeepEqual(opened, wantOpened) || !reflect.DeepEqual(skills, wantSkills) {
					t.Fatalf("created = %+v, opened = %+v, skills = %+v", created, opened, skills)
				}
			},
		},
		{
			// 连上的不是本机 serve：新建和草稿 Skills 都不带目录，连上后草稿 Skills 不重新拉取。
			name: "remote_server_create_and_draft_skills_carry_no_directory",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 连接远端服务并发送输入，检查创建和草稿 Skills 都没有目录。
				h.waitFor("startup skills requested", func() bool { return len(h.client.getSkillRequests()) == 1 })
				h.emit(&runtimeclient.ConnectionChanged{
					State:    runtimeclient.ConnectionConnected,
					ServerID: "other-server",
				})
				h.typeText("你好")
				h.press("enter")
				var created []runtimeclient.CreateConversationRequest
				h.waitFor("draft opened and skills listed", func() bool {
					h.client.set(func(c *fakeClient) { created = c.createRequests })
					return len(h.client.getCalls("SubmitInput")) == 1 && !h.model.isOpeningDraft && len(h.client.getSkillRequests()) == 2
				})
				wantCreated := []runtimeclient.CreateConversationRequest{{AgentName: "assistant"}}
				wantSkills := []runtimeclient.GetSkillsRequest{{AgentName: "assistant"}, {
					ConversationID: "c1",
					AgentName:      "assistant",
				}}
				if skills := h.client.getSkillRequests(); !reflect.DeepEqual(created, wantCreated) || !reflect.DeepEqual(skills, wantSkills) {
					t.Fatalf("created = %+v, skills = %+v", created, skills)
				}
			},
		},
		{
			name: "enter_during_creation_queues_in_order",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 暂停对话创建，连续提交两条输入，再放行并核对顺序。
				gate := make(chan struct{})
				h.client.set(func(c *fakeClient) { c.createGate = gate })
				h.typeText("一")
				h.press("enter")
				h.typeText("二")
				h.press("enter")
				close(gate)
				h.waitFor("both submitted", func() bool { return len(h.client.getCalls("SubmitInput")) == 2 })
				want := []string{"SubmitInput c1 normal 一", "SubmitInput c1 normal 二"}
				if calls := h.client.getCalls("SubmitInput"); !slices.Equal(calls, want) {
					t.Fatalf("submits = %v, want %v", calls, want)
				}
			},
		},
		{
			name: "create_failure_restores_all_pending",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 暂停创建并预设失败，核对两条待发送输入按顺序恢复。
				gate := make(chan struct{})
				h.client.set(func(c *fakeClient) { c.createGate, c.createErr = gate, errors.New("boom") })
				h.typeText("一")
				h.press("enter")
				h.typeText("二")
				h.press("enter")
				close(gate)
				h.waitFor("draft restored", func() bool { return !h.model.isOpeningDraft })
				if value := h.model.composer.getValue(); value != "一\n二" {
					t.Fatalf("composer = %q, want both inputs in order", value)
				}
				if !strings.Contains(h.model.notice.text, "新建对话失败") || !h.model.notice.isError {
					t.Fatalf("notice = %+v", h.model.notice)
				}
				if calls := h.client.getCalls("OpenConversation", "SubmitInput"); len(calls) != 0 {
					t.Fatalf("calls after failed create = %v", calls)
				}
			},
		},
		{
			// 打开对话失败后不再发出首条输入。
			name: "open_failure_does_not_submit",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 预设打开失败，核对没有发送请求且草稿恢复。
				h.client.set(func(c *fakeClient) { c.openErr = errors.New("unreachable") })
				h.typeText("你好")
				h.press("enter")
				h.waitFor("draft restored", func() bool { return !h.model.isOpeningDraft })
				if calls := h.client.getCalls("SubmitInput"); len(calls) != 0 {
					t.Fatalf("submitted after failed open: %v", calls)
				}
				if h.model.conversation != nil || h.model.composer.getValue() != "你好" {
					t.Fatalf("conversation = %v, composer = %q", h.model.conversation, h.model.composer.getValue())
				}
			},
		},
		{
			// 发送失败时原文回到输入框，接在期间输入的新内容之前。
			name: "submit_failure_restores_before_new_text",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 暂停发送并输入新文字，放行失败后核对原文排在新文字之前。
				h.openDraftConversation("开场")
				gate := make(chan struct{})
				h.client.set(func(c *fakeClient) { c.submitGate, c.submitErr = gate, errors.New("lost") })
				h.typeText("原文")
				h.press("enter")
				h.typeText("新内容")
				close(gate)
				h.waitFor("draft restored", func() bool { return strings.HasPrefix(h.model.composer.getValue(), "原文") })
				if value := h.model.composer.getValue(); value != "原文\n新内容" {
					t.Fatalf("composer = %q", value)
				}
				if !strings.Contains(h.model.notice.text, "发送失败") {
					t.Fatalf("notice = %+v", h.model.notice)
				}
			},
		},
		{
			// 原始多行输入交给 Runtime，TUI 不再自行提取标题。
			name: "multiline_first_input_keeps_original_content",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 粘贴带空白的多行输入，核对提交文字原样保留。
				h.send(tea.PasteMsg{Content: "\n  第一行   标题\n第二行"})
				h.press("enter")
				h.waitFor("draft opening finished", func() bool { return !h.model.isOpeningDraft })
				if calls := h.client.getCalls("SubmitInput"); !slices.Equal(calls, []string{"SubmitInput c1 normal \n  第一行   标题\n第二行"}) {
					t.Fatalf("submits = %v, want the original text", calls)
				}
			},
		},
		{
			// 草稿还在新建对话时，别的提交（例如重连期间阻塞的旧对话输入）成功返回，不能推进待发送队列。
			name: "unrelated_submit_success_does_not_advance_draft_queue",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 暂停草稿创建，交付旧对话发送成功的结果，再放行并核对草稿队列。
				release := h.holdDraftCreation("一")
				h.send(inputSubmittedMsg{
					conversationID: "old",
					text:           "别处",
				})
				if !h.model.isOpeningDraft {
					t.Fatal("unrelated result ended draft opening")
				}
				release()
				h.waitFor("draft flow finished", func() bool { return !h.model.isOpeningDraft })
				if calls := h.client.getCalls("SubmitInput"); !slices.Equal(calls, []string{"SubmitInput c1 normal 一"}) {
					t.Fatalf("submits = %v", calls)
				}
			},
		},
		{
			// 别的提交失败时，原文回到输入框，但草稿打开流程不终止，排队的输入照常发出。
			name: "unrelated_submit_failure_does_not_abort_draft_queue",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 暂停草稿创建，交付旧对话发送失败的结果，核对旧输入恢复且草稿照常发送。
				release := h.holdDraftCreation("一")
				h.send(inputSubmittedMsg{
					conversationID: "old",
					text:           "别处",
					err:            errors.New("lost"),
				})
				if !h.model.isOpeningDraft {
					t.Fatal("unrelated result ended draft opening")
				}
				if value := h.model.composer.getValue(); value != "别处" {
					t.Fatalf("composer = %q, want the unrelated input restored", value)
				}
				if !strings.Contains(h.model.notice.text, "发送失败") || !h.model.notice.isError {
					t.Fatalf("notice = %+v", h.model.notice)
				}
				release()
				h.waitFor("draft flow finished", func() bool { return !h.model.isOpeningDraft })
				if calls := h.client.getCalls("SubmitInput"); !slices.Equal(calls, []string{"SubmitInput c1 normal 一"}) {
					t.Fatalf("submits = %v", calls)
				}
				if value := h.model.composer.getValue(); value != "别处" {
					t.Fatalf("composer after flow = %q", value)
				}
			},
		},
		{
			name: "ctrl_c_clears_then_confirms_quit",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 依次检查清空输入框、退出提示、确认超时和窗口内再次确认。
				h.typeText("abc")
				h.press("ctrl+c")
				if h.model.composer.getValue() != "" || h.model.isQuitting {
					t.Fatalf("first ctrl+c: composer = %q, quitting = %v", h.model.composer.getValue(), h.model.isQuitting)
				}
				h.press("ctrl+c")
				if h.model.notice.text != "再按 Ctrl+C 退出" || h.model.isQuitting {
					t.Fatalf("second ctrl+c: notice = %q, quitting = %v", h.model.notice.text, h.model.isQuitting)
				}
				h.now = h.now.Add(2 * time.Second)
				h.press("ctrl+c")
				if h.model.isQuitting {
					t.Fatal("ctrl+c after the confirm window quit")
				}
				h.now = h.now.Add(500 * time.Millisecond)
				h.press("ctrl+c")
				if !h.model.isQuitting || h.view() != "" {
					t.Fatalf("confirmed ctrl+c: quitting = %v, view = %q", h.model.isQuitting, h.view())
				}
			},
		},
		{
			name: "ctrl_d_quits_only_when_empty",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 输入框有内容时按 Ctrl+D，再清空后按 Ctrl+D，核对只在空输入时退出。
				h.typeText("a")
				h.press("ctrl+d")
				if h.model.isQuitting {
					t.Fatal("ctrl+d quit with a draft")
				}
				h.press("backspace", "ctrl+d")
				if !h.model.isQuitting {
					t.Fatal("ctrl+d did not quit with an empty composer")
				}
			},
		},
		{
			name: "notice_cleared_by_next_key",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 触发退出确认提示，再输入字符并检查提示清除。
				h.press("ctrl+c")
				if h.model.notice.text == "" {
					t.Fatal("no confirm hint")
				}
				h.typeText("x")
				if h.model.notice.text != "" {
					t.Fatalf("notice after key = %q", h.model.notice.text)
				}
			},
		},
		{
			name: "enter_while_running_steers",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开对话并发出首条输入。
				h.openDraftConversation("开场")
				// 2. 保存首条输入移除它的排队记录，再让根 Session 开始运行。
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 1,
						Item:    &defs.Item{ID: "input-1"},
					}},
				})
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				// 3. 按回车提交补充输入，核对插话模式和空的排队列表。
				h.typeText("补充")
				h.press("enter")
				h.waitFor("steer submitted", func() bool { return h.model.notice.text != "" })
				if calls := h.client.getCalls("SubmitInput"); calls[len(calls)-1] != "SubmitInput c1 steer 补充" {
					t.Fatalf("submits = %v", calls)
				}
				if h.model.notice.text != "已插入，将在下一步读入" || len(h.model.queuedInputs.entries) != 0 {
					t.Fatalf("notice = %q, queued = %v", h.model.notice.text, h.model.queuedInputs.entries)
				}
			},
		},
		{
			name: "tab_while_running_queues_until_saved",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 运行中按 Tab 排队，核对提交模式、预览和保存后移除。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 1,
						Item:    &defs.Item{ID: "input-1"},
					}},
				})
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.typeText("接下来检查 race")
				h.press("tab")
				h.waitFor("queued", func() bool { return len(h.model.queuedInputs.entries) == 1 })
				if calls := h.client.getCalls("SubmitInput"); calls[len(calls)-1] != "SubmitInput c1 normal 接下来检查 race" {
					t.Fatalf("submits = %v", calls)
				}
				requireGolden(t, "running_queue_80x24", h.view())
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 2,
						Item:    &defs.Item{ID: "input-2"},
					}},
				})
				if len(h.model.queuedInputs.entries) != 0 {
					t.Fatalf("queue after saved = %v", h.model.queuedInputs.entries)
				}
			},
		},
		{
			// UserPromptSubmit 拦下的输入不保存，没有 items/saved；Turn 结束时列出它的 ID，排队预览随之消失。
			name: "turn_finished_removes_blocked_input_from_queue",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 交付含被拦输入 ID 的 Turn 结束事件，核对排队预览清除。
				h.openDraftConversation("开场")
				h.waitFor("queued", func() bool { return len(h.model.queuedInputs.entries) == 1 })
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.emit(&runtimeclient.TurnFinished{
					ConversationID: "c1",
					TurnID:         "t1",
					Status:         defs.TurnStatusCompleted,
					InputItemIDs:   []defs.ItemID{"input-1"},
				})
				if len(h.model.queuedInputs.entries) != 0 || strings.Contains(h.view(), "排队：") {
					t.Fatalf("queue after turn finished = %v, view:\n%s", h.model.queuedInputs.entries, h.view())
				}
			},
		},
		{
			name: "tab_while_idle_does_nothing",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 空闲时输入草稿并按 Tab，核对草稿未提交。
				h.openDraftConversation("开场")
				h.typeText("草稿")
				h.press("tab")
				if h.model.composer.getValue() != "草稿" || len(h.client.getCalls("SubmitInput")) != 1 {
					t.Fatalf("composer = %q, submits = %v", h.model.composer.getValue(), h.client.getCalls("SubmitInput"))
				}
			},
		},
		{
			name: "esc_twice_interrupts",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 启动根 Turn，检查首次 Esc 提示、第二次 Esc 发出中断。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.press("esc")
				if h.model.notice.text != "再按 Esc 中断" || len(h.client.getCalls("InterruptTurn")) != 0 {
					t.Fatalf("first esc: notice = %q", h.model.notice.text)
				}
				h.press("esc")
				h.waitFor("interrupt", func() bool { return len(h.client.getCalls("InterruptTurn")) == 1 })
				if calls := h.client.getCalls("InterruptTurn"); !slices.Equal(calls, []string{"InterruptTurn c1"}) {
					t.Fatalf("interrupts = %v", calls)
				}
			},
		},
		{
			name: "ctrl_c_while_running_interrupts_immediately",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 启动根 Turn 并按 Ctrl+C，核对立即中断且不退出。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.press("ctrl+c")
				h.waitFor("interrupt", func() bool { return len(h.client.getCalls("InterruptTurn")) == 1 })
				if calls := h.client.getCalls("InterruptTurn"); !slices.Equal(calls, []string{"InterruptTurn c1"}) {
					t.Fatalf("interrupts = %v", calls)
				}
				if h.model.isQuitting {
					t.Fatal("ctrl+c while running quit")
				}
			},
		},
		{
			name: "interrupt_failure_shows_error",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 预设中断失败，运行时按 Ctrl+C 并检查错误提示。
				h.openDraftConversation("开场")
				h.client.set(func(c *fakeClient) { c.interruptErr = errors.New("no active turn") })
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.press("ctrl+c")
				h.waitFor("error notice", func() bool { return h.model.notice.isError })
				if !strings.Contains(h.model.notice.text, "中断失败") {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			// 空闲时 Esc 不中断：草稿里什么都不做，打开对话后是回退的确认提示。
			name: "esc_idle_never_interrupts",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 分别在草稿和空闲对话按 Esc，核对没有中断请求。
				h.press("esc")
				if h.model.notice.text != "" {
					t.Fatalf("draft esc: notice = %q", h.model.notice.text)
				}
				h.openDraftConversation("开场")
				h.press("esc")
				if h.model.notice.text != "再按 Esc 回退" || len(h.client.getCalls("InterruptTurn")) != 0 {
					t.Fatalf("idle esc: notice = %q, calls = %v", h.model.notice.text, h.client.getCalls("InterruptTurn"))
				}
			},
		},
		{
			// 空闲时的第一下 Esc 是回退确认；1 秒内根 Session 自动开始运行时，第二下 Esc 只是中断的第一下确认，不会中断。
			name: "esc_rewind_confirmation_does_not_count_for_interrupt",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 先触发回退确认，再启动根 Turn，检查中断需重新连按两次 Esc。
				h.openDraftConversation("开场")
				h.press("esc")
				if h.model.notice.text != "再按 Esc 回退" {
					t.Fatalf("first esc: notice = %q", h.model.notice.text)
				}
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.press("esc")
				if h.model.notice.text != "再按 Esc 中断" || len(h.client.getCalls("InterruptTurn")) != 0 {
					t.Fatalf("second esc: notice = %q, calls = %v", h.model.notice.text, h.client.getCalls("InterruptTurn"))
				}
				h.press("esc")
				h.waitFor("interrupt", func() bool { return len(h.client.getCalls("InterruptTurn")) == 1 })
			},
		},
		{
			// 已经同步过的对话再次同步（serve 让对话原地换上了新配置、关闭了订阅，例如别的客户端 reload 了配置）时重新拉取模型目录，footer 随之更新；
			// 打开时的第一次同步不拉。
			name: "resync_refetches_model_list",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 首次同步后更换模型预设，再同步并检查只在第二次拉取模型目录。
				h.openDraftConversation("开场")
				synced := &runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   *h.model.conversation,
					SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo("")},
				}
				h.emit(synced)
				reloaded := defs.ModelRef{
					ProviderID: "gw",
					ModelID:    "new",
				}
				h.client.set(func(c *fakeClient) {
					c.modelList = runtime.ModelList{
						Models: []*defs.ModelDefinition{{
							Ref:         reloaded,
							DisplayName: "New Model",
						}},
						AgentDefaultModel: reloaded,
					}
				})
				h.emit(synced)
				h.waitFor("model list refetched", func() bool { return h.model.modelDisplayNames[reloaded] == "New Model" })
				if calls := h.client.getCalls("ListModels"); len(calls) != 1 {
					t.Fatalf("model list calls = %v, want one after the resync only", calls)
				}
			},
		},
		{
			name: "events_of_other_conversation_ignored",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 交付其他对话的运行状态，核对当前界面未开始运行。
				h.openDraftConversation("开场")
				other := newRootInfo("t1")
				other.Session.ConversationID = "c9"
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c9",
					Info:           other,
				})
				if h.model.sessionStatuses.isAnyRunning() {
					t.Fatal("status of another conversation applied")
				}
			},
		},
		{
			name: "reconnecting_shows_status_line",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 交付带次数、等待时间和错误的重连事件，检查状态行与页脚。
				h.emit(&runtimeclient.ConnectionChanged{
					State:   runtimeclient.ConnectionReconnecting,
					Attempt: 2,
					RetryIn: 2 * time.Second,
					Err:     errors.New("EOF"),
				})
				if view := h.view(); !strings.Contains(view, "⚠ 重连中（第 2 次，2s 后重试：EOF）") || !strings.Contains(view, "重连中 · 默认模型") {
					t.Fatalf("view = %q", view)
				}
			},
		},
		{
			name: "running_status_line_shows_elapsed_only_when_start_is_known",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 先检查已知起点的运行耗时，再同步无起点的运行信息并检查不显示耗时。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.now = h.now.Add(12 * time.Second)
				if view := h.view(); !strings.Contains(view, "◐ 生成中 12s · esc esc 中断") {
					t.Fatalf("view = %q", view)
				}
				// 重连后的同步里没有开始时间，只写动作不写耗时。
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo("t2")},
				})
				if view := h.view(); !strings.Contains(view, "◐ 生成中 · esc esc 中断") {
					t.Fatalf("view after sync = %q", view)
				}
			},
		},
		{
			name: "spinner_ticks_only_while_a_session_runs",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 配置两帧转动图标，检查空闲、运行、停止和再次运行时的帧变化。
				// 两帧、1ms 一帧：能看出重新开始后是否真的在推进。
				h.model.spinner = spinner.New(spinner.WithSpinner(spinner.Spinner{
					Frames: []string{"a", "b"},
					FPS:    time.Millisecond,
				}))
				h.openDraftConversation("开场")
				if h.model.isSpinnerTicking {
					t.Fatal("spinner ticking in an idle conversation")
				}
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				if !h.model.isSpinnerTicking {
					t.Fatal("spinner not ticking while the root session runs")
				}
				// 上一帧还在路上：空闲后它到达时停下，不再安排下一帧。
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo(""),
				})
				h.waitFor("spinner stopped", func() bool { return !h.model.isSpinnerTicking })
				if frame := h.model.spinner.View(); frame != "a" {
					t.Fatalf("spinner advanced while idle: %q", frame)
				}
				// 再次开始运行：重新转动，并真的推进到下一帧。
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t2"),
				})
				if !h.model.isSpinnerTicking {
					t.Fatal("spinner did not restart when a new turn began")
				}
				h.waitFor("spinner advanced", func() bool { return h.model.spinner.View() == "b" })
			},
		},
		{
			name: "tab_while_running_ignores_blank_composer",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 运行中输入空格并按 Tab。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.typeText("  ")
				h.press("tab")
				// 2. 核对输入框保留空格且没有新增提交。
				if h.model.composer.getValue() != "  " || len(h.client.getCalls("SubmitInput")) != 1 {
					t.Fatalf("composer = %q, submits = %v", h.model.composer.getValue(), h.client.getCalls("SubmitInput"))
				}
			},
		},
		{
			name: "reply_prints_through_model_in_order",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 依次交付运行、开轮、保存输入和回复增量，检查实时尾部与定稿内容。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.emit(&runtimeclient.TurnStarted{
					ConversationID: "c1",
					TurnID:         "t1",
				})
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					TurnID:         "t1",
					Items:          logItems(userItem("input-1", "开场")),
				})
				started := itemStarted(0, replyItem("r1", ""))
				started.ConversationID = "c1"
				delta := itemDelta(0, "第一段\n\n正在写第二段")
				delta.ConversationID = "c1"
				h.emit(started, delta)
				requireGolden(t, "streaming_80x24", h.view())
				if printed := h.getPrinted(); !strings.Contains(printed, "› 开场") || strings.Contains(printed, "第一段") {
					t.Fatalf("printed = %q", printed)
				}
			},
		},
		{
			// 输入保存事件先于提交结果到达时，不再加入排队预览。
			name: "queued_input_skipped_when_already_saved",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 先交付输入保存，再交付提交结果，检查没有加入排队预览。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					TurnID:         "t2",
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 5,
						Item:    userItem("input-9", "下一条"),
					}},
				})
				h.send(inputSubmittedMsg{
					conversationID: "c1",
					text:           "下一条",
					mode:           session.SessionInputModeNormal,
					itemID:         "input-9",
				})
				if slices.ContainsFunc(h.model.queuedInputs.entries, func(entry queuedInput) bool { return entry.itemID == "input-9" }) {
					t.Fatalf("queued = %v", h.model.queuedInputs.entries)
				}
			},
		},
		{
			name: "reasoning_shows_thinking_verb_until_completed",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 交付推理开始与完成事件，核对状态文字从思考中变回生成中。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				reasoning := &defs.Item{
					ID:          "x1",
					TurnID:      "t1",
					ModelCallID: "m1",
					Kind:        defs.ItemReasoning,
					Reasoning:   &defs.Reasoning{},
				}
				started, completed := itemStarted(0, reasoning), itemCompleted(0, reasoning)
				started.ConversationID, completed.ConversationID = "c1", "c1"
				h.emit(started)
				if view := h.view(); !strings.Contains(view, "◐ 思考中 0s · esc esc 中断") {
					t.Fatalf("view while reasoning = %q", view)
				}
				h.emit(completed)
				if view := h.view(); !strings.Contains(view, "◐ 生成中 0s · esc esc 中断") {
					t.Fatalf("view after reasoning = %q", view)
				}
			},
		},
		{
			// 还没有结果的调用参数没写 executor 时，工具树按当前对话的默认 Executor 显示。
			name: "running_tool_shows_default_executor",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 同步默认 gpu 的对话并启动无 executor 参数的工具，核对工具树显示 gpu。
				h.openDraftConversation("开场")
				conversation := *h.model.conversation
				conversation.ExecutorSettings.DefaultExecutorID = "gpu"
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   conversation,
					SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo("t1")},
				})
				started := itemStarted(0, toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`))
				started.ConversationID = "c1"
				h.emit(started)
				if live := ansi.Strip(strings.Join(h.model.transcript.getLiveLines("◐"), "\n")); !strings.Contains(live, "bash@gpu 1 次") {
					t.Fatalf("live tail = %q, want the running bash call under bash@gpu", live)
				}
			},
		},
		{
			// 宽度变化后实时尾部按新宽度重排。
			name: "resize_rerenders_live_tail",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 写入长回复增量，再缩窄到四十列并检查每行宽度。
				h.openDraftConversation("开场")
				started := itemStarted(0, replyItem("r1", ""))
				delta := itemDelta(0, strings.Repeat("很长的句子", 10))
				started.ConversationID, delta.ConversationID = "c1", "c1"
				h.emit(started, delta)
				if !slices.ContainsFunc(h.model.transcript.getLiveLines("◐"), func(line string) bool { return ansi.StringWidth(line) > 40 }) {
					t.Fatalf("live tail at width 80 = %q", h.model.transcript.getLiveLines("◐"))
				}
				h.send(tea.WindowSizeMsg{
					Width:  40,
					Height: 24,
				})
				for _, line := range h.model.transcript.getLiveLines("◐") {
					if width := ansi.StringWidth(line); width > 40 {
						t.Fatalf("live line %q width %d after resize to 40", line, width)
					}
				}
			},
		},
		{
			// 重连同步时，写过段落但既没保存也不在同步结果里的回复补"未保存"标记，并清掉实时尾部。
			name: "session_sync_marks_unsaved_reply_through_model",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 写出部分回复后同步没有生成中 Item 的结果，核对补上未保存标记并清空实时尾部。
				h.openDraftConversation("开场")
				started := itemStarted(0, replyItem("r1", ""))
				delta := itemDelta(0, "第一段\n\n第二\n")
				started.ConversationID, delta.ConversationID = "c1", "c1"
				h.emit(started, delta)
				if printed := h.getPrinted(); !strings.Contains(printed, "● 第一段") || strings.Contains(printed, "已中断") {
					t.Fatalf("printed before sync = %q", printed)
				}
				h.emit(&runtimeclient.SessionSynced{ConversationID: "c1"})
				if printed := h.getPrinted(); !strings.Contains(printed, "已中断 · 以上内容未保存") {
					t.Fatalf("printed after sync = %q", printed)
				}
				if live := h.model.transcript.getLiveLines("◐"); len(live) != 0 {
					t.Fatalf("live tail after sync = %q", live)
				}
			},
		},
		{
			// 面板打开时按键跳过输入框：草稿保持不动，Ctrl+D 在有草稿时不退出，Esc 关闭面板后草稿仍在。Ctrl+C 见下一个用例。
			name: "panel_open_keeps_draft_untouched",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开面板后发送文字、粘贴与按键，检查草稿保留且没有新建或提交。
				h.typeText("草稿")
				h.model.activePanel = &helpPanel{}
				h.typeText("x")
				h.send(tea.PasteMsg{Content: "粘贴"})
				h.press("ctrl+d", "enter")
				// 只看面板按键不该触发的新建与提交。
				if value := h.model.composer.getValue(); value != "草稿" || h.model.isQuitting || len(h.client.getCalls("CreateConversation", "SubmitInput")) != 0 {
					t.Fatalf("composer = %q, quitting = %v, calls = %v", value, h.model.isQuitting, h.client.getCalls(""))
				}
				h.press("esc")
				if h.model.activePanel != nil || h.model.composer.getValue() != "草稿" {
					t.Fatalf("panel = %v, composer = %q", h.model.activePanel, h.model.composer.getValue())
				}
			},
		},
		{
			// 面板打开且 Turn 运行时，Ctrl+C 先清空草稿，再按才中断；面板保持打开。
			name: "ctrl_c_with_panel_and_draft_while_running_clears_draft_first",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 运行中打开面板并填入草稿，检查 Ctrl+C 先清空、再中断且面板保持打开。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.press("?")
				h.model.composer.setValue("草稿")
				if h.model.activePanel == nil {
					t.Fatal("? did not open the help panel")
				}
				h.press("ctrl+c")
				if value := h.model.composer.getValue(); value != "" || h.model.activePanel == nil || len(h.client.getCalls("InterruptTurn")) != 0 {
					t.Fatalf("composer = %q, panel = %v, interrupts = %v", value, h.model.activePanel, h.client.getCalls("InterruptTurn"))
				}
				h.press("ctrl+c")
				h.waitFor("interrupt", func() bool { return len(h.client.getCalls("InterruptTurn")) == 1 })
				if h.model.activePanel == nil || h.model.isQuitting {
					t.Fatalf("panel = %v, quitting = %v after interrupt", h.model.activePanel, h.model.isQuitting)
				}
			},
		},
		{
			// 空闲且面板打开时，Ctrl+C 清空草稿，不显示退出提示。
			name: "ctrl_c_with_panel_and_draft_while_idle_clears_draft",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 空闲时打开面板并按 Ctrl+C，检查只清空草稿而不提示退出。
				h.typeText("草稿")
				h.model.activePanel = &helpPanel{}
				h.press("ctrl+c")
				if value := h.model.composer.getValue(); value != "" || h.model.activePanel == nil || h.model.notice.text != "" {
					t.Fatalf("composer = %q, panel = %v, notice = %q", value, h.model.activePanel, h.model.notice.text)
				}
			},
		},
		{
			// 面板打开且输入框为空时，Ctrl+D 仍然退出。
			name: "panel_open_ctrl_d_quits_when_composer_empty",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 空输入打开面板并按 Ctrl+D，检查退出状态。
				h.press("?", "ctrl+d")
				if !h.model.isQuitting {
					t.Fatal("ctrl+d did not quit with a panel open and an empty composer")
				}
			},
		},
		{
			name: "running_step_group_view",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开对话，让根 Session 开始运行 t1。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				},
					&runtimeclient.TurnStarted{
						ConversationID: "c1",
						TurnID:         "t1",
					})
				// 2. 保存首条输入并清除它的排队记录。
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					TurnID:         "t1",
					Items:          logItems(userItem("input-1", "开场")),
				})
				// 3. 交付一段已完成的推理、一个带进度的运行中调用和一段生成中的推理，核对运行画面。
				completed := itemStarted(0, reasoningItem("x1", "看看", defs.ReasoningCompleted))
				started := itemStarted(1, toolCallItem("c1", "call-1", "bash", `{"command":"go test -race ./..."}`))
				generating := itemStarted(2, reasoningItem("x2", "", ""))
				delta := reasoningDelta(2, []string{"so the field is description"}, nil)
				for _, event := range []*runtimeclient.ItemChanged{completed, started, generating, delta} {
					event.ConversationID = "c1"
				}
				h.emit(completed, started, &runtimeclient.ToolProgress{
					ConversationID: "c1",
					TurnID:         "t1",
					ItemIndex:      1,
					Progress: agent.AgentToolProgress{
						CallID:  "call-1",
						Stream:  "stdout",
						Content: json.RawMessage(`"ok  internal/tui  1.2s\n"`),
					},
				})
				h.emit(generating, delta)
				requireGolden(t, "step_group_running_80x24", h.view())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// TestTUIConversationTitles 检查标题更新后的主视图、展开视图和状态面板。
func TestTUIConversationTitles(t *testing.T) {
	// 1. 发送标题变更和重新同步事件，核对标题及历史内容。
	tests := []struct {
		name      string
		events    []runtimeclient.ClientEvent
		wantTitle string
		wantHeads int
	}{
		{
			name: "saved_title_update",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "后端标题",
				},
			},
			wantTitle: "后端标题",
			wantHeads: 1,
		},
		{
			name: "repeated_title_does_not_print_again",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "后端标题",
				},
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "后端标题",
				},
			},
			wantTitle: "后端标题",
			wantHeads: 1,
		},
		{
			name: "other_conversation_ignored",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c9",
					Title:          "别处标题",
				},
			},
		},
		{
			name: "resync_restores_saved_title",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   defs.Conversation{Title: "重连标题"},
				},
			},
			wantTitle: "重连标题",
			wantHeads: 1,
		},
		{
			name: "explicit_clear_restores_placeholder",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "后端标题",
				},
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "",
				},
			},
			wantHeads: 2,
		},
		{
			name: "resync_does_not_repeat_title_header",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          "后端标题",
				},
				&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation:   defs.Conversation{Title: "后端标题"},
				},
			},
			wantTitle: "后端标题",
			wantHeads: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 40)
			h.openDraftConversation("用户输入")
			h.emit(&runtimeclient.ItemsSaved{
				ConversationID: "c1",
				Items:          logItems(userItem("input-1", "用户输入"), replyItem("r1", "已经显示的回复")),
			})
			h.typeText("保留草稿")
			before := h.getPrinted()
			h.emit(tt.events...)
			if h.model.conversation.Title != tt.wantTitle {
				t.Fatalf("title = %q, want %q", h.model.conversation.Title, tt.wantTitle)
			}
			printed := strings.TrimPrefix(h.getPrinted(), before)
			if heads := strings.Count(printed, "── "); heads != tt.wantHeads || strings.Contains(printed, "已经显示的回复") {
				t.Fatalf("title update printed %q, want %d headers without replaying content", printed, tt.wantHeads)
			}
			if h.model.shouldRebuildScrollback || h.model.composer.getValue() != "保留草稿" {
				t.Fatalf("title update rebuilt scrollback or changed the draft")
			}
			h.press("ctrl+o")
			wantDisplay := getConversationTitle(*h.model.conversation)
			if expanded := strings.Join(h.model.getExpandedLines(), "\n"); !strings.Contains(expanded, "── "+wantDisplay+" · c1") || strings.Count(expanded, "已经显示的回复") != 1 {
				t.Fatalf("expanded view = %q, want the saved title and retained history", expanded)
			}
			h.model.expandView = nil
			h.model.activePanel = &statusPanel{details: &conversation.SessionDetails{
				ConversationID: "c1",
				Title:          "过期标题",
			}}
			if view := h.view(); !strings.Contains(view, wantDisplay) || strings.Contains(view, "过期标题") {
				t.Fatalf("status view = %q, want the latest saved title", view)
			}
		})
	}
}

// TestQueuePreviewSelfHeal 检查状态推送和同步后的排队预览。
func TestQueuePreviewSelfHeal(t *testing.T) {
	// 1. 构造根 Session 停止、停止中、空闲和子 Session 停止的事件。
	stopped := newRootInfo("")
	stopped.State = session.SessionStateStopped
	stopping := newRootInfo("t1")
	stopping.State = session.SessionStateStopping
	stoppedSubagent := newChildInfo("s2", "检查", "")
	stoppedSubagent.State = session.SessionStateStopped
	tests := []struct {
		name     string
		event    runtimeclient.ClientEvent
		wantKept bool
	}{
		// 两个 Turn 之间的空闲推送保留排队预览。
		{
			name: "idle_push_between_turns_keeps_queue",
			event: &runtimeclient.SessionStatusChanged{
				ConversationID: "c1",
				Info:           newRootInfo(""),
			},
			wantKept: true,
		},
		{
			name: "stopped_push_clears_queue",
			event: &runtimeclient.SessionStatusChanged{
				ConversationID: "c1",
				Info:           stopped,
			},
		},
		// 根 Session 停止中时，活动 Turn 尚未结束也清空排队预览。
		{
			name: "stopping_push_clears_queue",
			event: &runtimeclient.SessionStatusChanged{
				ConversationID: "c1",
				Info:           stopping,
			},
		},
		{
			name: "stopped_subagent_push_keeps_queue",
			event: &runtimeclient.SessionStatusChanged{
				ConversationID: "c1",
				Info:           stoppedSubagent,
			},
			wantKept: true,
		},
		{
			name: "resync_with_idle_root_clears_queue",
			event: &runtimeclient.ConversationSynced{
				ConversationID: "c1",
				SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo("")},
			},
		},
		{
			name: "resync_with_running_root_keeps_queue",
			event: &runtimeclient.ConversationSynced{
				ConversationID: "c1",
				SessionInfos:   []runtime.SessionRuntimeInfo{newRootInfo("t1")},
			},
			wantKept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24)
			h.openRunningQueue("接下来检查 race")
			h.emit(tt.event)
			if kept := strings.Contains(h.view(), "排队：接下来检查 race"); kept != tt.wantKept {
				t.Fatalf("queue preview kept = %v, want %v; view:\n%s", kept, tt.wantKept, h.view())
			}
		})
	}
}

// TestTUIViewSizes 检查不同尺寸下的多行草稿。
func TestTUIViewSizes(t *testing.T) {
	// 1. 在指定终端尺寸粘贴草稿并与预期画面比较。
	tests := []struct {
		name          string
		width, height int
		draft         string
	}{
		{
			name:   "draft_multiline_40x24",
			width:  40,
			height: 24,
			draft:  "第一行\n第二行很长很长很长很长很长很长很长很长很长",
		},
		{
			name:   "draft_multiline_140x24",
			width:  140,
			height: 24,
			draft:  "第一行\n第二行",
		},
		{
			name:   "tiny_height_3",
			width:  80,
			height: 3,
			draft:  "a\nb\nc",
		},
		{
			name:   "tiny_height_1",
			width:  80,
			height: 1,
			draft:  "a\nb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, tt.width, tt.height)
			h.send(tea.PasteMsg{Content: tt.draft})
			requireGolden(t, tt.name, h.view())
		})
	}
}

// panicEventsClient 的 Events 会 panic，用来模拟界面运行中的崩溃。
type panicEventsClient struct {
	*fakeClient
}

// Events 在读取事件时触发测试崩溃。
func (panicEventsClient) Events() <-chan runtimeclient.ClientEvent {
	// 1. 抛出事件读取错误。
	panic("events broke")
}

// TestFlushPrints 检查实时区域变矮后的定稿行写出顺序。
func TestFlushPrints(t *testing.T) {
	// 1. 组合变矮时间、等待状态和退出状态，核对立即写出或暂存。
	tests := []struct {
		name string
		// shrunkAgo 是实时区域多久之前变矮，0 表示没有变矮过。
		shrunkAgo time.Duration
		// render 是否改由真实的 View 产生变矮？打开帮助面板、画一帧、关闭面板、再画一帧。
		render     bool
		isWaiting  bool
		isQuitting bool
		// hasNext 是否给 flushPrints 传入后续命令？传入时它排在 ClearScreen 之后。
		hasNext     bool
		wantPrinted bool
	}{
		{
			name:        "no_recent_shrink_prints_now",
			wantPrinted: true,
		},
		{
			name:        "clear_screen_before_next_cmd",
			hasNext:     true,
			wantPrinted: true,
		},
		{
			name:        "settled_shrink_prints_now",
			shrunkAgo:   liveFrameSettleDelay,
			wantPrinted: true,
		},
		// 实时区域刚变矮：终端上可能还是旧帧，先暂存，等计时消息放行。
		{
			name:      "recent_shrink_holds_prints",
			shrunkAgo: 10 * time.Millisecond,
		},
		{
			name:      "waiting_keeps_holding",
			isWaiting: true,
		},
		{
			name:   "panel_close_holds_prints",
			render: true,
		},
		{
			name:        "quitting_prints_despite_shrink",
			shrunkAgo:   10 * time.Millisecond,
			isQuitting:  true,
			wantPrinted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24)
			if tt.shrunkAgo > 0 {
				h.model.liveShrunkAt = h.now.Add(-tt.shrunkAgo)
			}
			if tt.render {
				h.press("?")
				h.view()
				h.press("esc")
				if lines := strings.Split(h.view(), "\n"); h.model.lastLiveHeight != len(lines) || !h.model.liveShrunkAt.Equal(h.now) {
					t.Fatalf("live height = %d (view %d), shrunk at %v", h.model.lastLiveHeight, len(lines), h.model.liveShrunkAt)
				}
			}
			h.model.isWaitingLiveFrame, h.model.isQuitting = tt.isWaiting, tt.isQuitting
			h.model.pendingPrints = []string{"一行"}
			var next tea.Cmd
			if tt.hasNext {
				next = func() tea.Msg { return noticeExpiredMsg{id: -1} }
			}
			cmd := h.model.flushPrints(next)
			if isPrinted := h.model.pendingPrints == nil; isPrinted != tt.wantPrinted || (tt.wantPrinted && cmd == nil) {
				t.Fatalf("printed = %v, cmd nil = %v, want printed %v", isPrinted, cmd == nil, tt.wantPrinted)
			}
			if tt.wantPrinted {
				// 检查定稿行之后、后续命令之前执行 ClearScreen。
				requireSequenceEnds(t, cmd, tt.hasNext)
				return
			}
			// 放行之后同一批定稿行写出。
			if !tt.isWaiting && cmd == nil {
				t.Fatal("hold did not schedule a release")
			}
			h.now = h.now.Add(liveFrameSettleDelay)
			_, cmd = h.model.Update(liveFrameSettledMsg{})
			if h.model.pendingPrints != nil || cmd == nil || h.model.isWaitingLiveFrame {
				t.Fatalf("held prints not released: pending = %q, cmd nil = %v", h.model.pendingPrints, cmd == nil)
			}
			requireSequenceEnds(t, cmd, false)
		})
	}
}

// requireSequenceEnds 检查写出命令尾部的清屏和后续命令。
func requireSequenceEnds(t *testing.T, cmd tea.Cmd, hasNext bool) {
	t.Helper()
	// 1. 读取命令序列，核对定稿行之后的 ClearScreen 和可选末尾命令。
	sequence := reflect.ValueOf(cmd())
	if sequence.Kind() != reflect.Slice {
		t.Fatalf("cmd is not a sequence: %T", cmd())
	}
	clearIndex := sequence.Len() - 2
	if hasNext {
		clearIndex--
	}
	if clearIndex < 1 || sequence.Index(clearIndex).Interface().(tea.Cmd)() != tea.ClearScreen() {
		t.Fatalf("sequence of %d commands does not clear the screen at %d", sequence.Len(), clearIndex)
	}
	if hasNext && sequence.Index(clearIndex+2).Interface().(tea.Cmd)() != (noticeExpiredMsg{id: -1}) {
		t.Fatal("next command is not last")
	}
}

// TestTUIRebuildScrollback 检查重建历史后的内容、草稿和输出错误。
func TestTUIRebuildScrollback(t *testing.T) {
	// 1. 调整界面宽度，将历史重放到普通输出或失败的管道。
	for _, tt := range []struct {
		name string
		fail bool
	}{
		{name: "history_and_draft_survive"},
		{
			name: "output_error_returned",
			fail: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24)
			h.model.transcript.handleItemsSaved(logItems(userItem("u1", "历史输入"), replyItem("r1", "历史回复")))
			h.typeText("保留草稿")
			h.model.handleWindowSize(tea.WindowSizeMsg{
				Width:  30,
				Height: 24,
			})
			var output strings.Builder
			var writer io.Writer = &output
			wantErr := errors.New("terminal write failed")
			if tt.fail {
				reader, pipe := io.Pipe()
				reader.CloseWithError(wantErr)
				t.Cleanup(func() { pipe.Close() })
				writer = pipe
			}
			err := h.model.rebuildScrollback(writer)
			if tt.fail {
				if !errors.Is(err, wantErr) {
					t.Fatalf("error = %v, want %v", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"\x1b[2J\x1b[H\x1b[3J", "历史输入", "历史回复"} {
				if strings.Count(output.String(), text) != 1 {
					t.Fatalf("replay duplicated or lost %q: %q", text, output.String())
				}
			}
			if h.model.composer.getValue() != "保留草稿" {
				t.Fatal("replay changed the input draft")
			}
		})
	}
}

// TestTUIRun 检查事件流结束、取消和崩溃时的运行结果。
func TestTUIRun(t *testing.T) {
	// 1. 分别关闭事件流、取消上下文和触发 panic，核对 Run 的错误。
	tests := []struct {
		name    string
		client  func(t *testing.T) runtimeclient.Client
		ctx     func(t *testing.T) context.Context
		wantErr bool
	}{
		{
			name: "events_closed_quits_without_error",
			client: func(t *testing.T) runtimeclient.Client {
				client := newFakeClient()
				close(client.events)
				return client
			},
			ctx: func(t *testing.T) context.Context { return t.Context() },
		},
		{
			name: "canceled_context_is_not_an_error",
			client: func(t *testing.T) runtimeclient.Client {
				client := newFakeClient()
				t.Cleanup(func() { close(client.events) })
				return client
			},
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
		},
		{
			// bubbletea 将 panic 包装成 ErrProgramKilled 并向 stderr 打印堆栈；Run 返回错误。
			name:    "panic_is_returned_as_error",
			client:  func(t *testing.T) runtimeclient.Client { return panicEventsClient{newFakeClient()} },
			ctx:     func(t *testing.T) context.Context { return t.Context() },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := NewTUIOpt(tt.client(t), "assistant", tuiTestWorkspaceDirectory, strings.NewReader(""), io.Discard)
			opt.MarkdownStyle = markdownStyleNoTTY
			view, err := NewTUI(opt)
			if err != nil {
				t.Fatal(err)
			}
			if err := view.Run(tt.ctx(t)); (err != nil) != tt.wantErr {
				t.Fatalf("Run err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestLocalServerDetection 检查连接与重连后的本机服务判定。
func TestLocalServerDetection(t *testing.T) {
	// 1. 按顺序发送 server_id 和重连事件，核对本机标记。
	tests := []struct {
		name          string
		localServerID string
		// serverIDs 是依次收到的 connected 事件带的 server_id。
		serverIDs []string
		wantLocal bool
	}{
		{
			name:          "same_server_id_is_local",
			localServerID: "local-1",
			serverIDs:     []string{"local-1"},
			wantLocal:     true,
		},
		{
			name:          "different_server_id_is_remote",
			localServerID: "local-1",
			serverIDs:     []string{"remote-1"},
		},
		{
			name:          "server_without_id_is_remote",
			localServerID: "local-1",
			serverIDs:     []string{""},
		},
		{
			name:          "empty_ids_never_match",
			localServerID: "",
			serverIDs:     []string{""},
		},
		{
			name:          "no_local_server_id_is_remote",
			localServerID: "",
			serverIDs:     []string{"remote-1"},
		},
		{
			name:          "reconnect_to_other_server_becomes_remote",
			localServerID: "local-1",
			serverIDs:     []string{"local-1", "remote-1"},
		},
		{
			name:          "reconnect_to_local_server_becomes_local",
			localServerID: "local-1",
			serverIDs:     []string{"remote-1", "local-1"},
			wantLocal:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeClient()
			t.Cleanup(func() { close(client.events) })
			h := newTUIHarnessWithClient(t, client, "assistant", tuiTestWorkspaceDirectory, 80, 24, func(opt *TUIOpt) { opt.LocalServerID = tt.localServerID })
			for _, serverID := range tt.serverIDs {
				h.emit(&runtimeclient.ConnectionChanged{
					State:    runtimeclient.ConnectionConnected,
					ServerID: serverID,
				})
				// 每次连上之后断线：重连中的事件不带 server_id，也不改变本机判定。
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionReconnecting})
			}
			if h.model.isLocalServer != tt.wantLocal {
				t.Fatalf("isLocalServer = %v, want %v", h.model.isLocalServer, tt.wantLocal)
			}
		})
	}
}

// TestDraftExecutorSettings 检查连接、重连和新建草稿时草稿的 Executor 设置。
func TestDraftExecutorSettings(t *testing.T) {
	// 1. 准备本机目录与对话设置，逐场景核对草稿保留或复制的设置。
	localSettings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: tuiTestWorkspaceDirectory}}
	conversationSettings := defs.ExecutorSettings{
		DefaultExecutorID:    "gpu",
		ExecutorIDs:          []string{"local", "gpu"},
		WorkspaceDirectories: map[string]string{"local": "/work/other"},
	}
	tests := []struct {
		name string
		// serverIDs 是依次收到的 connected 事件带的 server_id；每次之后断线。
		serverIDs []string
		// isWorkspaceDirectoryMapped 是 TUIOpt.IsWorkspaceDirectoryMapped：界面的目录是调用方按映射换成的 serve 路径。
		isWorkspaceDirectoryMapped bool
		// run 在连上之后执行，为 nil 时不做别的。
		run  func(h *tuiHarness)
		want defs.ExecutorSettings
	}{
		{
			name:      "first_connect_to_local_server_uses_cwd",
			serverIDs: []string{tuiTestLocalServerID},
			want:      localSettings,
		},
		{
			name:      "first_connect_to_remote_server_is_empty",
			serverIDs: []string{"remote"},
		},
		{
			name:                       "first_connect_to_remote_server_uses_mapped_directory",
			serverIDs:                  []string{"remote"},
			isWorkspaceDirectoryMapped: true,
			want:                       localSettings,
		},
		// 之后重连不重置草稿，即使换到了另一台 serve。
		{
			name:      "reconnect_to_remote_keeps_draft",
			serverIDs: []string{tuiTestLocalServerID, "remote"},
			want:      localSettings,
		},
		{
			name:      "reconnect_to_local_keeps_draft",
			serverIDs: []string{"remote", tuiTestLocalServerID},
		},
		{
			name:      "new_copies_current_conversation_settings",
			serverIDs: []string{tuiTestLocalServerID},
			run: func(h *tuiHarness) {
				// 1. 打开对话并同步 Executor 设置，再执行 /new 检查草稿复制了设置。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation: defs.Conversation{
						ID:               "c1",
						ExecutorSettings: conversationSettings,
					},
				})
				h.typeText("/new")
				h.press("enter")
			},
			want: conversationSettings,
		},
		{
			name:      "new_in_draft_keeps_draft",
			serverIDs: []string{tuiTestLocalServerID},
			run: func(h *tuiHarness) {
				// 1. 在草稿中执行 /new 并核对设置未变。
				h.typeText("/new")
				h.press("enter")
			},
			want: localSettings,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24)
			h.model.opt.IsWorkspaceDirectoryMapped = tt.isWorkspaceDirectoryMapped
			for _, serverID := range tt.serverIDs {
				h.emit(&runtimeclient.ConnectionChanged{
					State:    runtimeclient.ConnectionConnected,
					ServerID: serverID,
				})
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionReconnecting})
			}
			if tt.run != nil {
				tt.run(h)
			}
			if h.model.conversation != nil || !reflect.DeepEqual(h.model.draftExecutorSettings, tt.want) {
				t.Fatalf("conversation = %v, draft settings = %+v, want %+v", h.model.conversation, h.model.draftExecutorSettings, tt.want)
			}
		})
	}
}

// TestCDCommand 检查草稿和已打开对话的目录切换。
func TestCDCommand(t *testing.T) {
	// 1. 准备目录解析和设置预设，核对 /cd 的请求、提示与 Skills。
	busy := fmt.Errorf("conversation c1: %w", session.ErrSessionBusy)
	// newOpenedSettings 返回已打开对话 c1 的设置：默认是列表里第一个在范围内的 local，gpu 另有目录。
	newOpenedSettings := func() defs.ExecutorSettings {
		return defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": tuiTestWorkspaceDirectory, "gpu": "/data/x"}}
	}
	// openWithSettings 打开对话 c1，并像重新同步一样让它的记录带上 newOpenedSettings。
	openWithSettings := func(h *tuiHarness) {
		h.openDraftConversation("开场")
		h.emit(&runtimeclient.ConversationSynced{
			ConversationID: "c1",
			Conversation: defs.Conversation{
				ID:               "c1",
				ExecutorSettings: newOpenedSettings(),
			},
		})
	}
	tests := []struct {
		name string
		// preset 在连上之前设置 fakeClient 的预设，为 nil 时用默认预设。
		preset func(c *fakeClient)
		run    func(t *testing.T, h *tuiHarness)
	}{
		{
			// 草稿：在默认 Executor（列表里第一个在范围内的 local）上以当前目录为基准解析，只改草稿设置，不调用 set，重新拉取草稿 Skills。
			name: "draft_changes_only_draft_settings",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 执行草稿 /cd，核对目录解析、Skills 重载和未发出设置请求。
				h.waitFor("draft skills reloaded after connect", func() bool { return len(h.client.getSkillRequests()) == 2 })
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("draft skills reloaded after cd", func() bool { return len(h.client.getSkillRequests()) == 3 })
				want := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/work/lib"}}
				var resolved []runtimeclient.ResolveWorkspaceDirectoryRequest
				h.client.set(func(c *fakeClient) { resolved = c.resolveRequests })
				wantResolved := []runtimeclient.ResolveWorkspaceDirectoryRequest{{
					ExecutorID:    "local",
					Directory:     "../lib",
					BaseDirectory: tuiTestWorkspaceDirectory,
				}}
				skills := h.client.getSkillRequests()
				if !reflect.DeepEqual(resolved, wantResolved) || !reflect.DeepEqual(h.model.draftExecutorSettings, want) ||
					!reflect.DeepEqual(skills[2], runtimeclient.GetSkillsRequest{
						AgentName:        "assistant",
						ExecutorSettings: want,
					}) {
					t.Fatalf("resolved = %+v, draft = %+v, skills = %+v", resolved, h.model.draftExecutorSettings, skills)
				}
				if calls := h.client.getCalls("SetConversationExecutorSettings"); len(calls) != 0 {
					t.Fatalf("set calls = %v", calls)
				}
				if h.model.notice.text != "工作位置：local:/work/lib，下一轮生效" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			// 已打开对话：设置复制一份，只改默认 Executor 那一项目录，调用 set；成功后对话的设置随之更新。
			name: "open_conversation_sets_only_default_directory",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开预设对话后执行 /cd，核对只修改默认 Executor 的目录。
				openWithSettings(h)
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("settings set", func() bool { return strings.HasPrefix(h.model.notice.text, "工作位置") })
				want := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/work/lib", "gpu": "/data/x"}}
				var sent []defs.ExecutorSettings
				h.client.set(func(c *fakeClient) { sent = c.settingsRequests })
				if calls := h.client.getCalls("SetConversationExecutorSettings"); !slices.Equal(calls, []string{"SetConversationExecutorSettings c1"}) || !reflect.DeepEqual(sent, []defs.ExecutorSettings{want}) {
					t.Fatalf("set calls = %v, settings = %+v", calls, sent)
				}
				if h.model.notice.text != "工作位置：local:/work/lib，下一轮生效" || !reflect.DeepEqual(h.model.conversation.ExecutorSettings, want) {
					t.Fatalf("notice = %q, conversation settings = %+v", h.model.notice.text, h.model.conversation.ExecutorSettings)
				}
			},
		},
		{
			// 已打开对话改设置后 serve 让对话原地换上新设置、客户端重新同步：Skills 按新的默认 Executor 重新拉取，补全里出现新目录的 Skill。
			name: "resync_after_cd_reloads_skills",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开对话并更换 Skills 预设，执行 /cd 后同步并检查新补全。
				openWithSettings(h)
				// 打开和第一次同步各拉取一次；fakeClient 在收到请求时就定下返回值，之后换掉的 Skills 只影响后面的请求。
				countConversationRequests := func() int {
					return len(slices.DeleteFunc(h.client.getSkillRequests(), func(request runtimeclient.GetSkillsRequest) bool { return request.ConversationID != "c1" }))
				}
				h.waitFor("skills listed on open and first sync", func() bool { return countConversationRequests() == 2 })
				h.client.set(func(c *fakeClient) {
					c.skills = []skill.SkillInfo{{
						Name:        "remote-skill",
						Description: "Skill from the new directory",
					}}
				})
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("settings set", func() bool { return strings.HasPrefix(h.model.notice.text, "工作位置") })
				settings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/work/lib", "gpu": "/data/x"}}
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation: defs.Conversation{
						ID:               "c1",
						ExecutorSettings: settings,
					},
				})
				h.waitFor("skills reloaded after resync", func() bool {
					return slices.ContainsFunc(h.model.skills, func(listed skill.SkillInfo) bool { return listed.Name == "remote-skill" })
				})
				h.typeText("/remote")
				if matches := h.model.getSlashMatches(); len(matches) != 1 || matches[0].name != "remote-skill" {
					t.Fatalf("slash matches = %+v, want remote-skill", matches)
				}
			},
		},
		{
			name:   "busy_conversation_keeps_settings",
			preset: func(c *fakeClient) { c.setSettingsErr = busy },
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开对话后执行 /cd，检查忙碌错误和原设置保留。
				openWithSettings(h)
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("set refused", func() bool { return h.model.notice.isError })
				// 与新建的副本比较：/cd 只改自己那份副本，对话记录里的设置原样保留。
				if h.model.notice.text != "有任务正在执行，等它结束或按两次 Esc 中断后再改" || !reflect.DeepEqual(h.model.conversation.ExecutorSettings, newOpenedSettings()) {
					t.Fatalf("notice = %q, conversation settings = %+v", h.model.notice.text, h.model.conversation.ExecutorSettings)
				}
			},
		},
		{
			name: "resolve_failure_shows_error",
			preset: func(c *fakeClient) {
				c.resolveErr = errors.New("invalid executor settings: not a directory: /work/lib")
			},
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 执行目录解析失败的 /cd，检查错误提示和草稿目录保留。
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("resolve failed", func() bool { return h.model.notice.isError })
				if h.model.notice.text != "切换目录失败：invalid executor settings: not a directory: /work/lib" ||
					h.model.draftExecutorSettings.WorkspaceDirectories["local"] != tuiTestWorkspaceDirectory {
					t.Fatalf("notice = %q, draft = %+v", h.model.notice.text, h.model.draftExecutorSettings)
				}
			},
		},
		{
			name: "without_argument_shows_location_and_usage",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 执行无参数 /cd，检查位置提示、用法和没有解析请求。
				h.typeText("/cd")
				h.press("enter")
				if h.model.notice.text != "当前位置：local:/work/project；用法：/cd <目录>" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
				var resolved []runtimeclient.ResolveWorkspaceDirectoryRequest
				h.client.set(func(c *fakeClient) { resolved = c.resolveRequests })
				if len(resolved) != 0 {
					t.Fatalf("resolved = %+v", resolved)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24, func(c *fakeClient) {
				c.executors, c.resolvedDirectory = testExecutors, "/work/lib"
				if tt.preset != nil {
					tt.preset(c)
				}
			})
			h.connectLocalServer()
			tt.run(t, h)
		})
	}
}

// TestOpenFailedConversation 检查打开或同步失败后的设置修改与重试。
func TestOpenFailedConversation(t *testing.T) {
	// 1. 预设连接失败的对话，核对 /executor、/cd 和 /new 的结果。
	stored := defs.Conversation{
		ID:            "c7",
		RootSessionID: "s7",
		Title:         "旧对话",
		ExecutorSettings: defs.ExecutorSettings{
			DefaultExecutorID:    "gpu",
			WorkspaceDirectories: map[string]string{"local": "/work/old"},
		},
	}
	openErr := errors.New(`executor "gpu": dial tcp 10.0.0.2:22: i/o timeout`)
	// resumeStoredAndFail 让打开对话失败，用 /resume 打开 c7，等它打开失败、回到草稿。
	resumeStoredAndFail := func(h *tuiHarness) {
		h.client.set(func(c *fakeClient) { c.openErr = openErr })
		h.typeText("/resume")
		h.press("enter")
		h.waitFor("conversations listed", func() bool { return strings.Contains(h.view(), "旧对话") })
		h.press("enter")
		h.waitFor("open failed", func() bool { return h.model.openFailedConversation != nil })
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			// 默认 Executor 连不上的对话打开失败：用 /executor 把默认换成 local，按新设置改好后打开它。
			name: "executor_panel_fixes_and_opens",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 恢复连接失败的对话，再将默认 Executor 改成 local 并检查重新打开。
				resumeStoredAndFail(h)
				want := `打开对话失败：executor "gpu": dial tcp 10.0.0.2:22: i/o timeout；可用 /executor 或 /cd 修改它的设置`
				if h.model.conversation != nil || h.model.openFailedConversation.ID != "c7" || h.model.notice.text != want {
					t.Fatalf("conversation = %v, failed = %v, notice = %q", h.model.conversation, h.model.openFailedConversation, h.model.notice.text)
				}
				h.client.set(func(c *fakeClient) { c.openErr = nil })
				h.typeText("/executor")
				h.press("enter")
				h.waitFor("executors listed", func() bool { return !h.model.activePanel.(*executorPanel).fetch.isLoading })
				h.press("enter")
				h.waitFor("reopened", func() bool { return h.model.conversation != nil && len(h.client.getCalls("OpenConversation")) == 2 })
				var sent []defs.ExecutorSettings
				h.client.set(func(c *fakeClient) { sent = c.settingsRequests })
				wantSettings := defs.ExecutorSettings{
					DefaultExecutorID:    "local",
					WorkspaceDirectories: map[string]string{"local": "/work/old"},
				}
				if calls := h.client.getCalls("SetConversationExecutorSettings"); !slices.Equal(calls, []string{"SetConversationExecutorSettings c7"}) || !reflect.DeepEqual(sent, []defs.ExecutorSettings{wantSettings}) {
					t.Fatalf("set calls = %v, settings = %+v", calls, sent)
				}
				if h.model.conversation.ID != "c7" || !reflect.DeepEqual(h.model.conversation.ExecutorSettings, wantSettings) || h.model.openFailedConversation != nil {
					t.Fatalf("conversation = %+v, failed = %v", h.model.conversation, h.model.openFailedConversation)
				}
			},
		},
		{
			// 重新同步被 serve 拒绝时同样回到草稿并记下这个对话；/cd 作用于它，按它的默认 Executor 和目录解析，改好后打开它。
			name: "sync_failure_enters_same_state",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 交付同步失败，再执行 /cd 并核对目录解析、设置与重新打开顺序。
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationSyncFailed{
					ConversationID: "c1",
					Err:            openErr,
				})
				if h.model.conversation != nil || h.model.openFailedConversation == nil || h.model.openFailedConversation.ID != "c1" ||
					!strings.HasSuffix(h.model.notice.text, "；可用 /executor 或 /cd 修改它的设置") {
					t.Fatalf("conversation = %v, failed = %v, notice = %q", h.model.conversation, h.model.openFailedConversation, h.model.notice.text)
				}
				h.typeText("/cd ../lib")
				h.press("enter")
				h.waitFor("reopened", func() bool { return h.model.conversation != nil && len(h.client.getCalls("OpenConversation")) == 2 })
				var resolved []runtimeclient.ResolveWorkspaceDirectoryRequest
				h.client.set(func(c *fakeClient) { resolved = c.resolveRequests })
				wantResolved := []runtimeclient.ResolveWorkspaceDirectoryRequest{{
					ExecutorID:    "local",
					Directory:     "../lib",
					BaseDirectory: tuiTestWorkspaceDirectory,
				}}
				if calls := h.client.getCalls("SetConversationExecutorSettings", "OpenConversation"); !reflect.DeepEqual(resolved, wantResolved) ||
					!slices.Equal(calls, []string{"OpenConversation c1", "SetConversationExecutorSettings c1", "OpenConversation c1"}) {
					t.Fatalf("resolved = %+v, calls = %v", resolved, calls)
				}
			},
		},
		{
			name: "new_clears_failed_conversation",
			run: func(t *testing.T, h *tuiHarness) {
				// 1. 打开对话失败后执行 /new，检查失败记录清除且草稿目录不变。
				resumeStoredAndFail(h)
				h.typeText("/new")
				h.press("enter")
				if h.model.openFailedConversation != nil {
					t.Fatalf("failed conversation kept after /new: %+v", h.model.openFailedConversation)
				}
				// 草稿设置不受打开失败的对话影响。
				if _, settings := h.model.getEditableExecutorSettings(); settings.WorkspaceDirectories["local"] != tuiTestWorkspaceDirectory {
					t.Fatalf("current settings = %+v", settings)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24, func(c *fakeClient) {
				c.executors, c.conversations, c.resolvedDirectory = testExecutors, []defs.Conversation{stored}, "/work/lib"
			})
			h.connectLocalServer()
			tt.run(t, h)
		})
	}
}

// ── 测试驱动 ──

// tuiHarness 在测试 goroutine 中像 bubbletea 一样驱动 tuiModel：update 处理消息、收集定稿行，
// 返回的命令在后台 goroutine 执行，结果消息回到 msgs，由 waitFor 依次处理。
type tuiHarness struct {
	t       *testing.T
	model   *tuiModel
	client  *fakeClient
	msgs    chan tea.Msg
	done    chan struct{}
	printed []string
	now     time.Time
}

// newTUIHarness 创建带假客户端的界面测试驱动。
func newTUIHarness(t *testing.T, width, height int, presets ...func(c *fakeClient)) *tuiHarness {
	t.Helper()
	// 1. 应用客户端预设，登记事件流关闭，再初始化界面。
	client := newFakeClient()
	for _, preset := range presets {
		client.set(preset)
	}
	t.Cleanup(func() { close(client.events) })
	h := newTUIHarnessWithClient(t, client, "assistant", tuiTestWorkspaceDirectory, width, height)
	h.client = client
	return h
}

// newTUIHarnessWithClient 使用给定客户端创建并初始化界面测试驱动。
func newTUIHarnessWithClient(t *testing.T, client runtimeclient.Client, agentName, workspaceDirectory string, width, height int, configureOpt ...func(*TUIOpt)) *tuiHarness {
	t.Helper()
	// 1. 设置界面依赖和启动选项；客户端事件流由调用方关闭。
	opt := NewTUIOpt(client, agentName, workspaceDirectory, strings.NewReader(""), io.Discard)
	opt.MarkdownStyle = markdownStyleNoTTY
	opt.LocalServerID = tuiTestLocalServerID
	for _, configure := range configureOpt {
		configure(opt)
	}
	// 2. 建立测试消息通道、结束信号和固定时钟。
	h := &tuiHarness{
		t:    t,
		msgs: make(chan tea.Msg, 1024),
		done: make(chan struct{}),
		now:  time.Date(2026, 10, 14, 10, 0, 0, 0, time.Local),
	}
	t.Cleanup(func() { close(h.done) })
	// 3. 创建界面，注入时钟和单帧图标，再发送尺寸并运行 Init。
	h.model = newTUIModel(t.Context(), opt, nil)
	h.model.now = func() time.Time { return h.now }
	h.model.spinner = spinner.New(spinner.WithSpinner(spinner.Spinner{
		Frames: []string{"◐"},
		FPS:    time.Second,
	}))
	h.send(tea.WindowSizeMsg{
		Width:  width,
		Height: height,
	})
	h.run(h.model.Init())
	return h
}

// send 处理界面消息并收集定稿行。
func (h *tuiHarness) send(msg tea.Msg) {
	// 1. 直接调用 update（不经过 Update 和 flushPrints），取出 pendingPrints，再运行返回的命令。
	h.t.Helper()
	cmd := h.model.update(msg)
	h.printed = append(h.printed, h.model.pendingPrints...)
	h.model.pendingPrints = nil
	h.run(cmd)
}

// emit 依次向界面交付客户端事件。
func (h *tuiHarness) emit(events ...runtimeclient.ClientEvent) {
	// 1. 将每条事件包装成界面消息并处理。
	for _, event := range events {
		h.send(clientEventMsg{event: event})
	}
}

// run 在后台执行界面命令并交回结果。
func (h *tuiHarness) run(cmd tea.Cmd) {
	// 1. 跳过空命令，拆开批量命令，其余结果送入消息通道或在测试结束后丢弃。
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, each := range batch {
				h.run(each)
			}
			return
		}
		if msg == nil {
			return
		}
		select {
		case h.msgs <- msg:
		case <-h.done:
		}
	}()
}

// waitFor 处理后台消息直至条件成立。
func (h *tuiHarness) waitFor(what string, cond func() bool) {
	// 1. 在十秒内处理消息，超时输出界面和可用的客户端调用记录。
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case msg := <-h.msgs:
			h.send(msg)
		case <-deadline:
			// 用 fakeClient 时同时打印它记录的调用，端到端驱动没有 fake。
			if h.client != nil {
				h.t.Fatalf("timed out waiting for %s; calls = %v\nview:\n%s", what, h.client.getCalls(), h.view())
			}
			h.t.Fatalf("timed out waiting for %s\nview:\n%s", what, h.view())
		}
	}
}

// press 按名称依次发送按键。
func (h *tuiHarness) press(names ...string) {
	// 1. 将 enter、ctrl+c、shift+tab 等名称转换为按键消息。
	h.t.Helper()
	for _, name := range names {
		h.send(newKeyPress(name))
	}
}

// typeText 逐字向界面输入文本。
func (h *tuiHarness) typeText(text string) {
	// 1. 将每个字符转换为按键消息并处理。
	h.t.Helper()
	for _, r := range text {
		h.send(tea.KeyPressMsg{
			Code: r,
			Text: string(r),
		})
	}
}

// connectLocalServer 连接本机服务并等待 Executor 列表。
func (h *tuiHarness) connectLocalServer() {
	// 1. 发送本机 server_id，等待预设的非空 Executor 列表加载。
	h.t.Helper()
	h.emit(&runtimeclient.ConnectionChanged{
		State:    runtimeclient.ConnectionConnected,
		ServerID: tuiTestLocalServerID,
	})
	h.waitFor("executors loaded", func() bool { return len(h.model.executors) > 0 })
}

// openDraftConversation 从草稿发送首条输入并等待对话打开。
func (h *tuiHarness) openDraftConversation(firstInput string) {
	// 1. 输入文字并按回车，等待对话创建和提交完成。
	h.t.Helper()
	h.typeText(firstInput)
	h.press("enter")
	h.waitFor("draft conversation opened", func() bool {
		return h.model.conversation != nil && !h.model.isOpeningDraft
	})
}

// openRunningQueue 打开运行中的对话并排入一条输入。
func (h *tuiHarness) openRunningQueue(text string) {
	// 1. 打开 c1，发送首条输入已保存（排队记录里不再有它）和根 Session 开始运行的事件。
	h.t.Helper()
	h.openDraftConversation("开场")
	h.emit(&runtimeclient.ItemsSaved{
		ConversationID: "c1",
		Items: []runtimeprotocol.LoggedItem{{
			Ordinal: 1,
			Item:    &defs.Item{ID: "input-1"},
		}},
	})
	h.emit(&runtimeclient.SessionStatusChanged{
		ConversationID: "c1",
		Info:           newRootInfo("t1"),
	})
	// 2. 运行中按 Tab 排队，等待列表只剩本次输入。
	h.typeText(text)
	h.press("tab")
	h.waitFor("queued", func() bool { return len(h.model.queuedInputs.entries) == 1 })
}

// holdDraftCreation 发送草稿并暂停对话创建，返回放行函数。
func (h *tuiHarness) holdDraftCreation(text string) (release func()) {
	// 1. 给 CreateConversation 设置通道，输入文字并提交。
	h.t.Helper()
	gate := make(chan struct{})
	h.client.set(func(c *fakeClient) { c.createGate = gate })
	h.typeText(text)
	h.press("enter")
	return func() { close(gate) }
}

// view 返回去掉样式和行尾空格的实时区域。
func (h *tuiHarness) view() string {
	// 1. 读取 View 的内容并清理显示样式与空格。
	return trimTrailingSpaces(ansi.Strip(h.model.View().Content))
}

// getPrinted 返回去掉样式和行尾空格的全部定稿行。
func (h *tuiHarness) getPrinted() string {
	// 1. 连接已收集的行并清理显示样式与空格。
	return trimTrailingSpaces(ansi.Strip(strings.Join(h.printed, "\n")))
}

// trimTrailingSpaces 去掉每行末尾的空格。
func trimTrailingSpaces(text string) string {
	// 1. 拆分文本，逐行去掉空格后重新连接。
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// newKeyPress 将键名和修饰键前缀转换为按键消息。
func newKeyPress(name string) tea.KeyPressMsg {
	// 1. 建立特殊键表，再读取 ctrl+、alt+ 和 shift+ 前缀。
	codes := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "backspace": tea.KeyBackspace,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd, "space": tea.KeySpace,
	}
	var mod tea.KeyMod
	for _, modifier := range []struct {
		prefix string
		mod    tea.KeyMod
	}{{
		"ctrl+",
		tea.ModCtrl,
	}, {
		"alt+",
		tea.ModAlt,
	}, {
		"shift+",
		tea.ModShift,
	}} {
		if rest, found := strings.CutPrefix(name, modifier.prefix); found {
			mod, name = mod|modifier.mod, rest
		}
	}
	if code, found := codes[name]; found {
		return tea.KeyPressMsg{
			Code: code,
			Mod:  mod,
		}
	}
	r, _ := utf8.DecodeRuneInString(name)
	if mod != 0 {
		return tea.KeyPressMsg{
			Code: r,
			Mod:  mod,
		}
	}
	return tea.KeyPressMsg{
		Code: r,
		Text: name,
	}
}

// requireGolden 比较测试画面与预期画面。
func requireGolden(t *testing.T, name, got string) {
	t.Helper()
	// 1. 读取 testdata 下的画面文件，带 -update 时改为写入本次结果。
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run go test ./internal/tui -update)", path, err)
	}
	if string(want) != got {
		t.Fatalf("%s mismatch\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// ── fakeClient ──

// fakeClient 是测试用的 Client：按调用顺序记录调用，按预设返回结果或错误；预设的 gate 非 nil 时，对应调用等它关闭后才返回。
// 预设字段用 set 在锁内修改。
type fakeClient struct {
	// 未使用的客户端能力沿用接口占位；TUI 只调用下方实现的方法。
	runtimeclient.Client
	events chan runtimeclient.ClientEvent

	mutex             sync.Mutex
	calls             []string
	conversationCount int
	itemCount         int
	createErr         error
	openErr           error
	submitErr         error
	interruptErr      error
	compactErr        error
	setModelErr       error
	createGate        chan struct{}
	submitGate        chan struct{}
	conversations     []defs.Conversation
	createRequests    []runtimeclient.CreateConversationRequest
	openRequests      []runtimeclient.OpenConversationRequest
	skillRequests     []runtimeclient.GetSkillsRequest
	submitRequests    []runtimeclient.SubmitInputRequest
	skills            []skill.SkillInfo
	executors         []conversation.ExecutorDetails
	listExecutorsErr  error
	// resolveRequests 是收到的目录解析请求；resolvedDirectory、resolveErr 是对它们的预设回答。
	resolveRequests   []runtimeclient.ResolveWorkspaceDirectoryRequest
	resolvedDirectory string
	resolveErr        error
	// settingsRequests 是 SetConversationExecutorSettings 收到的设置，按请求顺序；setSettingsErr 是预设的失败。
	settingsRequests []defs.ExecutorSettings
	setSettingsErr   error
	modelList        runtime.ModelList
	contextUsage     runtime.SessionContextUsage
	sessionDetails   conversation.SessionDetails
	// providers、providersDefaultModel 是 ListProviders 的回答；providerWrites 是 SetProvider 收到的 Provider 和默认模型，按请求顺序。
	providers             []defs.ProviderConfig
	providersDefaultModel defs.ModelRef
	providerWrites        []fakeProviderWrite
	// configWriteResult、configWriteErr 是 SetProvider、DeleteProvider、SetDefaultModel、ReloadConfig 共用的预设回答。
	configWriteResult runtime.SetDefinitionsResult
	configWriteErr    error
	// catalogProviders、catalogModels、fetchedModels 和对应的 err 是模型目录三个方法的预设回答；和真的 Client 一样，出错时不带数据。
	// catalogGate 非 nil 时 ListCatalogProviders 等它关闭才返回，模拟第一次下载 models.dev 很慢。
	catalogGate         chan struct{}
	catalogProviders    []defs.CatalogProvider
	catalogProvidersErr error
	catalogModels       map[string]*defs.ProviderModelConfig
	catalogModelsErr    error
	fetchedModels       map[string]*defs.ProviderModelConfig
	fetchModelsErr      error
	// mcpServers、listMCPServersErr 是 ListMCPServers 的预设回答；和真的 Client 一样，出错时不带数据。
	mcpServers        []conversation.MCPServerInfo
	listMCPServersErr error
	// forkRequests 是 ForkConversation 收到的请求，按请求顺序；forkErr、deleteErr 是分叉、删除的预设失败。
	// 分叉成功时按序号编出新对话，归档、删除改的是 conversations。
	forkRequests []runtimeclient.ForkConversationRequest
	forkErr      error
	deleteErr    error
}

// newFakeClient 创建带默认模型的假客户端。
func newFakeClient() *fakeClient {
	// 1. 建立事件通道并预设一个可用模型。
	return &fakeClient{
		events: make(chan runtimeclient.ClientEvent),
		modelList: runtime.ModelList{Models: []*defs.ModelDefinition{{
			Ref: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "model",
			},
			DisplayName: "Test Model",
		}}},
	}
}

// set 在锁内修改客户端预设。
func (c *fakeClient) set(change func(c *fakeClient)) {
	// 1. 取得锁并应用传入的修改。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	change(c)
}

// getCalls 返回符合前缀的调用记录。
func (c *fakeClient) getCalls(prefixes ...string) []string {
	// 1. 在锁内按顺序筛选调用；前缀为空或未给出时返回全部。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	var calls []string
	for _, call := range c.calls {
		if len(prefixes) == 0 || slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(call, prefix) }) {
			calls = append(calls, call)
		}
	}
	return calls
}

// getSkillRequests 返回按接收顺序排列的 Skills 请求。
func (c *fakeClient) getSkillRequests() []runtimeclient.GetSkillsRequest {
	// 1. 在锁内复制 Skills 请求列表。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return slices.Clone(c.skillRequests)
}

// getForkRequests 返回按接收顺序排列的分叉请求。
func (c *fakeClient) getForkRequests() []runtimeclient.ForkConversationRequest {
	// 1. 在锁内复制分叉请求列表。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return slices.Clone(c.forkRequests)
}

// record 记录一次客户端调用。
func (c *fakeClient) record(call string) {
	// 1. 在锁内把调用文字追加到记录。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.calls = append(c.calls, call)
}

// Run 运行假客户端。
func (c *fakeClient) Run(ctx context.Context) error {
	// 1. 收到取消信号后返回上下文错误。
	<-ctx.Done()
	return ctx.Err()
}

// Events 返回假客户端的事件流。
func (c *fakeClient) Events() <-chan runtimeclient.ClientEvent {
	// 1. 读取预先建立的事件通道。
	return c.events
}

// GetConversation 到 GetTurns 这几个方法只服务 app-server，TUI 不调用，被调用时 panic。

// GetConversation 拒绝界面测试未使用的对话查询。
func (c *fakeClient) GetConversation(context.Context, defs.ConversationID) (defs.Conversation, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call GetConversation")
}

// GetSessionByID 拒绝界面测试未使用的 Session 查询。
func (c *fakeClient) GetSessionByID(context.Context, defs.SessionID) (defs.Session, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call GetSessionByID")
}

// ListSessions 拒绝界面测试未使用的 Session 列表查询。
func (c *fakeClient) ListSessions(context.Context, *runtimeclient.ListSessionsRequest) (runtimeclient.ListSessionsResult, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call ListSessions")
}

// ListItems 拒绝界面测试未使用的 Item 列表查询。
func (c *fakeClient) ListItems(context.Context, *runtimeclient.ListItemsRequest) (runtimeclient.ListItemsResult, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call ListItems")
}

// ListTurns 拒绝界面测试未使用的 Turn 列表查询。
func (c *fakeClient) ListTurns(context.Context, *runtimeclient.ListTurnsRequest) (runtimeclient.ListTurnsResult, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call ListTurns")
}

// GetTurns 拒绝界面测试未使用的 Turn 查询。
func (c *fakeClient) GetTurns(context.Context, *runtimeclient.GetTurnsRequest) ([]defs.Turn, error) {
	// 1. 调用时直接 panic。
	panic("the TUI does not call GetTurns")
}

// CreateConversation 创建测试对话。
func (c *fakeClient) CreateConversation(_ context.Context, request *runtimeclient.CreateConversationRequest) (defs.Conversation, error) {
	// 1. 记录 Agent 和模型，在锁内保存请求、递增编号并读取暂停通道与错误。
	c.record(strings.TrimSpace("CreateConversation " + request.AgentName + " " + formatFakeSelection(request.Model)))
	c.mutex.Lock()
	c.createRequests = append(c.createRequests, *request)
	c.conversationCount++
	number, gate, err := c.conversationCount, c.createGate, c.createErr
	c.mutex.Unlock()
	// 2. 等待创建放行，再返回预设错误或带请求设置的编号对话。
	if gate != nil {
		<-gate
	}
	if err != nil {
		return defs.Conversation{}, err
	}
	return defs.Conversation{
		ID:               defs.ConversationID(fmt.Sprintf("c%d", number)),
		RootSessionID:    defs.SessionID(fmt.Sprintf("s%d", number)),
		ExecutorSettings: request.ExecutorSettings,
	}, nil
}

// ListConversations 按归档状态返回预设对话。
func (c *fakeClient) ListConversations(_ context.Context, request *runtimeclient.ListConversationsRequest) (runtimeclient.ListConversationsResult, error) {
	// 1. 记录排序、条数和归档标记，在锁内筛选对话并补上根 Session。
	call := fmt.Sprintf("ListConversations %s %d", request.Order.By, request.Limit)
	if request.Archived {
		call += " archived"
	}
	c.record(call)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	var listed []dao.ConversationListEntry
	for _, each := range c.conversations {
		if (each.ArchivedAt != 0) == request.Archived {
			listed = append(listed, dao.ConversationListEntry{
				Conversation: each,
				RootSession: defs.Session{
					ID:             each.RootSessionID,
					ConversationID: each.ID,
				},
			})
		}
	}
	return runtimeclient.ListConversationsResult{Conversations: listed}, nil
}

// SetConversationTitle 拒绝界面测试未使用的标题设置。
func (c *fakeClient) SetConversationTitle(context.Context, defs.ConversationID, string) error {
	// 1. 调用时直接 panic。
	panic("the TUI does not call SetConversationTitle")
}

// ForkConversation 分叉测试对话。
func (c *fakeClient) ForkConversation(_ context.Context, request *runtimeclient.ForkConversationRequest) (defs.Conversation, error) {
	// 1. 保存请求，出错时返回错误，否则生成带来源 ID 和递增编号的新对话。
	c.record(strings.TrimSpace(fmt.Sprintf("ForkConversation %s %s", request.ConversationID, request.BeforeTurnID)))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.forkRequests = append(c.forkRequests, *request)
	if c.forkErr != nil {
		return defs.Conversation{}, c.forkErr
	}
	c.conversationCount++
	return defs.Conversation{
		ID:               defs.ConversationID(fmt.Sprintf("c%d", c.conversationCount)),
		RootSessionID:    defs.SessionID(fmt.Sprintf("s%d", c.conversationCount)),
		ForkedFromID:     request.ConversationID,
		ExecutorSettings: request.ExecutorSettings,
	}, nil
}

// SetConversationArchived 修改预设对话的归档状态。
func (c *fakeClient) SetConversationArchived(_ context.Context, request *runtimeclient.SetConversationArchivedRequest) (defs.Conversation, error) {
	// 1. 记录请求，在锁内查找对话并设置 ArchivedAt。
	c.record(fmt.Sprintf("SetConversationArchived %s %t", request.ConversationID, request.Archived))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	index := slices.IndexFunc(c.conversations, func(listed defs.Conversation) bool { return listed.ID == request.ConversationID })
	c.conversations[index].ArchivedAt = 0
	if request.Archived {
		c.conversations[index].ArchivedAt = 1
	}
	return c.conversations[index], nil
}

// DeleteConversation 删除测试对话。
func (c *fakeClient) DeleteConversation(_ context.Context, request *runtimeclient.DeleteConversationRequest) error {
	// 1. 记录请求，检查错误后从对话列表删除匹配项。
	c.record("DeleteConversation " + string(request.ConversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.conversations = slices.DeleteFunc(c.conversations, func(listed defs.Conversation) bool { return listed.ID == request.ConversationID })
	return nil
}

// SetConversationExecutorSettings 记录 Executor 设置并返回更新后的对话。
func (c *fakeClient) SetConversationExecutorSettings(_ context.Context, conversationID defs.ConversationID, settings defs.ExecutorSettings) (defs.Conversation, error) {
	// 1. 保存设置请求，出错时返回错误，否则读取预设对话或按 ID 生成记录。
	c.record("SetConversationExecutorSettings " + string(conversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.settingsRequests = append(c.settingsRequests, settings)
	if c.setSettingsErr != nil {
		return defs.Conversation{}, c.setSettingsErr
	}
	record := defs.Conversation{
		ID:            conversationID,
		RootSessionID: defs.SessionID("s" + strings.TrimPrefix(string(conversationID), "c")),
	}
	if index := slices.IndexFunc(c.conversations, func(listed defs.Conversation) bool { return listed.ID == conversationID }); index >= 0 {
		record = c.conversations[index]
	}
	record.ExecutorSettings = settings
	return record, nil
}

// SubmitInput 接收测试输入。
func (c *fakeClient) SubmitInput(_ context.Context, request *runtimeclient.SubmitInputRequest) (defs.ItemID, error) {
	// 1. 记录对话、模式和正文，保存请求并读取暂停通道与错误。
	c.record(fmt.Sprintf("SubmitInput %s %s %s", request.ConversationID, request.Mode, getFakeContentText(request.Content)))
	c.mutex.Lock()
	c.submitRequests = append(c.submitRequests, *request)
	c.itemCount++
	number, gate, err := c.itemCount, c.submitGate, c.submitErr
	c.mutex.Unlock()
	// 2. 等待发送放行，再返回预设错误或编号输入 ID。
	if gate != nil {
		<-gate
	}
	if err != nil {
		return "", err
	}
	return defs.ItemID(fmt.Sprintf("input-%d", number)), nil
}

// InterruptTurn 记录中断请求并返回预设结果。
func (c *fakeClient) InterruptTurn(_ context.Context, conversationID defs.ConversationID) (defs.TurnID, error) {
	// 1. 记录对话 ID，在锁内读取中断错误并返回 t1。
	c.record("InterruptTurn " + string(conversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return "t1", c.interruptErr
}

// CompactContext 记录压缩请求并返回预设结果。
func (c *fakeClient) CompactContext(_ context.Context, conversationID defs.ConversationID) (defs.TurnID, error) {
	// 1. 记录对话 ID，在锁内读取压缩错误并返回 t1。
	c.record("CompactContext " + string(conversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return "t1", c.compactErr
}

// OpenConversation 打开测试对话。
func (c *fakeClient) OpenConversation(_ context.Context, request *runtimeclient.OpenConversationRequest) (defs.Conversation, error) {
	// 1. 保存打开请求，检查错误后查找对话；未预设时按 ID 生成根 Session。
	conversationID := request.ConversationID
	c.record("OpenConversation " + string(conversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.openRequests = append(c.openRequests, *request)
	if c.openErr != nil {
		return defs.Conversation{}, c.openErr
	}
	for _, listed := range c.conversations {
		if listed.ID == conversationID {
			return listed, nil
		}
	}
	return defs.Conversation{
		ID:            conversationID,
		RootSessionID: defs.SessionID("s" + strings.TrimPrefix(string(conversationID), "c")),
	}, nil
}

// ListModels 记录模型列表查询并返回预设目录。
func (c *fakeClient) ListModels(_ context.Context, agentName string) (runtime.ModelList, error) {
	// 1. 记录 Agent 名，在锁内读取模型目录。
	c.record("ListModels " + agentName)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.modelList, nil
}

// GetContextUsage 记录用量查询并返回预设用量。
func (c *fakeClient) GetContextUsage(_ context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (runtime.SessionContextUsage, error) {
	// 1. 记录对话和 Session，在锁内读取用量。
	c.record("GetContextUsage " + string(conversationID) + " " + string(sessionID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.contextUsage, nil
}

// GetSessionDetails 记录详情查询并返回预设详情。
func (c *fakeClient) GetSessionDetails(_ context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (conversation.SessionDetails, error) {
	// 1. 记录对话和 Session，在锁内读取详情。
	c.record("GetSessionDetails " + string(conversationID) + " " + string(sessionID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.sessionDetails, nil
}

// SetSessionModel 设置测试 Session 的模型。
func (c *fakeClient) SetSessionModel(_ context.Context, request *runtimeclient.SetSessionModelRequest) (defs.Session, error) {
	// 1. 记录对话、Session 和模型，检查错误后生成含新选择的 Session。
	c.record(fmt.Sprintf("SetSessionModel %s %s %s", request.ConversationID, request.SessionID, formatFakeSelection(request.Model)))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.setModelErr != nil {
		return defs.Session{}, c.setModelErr
	}
	return defs.Session{
		ID:             request.SessionID,
		ConversationID: request.ConversationID,
		Model:          request.Model,
	}, nil
}

// formatFakeSelection 把模型选择写成 provider/model·effort。
func formatFakeSelection(selection defs.ModelSelection) string {
	// 1. 零值返回空，未指定强度时只返回模型引用，否则附上强度。
	if selection == (defs.ModelSelection{}) {
		return ""
	}
	if selection.ReasoningEffort == "" {
		return selection.Ref.String()
	}
	return selection.Ref.String() + "·" + selection.ReasoningEffort
}

// GetSkills 记录 Skills 请求并返回预设列表。
func (c *fakeClient) GetSkills(_ context.Context, request *runtimeclient.GetSkillsRequest) ([]skill.SkillInfo, error) {
	// 1. 在锁内保存请求并复制 Skills 列表。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.skillRequests = append(c.skillRequests, *request)
	return slices.Clone(c.skills), nil
}

// GetHooks 返回空 hook 列表。
func (c *fakeClient) GetHooks(context.Context) ([]*hook.HookInfo, error) {
	// 1. 返回空列表和 nil 错误。
	return nil, nil
}

// ListExecutors 记录 Executor 查询并返回预设列表与错误。
func (c *fakeClient) ListExecutors(context.Context) ([]conversation.ExecutorDetails, error) {
	// 1. 记录调用，在锁内复制 Executor 列表并读取错误。
	c.record("ListExecutors")
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return slices.Clone(c.executors), c.listExecutorsErr
}

// ListMCPServers 记录查询并返回预设的 MCP 服务列表。
func (c *fakeClient) ListMCPServers(_ context.Context, conversationID defs.ConversationID) ([]conversation.MCPServerInfo, error) {
	// 1. 记录对话，出错时返回空列表，否则复制 MCP 服务列表。
	c.record("ListMCPServers " + string(conversationID))
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.listMCPServersErr != nil {
		return nil, c.listMCPServersErr
	}
	return slices.Clone(c.mcpServers), nil
}

// SetClientMCPServers 接收客户端 MCP 设置并返回成功。
func (c *fakeClient) SetClientMCPServers(context.Context, defs.ConversationID, []string) error {
	// 1. 直接返回 nil。
	return nil
}

// SendClientMCPMessage 接收客户端 MCP 消息并返回成功。
func (c *fakeClient) SendClientMCPMessage(context.Context, string, json.RawMessage) error {
	// 1. 直接返回 nil。
	return nil
}

// SendClientMCPClosed 接收客户端 MCP 关闭通知并返回成功。
func (c *fakeClient) SendClientMCPClosed(context.Context, string, error) error {
	// 1. 直接返回 nil。
	return nil
}

// GetWorkspaceDirectories 返回空目录候选。
func (c *fakeClient) GetWorkspaceDirectories(_ context.Context, request *runtimeclient.GetWorkspaceDirectoriesRequest) (*conversation.WorkspaceDirectories, error) {
	// 1. 返回请求目录和空候选。
	return &conversation.WorkspaceDirectories{
		Directory:   request.Directory,
		Directories: []string{},
	}, nil
}

// ResolveWorkspaceDirectory 记录目录解析请求并返回预设目录和错误。
func (c *fakeClient) ResolveWorkspaceDirectory(_ context.Context, request *runtimeclient.ResolveWorkspaceDirectoryRequest) (string, error) {
	// 1. 在锁内保存请求，再读取解析结果。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.resolveRequests = append(c.resolveRequests, *request)
	return c.resolvedDirectory, c.resolveErr
}

// ListProviders 返回预设 Provider 列表、默认模型和空模型默认配置。
func (c *fakeClient) ListProviders(context.Context) ([]defs.ProviderConfig, defs.ModelRef, defs.ModelDefaults, error) {
	// 1. 记录查询，在锁内复制 Provider 列表并读取默认模型。
	c.record("ListProviders")
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return slices.Clone(c.providers), c.providersDefaultModel, defs.ModelDefaults{}, nil
}

// fakeProviderWrite 是 fakeClient.SetProvider 收到的一次写入。
type fakeProviderWrite struct {
	provider     defs.ProviderConfig
	defaultModel defs.ModelRef
}

// SetProvider 记录 Provider 写入并将成功写入的模型加入目录。
func (c *fakeClient) SetProvider(_ context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (runtime.SetDefinitionsResult, error) {
	// 1. 保存 Provider 和默认模型，出错时返回预设错误。
	c.record("SetProvider " + provider.ID + " " + defaultModel.String())
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.providerWrites = append(c.providerWrites, fakeProviderWrite{
		provider:     provider,
		defaultModel: defaultModel,
	})
	if c.configWriteErr != nil {
		return runtime.SetDefinitionsResult{}, c.configWriteErr
	}
	// 2. 按模型 ID 排序追加到目录，返回预设配置写入结果。
	for _, modelID := range slices.Sorted(maps.Keys(provider.Models)) {
		c.modelList.Models = append(c.modelList.Models, &defs.ModelDefinition{
			Ref: defs.ModelRef{
				ProviderID: provider.ID,
				ModelID:    modelID,
			},
			DisplayName: modelID,
		})
	}
	return c.configWriteResult, nil
}

// DeleteProvider 记录 Provider 删除并返回预设结果。
func (c *fakeClient) DeleteProvider(_ context.Context, providerID string) (runtime.SetDefinitionsResult, error) {
	// 1. 记录 Provider ID，在锁内读取配置写入结果和错误。
	c.record("DeleteProvider " + providerID)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.configWriteResult, c.configWriteErr
}

// SetDefaultModel 记录默认模型设置并返回预设结果。
func (c *fakeClient) SetDefaultModel(_ context.Context, ref defs.ModelRef) (runtime.SetDefinitionsResult, error) {
	// 1. 记录模型引用，在锁内读取配置写入结果和错误。
	c.record("SetDefaultModel " + ref.String())
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.configWriteResult, c.configWriteErr
}

// ReloadConfig 记录配置重载并返回预设结果。
func (c *fakeClient) ReloadConfig(context.Context) (runtime.SetDefinitionsResult, error) {
	// 1. 记录调用，在锁内读取配置写入结果和错误。
	c.record("ReloadConfig")
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.configWriteResult, c.configWriteErr
}

// ListCatalogProviders 查询测试目录的 Provider 列表。
func (c *fakeClient) ListCatalogProviders(context.Context) ([]defs.CatalogProvider, error) {
	// 1. 记录查询，读取暂停通道并等待它关闭。
	c.record("ListCatalogProviders")
	c.mutex.Lock()
	gate := c.catalogGate
	c.mutex.Unlock()
	if gate != nil {
		<-gate
	}
	// 2. 等待结束后读取预设错误，成功时复制目录 Provider 列表。
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.catalogProvidersErr != nil {
		return nil, c.catalogProvidersErr
	}
	return slices.Clone(c.catalogProviders), nil
}

// ListCatalogModels 查询测试目录的模型列表。
func (c *fakeClient) ListCatalogModels(_ context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 记录 Provider ID，出错时返回空数据，否则在锁内复制模型映射。
	c.record("ListCatalogModels " + catalogProviderID)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.catalogModelsErr != nil {
		return nil, c.catalogModelsErr
	}
	return maps.Clone(c.catalogModels), nil
}

// FetchProviderModels 提供测试 Provider 的模型拉取结果。
func (c *fakeClient) FetchProviderModels(_ context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 记录 Provider、协议与地址，出错时返回空数据，否则复制模型映射。
	c.record("FetchProviderModels " + provider.ID + " " + provider.Protocol + " " + provider.BaseURL)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.fetchModelsErr != nil {
		return nil, c.fetchModelsErr
	}
	return maps.Clone(c.fetchedModels), nil
}

// getFakeContentText 拼接输入内容中的文本。
func getFakeContentText(content []*defs.Content) string {
	// 1. 按顺序读取各段文字并直接连接。
	var texts []string
	for _, part := range content {
		texts = append(texts, part.Text.Text)
	}
	return strings.Join(texts, "")
}

// SteerInput 记录排队输入转补充请求。
func (c *fakeClient) SteerInput(_ context.Context, conversationID defs.ConversationID, itemID defs.ItemID, turnID defs.TurnID) error {
	// 1. 记录请求。
	c.record(fmt.Sprintf("SteerInput %s %s %s", conversationID, itemID, turnID))
	return nil
}

// InterruptTurnWithInput 记录排队输入优先请求。
func (c *fakeClient) InterruptTurnWithInput(_ context.Context, conversationID defs.ConversationID, turnID defs.TurnID, itemID defs.ItemID) error {
	// 1. 记录请求。
	c.record(fmt.Sprintf("InterruptTurnWithInput %s %s %s", conversationID, turnID, itemID))
	return nil
}

// WithdrawInput 记录原子取回或撤回请求。
func (c *fakeClient) WithdrawInput(_ context.Context, conversationID defs.ConversationID, itemID defs.ItemID, returnContent bool) ([]*defs.Content, error) {
	// 1. 记录请求并返回空内容。
	c.record(fmt.Sprintf("WithdrawInput %s %s %t", conversationID, itemID, returnContent))
	return nil, nil
}
