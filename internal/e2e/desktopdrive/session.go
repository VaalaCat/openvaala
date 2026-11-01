package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// startTimeout 是 start 等 daemon 把桌面准备好的最长时间，包含编译之后的全部步骤。
	startTimeout = 3 * time.Minute
	// windowTimeout 是等桌面窗口和自动化连接出现的最长时间。
	windowTimeout = 90 * time.Second
)

// runStartCommand 编译桌面，在后台启动持有整个会话的 daemon，等状态文件出现后打印连接信息。
func runStartCommand(arguments []string) error {
	// 1. 解析参数，拒绝重复启动，清理上次残留的状态。
	flags, statePath := newFlags("start")
	width := flags.Int("width", 1280, "desktop window width in DIPs")
	height := flags.Int("height", 800, "desktop window height in DIPs")
	language := flags.String("lang", "zh-CN", "desktop language: zh-CN or en")
	noModels := flags.Bool("no-models", false, "write no model provider so the first-run onboarding shows")
	skipBuild := flags.Bool("skip-build", false, "reuse the desktop binary built by the previous start")
	binary := flags.String("binary", "", "use this prebuilt desktop binary (built with -tags desktop_e2e)")
	if _, err := parseFlags(flags, arguments); err != nil {
		return err
	}
	if *language == "zh" {
		*language = "zh-CN"
	}
	if *language != "zh-CN" && *language != "en" {
		return fmt.Errorf("--lang must be zh-CN or en")
	}
	if existing, err := loadState(*statePath); err == nil {
		if isProcessAlive(existing.DaemonPID) {
			return fmt.Errorf("a desktop is already running (daemon pid %d); run `desktopdrive stop` first", existing.DaemonPID)
		}
		cleanupState(*statePath, existing)
	}
	directory := getStateDirectory(*statePath)
	logDirectory := filepath.Join(directory, "logs")
	for _, each := range []string{logDirectory, filepath.Join(directory, "bin")} {
		if err := os.MkdirAll(each, 0700); err != nil {
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(directory, "shots"))
	// 2. 编译带自动化入口的桌面程序；它同时是完整 CLI，serve 也用它启动。
	desktopBinary := filepath.Join(directory, "bin", "openvaala-desktop")
	switch {
	case *binary != "":
		absolute, err := filepath.Abs(*binary)
		if err != nil {
			return err
		}
		desktopBinary = absolute
	case !*skipBuild:
		fmt.Println("building desktop (tags desktop_e2e)...")
		build := exec.Command("go", "build", "-tags", "desktop_e2e", "-ldflags", "-X main.version=desktopdrive", "-o", desktopBinary, "github.com/VaalaCat/openvaala/cmd/openvaala-desktop")
		if output, err := build.CombinedOutput(); err != nil {
			return fmt.Errorf("build desktop: %w\n%s", err, output)
		}
	}
	if _, err := os.Stat(desktopBinary); err != nil {
		return fmt.Errorf("desktop binary: %w", err)
	}
	// 编译假 MCP server，供预置配置里的 fake 使用；编译失败只影响 MCP 状态演示，不挡住启动。
	if !*skipBuild && *binary == "" {
		build := exec.Command("go", "build", "-o", filepath.Join(directory, "bin", "fake-mcp"), "github.com/VaalaCat/openvaala/pkg/mcpclient/testdata/mcpserver")
		build.Env = append(os.Environ(), "GOWORK=off")
		if output, err := build.CombinedOutput(); err != nil {
			fmt.Printf("warning: build fake MCP server: %v\n%s", err, output)
		}
	}
	// 3. 以新会话启动自己的 daemon 子命令，日志写进 logs/daemon.log。
	self, err := os.Executable()
	if err != nil {
		return err
	}
	daemonLog, err := os.Create(filepath.Join(logDirectory, "daemon.log"))
	if err != nil {
		return err
	}
	defer func() { _ = daemonLog.Close() }()
	command := exec.Command(self, "daemon", "--state", *statePath, "--binary", desktopBinary,
		"--width", fmt.Sprint(*width), "--height", fmt.Sprint(*height), "--lang", *language, fmt.Sprintf("--no-models=%v", *noModels))
	command.Stdout, command.Stderr = daemonLog, daemonLog
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	// 4. 等 daemon 写出属于它的状态文件；它先退出就报告日志。
	deadline := time.After(startTimeout)
	for {
		select {
		case err := <-exited:
			log, _ := os.ReadFile(daemonLog.Name())
			return fmt.Errorf("daemon exited before the desktop was ready: %v\n%s", err, log)
		case <-deadline:
			_ = syscall.Kill(command.Process.Pid, syscall.SIGTERM)
			return fmt.Errorf("desktop was not ready in %s (see %s)", startTimeout, daemonLog.Name())
		case <-time.After(100 * time.Millisecond):
		}
		state, err := loadState(*statePath)
		if err != nil || state.DaemonPID != command.Process.Pid {
			continue
		}
		fmt.Printf("desktop ready: display %s, window %dx%d, language %s, models %v\n", state.Display, state.Width, state.Height, state.Language, !*noModels)
		fmt.Printf("state: %s\nscreenshots: %s/<n>.png\nlogs: %s\n", *statePath, state.ShotsDirectory, state.LogDirectory)
		return nil
	}
}

// runDaemonCommand 是 start 启动的后台入口：准备环境、起 Xvfb、serve、假模型和桌面，写状态文件后阻塞到被要求退出，
// 退出前按相反顺序收尾并删除临时目录。
func runDaemonCommand(arguments []string) error {
	// 1. 解析参数；SIGTERM、SIGINT 时退出。
	flags, statePath := newFlags("daemon")
	width := flags.Int("width", 1280, "")
	height := flags.Int("height", 800, "")
	language := flags.String("lang", "zh-CN", "")
	noModels := flags.Bool("no-models", false, "")
	binary := flags.String("binary", "", "")
	if _, err := parseFlags(flags, arguments); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	directory := getStateDirectory(*statePath)
	logDirectory := filepath.Join(directory, "logs")
	state := &driveState{
		DaemonPID: os.Getpid(), Binary: *binary, ShotsDirectory: filepath.Join(directory, "shots"), LogDirectory: logDirectory,
		Width: *width, Height: *height, Language: *language,
	}
	if err := os.MkdirAll(state.ShotsDirectory, 0700); err != nil {
		return err
	}
	// 2. 建临时目录；它的路径要短，automation.sock 的路径受 Unix socket 长度限制。
	root, err := os.MkdirTemp("", "ovdd-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	state.Root = root
	state.Home = filepath.Join(root, "home")
	state.Socket = filepath.Join(root, "automation.sock")
	// 3. 在回环地址起假模型和控制接口。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	state.Control = "http://" + listener.Addr().String()
	model := newFakeModel()
	modelServer := &http.Server{Handler: newModelHandler(model)}
	go func() { _ = modelServer.Serve(listener) }()
	defer func() { _ = modelServer.Close() }()
	// 4. 写配置和预置文件。
	serveAddress, err := getFreeAddress()
	if err != nil {
		return err
	}
	token := rand.Text()
	state.Config, err = writeEnvironmentFiles(root, state.Home, serveAddress, token, state.Control+"/v1", *language, *width, *height, !*noModels, filepath.Join(directory, "bin", "fake-mcp"))
	if err != nil {
		return err
	}
	// 5. 起 Xvfb。
	xvfbLog, err := os.Create(filepath.Join(logDirectory, "xvfb.log"))
	if err != nil {
		return err
	}
	defer func() { _ = xvfbLog.Close() }()
	xvfb, display, err := startXvfb(*width, *height, xvfbLog)
	if err != nil {
		return err
	}
	defer xvfb.stop()
	state.Display, state.XvfbPID = display, xvfb.command.Process.Pid
	// 6. 起 serve 并等健康检查；桌面连接它而不是自己再起一个，这样能先预置对话。
	serveLog, err := os.Create(filepath.Join(logDirectory, "serve.log"))
	if err != nil {
		return err
	}
	defer func() { _ = serveLog.Close() }()
	serveCommand := exec.Command(*binary, "--config-file", state.Config, "serve")
	serveCommand.Env = getChildEnvironment(state.Home)
	serveCommand.Dir = state.Home
	serveCommand.Stdout, serveCommand.Stderr = serveLog, serveLog
	serve, err := startChild(serveCommand)
	if err != nil {
		return err
	}
	defer stopServe(*binary, state, serve)
	state.ServePID = serve.command.Process.Pid
	if err := waitServeHealthy(serveAddress, serve); err != nil {
		return err
	}
	// 7. 预置对话；没有模型服务时不预置。
	if !*noModels {
		if err := seedConversations(ctx, serveAddress, token, filepath.Join(root, "workspace"), model); err != nil {
			return fmt.Errorf("seed conversations: %w", err)
		}
	}
	// 8. 起桌面，等原生窗口出现。
	desktopLog, err := os.Create(filepath.Join(logDirectory, "desktop.log"))
	if err != nil {
		return err
	}
	defer func() { _ = desktopLog.Close() }()
	// MyGo 的单实例锁放在 TMPDIR 下；每个会话用自己的临时目录，同时运行的两个会话才不会互相当成第二个实例。
	desktopTemporary := filepath.Join(root, "tmp")
	if err := os.MkdirAll(desktopTemporary, 0700); err != nil {
		return err
	}
	desktopCommand := exec.Command(*binary, "--config-file", state.Config, "app", "--native")
	desktopCommand.Env = getChildEnvironment(state.Home, "DISPLAY="+display, "MYGO_AUTOMATION="+state.Socket, "GSETTINGS_BACKEND=memory", "TMPDIR="+desktopTemporary)
	desktopCommand.Dir = state.Home
	desktopCommand.Stdout, desktopCommand.Stderr = desktopLog, desktopLog
	desktop, err := startChild(desktopCommand)
	if err != nil {
		return err
	}
	defer desktop.stop()
	state.DesktopPID = desktop.command.Process.Pid
	if err := waitDesktopWindow(ctx, state.Socket, desktop); err != nil {
		return err
	}
	// 9. 写状态文件，阻塞到被要求退出或桌面自己退出（例如从菜单退出）。
	if err := writeState(*statePath, state); err != nil {
		return err
	}
	defer func() { _ = os.Remove(*statePath) }()
	select {
	case <-ctx.Done():
		fmt.Println("daemon: stop requested")
	case <-desktop.exited:
		fmt.Println("daemon: desktop exited, cleaning up")
	}
	return nil
}

// stopServe 让 serve 优雅退出：先用 serve stop 停掉配置所在 HOME 的服务（也覆盖桌面自己起的），再收尾 daemon 启动的进程。
func stopServe(binary string, state *driveState, serve *childProcess) {
	// 1. serve stop 最多等 10 秒。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--config-file", state.Config, "serve", "stop")
	command.Env = getChildEnvironment(state.Home)
	_ = command.Run()
	serve.stop()
}

// waitDesktopWindow 等自动化 socket 可连接且出现原生内容窗口，再尽力等输入框出现（后台连接完成后才有）。
func waitDesktopWindow(ctx context.Context, socket string, desktop *childProcess) error {
	// 1. 拨号并轮询窗口列表。
	deadline := time.After(windowTimeout)
	var client *automationClient
	for {
		if client == nil {
			client, _ = dialAutomation(socket)
		}
		if client != nil {
			windows, err := client.getWindows()
			if err == nil && len(windows) > 0 {
				break
			}
		}
		select {
		case <-desktop.exited:
			return fmt.Errorf("desktop exited before showing a window (see the desktop log)")
		case <-deadline:
			return fmt.Errorf("desktop window did not appear in %s (see the desktop log)", windowTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { _ = client.close() }()
	// 2. 输入框出现说明界面已经连上后台；超时也继续，让代理自己看到当时的界面。
	waitEnd := time.Now().Add(20 * time.Second)
	for time.Now().Before(waitEnd) {
		if tree, err := client.getTree(); err == nil {
			for _, node := range tree.Nodes {
				if node.keyString() == "composer-text" {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// writeState 原子写状态文件。
func writeState(path string, state *driveState) error {
	// 1. 先写临时文件再改名，其他子命令不会读到半个文件。
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", payload, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// runStopCommand 要求 daemon 收尾并等它退出；daemon 已经不在时清理残留的进程和目录。
func runStopCommand(arguments []string) error {
	// 1. 读状态文件。
	flags, statePath := newFlags("stop")
	if _, err := parseFlags(flags, arguments); err != nil {
		return err
	}
	state, err := loadState(*statePath)
	if err != nil {
		return err
	}
	// 2. 给 daemon 发 SIGTERM，等它收尾完退出；超时就强杀并手工清理。
	if isProcessAlive(state.DaemonPID) {
		_ = syscall.Kill(state.DaemonPID, syscall.SIGTERM)
		deadline := time.Now().Add(60 * time.Second)
		for isProcessAlive(state.DaemonPID) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if isProcessAlive(state.DaemonPID) {
			_ = syscall.Kill(state.DaemonPID, syscall.SIGKILL)
		}
	}
	cleanupState(*statePath, state)
	fmt.Printf("stopped; screenshots kept in %s, logs in %s\n", state.ShotsDirectory, state.LogDirectory)
	return nil
}

// cleanupState 强杀状态文件里记录的残留进程，删除临时目录和状态文件；daemon 正常收尾时这些已经不存在。
func cleanupState(statePath string, state *driveState) {
	// 1. 进程组组长是各子进程自己，整组杀掉。
	for _, pid := range []int{state.DesktopPID, state.ServePID, state.XvfbPID} {
		if isProcessAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	// 2. 删除临时目录和状态文件。
	if state.Root != "" {
		_ = os.RemoveAll(state.Root)
	}
	_ = os.Remove(statePath)
}

// runStatusCommand 打印状态文件和各进程是否还活着。
func runStatusCommand(arguments []string) error {
	// 1. 读状态文件。
	flags, statePath := newFlags("status")
	if _, err := parseFlags(flags, arguments); err != nil {
		return err
	}
	state, err := loadState(*statePath)
	if err != nil {
		return err
	}
	// 2. 逐个进程检查。
	for _, process := range []struct {
		name string
		pid  int
	}{{"daemon", state.DaemonPID}, {"desktop", state.DesktopPID}, {"serve", state.ServePID}, {"xvfb", state.XvfbPID}} {
		fmt.Printf("%-8s pid %-8d alive=%v\n", process.name, process.pid, isProcessAlive(process.pid))
	}
	fmt.Printf("display %s  socket %s\nhome %s\nshots %s\nlogs %s\nmodel control %s\n", state.Display, state.Socket, state.Home, state.ShotsDirectory, state.LogDirectory, state.Control)
	return nil
}
