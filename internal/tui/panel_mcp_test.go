package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/charmbracelet/x/ansi"
)

// newMCPTestServer 创建指定 Executor 的 MCP 连接样例。
func newMCPTestServer(executorID, serverID string, state mcpclient.ConnectionState, tools ...mcpclient.ConnectionTool) conversation.MCPServerInfo {
	// 1. 写入 Executor、服务、状态和工具列表。
	return conversation.MCPServerInfo{
		ExecutorID: executorID,
		ConnectionInfo: mcpclient.ConnectionInfo{
			ServerID: serverID,
			State:    state,
			Tools:    tools,
		},
	}
}

// getMCPTestServers 返回不同 Executor 和状态的连接样例。
func getMCPTestServers() []conversation.MCPServerInfo {
	// 1. 在两个 Executor 上创建有工具、失败、连接中和无工具连接，并标记必需项。
	fake := newMCPTestServer("local", "fake", mcpclient.ConnectionStateConnected,
		mcpclient.ConnectionTool{
			Name:         "mcp__fake__echo",
			OriginalName: "echo",
			Description:  "Echo the input",
		},
		mcpclient.ConnectionTool{
			Name:         "mcp__fake__add",
			OriginalName: "add",
			Description:  "Add two numbers",
		},
	)
	broken := newMCPTestServer("local", "broken", mcpclient.ConnectionStateFailed)
	broken.Error = "dial tcp: i/o timeout"
	slow := newMCPTestServer("ssh-dev", "slow", mcpclient.ConnectionStateConnecting)
	slow.Required = true
	empty := newMCPTestServer("ssh-dev", "empty", mcpclient.ConnectionStateConnected)
	empty.Required = true
	return []conversation.MCPServerInfo{fake, broken, slow, empty}
}

// getMCPTestManyServers 返回一组已连接服务样例。
func getMCPTestManyServers() []conversation.MCPServerInfo {
	// 1. 按名称顺序创建同一 Executor 的六个已连接服务。
	var servers []conversation.MCPServerInfo
	for _, serverID := range []string{"s0", "s1", "s2", "s3", "s4", "s5"} {
		servers = append(servers, newMCPTestServer("local", serverID, mcpclient.ConnectionStateConnected))
	}
	return servers
}

// getMCPTestLongToolServers 返回工具列表较长的服务和后续空服务。
func getMCPTestLongToolServers() []conversation.MCPServerInfo {
	// 1. 创建十二个工具，再组成两个服务的连接列表。
	var tools []mcpclient.ConnectionTool
	for number := 1; number <= 12; number++ {
		tools = append(tools, mcpclient.ConnectionTool{
			OriginalName: fmt.Sprintf("tool_%02d", number),
			Description:  fmt.Sprintf("Tool %02d description", number),
		})
	}
	return []conversation.MCPServerInfo{
		newMCPTestServer("local", "serena", mcpclient.ConnectionStateConnected, tools...),
		newMCPTestServer("local", "next", mcpclient.ConnectionStateConnected),
	}
}

// getMCPTestFailedServer 返回使用指定失败原因的 MCP 连接。
func getMCPTestFailedServer(reason string) []conversation.MCPServerInfo {
	// 1. 创建失败连接并写入原因。
	server := newMCPTestServer("local", "github", mcpclient.ConnectionStateFailed)
	server.Error = reason
	return []conversation.MCPServerInfo{server}
}

// getMCPPanelBody 返回去掉边框和行尾空格的面板正文。
func getMCPPanelBody(view string) []string {
	// 1. 读取面板边框内的行并移除边框及行尾空格。
	var body []string
	for line := range strings.SplitSeq(view, "\n") {
		if text, found := strings.CutPrefix(line, "│ "); found {
			body = append(body, strings.TrimRight(strings.TrimSuffix(text, "│"), " "))
		}
	}
	return body
}

