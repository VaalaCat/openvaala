package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// actionSettleTimeout 是动作之后等界面画完的最长时间；流式输出一直在画时到点就返回。
	actionSettleTimeout = 3 * time.Second
	// pollInterval 是 wait 和重新定位控件时重读控件树的间隔。
	pollInterval = 100 * time.Millisecond
)

// desktopSession 是一次子命令对运行中桌面的连接，以及它需要的窗口内容大小。
type desktopSession struct {
	client *automationClient
	state  *driveState
	// content 是窗口内容区的大小，用来判断控件是否在窗口内。
	content automationBounds
}

// connectDesktop 读状态文件并连上桌面的自动化 socket，同时取窗口内容大小。
func connectDesktop(statePath string) (*desktopSession, error) {
	// 1. 状态文件在但 daemon 已经不在时，提示先 stop 再 start。
	state, err := loadState(statePath)
	if err != nil {
		return nil, err
	}
	if !isProcessAlive(state.DaemonPID) {
		return nil, fmt.Errorf("the desktop session is gone (daemon %d exited, see %s); run `desktopdrive stop` then `start`", state.DaemonPID, state.LogDirectory)
	}
	client, err := dialAutomation(state.Socket)
	if err != nil {
		return nil, fmt.Errorf("connect desktop automation: %w", err)
	}
	// 2. 取焦点窗口（没有焦点时第一个）的内容大小。
	windows, err := client.getWindows()
	if err != nil {
		return nil, err
	}
	session := &desktopSession{client: client, state: state, content: automationBounds{Width: float64(state.Width), Height: float64(state.Height)}}
	for _, window := range windows {
		if window.Kind == "content" && (window.Focused || session.content.Width == 0) {
			session.content = window.ContentBounds
		}
	}
	return session, nil
}

// runDesktopCommand 执行驱动桌面的子命令：连上桌面、解析参数、做动作；动作命令之后等界面画完。
func runDesktopCommand(command string, arguments []string) error {
	// 1. 各命令自己的 flag。
	flags, statePath := newFlags(command)
	all := flags.Bool("all", false, "tree: print every element, including invisible and empty containers")
	maxLength := flags.Int("max", 100, "tree, find: truncate label, value and description to this many characters, 0 for no limit")
	nth := flags.Int("nth", 0, "press, focus, set: choose the Nth match, counting from 1")
	timeout := flags.Duration("timeout", 0, "press, focus, set: how long to wait for the element; wait: how long to wait for the text; idle: how long to wait for the window to settle")
	right := flags.Bool("right", false, "click: use the right mouse button")
	clicks := flags.Int("clicks", 1, "click: number of quick presses, 2 for a double click")
	positional, err := parseFlags(flags, arguments)
	if err != nil {
		return err
	}
	// 2. 连上桌面后按命令分发。
	session, err := connectDesktop(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = session.client.close() }()
	target := targetOption{nth: *nth, timeout: *timeout}
	switch command {
	case "windows":
		return session.runWindows()
	case "tree":
		return session.runTree(*all, *maxLength)
	case "find":
		return session.runFind(positional, *maxLength)
	case "press":
		return session.runPress(positional, target)
	case "focus":
		return session.runFocus(positional, target)
	case "set":
		return session.runSet(positional, target)
	case "type":
		return session.runType(positional)
	case "key":
		return session.runKey(positional)
	case "click":
		return session.runClick(positional, *right, *clicks)
	case "scroll":
		return session.runScroll(positional)
	case "menu":
		return session.runMenu(positional)
	case "wait":
		return session.runWait(positional, *timeout)
	case "idle":
		return session.runIdle(*timeout)
	case "shot":
		return session.runShot(positional)
	}
	return fmt.Errorf("unknown command %q; run `desktopdrive help`", command)
}

// targetOption 是按文字定位控件的选项：nth 从 1 数，0 表示必须唯一；timeout 为 0 时用默认等待。
type targetOption struct {
	nth     int
	timeout time.Duration
}

// finish 等界面画完，再打印动作摘要；一直在动画时在摘要后注明。
func (s *desktopSession) finish(summary string) error {
	// 1. 等待画完。
	settled, err := s.client.settle(actionSettleTimeout)
	if isDesktopGone(err) {
		fmt.Println(summary + " (desktop exited; the session is cleaned up)")
		return nil
	}
	if err != nil {
		return err
	}
	if !settled {
		summary += " (ui still animating)"
	}
	fmt.Println(summary)
	return nil
}

