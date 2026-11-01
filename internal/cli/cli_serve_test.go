package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"gopkg.in/yaml.v3"
)

const serveTestToken = "serve-token"

// TestServe 检查 serve 启停、连接文件、令牌、重新加载和空闲退出。
func TestServe(t *testing.T) {
	// 1. 列出服务运行和错误场景。
	tests := []struct {
		name string
		run  func(t *testing.T, modelURL string)
	}{
		{
			name: "serves_turn_over_websocket",
			run: func(t *testing.T, modelURL string) {
				finished := runServePrompts(t, writeTestConfig(t, modelURL, "assistant"), "你好")
				if finished[0].Status != defs.TurnStatusCompleted {
					t.Fatalf("turn finished = %+v", finished[0])
				}
			},
		},
		{
			name: "generates_token_without_config",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, ""), listen)
				file := getTestRuntimeServerFile(t)
				if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(file.Token) {
					t.Fatalf("token = %q, want 64 hexadecimal characters", file.Token)
				}
				if want := "ws://" + listen + "/api/v1/openvaala"; file.URL != want {
					t.Fatalf("url = %q, want %q", file.URL, want)
				}
				// 使用文件里的 token 连接并执行一轮。
				client, stopClient := connectServeClient(t, listen, file.Token)
				defer stopClient()
				finished := runClientPrompts(t, client, "你好")
				if finished[0].Status != defs.TurnStatusCompleted {
					t.Fatalf("turn finished = %+v", finished[0])
				}
			},
		},
		{
			name: "reuses_token_across_restarts",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				first := startTestServe(t, configPath, listen)
				firstToken := getTestRuntimeServerFile(t).Token
				if err := first.stop(t); err != nil {
					t.Fatalf("first serve = %v", err)
				}
				startTestServe(t, configPath, listen)
				if token := getTestRuntimeServerFile(t).Token; token != firstToken {
					t.Fatalf("token after restart = %q, want %q", token, firstToken)
				}
			},
		},
		{
			name: "generates_server_id_without_previous_file",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, ""), listen)
				file := getTestRuntimeServerFile(t)
				if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(file.ServerID) {
					t.Fatalf("server_id = %q, want 32 hexadecimal characters", file.ServerID)
				}
				// initialize 返回的就是文件里的 server_id。
				if serverID := getServeServerID(t, listen, file.Token); serverID != file.ServerID {
					t.Fatalf("initialize server_id = %q, want %q", serverID, file.ServerID)
				}
			},
		},
		{
			name: "reuses_server_id_across_restarts",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				first := startTestServe(t, configPath, listen)
				firstServerID := getTestRuntimeServerFile(t).ServerID
				if err := first.stop(t); err != nil {
					t.Fatalf("first serve = %v", err)
				}
				startTestServe(t, configPath, listen)
				file := getTestRuntimeServerFile(t)
				if file.ServerID != firstServerID {
					t.Fatalf("server_id after restart = %q, want %q", file.ServerID, firstServerID)
				}
				if serverID := getServeServerID(t, listen, file.Token); serverID != firstServerID {
					t.Fatalf("initialize server_id after restart = %q, want %q", serverID, firstServerID)
				}
			},
		},
		{
			name: "keeps_server_id_from_previous_file",
			run: func(t *testing.T, modelURL string) {
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:      "ws://127.0.0.1:1/api/v1/openvaala",
					Token:    "previous-token",
					ServerID: "previous-server-id",
				})
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, "configured-token"), listen)
				if serverID := getTestRuntimeServerFile(t).ServerID; serverID != "previous-server-id" {
					t.Fatalf("server_id = %q, want the previous one", serverID)
				}
				if serverID := getServeServerID(t, listen, "configured-token"); serverID != "previous-server-id" {
					t.Fatalf("initialize server_id = %q, want the previous one", serverID)
				}
			},
		},
		{
			name: "generates_server_id_when_previous_file_has_none",
			run: func(t *testing.T, modelURL string) {
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   "ws://127.0.0.1:1/api/v1/openvaala",
					Token: "previous-token",
				})
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, ""), listen)
				if serverID := getTestRuntimeServerFile(t).ServerID; !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(serverID) {
					t.Fatalf("server_id = %q, want 32 hexadecimal characters", serverID)
				}
			},
		},
		{
			name: "config_token_written_to_file",
			run: func(t *testing.T, modelURL string) {
				// 文件里已有别的 token 时，配置的 token 优先。
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   "ws://127.0.0.1:1/api/v1/openvaala",
					Token: "previous-token",
				})
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, "configured-token"), listen)
				if token := getTestRuntimeServerFile(t).Token; token != "configured-token" {
					t.Fatalf("token = %q, want the configured token", token)
				}
			},
		},
		{
			name: "file_url_uses_loopback",
			run: func(t *testing.T, modelURL string) {
				ipv6Listener, err := net.Listen("tcp", "[::]:0")
				if err != nil {
					t.Skipf("IPv6 is unavailable: %v", err)
				}
				_ = ipv6Listener.Close()
				for _, tt := range []struct{ name, host string }{
					{
						name: "port_only",
						host: "",
					},
					{
						name: "ipv4_any",
						host: "0.0.0.0",
					},
					{
						name: "ipv6_any",
						host: "[::]",
					},
				} {
					_, port, err := net.SplitHostPort(getFreeServeAddress(t))
					if err != nil {
						t.Fatal(err)
					}
					t.Run(tt.name, func(t *testing.T) {
						startTestServe(t, newServeConfig(t, modelURL, tt.host+":"+port, ""), "127.0.0.1:"+port)
						if want := "ws://127.0.0.1:" + port + "/api/v1/openvaala"; getTestRuntimeServerFile(t).URL != want {
							t.Fatalf("url = %q, want %q", getTestRuntimeServerFile(t).URL, want)
						}
					})
				}
			},
		},
		{
			name: "file_permissions",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				// 日志使用另一个目录，连接文件目录由 serve 写入时创建。
				appendConfigText(t, configPath, "telemetry:\n  log_file: "+filepath.Join(t.TempDir(), "serve.log")+"\n")
				startTestServe(t, configPath, listen)
				path := getTestRuntimeServerFilePath(t)
				for _, check := range []struct {
					path string
					want os.FileMode
				}{{
					path,
					0600,
				}, {
					filepath.Dir(path),
					0700,
				}} {
					info, err := os.Stat(check.path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != check.want {
						t.Fatalf("%s mode = %v, want %v", check.path, info.Mode().Perm(), check.want)
					}
				}
			},
		},
		{
			name: "file_records_executable",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, ""), listen)
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				executable, err = filepath.EvalSymlinks(executable)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(executable)
				if err != nil {
					t.Fatal(err)
				}
				file := getTestRuntimeServerFile(t)
				if file.Executable != executable || !file.ExecutableModifiedAt.Equal(info.ModTime()) {
					t.Fatalf("file = %+v, want executable %s modified at %v", file, executable, info.ModTime())
				}
				payload, err := os.ReadFile(getTestRuntimeServerFilePath(t))
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"url:", "token:", "executable:", "executable_modified_at:"} {
					if !strings.Contains(string(payload), key) {
						t.Fatalf("file = %s, want key %s", payload, key)
					}
				}
			},
		},
		{
			name: "port_in_use_keeps_file",
			run: func(t *testing.T, modelURL string) {
				// 用普通 TCP 监听器占用端口，健康检查不可访问，错误不含 serve stop 提示。
				holder, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close()
				listen := holder.Addr().String()
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   "ws://" + listen + "/api/v1/openvaala",
					Token: "previous-token",
				})
				before := readTestRuntimeServerFile(t)
				_, err = runCLI(t, newServeConfig(t, modelURL, listen, ""), "serve")
				if err == nil || !strings.HasPrefix(err.Error(), "listen "+listen+": ") || strings.Contains(err.Error(), "listen tcp") ||
					strings.Contains(err.Error(), "a runtime server is already running") {
					t.Fatalf("serve error = %v, want a single listen error without the serve stop hint", err)
				}
				if after := readTestRuntimeServerFile(t); !bytes.Equal(after, before) {
					t.Fatalf("file changed to %s, want it unchanged", after)
				}
			},
		},
		{
			name: "port_in_use_by_serve_hints_stop",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				startTestServe(t, configPath, listen)
				before := readTestRuntimeServerFile(t)
				_, err := runCLI(t, configPath, "serve")
				if want := "; a runtime server is already running, run openvaala serve stop first"; err == nil || !strings.HasSuffix(err.Error(), want) ||
					!strings.HasPrefix(err.Error(), "listen "+listen+": ") || strings.Contains(err.Error(), "listen tcp") {
					t.Fatalf("second serve error = %v, want a single listen error ending with %q", err, want)
				}
				if after := readTestRuntimeServerFile(t); !bytes.Equal(after, before) {
					t.Fatalf("file changed to %s, want it unchanged", after)
				}
			},
		},
		{
			name: "second_serve_other_port_locked",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				startTestServe(t, newServeConfig(t, modelURL, listen, ""), listen)
				before := readTestRuntimeServerFile(t)
				otherListen := getFreeServeAddress(t)
				_, err := runCLI(t, newServeConfig(t, modelURL, otherListen, ""), "serve")
				if err == nil || !strings.Contains(err.Error(), "another openvaala process is using") {
					t.Fatalf("second serve error = %v, want the data directory lock error", err)
				}
				if after := readTestRuntimeServerFile(t); !bytes.Equal(after, before) {
					t.Fatalf("file changed to %s, want it unchanged", after)
				}
				// 启动失败时释放已经绑定的端口。
				otherListener, err := net.Listen("tcp", otherListen)
				if err != nil {
					t.Fatalf("port %s is still held after the failed start: %v", otherListen, err)
				}
				_ = otherListener.Close()
			},
		},
		{
			name: "stop_without_server",
			run: func(t *testing.T, modelURL string) {
				requireServeStop(t, writeTestConfig(t, modelURL, "assistant"), "no runtime server is running\n", "")
			},
		},
		{
			name: "stop_with_stale_file",
			run: func(t *testing.T, modelURL string) {
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   "ws://" + getFreeServeAddress(t) + "/api/v1/openvaala",
					Token: "stale-token",
				})
				requireServeStop(t, writeTestConfig(t, modelURL, "assistant"), "no runtime server is running\n", "")
			},
		},
		{
			name: "stop_running_server",
			run: func(t *testing.T, _ string) {
				// 模型一直不回复，stop 时 Runtime 里有一个 Turn 在执行。
				turnStarted := make(chan struct{}, 1)
				heldModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// 读完请求正文，再等待连接关闭。
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case turnStarted <- struct{}{}:
					default:
					}
					<-r.Context().Done()
				}))
				defer heldModel.Close()
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, heldModel.URL, listen, serveTestToken)
				serve := startTestServe(t, configPath, listen)
				client, stopClient := connectServeClient(t, listen, serveTestToken)
				defer stopClient()
				conversation, err := client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				if _, err := client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
					ConversationID: conversation.ID,
					Content: []*defs.Content{{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "你好"},
					}},
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-turnStarted:
				case <-time.After(10 * time.Second):
					t.Fatal("the turn did not reach the model")
				}
				requireServeStop(t, configPath, "stopped\n", "")
				// 收到 stopped 后尝试取得数据目录锁。
				dataDirectory, err := ensureDataDirectory()
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := lockDataDirectory(dataDirectory)
				if err != nil {
					t.Fatalf("data directory is still locked after stopped: %v", err)
				}
				if err := unlock(); err != nil {
					t.Fatal(err)
				}
				if exited, err := serve.wait(t, 10*time.Second); !exited || err != nil {
					t.Fatalf("serve exited = %v, err = %v; want it to return nil after stop", exited, err)
				}
			},
		},
		{
			// 健康检查失败但数据目录仍锁定时继续等待，锁释放后输出 stopped。
			name: "stop_waits_for_data_directory_lock",
			run: func(t *testing.T, modelURL string) {
				unlock := holdFakeStoppingServe(t)
				configPath := writeTestConfig(t, modelURL, "assistant")
				type stopResult struct {
					stdout string
					err    error
				}
				done := make(chan stopResult, 1)
				go func() {
					stdout, err := runCLI(t, configPath, "serve", "stop")
					done <- stopResult{
						stdout,
						err,
					}
				}()
				select {
				case result := <-done:
					t.Fatalf("serve stop returned %+v while the data directory lock is held", result)
				case <-time.After(1500 * time.Millisecond):
				}
				if err := unlock(); err != nil {
					t.Fatal(err)
				}
				select {
				case result := <-done:
					if result.err != nil || result.stdout != "stopped\n" {
						t.Fatalf("serve stop = %q, %v; want stopped", result.stdout, result.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("serve stop did not finish after the lock was released")
				}
			},
		},
		{
			// 等待期间 ctx 结束：没有确认 serve 停止，不输出 stopped。
			name: "stop_cancelled_while_waiting",
			run: func(t *testing.T, modelURL string) {
				unlock := holdFakeStoppingServe(t)
				defer func() { _ = unlock() }()
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				defer cancel()
				stdout, err := runCLIWithContext(ctx, writeTestConfig(t, modelURL, "assistant"), "serve", "stop")
				if !errors.Is(err, context.DeadlineExceeded) || stdout != "" {
					t.Fatalf("serve stop = %q, %v; want a context error and no output", stdout, err)
				}
			},
		},
		{
			name: "stop_rejected_token",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				serve := startTestServe(t, configPath, listen)
				file := getTestRuntimeServerFile(t)
				file.Token = "other-token"
				writeTestRuntimeServerFile(t, file)
				requireServeStop(t, configPath, "", "runtime server rejected the token")
				if exited, _ := serve.wait(t, 300*time.Millisecond); exited {
					t.Fatal("serve stopped although stop was rejected")
				}
			},
		},
		{
			name: "reload_without_server",
			run: func(t *testing.T, modelURL string) {
				requireServeReload(t, writeTestConfig(t, modelURL, "assistant"), "no runtime server is running\n", "")
			},
		},
		{
			name: "reload_with_stale_file",
			run: func(t *testing.T, modelURL string) {
				writeTestRuntimeServerFile(t, RuntimeServerConnection{
					URL:   "ws://" + getFreeServeAddress(t) + "/api/v1/openvaala",
					Token: "stale-token",
				})
				requireServeReload(t, writeTestConfig(t, modelURL, "assistant"), "no runtime server is running\n", "")
			},
		},
		{
			name: "reload_with_wrong_token",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, "")
				startTestServe(t, configPath, listen)
				file := getTestRuntimeServerFile(t)
				file.Token = "other-token"
				writeTestRuntimeServerFile(t, file)
				requireServeReload(t, configPath, "", "runtime server rejected the token")
			},
		},
		{
			// 修改启动配置中的 Provider 文件，重新加载后检查新增模型。
			name: "reload_running_server",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, serveTestToken)
				startTestServe(t, configPath, listen)
				writeTestFiles(t, map[string]string{
					filepath.Join(filepath.Dir(configPath), "models", "local.yaml"): "id: local\nprotocol: openai_chat\nbase_url: " + modelURL + "\nmodels:\n  default: {}\n  second: {}\n",
				})
				requireServeReload(t, configPath, "configuration reloaded\n", "")
				client, stopClient := connectServeClient(t, listen, serveTestToken)
				defer stopClient()
				list, err := client.ListModels(t.Context(), "")
				if err != nil {
					t.Fatal(err)
				}
				var refs []string
				for _, model := range list.Models {
					refs = append(refs, model.Ref.String())
				}
				if want := []string{"local/default", "local/second"}; !slices.Equal(refs, want) {
					t.Fatalf("models after reload = %v, want %v", refs, want)
				}
			},
		},
		{
			// 对话正在等模型回复：reload 不让它换上新配置，第二行列出它。
			name: "reload_with_running_conversation",
			run: func(t *testing.T, _ string) {
				// 模型等待 release 或请求取消，收尾先释放请求再关闭模型服务。
				turnStarted, release := make(chan struct{}, 1), make(chan struct{})
				heldModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case turnStarted <- struct{}{}:
					default:
					}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer heldModel.Close()
				defer close(release)
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, heldModel.URL, listen, serveTestToken)
				startTestServe(t, configPath, listen)
				client, stopClient := connectServeClient(t, listen, serveTestToken)
				defer stopClient()
				conversation, err := client.CreateConversation(t.Context(), &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.OpenConversation(t.Context(), &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
					t.Fatal(err)
				}
				if _, err := client.SubmitInput(t.Context(), &runtimeclient.SubmitInputRequest{
					ConversationID: conversation.ID,
					Content: []*defs.Content{{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "你好"},
					}},
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-turnStarted:
				case <-time.After(10 * time.Second):
					t.Fatal("the turn did not reach the model")
				}
				requireServeReload(t, configPath, "configuration reloaded\n"+
					"1 conversations are running and keep the old configuration until the next reload: "+string(conversation.ID)+"\n", "")
			},
		},
		{
			name: "idle_timeout_exits",
			run: func(t *testing.T, modelURL string) {
				listen := getFreeServeAddress(t)
				configPath := newServeConfig(t, modelURL, listen, serveTestToken)
				serve := startTestServe(t, configPath, listen, "--idle-timeout", "1s")
				client, stopClient := connectServeClient(t, listen, serveTestToken)
				if finished := runClientPrompts(t, client, "你好"); finished[0].Status != defs.TurnStatusCompleted {
					t.Fatalf("turn finished = %+v", finished[0])
				}
				stopClient()
				if exited, err := serve.wait(t, 5*time.Second); !exited || err != nil {
					t.Fatalf("serve exited = %v, err = %v; want it to return nil after the idle timeout", exited, err)
				}
			},
		},
	}
	// 2. 在独立 HOME 和受控模型服务下执行各场景。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, modelTestResponse)
			}))
			defer model.Close()
			tt.run(t, model.URL)
		})
	}
}

