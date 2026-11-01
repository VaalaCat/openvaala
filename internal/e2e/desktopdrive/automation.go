package main

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// callTimeout 是单次自动化请求的最长等待；截图等大结果也在这个期限内读完。
const callTimeout = 30 * time.Second

// automationClient 是到 MyGo 原生自动化 socket 的顺序 NDJSON 连接；每个子命令自己建一条。
type automationClient struct {
	connection net.Conn
	encoder    *json.Encoder
	decoder    *json.Decoder
	nextID     int
}

// automationError 是自动化协议返回的错误，Code 可以用来判断，例如 no_element、timeout。
type automationError struct {
	Method  string
	Code    string
	Message string
}

// Error 返回带方法和错误码的错误文字。
func (e *automationError) Error() string {
	return fmt.Sprintf("automation %s: %s: %s", e.Method, e.Code, e.Message)
}

// isAutomationCode 错误是否是带指定错误码的自动化协议错误？
func isAutomationCode(err error, code string) bool {
	automation, ok := err.(*automationError)
	return ok && automation.Code == code
}

// automationBounds 是窗口内容坐标下的矩形，单位 DIP。
type automationBounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// automationNode 是无障碍树里的一个控件。
type automationNode struct {
	ID          string           `json:"id"`
	Parent      string           `json:"parent"`
	Bounds      automationBounds `json:"bounds"`
	Role        string           `json:"role"`
	Key         any              `json:"key"`
	Label       string           `json:"label"`
	Value       string           `json:"value"`
	Placeholder string           `json:"placeholder"`
	Description string           `json:"description"`
	States      []string         `json:"states"`
	Actions     []string         `json:"actions"`
}

// automationTree 是 tree 方法的结果。
type automationTree struct {
	Window int              `json:"window"`
	Focus  string           `json:"focus"`
	Nodes  []automationNode `json:"nodes"`
}

// automationWindow 是 windows 方法列出的一个窗口。
type automationWindow struct {
	ID            int              `json:"id"`
	Title         string           `json:"title"`
	Kind          string           `json:"kind"`
	Focused       bool             `json:"focused"`
	Visible       bool             `json:"visible"`
	ContentBounds automationBounds `json:"contentBounds"`
}

// keyString 返回控件的 Key 文字，没有 Key 时为空。
func (n *automationNode) keyString() string {
	if n.Key == nil {
		return ""
	}
	return fmt.Sprint(n.Key)
}

// hasState 控件是否带有指定状态？
func (n *automationNode) hasState(state string) bool {
	return slices.Contains(n.States, state)
}

// hasAction 控件是否支持指定动作？
func (n *automationNode) hasAction(action string) bool {
	return slices.Contains(n.Actions, action)
}

// dialAutomation 连接自动化 socket。
func dialAutomation(socket string) (*automationClient, error) {
	// 1. 连接失败时由调用方决定是否重试。
	connection, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return nil, err
	}
	return &automationClient{connection: connection, encoder: json.NewEncoder(connection), decoder: json.NewDecoder(connection)}, nil
}

// close 关闭连接。
func (c *automationClient) close() error {
	return c.connection.Close()
}