// isDesktopGone 错误是否说明桌面已经退出（例如动作触发了应用退出）？
func isDesktopGone(err error) bool {
	// 1. 连接被对端关闭时读到 EOF。
	return err != nil && strings.Contains(err.Error(), "EOF")
}

// runWindows 列出窗口。
func (s *desktopSession) runWindows() error {
	// 1. 每个窗口一行。
	windows, err := s.client.getWindows()
	if err != nil {
		return err
	}
	for _, window := range windows {
		fmt.Printf("window %d %q %s %d×%d focused=%v visible=%v\n", window.ID, window.Title, window.Kind, int(window.ContentBounds.Width), int(window.ContentBounds.Height), window.Focused, window.Visible)
	}
	return nil
}

// runTree 把控件树打印成缩进文本。
func (s *desktopSession) runTree(all bool, maxLength int) error {
	// 1. 读树，算出要打印的控件和缩进。
	tree, err := s.client.getTree()
	if err != nil {
		return err
	}
	depths, shown := getNodeDepths(tree, s.content, all)
	shortIDs := getShortIDs(tree.Nodes)
	// 2. 打印标题行和每个控件。
	focus := "none"
	if id, ok := shortIDs[tree.Focus]; ok {
		focus = "[" + id + "]"
	}
	fmt.Printf("# window %d %d×%d focus=%s shown %d of %d elements\n", tree.Window, int(s.content.Width), int(s.content.Height), focus, len(shown), len(tree.Nodes))
	for _, node := range shown {
		fmt.Println(strings.Repeat("  ", depths[node.ID]) + formatNode(node, shortIDs[node.ID], tree.Focus, maxLength, all))
	}
	return nil
}

// runFind 列出含有文字的控件。
func (s *desktopSession) runFind(positional []string, maxLength int) error {
	// 1. 在整棵树里找，包含屏幕外的控件。
	if len(positional) != 1 {
		return fmt.Errorf("usage: find <text>")
	}
	tree, err := s.client.getTree()
	if err != nil {
		return err
	}
	shortIDs := getShortIDs(tree.Nodes)
	count := 0
	for index := range tree.Nodes {
		node := &tree.Nodes[index]
		if containsText(node, positional[0]) {
			fmt.Println(formatNode(node, shortIDs[node.ID], tree.Focus, maxLength, true))
			count++
		}
	}
	if count == 0 {
		return fmt.Errorf("no element contains %q", positional[0])
	}
	return nil
}

// pickTargets 在树里按文字找可以执行动作的控件：先看 key，再看 label，再按包含匹配；
// 同一等级里优先窗口内的、没禁用的。没有任何控件支持动作时返回 isClickFallback 的结果：按文字匹配的任意控件。
func pickTargets(tree *automationTree, content automationBounds, query, action string) (nodes []*automationNode, isClickFallback bool) {
	// 1. #id 直接按 ID 后缀选。
	if id, ok := strings.CutPrefix(query, "#"); ok {
		for index := range tree.Nodes {
			if strings.HasSuffix(tree.Nodes[index].ID, id) {
				nodes = append(nodes, &tree.Nodes[index])
			}
		}
		return nodes, false
	}
	// 2. 先要求支持动作，没有再退回任意控件（用点击中心完成）；set 不退回。
	for _, requireAction := range []bool{true, false} {
		if !requireAction && action == "setValue" {
			break
		}
		for tier := 1; tier <= 3; tier++ {
			var matched []*automationNode
			for index := range tree.Nodes {
				node := &tree.Nodes[index]
				if matchTier(node, query) == tier && (!requireAction || node.hasAction(action)) {
					matched = append(matched, node)
				}
			}
			if len(matched) > 0 {
				return narrowTargets(matched, content), !requireAction
			}
		}
	}
	return nil, false
}