// requireServeStop 运行停止命令并检查输出或错误。
func requireServeStop(t *testing.T, configPath, wantStdout, wantErr string) {
	t.Helper()
	// 1. 执行 serve stop，按预期检查错误文本或成功输出。
	stdout, err := runCLI(t, configPath, "serve", "stop")
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("serve stop error = %v, want containing %q", err, wantErr)
		}
		return
	}
	if err != nil || stdout != wantStdout {
		t.Fatalf("serve stop = %q, %v; want %q", stdout, err, wantStdout)
	}
}

// requireServeReload 运行重新加载命令并检查输出和错误。
func requireServeReload(t *testing.T, configPath, wantStdout, wantErr string) {
	t.Helper()
	// 1. 执行 serve reload，检查返回错误和标准输出。
	stdout, err := runCLI(t, configPath, "serve", "reload")
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) || stdout != wantStdout {
			t.Fatalf("serve reload = %q, %v; want %q and an error containing %q", stdout, err, wantStdout, wantErr)
		}
		return
	}
	if err != nil || stdout != wantStdout {
		t.Fatalf("serve reload = %q, %v; want %q", stdout, err, wantStdout)
	}
}

// testServe 是在测试进程内运行的 serve 命令。
type testServe struct {
	cancel context.CancelFunc
	done   chan error
	exited bool
	err    error
}