// TestMCPPanel 检查 MCP 分组、展开、滚动和错误原因显示。
func TestMCPPanel(t *testing.T) {
	// 1. 准备不同连接、工具数量、错误和终端尺寸。
	const footer = "改 mcp_servers/*.yaml 后 /reload"
	// downs 返回 count 个向下键。
	downs := func(count int) []string { return slices.Repeat([]string{"down"}, count) }
	longError := mcpclient.ConnectionInfo{
		ServerID: "broken",
		State:    mcpclient.ConnectionStateFailed,
		Error:    "dial tcp 10.20.30.40:443: i/o timeout after 30s",
	}
	narrowServers := []conversation.MCPServerInfo{
		newMCPTestServer("local", "context7", mcpclient.ConnectionStateConnected,
			mcpclient.ConnectionTool{
				OriginalName: "resolve-library-id",
				Description:  "Resolve a package name to a library id usable by get-library-docs",
			}),
		{
			ExecutorID:     "ssh-dev-with-a-very-long-name-for-the-gpu-box",
			ConnectionInfo: longError,
		},
	}
	// 2. 列出面板按键和预期正文场景。
	tests := []struct {
		name          string
		width, height int
		// servers 是 ListMCPServers 预设返回的连接。
		servers []conversation.MCPServerInfo
		// fetchErr 是 ListMCPServers 预设返回的错误。
		fetchErr error
		// isDraft 是否停在草稿、不打开对话？草稿里 /mcp 面板没有对话可查。
		isDraft bool
		// keys 是打开面板之后按的键。
		keys []string
		// wantBody 是按完 keys 后面板边框内的全部正文行。
		wantBody []string
		// wantClosed 按完 keys 后面板是否应已关闭？
		wantClosed bool
	}{
		{
			// 按 Executor 分组、按返回的顺序；状态符号与文字对应，失败原因另起一行，必需的 server 末尾标注，已连接没有工具时写 0 个工具。
			name:    "groups_by_executor_with_state_and_notes",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			wantBody: []string{
				"local",
				"› ● fake    已连接  2 个工具",
				"  ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			// Enter 展开选中 server 的工具名（server 原名）和描述，再按一次收起。
			name:    "enter_expands_tools",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			keys:    []string{"enter"},
			wantBody: []string{
				"local",
				"› ● fake    已连接  2 个工具",
				"    echo  Echo the input",
				"    add   Add two numbers",
				"  ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			name:    "second_enter_collapses_tools",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			keys:    []string{"enter", "enter"},
			wantBody: []string{
				"local",
				"› ● fake    已连接  2 个工具",
				"  ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			// 光标只落在 server 上，跳过 Executor 标题和原因行；展开的 server 没有工具时写出来，其他 server 不受影响。
			name:    "cursor_moves_over_servers_and_expands_the_one_it_is_on",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			keys:    []string{"down", "enter"},
			wantBody: []string{
				"local",
				"  ● fake    已连接  2 个工具",
				"› ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"    没有工具",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			// 光标先进入展开的工具行再进入下一个 server：跳过 Executor 标题和原因行，工具行高亮。
			name:    "cursor_walks_through_expanded_tools",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			keys:    []string{"enter", "down", "down"},
			wantBody: []string{
				"local",
				"  ● fake    已连接  2 个工具",
				"    echo  Echo the input",
				"›   add   Add two numbers",
				"  ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			name:    "cursor_leaves_the_last_tool_for_the_next_server",
			width:   80,
			height:  24,
			servers: getMCPTestServers(),
			keys:    []string{"enter", "down", "down", "down"},
			wantBody: []string{
				"local",
				"  ● fake    已连接  2 个工具",
				"    echo  Echo the input",
				"    add   Add two numbers",
				"› ✗ broken  连接失败",
				"    dial tcp: i/o timeout",
				"ssh-dev",
				"  ◌ slow    连接中  必需",
				"  ● empty   已连接  0 个工具  必需",
				footer,
			},
		},
		{
			// 展开的 server 比窗口还长（面板窗口 11 行）：光标向下走到最后一个工具，窗口跟着滚，最后一个工具可见。
			name:    "expanded_tools_taller_than_the_window_reach_the_last_tool",
			width:   80,
			height:  24,
			servers: getMCPTestLongToolServers(),
			keys:    append([]string{"enter"}, downs(12)...),
			wantBody: []string{
				"    tool_02  Tool 02 description",
				"    tool_03  Tool 03 description",
				"    tool_04  Tool 04 description",
				"    tool_05  Tool 05 description",
				"    tool_06  Tool 06 description",
				"    tool_07  Tool 07 description",
				"    tool_08  Tool 08 description",
				"    tool_09  Tool 09 description",
				"    tool_10  Tool 10 description",
				"    tool_11  Tool 11 description",
				"›   tool_12  Tool 12 description",
				footer,
			},
		},
		{
			// 光标从最后一个工具继续向下，到下一个 server 行。
			name:    "cursor_moves_from_the_last_tool_to_the_next_server",
			width:   80,
			height:  24,
			servers: getMCPTestLongToolServers(),
			keys:    append([]string{"enter"}, downs(13)...),
			wantBody: []string{
				"    tool_03  Tool 03 description",
				"    tool_04  Tool 04 description",
				"    tool_05  Tool 05 description",
				"    tool_06  Tool 06 description",
				"    tool_07  Tool 07 description",
				"    tool_08  Tool 08 description",
				"    tool_09  Tool 09 description",
				"    tool_10  Tool 10 description",
				"    tool_11  Tool 11 description",
				"    tool_12  Tool 12 description",
				"› ● next    已连接  0 个工具",
				footer,
			},
		},
		{
			// End 直接到最后一行（下一个 server），再向上一格回到最后一个工具。
			name:    "up_from_the_next_server_returns_to_the_last_tool",
			width:   80,
			height:  24,
			servers: getMCPTestLongToolServers(),
			keys:    []string{"enter", "end", "up"},
			wantBody: []string{
				"    tool_03  Tool 03 description",
				"    tool_04  Tool 04 description",
				"    tool_05  Tool 05 description",
				"    tool_06  Tool 06 description",
				"    tool_07  Tool 07 description",
				"    tool_08  Tool 08 description",
				"    tool_09  Tool 09 description",
				"    tool_10  Tool 10 description",
				"    tool_11  Tool 11 description",
				"›   tool_12  Tool 12 description",
				"  ● next    已连接  0 个工具",
				footer,
			},
		},
		{
			// 光标一路滚到工具行之后向上回到工具列表开头：窗口跟着滚回去，server 行和 Executor 标题重新出现。
			name:    "cursor_back_at_the_server_row_scrolls_the_header_into_view",
			width:   80,
			height:  24,
			servers: getMCPTestLongToolServers(),
			keys:    append(append([]string{"enter"}, downs(12)...), slices.Repeat([]string{"up"}, 12)...),
			wantBody: []string{
				"local",
				"› ● serena  已连接  12 个工具",
				"    tool_01  Tool 01 description",
				"    tool_02  Tool 02 description",
				"    tool_03  Tool 03 description",
				"    tool_04  Tool 04 description",
				"    tool_05  Tool 05 description",
				"    tool_06  Tool 06 description",
				"    tool_07  Tool 07 description",
				"    tool_08  Tool 08 description",
				"    tool_09  Tool 09 description",
				footer,
			},
		},
		{
			// 光标在工具行上按 Enter 收起所属的 server，光标回到它的 server 行，列表恢复成未展开的样子。
			name:    "enter_on_a_tool_row_collapses_its_server",
			width:   80,
			height:  24,
			servers: getMCPTestLongToolServers(),
			keys:    append(append([]string{"enter"}, downs(12)...), "enter"),
			wantBody: []string{
				"local",
				"› ● serena  已连接  12 个工具",
				"  ● next    已连接  0 个工具",
				footer,
			},
		},
		{
			// 40 列宽：工具行高亮时同样按宽度截断。
			name:    "narrow_width_truncates_the_selected_tool_row",
			width:   40,
			height:  24,
			servers: narrowServers,
			keys:    []string{"enter", "down"},
			wantBody: []string{
				"local",
				"  ● context7  已连接  1 个工具",
				"›   resolve-library-id  Resolve a p…",
				"ssh-dev-with-a-very-long-name-for-t…",
				"  ✗ broken    连接失败",
				"    dial tcp 10.20.30.40:443: i/o",
				"    timeout after 30s",
				footer,
			},
		},
		{
			name:       "esc_closes",
			width:      80,
			height:     24,
			servers:    getMCPTestServers(),
			keys:       []string{"esc"},
			wantClosed: true,
		},
		{
			// 40 列宽（面板内容 36 列）：描述和过长的 Executor 名按显示宽度截断，失败原因折行，光标在 server 上时原因的每一行都在窗口内。
			name:    "narrow_width_truncates_names_and_wraps_the_reason",
			width:   40,
			height:  24,
			servers: narrowServers,
			keys:    []string{"enter", "down", "down"},
			wantBody: []string{
				"local",
				"  ● context7  已连接  1 个工具",
				"    resolve-library-id  Resolve a p…",
				"ssh-dev-with-a-very-long-name-for-t…",
				"› ✗ broken    连接失败",
				"    dial tcp 10.20.30.40:443: i/o",
				"    timeout after 30s",
				footer,
			},
		},
		{
			// 100 列宽时原因折成两行，stderr 结尾显示在第二行，续行使用相同缩进。
			name:    "wide_reason_wraps_to_a_second_line_keeping_the_stderr_tail",
			width:   100,
			height:  24,
			servers: getMCPTestFailedServer("MCP server process exited with code 1; stderr: github-mcp-server cannot start the session: missing GITHUB_TOKEN"),
			wantBody: []string{
				"local",
				"› ✗ github  连接失败",
				"    MCP server process exited with code 1; stderr: github-mcp-server cannot start the session:",
				"    missing GITHUB_TOKEN",
				footer,
			},
		},
		{
			// 恰好折成三行时不加省略号。
			name:    "reason_filling_exactly_three_lines_has_no_ellipsis",
			width:   40,
			height:  24,
			servers: getMCPTestFailedServer("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo"),
			wantBody: []string{
				"local",
				"› ✗ github  连接失败",
				"    alpha bravo charlie delta echo",
				"    foxtrot golf hotel india juliet",
				"    kilo",
				footer,
			},
		},
		{
			// 40 列宽：很长的原因最多占三行，第三行以 "…" 结尾，放不下的部分丢弃，不再多占行。
			name:    "very_long_reason_ends_in_an_ellipsis_on_the_third_line",
			width:   40,
			height:  24,
			servers: getMCPTestFailedServer("MCP server process exited with code 1; stderr: failed to read /home/dev/.config/github-mcp/config.yaml: no such file or directory; run github-mcp-server login first to create it, then retry; missing GITHUB_TOKEN"),
			wantBody: []string{
				"local",
				"› ✗ github  连接失败",
				"    MCP server process exited with",
				"    code 1; stderr: failed to read",
				"    /home/dev/.config/github-…",
				footer,
			},
		},
		{
			// 中文原因按显示宽度折行：宽字符占两列，不会超出面板。
			name:    "wide_characters_reason_wraps_by_display_width",
			width:   40,
			height:  24,
			servers: getMCPTestFailedServer("MCP server process exited with code 1; stderr: 缺少环境变量 GITHUB_TOKEN，请先配置令牌再重新连接"),
			wantBody: []string{
				"local",
				"› ✗ github  连接失败",
				"    MCP server process exited with",
				"    code 1; stderr: 缺少环境变量",
				"    GITHUB_TOKEN，请先配置令牌再重…",
				footer,
			},
		},
		{
			// 对话还没创建（草稿）：不发请求，只提示；底部仍说明怎么改配置。
			name:     "draft_has_no_connections_yet",
			width:    80,
			height:   24,
			isDraft:  true,
			wantBody: []string{"对话开始后才有 MCP 连接", footer},
		},
		{
			name:     "open_conversation_without_servers",
			width:    80,
			height:   24,
			wantBody: []string{"没有 MCP 连接", footer},
		},
		{
			name:     "fetch_failure_shows_the_error",
			width:    80,
			height:   24,
			fetchErr: errors.New("boom"),
			wantBody: []string{"✗ boom", footer},
		},
		{
			// 面板只有 4 行放列表：光标移到最后一个 server 时窗口跟着滚动，底部提示始终可见。
			name:    "short_terminal_scrolls_to_the_cursor",
			width:   80,
			height:  12,
			servers: getMCPTestManyServers(),
			keys:    []string{"end"},
			wantBody: []string{
				"  ● s2  已连接  0 个工具",
				"  ● s3  已连接  0 个工具",
				"  ● s4  已连接  0 个工具",
				"› ● s5  已连接  0 个工具",
				footer,
			},
		},
	}
	// 3. 打开面板，逐键操作并重绘，比较正文、宽度和关闭结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, tt.width, tt.height, func(c *fakeClient) { c.mcpServers, c.listMCPServersErr = tt.servers, tt.fetchErr })
			if !tt.isDraft {
				h.openDraftConversation("开场")
			}
			h.typeText("/mcp")
			h.press("enter")
			opened := h.model.activePanel.(*mcpPanel)
			h.waitFor("mcp panel loaded", func() bool { return !opened.fetch.isLoading })
			for _, key := range tt.keys {
				h.press(key)
				// 每次按键后重绘，更新随光标移动的滚动位置。
				h.view()
			}
			if tt.wantClosed {
				if h.model.activePanel != nil {
					t.Fatalf("panel after %v = %T, want closed", tt.keys, h.model.activePanel)
				}
				return
			}
			view := h.view()
			if body := getMCPPanelBody(view); !slices.Equal(body, tt.wantBody) {
				t.Fatalf("panel body:\n%s\nwant:\n%s\nview:\n%s", strings.Join(body, "\n"), strings.Join(tt.wantBody, "\n"), view)
			}
			for line := range strings.SplitSeq(view, "\n") {
				if width := ansi.StringWidth(line); width > tt.width {
					t.Fatalf("line %q is %d columns wide, want at most %d", line, width, tt.width)
				}
			}
			if title := strings.Split(view, "\n")[0]; !strings.Contains(title, "MCP（当前对话）") {
				t.Fatalf("title row = %q", title)
			}
		})
	}
}
