package workspace_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshTestServer 是测试进程内的 SSH 服务端：只接受 identityFile 对应的公钥；SFTP 子系统直接访问本机文件系统；
// exec 请求以新会话交给本机 loginShell 执行，pty-req 之后的 exec 在 pty 中执行；
// direct-tcpip 通道由服务端所在机器拨号目标地址并双向转发。
type sshTestServer struct {
	address      string
	identityFile string
	// loginShell 模拟远端用户的登录 shell，默认 sh。
	loginShell string
	listener   net.Listener
	config     *ssh.ServerConfig
	// connections 是还没关闭的连接，dropConnections 用它断开全部连接，模拟网络中断。
	connections syncmap.SyncMap[net.Conn, struct{}]
	// sftpChannels 是打开着的 SFTP 通道，dropSFTPChannels 用它只关 SFTP。
	sftpChannels syncmap.SyncMap[ssh.Channel, struct{}]
	// acceptedConnections 是接受过的连接数。
	acceptedConnections atomic.Int32
	// forwardedChannels 是转发过的 direct-tcpip 通道数。
	forwardedChannels atomic.Int32
	// silent 连接是否暂停回应？模拟远端静默消失。
	silent atomic.Bool
}

// sshTestConnection 是服务端接受的连接：silent 期间扣住读到的数据，服务端既收不到请求也不回应，TCP 连接保持打开。
type sshTestConnection struct {
	net.Conn
	silent *atomic.Bool
}

// Read 读取测试 SSH 连接的 TCP 数据。
func (c *sshTestConnection) Read(p []byte) (int, error) {
	// 1. 先读数据，再等待 silent 清除后返回。
	n, err := c.Conn.Read(p)
	for c.silent.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	return n, err
}

// newSSHTestServer 创建以 sh 登录的测试 SSH 服务端。
func newSSHTestServer(t *testing.T) *sshTestServer {
	// 1. 使用 sh 创建服务端。
	t.Helper()
	return newSSHTestServerWithLoginShell(t, "sh")
}

// newSSHTestServerWithLoginShell 创建指定登录 shell 的本机测试 SSH 服务端。
func newSSHTestServerWithLoginShell(t *testing.T, loginShell string) *sshTestServer {
	// 1. 清空 SSH_AUTH_SOCK，生成主机密钥和客户端身份。
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	identityFile, clientPublicKey := writeSSHTestIdentity(t)
	// 2. 配置公钥认证，监听本机随机端口，并登记测试结束后的关闭。
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientPublicKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown public key")
		},
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &sshTestServer{
		address:      listener.Addr().String(),
		identityFile: identityFile,
		loginShell:   loginShell,
		listener:     listener,
		config:       config,
	}
	go server.acceptConnections()
	t.Cleanup(func() {
		_ = listener.Close()
		server.dropConnections()
	})
	return server
}

// writeSSHTestIdentity 生成无口令 ed25519 私钥文件及对应公钥。
func writeSSHTestIdentity(t *testing.T) (string, ssh.PublicKey) {
	// 1. 生成密钥，写入 OpenSSH PEM 私钥文件并转换公钥。
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return path, sshPublicKey
}

// acceptConnections 接受 TCP 连接并启动 SSH 处理。
func (s *sshTestServer) acceptConnections() {
	// 1. 循环接受连接，记录数量和连接后后台处理。
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.acceptedConnections.Add(1)
		s.connections.Store(connection, struct{}{})
		go s.serveConnection(&sshTestConnection{
			Conn:   connection,
			silent: &s.silent,
		})
	}
}

// dropConnections 关闭全部测试连接。
func (s *sshTestServer) dropConnections() {
	// 1. 遍历已接受连接并逐个关闭。
	s.connections.Range(func(connection net.Conn, _ struct{}) bool {
		_ = connection.Close()
		return true
	})
}

// silenceConnections 暂停服务端数据处理和响应，测试结束时恢复。
func (s *sshTestServer) silenceConnections(t *testing.T) {
	// 1. 设置 silent，并登记清除该标记的清理函数。
	s.silent.Store(true)
	t.Cleanup(func() { s.silent.Store(false) })
}