// narrowTargets 在同等级的候选里优先保留窗口内的，再优先保留没禁用的。
func narrowTargets(nodes []*automationNode, content automationBounds) []*automationNode {
	// 1. 每一步只在能留下至少一个时才过滤。
	for _, keep := range []func(*automationNode) bool{
		func(n *automationNode) bool { return isNodeOnScreen(n, content) },
		func(n *automationNode) bool { return !n.hasState("disabled") },
	} {
		var kept []*automationNode
		for _, node := range nodes {
			if keep(node) {
				kept = append(kept, node)
			}
		}
		if len(kept) > 0 {
			nodes = kept
		}
	}
	return nodes
}

// resolveTarget 等控件出现并选出唯一目标；多个匹配且没给 nth 时立即报错并列出候选。
// 返回的 tree 和目标取自同一帧；isClickFallback 为 true 表示控件不支持该动作。
func (s *desktopSession) resolveTarget(query, action string, option targetOption) (node *automationNode, tree *automationTree, isClickFallback bool, err error) {
	// 1. 先等界面画完，避免点到上一帧的控件。
	if _, err := s.client.settle(time.Second); err != nil {
		return nil, nil, false, err
	}
	timeout := option.timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		// 2. 读树并按文字选候选。
		tree, err = s.client.getTree()
		if err != nil {
			return nil, nil, false, err
		}
		candidates, fallback := pickTargets(tree, s.content, query, action)
		switch {
		case len(candidates) == 1 && option.nth <= 1, len(candidates) > 0 && option.nth > 0 && option.nth <= len(candidates):
			choice := 0
			if option.nth > 0 {
				choice = option.nth - 1
			}
			return candidates[choice], tree, fallback, nil
		case len(candidates) > 1 && option.nth == 0:
			return nil, nil, false, describeAmbiguity(tree, candidates, query)
		case len(candidates) > 0:
			return nil, nil, false, fmt.Errorf("--nth %d is out of range: %q matches %d elements", option.nth, query, len(candidates))
		}
		// 3. 没找到：界面可能还在出现，到期前重试。
		if time.Now().After(deadline) {
			return nil, nil, false, fmt.Errorf("no element matches %q (waited %s); try `desktopdrive find %q` or `tree --all`", query, timeout, query)
		}
		time.Sleep(pollInterval)
	}
}

// describeAmbiguity 返回多个控件匹配时的错误，列出每个候选。
func describeAmbiguity(tree *automationTree, candidates []*automationNode, query string) error {
	// 1. 每个候选带序号和短 ID，方便选 --nth 或 #id。
	shortIDs := getShortIDs(tree.Nodes)
	var builder strings.Builder
	fmt.Fprintf(&builder, "%q matches %d elements; choose one with --nth N or #id:", query, len(candidates))
	for index, node := range candidates {
		fmt.Fprintf(&builder, "\n  %d. %s", index+1, formatNode(node, shortIDs[node.ID], tree.Focus, 60, true))
	}
	return fmt.Errorf("%s", builder.String())
}

// act 对控件执行无障碍动作；控件在虚拟列表里被换掉时（no_element）重新定位后再试，直到期限。
func (s *desktopSession) act(query, action, text string, option targetOption) (*automationNode, error) {
	// 1. 定位、滚到可见、执行；仅当框架确认动作没执行（no_element）时重试。
	deadline := time.Now().Add(max(option.timeout, 3*time.Second))
	for {
		node, tree, isClickFallback, err := s.resolveTarget(query, action, option)
		if err != nil {
			return nil, err
		}
		if node.hasState("disabled") {
			return nil, fmt.Errorf("%s is disabled", formatNode(node, getShortIDs(tree.Nodes)[node.ID], tree.Focus, 60, true))
		}
		_ = s.client.call("act", map[string]any{"id": node.ID, "action": "scrollIntoView"}, nil)
		if isClickFallback {
			return node, s.clickCenter(node)
		}
		params := map[string]any{"id": node.ID, "action": action}
		if action == "setValue" {
			params["text"] = text
		}
		err = s.client.call("act", params, nil)
		if err == nil {
			return node, nil
		}
		if !isAutomationCode(err, "no_element") || time.Now().After(deadline) {
			return nil, err
		}
	}
}