// call 发送一条请求，校验响应身份并把结果解码到 result；result 为 nil 时只检查错误。
func (c *automationClient) call(method string, params, result any) error {
	// 1. 每次请求设置期限后写出。
	c.nextID++
	if err := c.connection.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return err
	}
	request := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{c.nextID, method, params}
	if err := c.encoder.Encode(request); err != nil {
		return fmt.Errorf("automation %s write: %w", method, err)
	}
	// 2. 读响应：身份不一致或带 error 都不算成功。
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.decoder.Decode(&response); err != nil {
		return fmt.Errorf("automation %s read: %w", method, err)
	}
	if response.ID != request.ID {
		return fmt.Errorf("automation %s response id %d, want %d", method, response.ID, request.ID)
	}
	if response.Error != nil {
		return &automationError{Method: method, Code: response.Error.Code, Message: response.Error.Message}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

// getWindows 返回当前所有窗口。
func (c *automationClient) getWindows() ([]automationWindow, error) {
	// 1. 窗口列表不需要参数。
	var result struct {
		Windows []automationWindow `json:"windows"`
	}
	err := c.call("windows", nil, &result)
	return result.Windows, err
}

// getTree 返回焦点窗口最近一帧的控件树。
func (c *automationClient) getTree() (*automationTree, error) {
	// 1. 不传 window，由程序选焦点窗口。
	var tree automationTree
	if err := c.call("tree", nil, &tree); err != nil {
		return nil, err
	}
	return &tree, nil
}

// settle 等窗口画完并返回是否真的停了；动画和流式输出一直在画时超时，这不算错误。
func (c *automationClient) settle(timeout time.Duration) (bool, error) {
	// 1. timeout 错误码表示还在画。
	err := c.call("idle", map[string]any{"timeout": timeout.Milliseconds()}, nil)
	if isAutomationCode(err, "timeout") {
		return false, nil
	}
	return err == nil, err
}

// sendInput 把输入事件按顺序发给窗口内容。
func (c *automationClient) sendInput(events ...map[string]any) error {
	// 1. 一次请求内的事件在同一帧之前被处理。
	return c.call("input", map[string]any{"events": events}, nil)
}

// getShortIDs 返回每个控件 ID 的最短唯一后缀（至少 4 位），用来在 tree 输出里省 token。
func getShortIDs(nodes []automationNode) map[string]string {
	// 1. 逐个长度统计后缀出现次数，取第一个唯一的长度。
	shortIDs := make(map[string]string, len(nodes))
	for length := 4; length <= 20; length++ {
		counts := map[string]int{}
		for _, node := range nodes {
			counts[suffix(node.ID, length)]++
		}
		for _, node := range nodes {
			if _, done := shortIDs[node.ID]; !done && counts[suffix(node.ID, length)] == 1 {
				shortIDs[node.ID] = suffix(node.ID, length)
			}
		}
	}
	for _, node := range nodes {
		if _, done := shortIDs[node.ID]; !done {
			shortIDs[node.ID] = node.ID
		}
	}
	return shortIDs
}

// suffix 返回文字的最后 length 个字节，不足时返回全部。
func suffix(text string, length int) string {
	return text[max(0, len(text)-length):]
}

// isNodeOnScreen 控件是否在窗口内容范围内、有大小，且没有被标成屏幕外？
func isNodeOnScreen(node *automationNode, content automationBounds) bool {
	// 1. 屏幕外状态、零尺寸和完全在内容区之外的都不算。
	if node.hasState("offscreen") || node.Bounds.Width <= 0 || node.Bounds.Height <= 0 {
		return false
	}
	return node.Bounds.X < content.Width && node.Bounds.X+node.Bounds.Width > 0 &&
		node.Bounds.Y < content.Height && node.Bounds.Y+node.Bounds.Height > 0
}

// formatNode 把一个控件格式化成一行：[id] role "label" key=… value=… desc=… ph=… (x,y w×h) flags。
func formatNode(node *automationNode, shortID string, focusID string, maxLength int, showOffscreen bool) string {
	// 1. 角色和有值的字段。
	var builder strings.Builder
	fmt.Fprintf(&builder, "[%s] %s", shortID, node.Role)
	if node.Label != "" {
		builder.WriteString(" " + quoteText(node.Label, maxLength))
	}
	if key := node.keyString(); key != "" {
		builder.WriteString(" key=" + key)
	}
	if node.Value != "" {
		builder.WriteString(" value=" + quoteText(node.Value, maxLength))
	}
	if node.Placeholder != "" && node.Value == "" && node.Placeholder != node.Label {
		builder.WriteString(" ph=" + quoteText(node.Placeholder, maxLength))
	}
	if node.Description != "" && node.Description != node.Label {
		builder.WriteString(" desc=" + quoteText(node.Description, maxLength))
	}
	// 2. 位置大小和状态标记；focusable 是噪声，offscreen 只在要求时显示。
	fmt.Fprintf(&builder, " (%d,%d %d×%d)", int(node.Bounds.X), int(node.Bounds.Y), int(node.Bounds.Width), int(node.Bounds.Height))
	for _, state := range node.States {
		if state == "focusable" || (state == "offscreen" && !showOffscreen) {
			continue
		}
		builder.WriteString(" " + state)
	}
	if node.ID == focusID {
		builder.WriteString(" focused")
	}
	return builder.String()
}

// quoteText 把文字加引号并在超过 maxLength 个字符时截断，maxLength 为 0 时不截断。
func quoteText(text string, maxLength int) string {
	// 1. 截断后注明被省略的字符数。
	runes := []rune(text)
	if maxLength > 0 && len(runes) > maxLength {
		return strconv.Quote(string(runes[:maxLength])) + fmt.Sprintf("…(+%d)", len(runes)-maxLength)
	}
	return strconv.Quote(text)
}

// structuralRoles 是只负责布局的角色；没有任何文字时 tree 默认不打印它们，子控件上提一级。
var structuralRoles = []string{"group", "scroll", "list", "toolbar", "table", "row", "cell", "tabList", "radioGroup", "tree", "popup", "menu", "menuBar", "splitter"}

// getNodeDepths 返回应该打印的控件 ID 到缩进层级的映射：缩进只数被打印的祖先；all 为 false 时跳过屏幕外和没有文字的容器。
func getNodeDepths(tree *automationTree, content automationBounds, all bool) (map[string]int, []*automationNode) {
	// 1. 控件按先父后子排列，逐个决定是否打印并推出层级。
	depths := make(map[string]int, len(tree.Nodes))
	inherited := make(map[string]int, len(tree.Nodes))
	var shown []*automationNode
	for index := range tree.Nodes {
		node := &tree.Nodes[index]
		level := inherited[node.Parent]
		inherited[node.ID] = level
		if !all && !isNodeShown(node, content) {
			continue
		}
		depths[node.ID] = level
		inherited[node.ID] = level + 1
		shown = append(shown, node)
	}
	return depths, shown
}

// isNodeShown tree 默认是否打印这个控件？
func isNodeShown(node *automationNode, content automationBounds) bool {
	// 1. 看不见的不打印；没有任何文字的容器和空文本不打印。
	if !isNodeOnScreen(node, content) {
		return false
	}
	hasText := node.Label != "" || node.Value != "" || node.keyString() != "" || node.Description != "" || node.Placeholder != ""
	if hasText {
		return true
	}
	return !slices.Contains(structuralRoles, node.Role) && node.Role != "text" && node.Role != "heading" && node.Role != "image"
}

// matchTier 返回 text 与控件的匹配等级：1 key 完全相等，2 label 或 placeholder 完全相等，3 key、label、placeholder 包含，0 不匹配。
func matchTier(node *automationNode, text string) int {
	// 1. 完全相等优先，其次不分大小写的包含。
	if node.keyString() == text {
		return 1
	}
	if node.Label == text || node.Placeholder == text {
		return 2
	}
	lower := strings.ToLower(text)
	for _, field := range []string{node.keyString(), node.Label, node.Placeholder} {
		if field != "" && strings.Contains(strings.ToLower(field), lower) {
			return 3
		}
	}
	return 0
}

// containsText 控件的 label、value、key、description、placeholder 是否含有 text（不分大小写）？
func containsText(node *automationNode, text string) bool {
	// 1. 任一字段包含即可。
	lower := strings.ToLower(text)
	for _, field := range []string{node.Label, node.Value, node.keyString(), node.Description, node.Placeholder} {
		if field != "" && strings.Contains(strings.ToLower(field), lower) {
			return true
		}
	}
	return false
}