// dropSFTPChannels 只关闭 SFTP 通道，保留 SSH 连接。
func (s *sshTestServer) dropSFTPChannels() {
	// 1. 遍历 SFTP 通道并关闭。
	s.sftpChannels.Range(func(channel ssh.Channel, _ struct{}) bool {
		_ = channel.Close()
		return true
	})
}

// serveConnection 完成 SSH 握手并分发会话和端口转发通道。
func (s *sshTestServer) serveConnection(connection net.Conn) {
	// 1. 握手失败时关闭连接，成功后分发通道并忽略全局请求。
	defer s.connections.Delete(connection)
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, s.config)
	if err != nil {
		_ = connection.Close()
		return
	}
	defer serverConnection.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		switch newChannel.ChannelType() {
		case "session":
			channel, channelRequests, err := newChannel.Accept()
			if err != nil {
				continue
			}
			go s.serveSession(channel, channelRequests)
		case "direct-tcpip":
			go s.serveForward(newChannel)
		default:
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session and direct-tcpip channels are supported")
		}
	}
}

// serveForward 在测试服务端拨号目标并双向转发数据。
func (s *sshTestServer) serveForward(newChannel ssh.NewChannel) {
	// 1. 解码 direct-tcpip 的主机、端口和来源地址，再拨号目标。
	var payload struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "invalid direct-tcpip payload")
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	// 2. 接受转发通道，记录数量并启动双向复制；复制结束时关闭对应连接。
	channel, requests, err := newChannel.Accept()
	if err != nil {
		_ = target.Close()
		return
	}
	s.forwardedChannels.Add(1)
	go ssh.DiscardRequests(requests)
	go func() {
		_, _ = io.Copy(target, channel)
		_ = target.Close()
	}()
	go func() {
		_, _ = io.Copy(channel, target)
		_ = channel.Close()
	}()
}

// serveSession 处理测试 SSH 会话的 SFTP、PTY 和 exec 请求。
func (s *sshTestServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	// 1. 依次处理请求，记录 TTY，运行 SFTP 或启动命令并回复。
	var tty *sshTestPTY
	var terminal *sshTestTerminal
	var command *exec.Cmd
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var payload struct {
				Term          string
				Columns, Rows uint32
				Width, Height uint32
				Modes         string
			}
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			tty = &sshTestPTY{term: payload.Term, size: &pty.Winsize{Rows: uint16(payload.Rows), Cols: uint16(payload.Columns)}}
			_ = request.Reply(true, nil)
		case "window-change":
			var payload struct {
				Columns, Rows uint32
				Width, Height uint32
			}
			if err := ssh.Unmarshal(request.Payload, &payload); err == nil && terminal != nil {
				terminal.resize(&pty.Winsize{Rows: uint16(payload.Rows), Cols: uint16(payload.Columns)})
			}
		case "subsystem":
			_ = request.Reply(true, nil)
			s.sftpChannels.Store(channel, struct{}{})
			go func() {
				defer s.sftpChannels.Delete(channel)
				if server, err := sftp.NewServer(channel); err == nil {
					_ = server.Serve()
				}
				_ = channel.Close()
			}()
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			started, startedTerminal, err := startSSHTestCommand(channel, s.loginShell, payload.Command, tty)
			_ = request.Reply(err == nil, nil)
			command, terminal = started, startedTerminal
		default:
			_ = request.Reply(false, nil)
		}
	}
	// 2. 会话请求结束后，终止仍运行的命令进程组。
	if command != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

// sshTestPTY 是 pty-req 请求申请的终端：TERM 取值和初始尺寸。
type sshTestPTY struct {
	term string
	size *pty.Winsize
}

// sshTestTerminal 是测试 SSH 服务端 TTY 命令的 PTY master：window-change 修改尺寸与命令结束后的关闭互斥。
type sshTestTerminal struct {
	lock     sync.Mutex
	file     *os.File
	isClosed bool
}

// resize 修改 PTY 尺寸，已关闭时忽略。
func (t *sshTestTerminal) resize(size *pty.Winsize) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if !t.isClosed {
		_ = pty.Setsize(t.file, size)
	}
}

// close 关闭 PTY master。
func (t *sshTestTerminal) close() {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.isClosed = true
	_ = t.file.Close()
}

