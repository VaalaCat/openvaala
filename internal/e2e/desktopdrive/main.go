// Command desktopdrive 是给 AI 代理用的“原生桌面版 agent-browser”：在无显示的 Linux 上起一个真实的 OpenVaala 桌面，
// 让代理在 shell 里一步一步读控件树、点按钮、输入文字、按键、选菜单、截图。只用于开发和验收，不进 go test。
//
// 桌面用 -tags desktop_e2e 编译，经 MyGo 自动化插件（MYGO_AUTOMATION 指定的 Unix socket）被驱动；
// 环境是和 WebUI Playwright 测试同类的真实环境：临时 HOME、真实 serve、可控的假模型、几个预置对话和技能。
//
// 典型循环（先 go build -o /tmp/desktopdrive ./internal/e2e/desktopdrive，之后直接用 /tmp/desktopdrive）：
//
//	desktopdrive start                  编译、起 Xvfb、serve、假模型和桌面，后台运行，打印窗口和截图位置
//	desktopdrive tree                   看当前窗口的控件树
//	desktopdrive set composer-text hi   给输入框写文字
//	desktopdrive press 发送             按按钮
//	desktopdrive wait "fake model: hi"  等回复出现
//	desktopdrive shot                   截图，打印 PNG 路径，用图片阅读器打开
//	desktopdrive stop                   停掉全部进程并删除临时目录
//
// 子命令（每个都接受 --state 指定状态文件，默认 /tmp/openvaala-desktopdrive/state.json，
// 也可用环境变量 OPENVAALA_DESKTOPDRIVE_STATE；参数和 flag 可以混着写；不认识的 -xxx 当作文字，和 flag 重名的文字放在 -- 之后）：
//
//	start [--width 1280 --height 800 --lang zh-CN|en --no-models --skip-build --binary path]
//	    编译桌面（--binary 或 --skip-build 复用已有产物），准备临时 HOME、假模型、预置数据和配置，
//	    起 Xvfb 和桌面，等窗口出现后写状态文件并返回；后台由隐藏子命令 daemon 持有全部进程。
//	    预置两个技能、第二个执行器 sandbox，以及两个 MCP server：fake（假 server，已连接）和 broken（命令不存在，失败）。
//	    --no-models 不写模型服务配置，用来走首次引导。截图写进状态文件同目录的 shots/，日志在 logs/。
//	stop
//	    停桌面、serve、假模型和 Xvfb，删除临时 HOME；shots/ 和 logs/ 保留。
//	status
//	    打印状态文件内容和各进程是否还活着。
//	windows
//	    列出窗口 ID、标题、大小。
//	tree [--all] [--max 100]
//	    把当前窗口的控件打印成缩进文本，每行：[id] role "label" key=… value=… desc=… ph=… (x,y w×h) flags。
//	    默认跳过窗口外、屏幕外和没有任何文字的容器（它们的子控件上提一级）；--all 全部打印；--max 截断长文字，0 不截断。
//	    [id] 是控件 ID 的最短唯一后缀，可写成 #id 交给 press、focus、set；坐标是窗口内容坐标，和 click 一致。
//	find <text>
//	    列出 label、value、key、description、placeholder 含 text（不分大小写）的控件。
//	press <label-or-key|#id> [--nth N] [--timeout 3s]
//	    按按钮、勾选、打开菜单按钮；先滚到可见。先匹配 key，再匹配 label，再按包含匹配；
//	    优先可操作、没禁用、在屏幕内的控件；同名多个时报错并列出候选，用 --nth（从 1 数）选。
//	    没有按压动作的控件改为点击它的中心。
//	focus <label-or-key|#id> [--nth N]
//	    给控件键盘焦点。
//	set <label-or-key|#id> <text> [--nth N]
//	    整体替换文本输入框的内容（可用 key、label 或 placeholder 定位）。
//	type <text>
//	    把文字输入给当前焦点控件，像键盘打字。
//	key <combo>
//	    按键，写法同菜单加速键：Enter、Escape、Tab、shift+tab、Up、ctrl+k、cmdorctrl+n、F2。
//	click <x> <y> [--right] [--clicks 2]
//	    在窗口内容坐标点击；--right 右键，--clicks 2 双击。
//	scroll <x> <y> <dy>
//	    在该坐标滚轮滚动 dy 个 DIP，正数向下滚（内容上移），负数向上。
//	menu <path...>
//	    选应用菜单项，例如 menu 转到 设置（--lang en 时是 menu Go Settings），或 menu OpenVaala 退出；标签语言跟随桌面语言。
//	wait <text> [--timeout 10s]
//	    轮询控件树直到有控件的 label、value、key、description、placeholder 含 text，超时返回非零。
//	idle [--timeout 5s]
//	    等窗口画完。动画或流式输出一直在画时会超时，这不算错。
//	shot [file]
//	    截图保存为 PNG，默认 shots/<序号>.png，打印路径。
//	model reply <text> [--delay-ms N] [--reasoning t] [--hold]
//	    给假模型排下一条回复，文字里的 \n 换成换行；不排时它流式回显最后一条用户消息（fake model: <原文>）。
//	model tool <name> [json]    排一次工具调用，例如 model tool bash '{"command":"ls"}'
//	model error [--status 500] [message]   排一次 HTTP 错误
//	model reset                 清掉排着的回复
//	model models <id...>        设置 /v1/models 列出的模型
//	model requests              查看模型收到的请求数和最近一条用户消息
//
// 除 start、stop、status、model 外，所有动作命令执行后都会等 idle，再返回；所以紧跟着的 tree 和 shot 看到的是稳定界面。
// 发送消息后模型在流式输出，界面一直在画，用 wait 等回复文字比 idle 可靠。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// defaultStatePath 是状态文件的默认位置，同目录放 shots/、logs/ 和编译产物。
const defaultStatePath = "/tmp/openvaala-desktopdrive/state.json"