// startTestServe 在测试进程内启动 serve。
func startTestServe(t *testing.T, configPath, readyAddress string, serveArgs ...string) *testServe {
	t.Helper()
	// 1. 创建命令和取消 context，后台执行 serve。
	ctx, cancel := context.WithCancel(t.Context())
	command := NewCLI(&CLIOpt{
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	command.SetArgs(append([]string{"--config-file", configPath, "serve"}, serveArgs...))
	serve := &testServe{
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { serve.done <- command.ExecuteContext(ctx) }()
	// 2. 登记测试收尾时停止服务，等待健康检查可用后返回。
	t.Cleanup(func() { _ = serve.stop(t) })
	waitServeReady(t, readyAddress, serve)
	return serve
}

// getExit 读取 serve 退出结果。
func (s *testServe) getExit() (exited bool, err error) {
	// 1. 服务未标为退出时不阻塞地读取退出通道并保存结果，再返回保存的结果。
	if !s.exited {
		select {
		case s.err = <-s.done:
			s.exited = true
		default:
		}
	}
	return s.exited, s.err
}

// wait 限时等待 serve 退出。
func (s *testServe) wait(t *testing.T, timeout time.Duration) (exited bool, err error) {
	t.Helper()
	// 1. 未退出时等待结果或超时，超时返回 exited=false；重复调用返回已保存结果。
	if !s.exited {
		select {
		case s.err = <-s.done:
			s.exited = true
		case <-time.After(timeout):
			return false, nil
		}
	}
	return true, s.err
}

// stop 取消并等待 serve 退出，返回保存的执行结果。
func (s *testServe) stop(t *testing.T) error {
	t.Helper()
	// 1. 取消服务并最多等待 10 秒，仍未退出时报告测试失败。
	s.cancel()
	exited, err := s.wait(t, 10*time.Second)
	if !exited {
		t.Fatal("serve did not stop after cancel")
	}
	return err
}

// startServeClient 启动测试 serve 和 Client。
func startServeClient(t *testing.T, configPath string) (context.Context, runtimeclient.Client, func()) {
	t.Helper()
	// 1. 选择空闲端口并写入测试服务配置，启动服务和 Client。
	listen := getFreeServeAddress(t)
	appendServerConfig(t, configPath, listen, serveTestToken)
	serve := startTestServe(t, configPath, listen)
	client, stopClient := connectServeClient(t, listen, serveTestToken)
	// 2. 返回测试 context、Client 和先停 Client 再停 serve 的函数。
	return t.Context(), client, func() {
		stopClient()
		if err := serve.stop(t); err != nil {
			t.Fatalf("serve error = %v", err)
		}
	}
}

// connectServeClient 创建并运行 Client，返回停止和等待它的函数。
func connectServeClient(t *testing.T, listen, token string) (runtimeclient.Client, func()) {
	t.Helper()
	// 1. 使用指定地址和 token 创建 Client 并后台运行。
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt("ws://"+listen+"/api/v1/openvaala", token))
	if err != nil {
		t.Fatal(err)
	}
	clientCtx, stopClient := context.WithCancel(t.Context())
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(clientCtx) }()
	// 2. 返回收尾函数，取消 Client、读完事件并检查退出结果。
	return client, func() {
		stopClient()
		for range client.Events() {
		}
		if err := <-clientDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("client Run() = %v, want nil or context.Canceled", err)
		}
	}
}