// startSSHTestCommand 启动测试登录 shell，连接会话输入输出；TTY 命令同时返回 PTY master，供 window-change 修改尺寸。
func startSSHTestCommand(channel ssh.Channel, loginShell, commandLine string, tty *sshTestPTY) (*exec.Cmd, *sshTestTerminal, error) {
	// 1. TTY 时按请求的尺寸创建 PTY，TERM 取申请的终端类型，启动双向复制并在结束后发送退出状态。
	command := exec.Command(loginShell, "-c", commandLine)
	if tty != nil {
		command.Env = append(os.Environ(), "TERM="+tty.term)
		file, err := pty.StartWithSize(command, tty.size)
		if err != nil {
			return nil, nil, err
		}
		terminal := &sshTestTerminal{file: file}
		go func() { _, _ = io.Copy(file, channel) }()
		go func() {
			_, _ = io.Copy(channel, file)
			finishSSHTestCommand(channel, command)
			terminal.close()
		}()
		return command, terminal, nil
	}
	// 2. 非 TTY 以新会话启动，直接接入通道输入输出并后台等待结束。
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin = channel
	command.Stdout = channel
	command.Stderr = channel.Stderr()
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return nil, nil, err
	}
	go finishSSHTestCommand(channel, command)
	return command, nil, nil
}

// finishSSHTestCommand 等待命令结束并发送 SSH 退出状态。
func finishSSHTestCommand(channel ssh.Channel, command *exec.Cmd) {
	// 1. 等待命令，信号退出用 128 加信号值，再发送 exit-status 并关闭通道。
	_ = command.Wait()
	code := command.ProcessState.ExitCode()
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = 128 + int(status.Signal())
	}
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{uint32(code)}))
	_ = channel.Close()
}