// clickCenter 点击控件当前帧的中心；先重读一次位置，因为滚动会改变它。
func (s *desktopSession) clickCenter(node *automationNode) error {
	// 1. 滚动后重读位置，找不到就用旧位置。
	bounds := node.Bounds
	if tree, err := s.client.getTree(); err == nil {
		for index := range tree.Nodes {
			if tree.Nodes[index].ID == node.ID {
				bounds = tree.Nodes[index].Bounds
			}
		}
	}
	return s.client.sendInput(map[string]any{"type": "click", "x": bounds.X + bounds.Width/2, "y": bounds.Y + bounds.Height/2})
}

// describeNode 返回动作摘要里用的控件描述：role 和 label（或 key）。
func describeNode(node *automationNode) string {
	// 1. label 优先，其次 key，再次 placeholder。
	name := node.Label
	if name == "" {
		name = node.keyString()
	}
	if name == "" {
		name = node.Placeholder
	}
	return node.Role + " " + strconv.Quote(name)
}

// runPress 按控件。
func (s *desktopSession) runPress(positional []string, option targetOption) error {
	// 1. 参数是定位文字。
	if len(positional) != 1 {
		return fmt.Errorf("usage: press <label-or-key|#id> [--nth N]")
	}
	node, err := s.act(positional[0], "press", "", option)
	if err != nil {
		return err
	}
	return s.finish("pressed " + describeNode(node))
}

// runFocus 给控件键盘焦点。
func (s *desktopSession) runFocus(positional []string, option targetOption) error {
	// 1. 参数是定位文字。
	if len(positional) != 1 {
		return fmt.Errorf("usage: focus <label-or-key|#id> [--nth N]")
	}
	node, err := s.act(positional[0], "focus", "", option)
	if err != nil {
		return err
	}
	return s.finish("focused " + describeNode(node))
}

// runSet 整体替换文本输入框的内容，并核对设置后的值。
func (s *desktopSession) runSet(positional []string, option targetOption) error {
	// 1. 第一个参数定位输入框，其余用空格拼成文字（允许为空串来清空）。
	if len(positional) < 2 {
		return fmt.Errorf("usage: set <label-or-key|#id> <text> [--nth N]")
	}
	text := strings.Join(positional[1:], " ")
	node, err := s.act(positional[0], "setValue", text, option)
	if err != nil {
		return err
	}
	if err := s.finish("set " + describeNode(node)); err != nil {
		return err
	}
	// 2. 值没改成预期（例如控件改写了输入）时提醒。
	tree, err := s.client.getTree()
	if err != nil {
		return err
	}
	for index := range tree.Nodes {
		if tree.Nodes[index].ID == node.ID && tree.Nodes[index].Value != text && !tree.Nodes[index].hasState("password") {
			fmt.Printf("note: value is now %s\n", quoteText(tree.Nodes[index].Value, 100))
		}
	}
	return nil
}

// runType 把文字输入给焦点控件。
func (s *desktopSession) runType(positional []string) error {
	// 1. 多个参数用空格拼起来。
	if len(positional) == 0 {
		return fmt.Errorf("usage: type <text>")
	}
	text := strings.Join(positional, " ")
	if err := s.client.sendInput(map[string]any{"type": "text", "text": text}); err != nil {
		return err
	}
	return s.finish("typed " + quoteText(text, 60))
}

// runKey 按键或组合键。
func (s *desktopSession) runKey(positional []string) error {
	// 1. 写法同菜单加速键，由自动化插件解析。
	if len(positional) != 1 {
		return fmt.Errorf("usage: key <combo>, for example Enter, Escape, shift+tab, ctrl+k")
	}
	if err := s.client.sendInput(map[string]any{"type": "key", "key": positional[0]}); err != nil {
		return err
	}
	return s.finish("key " + positional[0])
}

// runClick 在窗口内容坐标点击。
func (s *desktopSession) runClick(positional []string, right bool, clicks int) error {
	// 1. 解析坐标。
	if len(positional) != 2 {
		return fmt.Errorf("usage: click <x> <y> [--right] [--clicks 2]")
	}
	x, y, err := parsePoint(positional[0], positional[1])
	if err != nil {
		return err
	}
	event := map[string]any{"type": "click", "x": x, "y": y, "clicks": clicks}
	button := "left"
	if right {
		event["button"] = "right"
		button = "right"
	}
	if err := s.client.sendInput(event); err != nil {
		return err
	}
	return s.finish(fmt.Sprintf("clicked (%g,%g) %s ×%d", x, y, button, clicks))
}