// getServeServerID 连接测试 serve，读取连接通知中的 server_id。
func getServeServerID(t *testing.T, listen, token string) string {
	t.Helper()
	// 1. 启动 Client，最多等 10 秒读取已连接事件并返回标识。
	client, stopClient := connectServeClient(t, listen, token)
	defer stopClient()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event := <-client.Events():
			if changed, ok := event.(*runtimeclient.ConnectionChanged); ok && changed.State == runtimeclient.ConnectionConnected {
				return changed.ServerID
			}
		case <-timeout:
			t.Fatal("client did not connect to the serve")
		}
	}
}

// runServePrompts 启动服务，执行全部输入并返回 Turn 结束通知。
func runServePrompts(t *testing.T, configPath string, prompts ...string) []*runtimeclient.TurnFinished {
	t.Helper()
	// 1. 启动 serve 和 Client，运行输入后依次停止二者。
	_, client, stop := startServeClient(t, configPath)
	finished := runClientPrompts(t, client, prompts...)
	stop()
	return finished
}

// runClientPrompts 创建并打开对话，逐条提交输入并等待 Turn 结束。
func runClientPrompts(t *testing.T, client runtimeclient.Client, prompts ...string) []*runtimeclient.TurnFinished {
	t.Helper()
	// 1. 创建并打开测试对话。
	ctx := t.Context()
	conversation, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{AgentName: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}); err != nil {
		t.Fatal(err)
	}
	// 2. 逐条提交文本输入，收集每个 Turn 的结束事件。
	var finished []*runtimeclient.TurnFinished
	for _, prompt := range prompts {
		if _, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
			ConversationID: conversation.ID,
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: prompt},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		finished = append(finished, waitTurnFinished(t, client))
	}
	return finished
}