// isProcessGone 本机进程是否不存在或已经成为僵尸？
func isProcessGone(pid int) bool {
	// 1. 读取 /proc 进程状态，读取失败或状态为 Z 时返回 true。
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

// startSSHCommandWithFailedTerminate 创建无法通过进程组号文件终止的测试命令。
func startSSHCommandWithFailedTerminate(t *testing.T, f *workspaceFixture) (workspace.Process, <-chan processExit, int, string) {
	// 1. 启动 sleep 并读取真实进程组号，登记测试结束后的强制终止。
	t.Helper()
	// 命令由 setsid 启动，bash 的 $$ 是新进程组号。
	process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
		Command:   "echo $$ > command.pgid; sleep 30",
		Directory: f.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	exited := waitInBackground(process)
	var pgid int
	waitForCondition(t, 5*time.Second, func() bool {
		content, err := os.ReadFile(filepath.Join(f.root, "command.pgid"))
		pgid, _ = strconv.Atoi(strings.TrimSpace(string(content)))
		return err == nil && pgid > 0
	})
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	// 2. 将进程组号文件改为 abc，核对 Terminate 失败、Wait 仍等待且命令还在。
	pgidPath := findSSHPGIDFile(t, pgid)
	if err := os.WriteFile(pgidPath, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := process.Terminate(); err == nil {
		t.Fatal("Terminate succeeded with an invalid process group id")
	}
	select {
	case exit := <-exited:
		t.Fatalf("Wait returned %d, %v after a failed Terminate", exit.code, exit.err)
	case <-time.After(300 * time.Millisecond):
	}
	if isProcessGone(pgid) {
		t.Fatal("command stopped although Terminate failed")
	}
	return process, exited, pgid, pgidPath
}

// findSSHPGIDFile 查找指定进程组号的远端记录文件。
func findSSHPGIDFile(t *testing.T, pgid int) string {
	// 1. 测试远端是本机，在五秒内轮询 /tmp 记录文件并匹配进程组号。
	t.Helper()
	var pgidPath string
	waitForCondition(t, 5*time.Second, func() bool {
		paths, _ := filepath.Glob("/tmp/openvaala-process-*.pgid")
		for _, path := range paths {
			if content, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(content)) == strconv.Itoa(pgid) {
				pgidPath = path
				return true
			}
		}
		return false
	})
	return pgidPath
}

// waitForCondition 等待测试条件成立。
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	// 1. 每五十毫秒检查条件和截止时间，超时则结束测试。
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSSHWorkspaceTelemetry 验证 SSH 首次连接及断开重连的日志。
func TestSSHWorkspaceTelemetry(t *testing.T) {
	// 1. 创建带遥测的测试服务端和工作区。
	p := telemetrytest.NewPipeline(t)
	server := newSSHTestServer(t)
	root := t.TempDir()
	opt := workspace.NewSSHWorkspaceOpt(root, "tester@"+server.address, server.identityFile)
	opt.Telemetry = p.Telemetry
	instance, err := workspace.NewSSHWorkspace(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })

	// 2. 子测试依次共用工作区，先连接再断开重连。
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "first_call_logs_connected",
			run: func(t *testing.T) {
				if _, err := instance.GetMetadata(t.Context(), root, true); err != nil {
					t.Fatal(err)
				}
				host, port, err := net.SplitHostPort(server.address)
				if err != nil {
					t.Fatal(err)
				}
				portNumber, err := strconv.Atoi(port)
				if err != nil {
					t.Fatal(err)
				}
				record := p.WaitLogRecords(t, "ssh connected", 1)[0]
				if record["server.address"] != host || record["server.port"] != float64(portNumber) {
					t.Fatalf("ssh connected log = %v, want server.address %q and server.port %d", record, host, portNumber)
				}
			},
		},
		{
			name: "dropped_connection_logs_redial",
			run: func(t *testing.T) {
				server.dropConnections()
				waitForCondition(t, 2*time.Second, func() bool {
					_, err := instance.GetMetadata(t.Context(), root, true)
					return err == nil
				})
				p.WaitLogRecords(t, "ssh connection closed", 1)
				if record := p.WaitLogRecords(t, "ssh connection lost, redialing", 1)[0]; record["level"] != "WARN" {
					t.Fatalf("redial log = %v", record)
				}
				p.WaitLogRecords(t, "ssh connected", 2)
			},
		},
	}
	// 3. 顺序运行连接及重连用例，核对日志。
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestSSHWorkspaceCloseTelemetry 验证关闭已连接和未连接工作区的日志。
func TestSSHWorkspaceCloseTelemetry(t *testing.T) {
	// 1. 准备是否先建立连接的用例。
	tests := []struct {
		name       string
		connect    bool
		wantClosed int
	}{
		{
			name:       "close_after_successful_call",
			connect:    true,
			wantClosed: 1,
		},
		{
			name:       "close_without_connection",
			connect:    false,
			wantClosed: 0,
		},
	}
	// 2. 逐项创建工作区，按是否建立连接关闭后核对日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			server := newSSHTestServer(t)
			root := t.TempDir()
			opt := workspace.NewSSHWorkspaceOpt(root, "tester@"+server.address, server.identityFile)
			opt.Telemetry = p.Telemetry
			instance, err := workspace.NewSSHWorkspace(opt)
			if err != nil {
				t.Fatal(err)
			}
			if tt.connect {
				if _, err := instance.GetMetadata(t.Context(), root, true); err != nil {
					t.Fatal(err)
				}
			}
			// Close 等待连接的后台任务退出，返回时 "ssh connection closed" 已写出。
			if err := instance.Close(); err != nil {
				t.Fatal(err)
			}
			closed := p.GetLogRecords(t, "ssh connection closed")
			if len(closed) != tt.wantClosed || (tt.wantClosed == 1 && closed[0]["level"] != "INFO") {
				t.Fatalf("ssh connection closed logs = %v, want %d INFO", closed, tt.wantClosed)
			}
			for _, message := range []string{"ssh connection lost, redialing", "ssh keepalive failed"} {
				if records := p.GetLogRecords(t, message); len(records) != 0 {
					t.Fatalf("%q logs = %v, want none", message, records)
				}
			}
		})
	}
}

// TestNewSSHWorkspace 验证 SSH 地址和根目录配置。
func TestNewSSHWorkspace(t *testing.T) {
	// 1. 逐项构造带用户、端口、IPv6 及无效地址的工作区。
	for _, tt := range []struct {
		name    string
		root    string
		address string
		wantErr bool
	}{
		{
			name:    "user_host_port",
			root:    "/srv/project",
			address: "me@devbox:2222",
		},
		{
			name:    "host_only",
			root:    "/srv/project",
			address: "devbox",
		},
		{
			name:    "ipv6_with_port",
			root:    "/srv/project",
			address: "me@[::1]:2222",
		},
		{
			name:    "relative_root",
			root:    "project",
			address: "me@devbox",
			wantErr: true,
		},
		{
			name:    "empty_host",
			root:    "/srv/project",
			address: "me@",
			wantErr: true,
		},
		{
			name:    "empty_address",
			root:    "/srv/project",
			address: "",
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(tt.root, tt.address, ""))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && instance.GetRootDirectory() != tt.root {
				t.Fatalf("root = %q", instance.GetRootDirectory())
			}
		})
	}
}