// runScroll 在坐标处滚动滚轮。
func (s *desktopSession) runScroll(positional []string) error {
	// 1. 解析坐标和距离。
	if len(positional) != 3 {
		return fmt.Errorf("usage: scroll <x> <y> <dy>")
	}
	x, y, err := parsePoint(positional[0], positional[1])
	if err != nil {
		return err
	}
	dy, err := strconv.ParseFloat(positional[2], 64)
	if err != nil {
		return err
	}
	if err := s.client.sendInput(map[string]any{"type": "wheel", "x": x, "y": y, "dx": 0, "dy": dy}); err != nil {
		return err
	}
	return s.finish(fmt.Sprintf("scrolled (%g,%g) dy=%g", x, y, dy))
}

// parsePoint 解析一对坐标。
func parsePoint(xText, yText string) (float64, float64, error) {
	// 1. 两个都必须是数字。
	x, err := strconv.ParseFloat(xText, 64)
	if err != nil {
		return 0, 0, err
	}
	y, err := strconv.ParseFloat(yText, 64)
	return x, y, err
}

// runMenu 选应用菜单项。
func (s *desktopSession) runMenu(positional []string) error {
	// 1. 路径上的标签是菜单栏里显示的文字，语言跟随桌面语言。
	if len(positional) == 0 {
		return fmt.Errorf("usage: menu <path...>, for example: menu 前往 设置")
	}
	if err := s.client.call("menu", map[string]any{"path": positional}, nil); err != nil && !isDesktopGone(err) {
		return err
	}
	return s.finish("menu " + strings.Join(positional, " > "))
}

// runWait 轮询控件树直到有控件含有文字。
func (s *desktopSession) runWait(positional []string, timeout time.Duration) error {
	// 1. 默认等 10 秒。
	if len(positional) != 1 {
		return fmt.Errorf("usage: wait <text> [--timeout 10s]")
	}
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		// 2. 每次读树，命中就打印前几个控件。
		tree, err := s.client.getTree()
		if err != nil {
			return err
		}
		shortIDs := getShortIDs(tree.Nodes)
		printed := 0
		for index := range tree.Nodes {
			if node := &tree.Nodes[index]; containsText(node, positional[0]) {
				if printed == 0 {
					fmt.Println("found:")
				}
				if printed++; printed <= 3 {
					fmt.Println("  " + formatNode(node, shortIDs[node.ID], tree.Focus, 100, true))
				}
			}
		}
		if printed > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no element contains %q after %s", positional[0], timeout)
		}
		time.Sleep(pollInterval)
	}
}

// runIdle 等窗口画完。
func (s *desktopSession) runIdle(timeout time.Duration) error {
	// 1. 默认等 5 秒；超时只说明还在画，不算失败。
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	settled, err := s.client.settle(timeout)
	if err != nil {
		return err
	}
	if settled {
		fmt.Println("idle")
	} else {
		fmt.Printf("not idle after %s (animation or streaming output keeps redrawing)\n", timeout)
	}
	return nil
}

// runShot 截图保存为 PNG 并打印路径。
func (s *desktopSession) runShot(positional []string) error {
	// 1. 确定输出路径：默认在 shots/ 里按序号递增。
	if len(positional) > 1 {
		return fmt.Errorf("usage: shot [file]")
	}
	path := ""
	if len(positional) == 1 {
		path = positional[0]
	} else {
		var err error
		if path, err = getNextShotPath(s.state.ShotsDirectory); err != nil {
			return err
		}
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	// 2. 短暂等画完，流式输出时不强求。
	if _, err := s.client.settle(500 * time.Millisecond); err != nil {
		return err
	}
	var image struct {
		PNG    string `json:"png"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if err := s.client.call("capture", nil, &image); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(image.PNG)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	fmt.Printf("%s (%d×%d)\n", path, image.Width, image.Height)
	return nil
}

// getNextShotPath 返回目录里下一个未用的截图路径 <序号>.png，序号从 001 递增。
func getNextShotPath(directory string) (string, error) {
	// 1. 目录不存在时先建。
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	next := 1
	for _, entry := range entries {
		if number, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".png")); err == nil && number >= next {
			next = number + 1
		}
	}
	return filepath.Join(directory, fmt.Sprintf("%03d.png", next)), nil
}