// waitTurnFinished 等待 Client 发出 Turn 结束通知。
func waitTurnFinished(t *testing.T, client runtimeclient.Client) *runtimeclient.TurnFinished {
	t.Helper()
	// 1. 最多等 10 秒读取 TurnFinished，期间忽略其他事件；事件流关闭或超时则失败。
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatal("client events closed before the turn finished")
			}
			if finished, isFinished := event.(*runtimeclient.TurnFinished); isFinished {
				return finished
			}
		case <-timeout:
			t.Fatal("turn did not finish")
		}
	}
}

// runCLI 使用指定配置限时执行命令并返回输出和错误。
func runCLI(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	// 1. 设置 10 秒超时并执行命令。
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return runCLIWithContext(ctx, configPath, args...)
}

// runCLIWithContext 在给定 context 下运行命令，返回标准输出和执行错误。
func runCLIWithContext(ctx context.Context, configPath string, args ...string) (string, error) {
	// 1. 创建命令并加入配置路径，执行后返回输出和错误。
	var stdout bytes.Buffer
	command := NewCLI(&CLIOpt{
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	command.SetArgs(append([]string{"--config-file", configPath}, args...))
	err := command.ExecuteContext(ctx)
	return stdout.String(), err
}

// holdFakeStoppingServe 模拟健康检查失败但仍持有数据目录锁的 serve。
func holdFakeStoppingServe(t *testing.T) (unlock func() error) {
	t.Helper()
	// 1. 启动接受 shutdown 返回 202、其他请求返回 503 的服务，并写入连接文件。
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/shutdown" && r.Header.Get("Authorization") == "Bearer fake-token" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(fake.Close)
	writeTestRuntimeServerFile(t, RuntimeServerConnection{
		URL:   "ws://" + strings.TrimPrefix(fake.URL, "http://") + "/api/v1/openvaala",
		Token: "fake-token",
	})
	// 2. 取得数据目录锁，登记测试结束时释放并返回解锁函数。
	dataDirectory, err := ensureDataDirectory()
	if err != nil {
		t.Fatal(err)
	}
	unlock, err = lockDataDirectory(dataDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unlock() })
	return unlock
}

// newServeConfig 写入测试配置并追加服务设置。
func newServeConfig(t *testing.T, modelURL, listen, token string) string {
	t.Helper()
	// 1. 创建模型和 Agent 配置，再追加监听地址与可选令牌。
	configPath := writeTestConfig(t, modelURL, "assistant")
	appendServerConfig(t, configPath, listen, token)
	return configPath
}

// appendServerConfig 为测试配置追加一次 server 段。
func appendServerConfig(t *testing.T, configPath, listen, token string) {
	t.Helper()
	// 1. 写入监听地址，token 非空时一并写入。
	text := "server:\n  listen: \"" + listen + "\"\n"
	if token != "" {
		text += "  token: \"" + token + "\"\n"
	}
	appendConfigText(t, configPath, text)
}

// appendConfigText 向已有测试配置文件追加文本。
func appendConfigText(t *testing.T, configPath, text string) {
	t.Helper()
	// 1. 以追加模式打开文件，写入文本后关闭。
	file, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// getTestRuntimeServerFilePath 读取本机连接文件路径。
func getTestRuntimeServerFilePath(t *testing.T) string {
	t.Helper()
	// 1. 调用路径查询并检查错误，失败时结束测试。
	path, err := getRuntimeServerFilePath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// getTestRuntimeServerFile 读取本机连接文件。
func getTestRuntimeServerFile(t *testing.T) RuntimeServerConnection {
	t.Helper()
	// 1. 读取并检查连接文件存在且解析成功，不存在或解析失败时结束测试。
	file, found, err := getRuntimeServerFile()
	if err != nil || !found {
		t.Fatalf("runtime server file found = %v, err = %v", found, err)
	}
	return file
}

// readTestRuntimeServerFile 读取本机连接文件的原始内容。
func readTestRuntimeServerFile(t *testing.T) []byte {
	t.Helper()
	// 1. 读取文件字节，失败时结束测试。
	payload, err := os.ReadFile(getTestRuntimeServerFilePath(t))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// writeTestRuntimeServerFile 按 serve 格式写入测试连接文件。
func writeTestRuntimeServerFile(t *testing.T, file RuntimeServerConnection) {
	t.Helper()
	// 1. 编码连接信息并写入连接文件路径。
	payload, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFiles(t, map[string]string{getTestRuntimeServerFilePath(t): string(payload)})
}

// getFreeServeAddress 分配并释放本机随机端口，返回监听地址。
func getFreeServeAddress(t *testing.T) string {
	t.Helper()
	// 1. 监听 127.0.0.1 随机端口，读取地址后关闭监听器。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// waitServeReady 等待测试 serve 的健康检查通过。
func waitServeReady(t *testing.T, listen string, serve *testServe) {
	t.Helper()
	// 1. 使用单次 1 秒超时的 HTTP Client，在 10 秒内轮询 healthz，返回 200 时结束等待，服务提前退出或等待超时则失败。
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if exited, err := serve.getExit(); exited {
			t.Fatalf("serve exited before ready: %v", err)
		}
		response, err := client.Get("http://" + listen + "/api/v1/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve at %s is not ready: %v", listen, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