// TestSSHWorkspace 验证远端连接复用、重连、文件访问和命令生命周期。
func TestSSHWorkspace(t *testing.T) {
	// 1. 逐项创建 SSH 工作区，核对网络、输入、停止及关闭结果。
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, f *workspaceFixture)
	}{
		{
			name: "connects_on_first_call",
			run: func(t *testing.T, f *workspaceFixture) {
				if got := f.sshServer.acceptedConnections.Load(); got != 0 {
					t.Fatalf("connections before first call = %d", got)
				}
				for range 2 {
					if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); err != nil {
						t.Fatal(err)
					}
				}
				if got := f.sshServer.acceptedConnections.Load(); got != 1 {
					t.Fatalf("connections = %d, want 1", got)
				}
			},
		},
		{
			name: "concurrent_first_calls_share_connection",
			run: func(t *testing.T, f *workspaceFixture) {
				var calls sync.WaitGroup
				var failures atomic.Int32
				for range 8 {
					calls.Go(func() {
						if _, err := f.workspace.GetMetadata(t.Context(), f.root+"/note.txt", false); err != nil {
							failures.Add(1)
						}
					})
				}
				calls.Wait()
				if failures.Load() != 0 || f.sshServer.acceptedConnections.Load() != 1 {
					t.Fatalf("failures = %d, connections = %d", failures.Load(), f.sshServer.acceptedConnections.Load())
				}
			},
		},
		{
			name: "reconnects_after_drop",
			run: func(t *testing.T, f *workspaceFixture) {
				process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
					Command:   "sleep 10",
					Directory: f.root,
				})
				if err != nil {
					t.Fatal(err)
				}
				f.sshServer.dropConnections()
				if _, err := waitProcess(t, process); err == nil {
					t.Fatal("Wait returned no error after the connection dropped")
				}
				// 后台任务发现断开后，下一次调用重新拨号。
				waitForCondition(t, 2*time.Second, func() bool {
					content, err := f.workspace.ReadFile(t.Context(), f.root+"/note.txt", 0, 10)
					return err == nil && string(content.Content) == "abcdef"
				})
				if got := f.sshServer.acceptedConnections.Load(); got != 2 {
					t.Fatalf("connections = %d, want 2", got)
				}
				// 重连完成后在旧连接上调用 Terminate，核对无法确认远端命令结束。
				if err := process.Terminate(); err == nil {
					t.Fatal("Terminate reported success after the connection dropped")
				}
			},
		},
		{
			name: "reconnects_after_sftp_channel_closed",
			run: func(t *testing.T, f *workspaceFixture) {
				if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); err != nil {
					t.Fatal(err)
				}
				f.sshServer.dropSFTPChannels()
				// SFTP 会话单独结束后连接被视为断开，下一次调用重新拨号。
				waitForCondition(t, 2*time.Second, func() bool {
					content, err := f.workspace.ReadFile(t.Context(), f.root+"/note.txt", 0, 10)
					return err == nil && string(content.Content) == "abcdef"
				})
				if got := f.sshServer.acceptedConnections.Load(); got != 2 {
					t.Fatalf("connections = %d, want 2", got)
				}
			},
		},
		{
			name: "terminate_kills_process_group",
			run: func(t *testing.T, f *workspaceFixture) {
				process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
					Command:   "sleep 30 & echo $! > bg.pid; wait",
					Directory: f.root,
				})
				if err != nil {
					t.Fatal(err)
				}
				var backgroundPID int
				waitForCondition(t, 5*time.Second, func() bool {
					content, err := os.ReadFile(filepath.Join(f.root, "bg.pid"))
					backgroundPID, _ = strconv.Atoi(strings.TrimSpace(string(content)))
					return err == nil && backgroundPID > 0
				})
				if err := process.Terminate(); err != nil {
					t.Fatalf("Terminate = %v", err)
				}
				// 已确认停下后再 Terminate 直接返回，不再开 kill 会话。
				started := time.Now()
				if err := process.Terminate(); err != nil || time.Since(started) > time.Second {
					t.Fatalf("second Terminate = %v after %v", err, time.Since(started))
				}
				if code, err := waitProcess(t, process); code != -1 || err != nil {
					t.Fatalf("code = %d, err = %v", code, err)
				}
				waitForCondition(t, 2*time.Second, func() bool { return isProcessGone(backgroundPID) })
			},
		},
		{
			name: "retry_after_failed_terminate_stops_command",
			run: func(t *testing.T, f *workspaceFixture) {
				process, exited, pgid, pgidPath := startSSHCommandWithFailedTerminate(t, f)
				// 进程组号恢复后重试成功。
				if err := os.WriteFile(pgidPath, []byte(strconv.Itoa(pgid)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := process.Terminate(); err != nil {
					t.Fatalf("retried Terminate = %v", err)
				}
				select {
				case exit := <-exited:
					if exit.code != -1 || exit.err != nil {
						t.Fatalf("code = %d, err = %v", exit.code, exit.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Wait did not return after Terminate")
				}
				waitForCondition(t, 2*time.Second, func() bool { return isProcessGone(pgid) })
			},
		},
		{
			name: "close_after_failed_terminate_releases_wait",
			run: func(t *testing.T, f *workspaceFixture) {
				process, exited, _, _ := startSSHCommandWithFailedTerminate(t, f)
				if err := process.Close(); err != nil {
					t.Fatalf("Close = %v", err)
				}
				// Close 不证明远端已停止，Wait 返回错误而不是 -1、nil。
				select {
				case exit := <-exited:
					if exit.err == nil {
						t.Fatalf("Wait = %d, nil after Close, want an error", exit.code)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Wait did not return after Close")
				}
			},
		},
		{
			name: "tty_input_reaches_command",
			run: func(t *testing.T, f *workspaceFixture) {
				var stdout bytes.Buffer
				process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
					Command:   "read line; echo got:$line",
					Directory: f.root,
					TTY:       true,
					Stdout:    &stdout,
				})
				if err != nil {
					t.Fatal(err)
				}
				if written, err := writeTerminalInput(t, process, []byte("hi\n")); written != 3 || err != nil {
					t.Fatalf("WriteTerminalInput = %d, %v", written, err)
				}
				if code, err := waitProcess(t, process); code != 0 || err != nil || !strings.Contains(stdout.String(), "got:hi") {
					t.Fatalf("code = %d, err = %v, stdout = %q", code, err, stdout.String())
				}
			},
		},
		{
			name: "background_process_released_after_exit",
			run: func(t *testing.T, f *workspaceFixture) {
				var stdout bytes.Buffer
				code, err := runProcess(t, f, &workspace.ProcessRequest{
					Command:   "sleep 30 & printf done",
					Directory: f.root,
					Stdout:    &stdout,
				}, false)
				if err != nil || code != 0 || stdout.String() != "done" {
					t.Fatalf("code = %d, err = %v, stdout = %q", code, err, stdout.String())
				}
			},
		},
		{
			name: "quoted_command_and_directory",
			run: func(t *testing.T, f *workspaceFixture) {
				directory := filepath.Join(f.root, "dir with 'quote'")
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				code, err := runProcess(t, f, &workspace.ProcessRequest{
					Command:   `printf '%s' "it's" > out.txt`,
					Directory: directory,
				}, false)
				if err != nil || code != 0 {
					t.Fatalf("code = %d, err = %v", code, err)
				}
				// 读取带引号与空格目录中的文件，核对命令输出。
				if content, err := os.ReadFile(filepath.Join(directory, "out.txt")); err != nil || string(content) != "it's" {
					t.Fatalf("content = %q, err = %v", content, err)
				}
			},
		},
		{
			name: "terminate_after_wait_is_immediate",
			run: func(t *testing.T, f *workspaceFixture) {
				process, err := f.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
					Command:   "true",
					Directory: f.root,
				})
				if err != nil {
					t.Fatal(err)
				}
				if code, err := waitProcess(t, process); code != 0 || err != nil {
					t.Fatalf("code = %d, err = %v", code, err)
				}
				started := time.Now()
				if err := process.Terminate(); err != nil {
					t.Fatalf("Terminate after Wait = %v", err)
				}
				if elapsed := time.Since(started); elapsed > time.Second {
					t.Fatalf("Terminate after Wait took %v", elapsed)
				}
			},
		},
		{
			name: "close_rejects_calls",
			run: func(t *testing.T, f *workspaceFixture) {
				if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); err != nil {
					t.Fatal(err)
				}
				if err := f.workspace.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("err = %v, want net.ErrClosed", err)
				}
			},
		},
		{
			name: "close_returns_after_remote_vanished",
			run: func(t *testing.T, f *workspaceFixture) {
				if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); err != nil {
					t.Fatal(err)
				}
				f.sshServer.silenceConnections(t)
				// 远端静默消失后退出不应等待 keepalive 判定断开。
				closed := make(chan error, 1)
				go func() { closed <- f.workspace.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Close blocked after the remote host vanished")
				}
			},
		},
		{
			name: "dial_reaches_listener_on_remote_machine",
			run: func(t *testing.T, f *workspaceFixture) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				go func() {
					connection, err := listener.Accept()
					if err != nil {
						return
					}
					defer connection.Close()
					_, _ = connection.Write([]byte("hello"))
				}()
				connection, err := f.workspace.Dial(t.Context(), "tcp", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				content, err := io.ReadAll(connection)
				if err != nil || string(content) != "hello" {
					t.Fatalf("content = %q, err = %v", content, err)
				}
				// 连接经 SSH 的 direct-tcpip 通道转发，且复用同一条 SSH 连接。
				if got := f.sshServer.forwardedChannels.Load(); got != 1 {
					t.Fatalf("forwarded channels = %d, want 1", got)
				}
				if got := f.sshServer.acceptedConnections.Load(); got != 1 {
					t.Fatalf("connections = %d, want 1", got)
				}
			},
		},
		{
			name: "home_directory_is_sftp_initial_directory",
			run: func(t *testing.T, f *workspaceFixture) {
				// 测试服务端的 SFTP 子系统直接访问本机，初始目录是本进程的工作目录。
				want, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				got, err := f.workspace.GetHomeDirectory(t.Context())
				if err != nil || got != want {
					t.Fatalf("GetHomeDirectory = %q, %v; want %q", got, err, want)
				}
			},
		},
		{
			name: "unknown_key_rejected",
			run: func(t *testing.T, f *workspaceFixture) {
				otherIdentity, _ := writeSSHTestIdentity(t)
				instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(f.root, "tester@"+f.sshServer.address, otherIdentity))
				if err != nil {
					t.Fatal(err)
				}
				defer instance.Close()
				if _, err := instance.GetMetadata(t.Context(), f.root, true); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "cancelled_context_interrupts_handshake",
			run: func(t *testing.T, f *workspaceFixture) {
				// 服务端只接受 TCP 不回应握手，核对取消后返回 ctx 错误。
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				go func() {
					for {
						connection, err := listener.Accept()
						if err != nil {
							return
						}
						defer connection.Close()
					}
				}()
				instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(f.root, "tester@"+listener.Addr().String(), f.sshServer.identityFile))
				if err != nil {
					t.Fatal(err)
				}
				defer instance.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				time.AfterFunc(100*time.Millisecond, cancel)
				start := time.Now()
				_, err = instance.GetMetadata(ctx, f.root, true)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("GetMetadata err = %v, want context.Canceled", err)
				}
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Fatalf("GetMetadata returned after %v, want shortly after the context was cancelled", elapsed)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newWorkspaceFixture(t, "ssh"))
		})
	}
}