// driveState 是 start 写给其他子命令的状态文件：窗口连接信息、各进程 PID 和目录。
type driveState struct {
	// Socket 是桌面自动化 socket。
	Socket string `json:"socket"`
	// Display 是 Xvfb 的 DISPLAY，例如 :99。
	Display string `json:"display"`
	// Control 是假模型的地址，模型服务的 base_url 是 Control + "/v1"。
	Control string `json:"control"`
	// DaemonPID 持有假模型并负责退出时收尾；其余 PID 是它启动的进程。
	DaemonPID  int `json:"daemonPID"`
	DesktopPID int `json:"desktopPID"`
	ServePID   int `json:"servePID"`
	XvfbPID    int `json:"xvfbPID"`
	// Root 是临时目录，stop 时删除；Home 是桌面和 serve 使用的 HOME；Config 是 config.yaml 路径。
	Root   string `json:"root"`
	Home   string `json:"home"`
	Config string `json:"config"`
	// Binary 是编译出的带自动化入口的桌面程序。
	Binary string `json:"binary"`
	// ShotsDirectory 和 LogDirectory 在状态文件同目录，stop 后保留。
	ShotsDirectory string `json:"shotsDirectory"`
	LogDirectory   string `json:"logDirectory"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Language       string `json:"language"`
}

// main 分发子命令，出错时把信息打到 stderr 并以非零状态退出。
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "desktopdrive:", err)
		os.Exit(1)
	}
}

// run 按第一个参数选择子命令。
func run(arguments []string) error {
	// 1. 没有子命令时打印用法。
	if len(arguments) == 0 || arguments[0] == "help" || arguments[0] == "--help" || arguments[0] == "-h" {
		fmt.Fprintln(os.Stderr, "usage: desktopdrive start|stop|status|windows|tree|find|press|focus|set|type|key|click|scroll|menu|wait|idle|shot|model ...")
		fmt.Fprintln(os.Stderr, "see the package comment of internal/e2e/desktopdrive/main.go for details")
		return nil
	}
	// 2. 会话生命周期、假模型控制和其余桌面动作各走自己的入口。
	command, rest := arguments[0], arguments[1:]
	switch command {
	case "start":
		return runStartCommand(rest)
	case "daemon":
		return runDaemonCommand(rest)
	case "stop":
		return runStopCommand(rest)
	case "status":
		return runStatusCommand(rest)
	case "model":
		return runModelCommand(rest)
	}
	return runDesktopCommand(command, rest)
}

// newFlags 创建带 --state 的 FlagSet，出错时不打印用法，由 run 统一报告。
func newFlags(name string) (*flag.FlagSet, *string) {
	// 1. 状态文件默认取环境变量，其次是默认路径。
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	state := os.Getenv("OPENVAALA_DESKTOPDRIVE_STATE")
	if state == "" {
		state = defaultStatePath
	}
	return flags, flags.String("state", state, "path of the state file written by start")
}

// parseFlags 解析 flag 并返回位置参数：flag 和位置参数可以混写，-- 之后全部是位置参数；
// 不认识的 -xxx（例如 -200 或以 - 开头的文字）按位置参数处理，不报错。
func parseFlags(flags *flag.FlagSet, arguments []string) ([]string, error) {
	// 1. 逐个参数判断是已定义的 flag 还是位置参数。
	var positional []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return append(positional, arguments[index+1:]...), nil
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		defined := flags.Lookup(name)
		if !strings.HasPrefix(argument, "-") || defined == nil {
			positional = append(positional, argument)
			continue
		}
		// 2. 布尔 flag 不带值时为 true，其余取下一个参数作为值。
		if boolean, ok := defined.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() && !hasValue {
			value = "true"
		} else if !hasValue {
			index++
			if index >= len(arguments) {
				return nil, fmt.Errorf("flag --%s needs a value", name)
			}
			value = arguments[index]
		}
		if err := flags.Set(name, value); err != nil {
			return nil, err
		}
	}
	return positional, nil
}

// loadState 读取状态文件；没有时提示先 start。
func loadState(path string) (*driveState, error) {
	// 1. 读取并解码。
	payload, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no running desktop (%s not found); run `desktopdrive start` first", path)
	}
	if err != nil {
		return nil, err
	}
	var state driveState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &state, nil
}

// isProcessAlive 进程是否还在运行？
func isProcessAlive(pid int) bool {
	// 1. 信号 0 只检查进程是否存在；僵尸进程已退出但未回收，同样按已退出处理。
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	payload, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err != nil || !strings.Contains(string(payload), ") Z")
}

// getStateDirectory 返回状态文件所在目录。
func getStateDirectory(statePath string) string {
	return filepath.Dir(statePath)
}