// TestSSHWorkspaceCloseDirectoryOperations 检查关闭独占连接会结束无响应的目录操作。
func TestSSHWorkspaceCloseDirectoryOperations(t *testing.T) {
	// 1. 分别挂起主目录查询、目录校验和目录读取。
	for _, tt := range []struct {
		name string
		call func(context.Context, workspace.Workspace, string) error
	}{
		{name: "getwd", call: func(ctx context.Context, instance workspace.Workspace, root string) error {
			_, err := instance.GetHomeDirectory(ctx)
			return err
		}},
		{name: "stat", call: func(ctx context.Context, instance workspace.Workspace, root string) error {
			_, err := instance.GetMetadata(ctx, root, true)
			return err
		}},
		{name: "readdir", call: func(ctx context.Context, instance workspace.Workspace, root string) error {
			_, err := instance.ReadDirectory(ctx, root)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkspaceFixture(t, "ssh")
			if _, err := f.workspace.GetMetadata(t.Context(), f.root, true); err != nil {
				t.Fatal(err)
			}
			f.sshServer.silenceConnections(t)
			finished := make(chan error, 1)
			go func() { finished <- tt.call(t.Context(), f.workspace, f.root) }()
			select {
			case err := <-finished:
				t.Fatalf("operation returned before connection was closed: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			// 2. 关闭整条连接，等待操作和连接的后台任务结束。
			closed := make(chan error, 1)
			go func() { closed <- f.workspace.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Close blocked on an unresponsive directory operation")
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("directory operation succeeded after connection was closed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("directory operation did not finish after Close")
			}
		})
	}
}

// TestSSHWorkspaceAgentCancellation 检查认证密钥查询无响应时请求仍能到期并结束。
func TestSSHWorkspaceAgentCancellation(t *testing.T) {
	// 1. 分别取消请求或等待期限，SSH agent 接受连接后不回答密钥查询。
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			server := newSSHTestServer(t)
			socket := filepath.Join(t.TempDir(), "agent.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("SSH_AUTH_SOCK", socket)
			agentFinished := make(chan struct{})
			go func() {
				defer close(agentFinished)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = io.Copy(io.Discard, connection)
			}()
			instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(t.TempDir(), "test@"+server.address, server.identityFile))
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			ctx, cancel := context.WithCancel(t.Context())
			wantError := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				wantError = context.DeadlineExceeded
			} else {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			defer cancel()
			// 2. 请求返回取消错误，agent 连接已经关闭，没有开始 SSH 连接。
			if _, err := instance.GetMetadata(ctx, instance.GetRootDirectory(), true); !errors.Is(err, wantError) {
				t.Fatalf("GetMetadata error = %v, want %v", err, wantError)
			}
			select {
			case <-agentFinished:
			case <-time.After(time.Second):
				t.Fatal("SSH agent connection remained open")
			}
			if got := server.acceptedConnections.Load(); got != 0 {
				t.Fatalf("SSH connections = %d, want 0", got)
			}
		})
	}
}

// TestSSHWorkspaceLoginShells 验证不同登录 shell 下命令传递及终止。
func TestSSHWorkspaceLoginShells(t *testing.T) {
	// 1. 逐个登录 shell 运行子测试。
	for _, loginShell := range []string{"sh", "bash", "zsh", "fish"} {
		t.Run(loginShell, func(t *testing.T) {
			// 1. 没装这个 shell 时跳过，否则用它创建测试服务端和 SSH 工作区。
			if _, err := exec.LookPath(loginShell); err != nil {
				t.Skipf("%s is not installed", loginShell)
			}
			server := newSSHTestServerWithLoginShell(t, loginShell)
			root := t.TempDir()
			instance, err := workspace.NewSSHWorkspace(workspace.NewSSHWorkspaceOpt(root, "tester@"+server.address, server.identityFile))
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			f := &workspaceFixture{
				workspace: instance,
				root:      root,
				sshServer: server,
			}
			// 2. 分别用普通会话和 TTY 执行含引号、反斜杠的命令，再验证终止命令。
			command := `printf '%s\n' "it's" 'a\b' > out.txt`
			for _, tty := range []bool{false, true} {
				code, err := runProcess(t, f, &workspace.ProcessRequest{
					Command:   command,
					Directory: root,
					TTY:       tty,
				}, false)
				if err != nil || code != 0 {
					t.Fatalf("tty=%v code = %d, err = %v", tty, code, err)
				}
				if content, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(content) != "it's\na\\b\n" {
					t.Fatalf("tty=%v content = %q, err = %v", tty, content, err)
				}
			}
			if code, err := runProcess(t, f, &workspace.ProcessRequest{
				Command:   "sleep 30",
				Directory: root,
			}, true); code != -1 || err != nil {
				t.Fatalf("terminate code = %d, err = %v", code, err)
			}
		})
	}
}
