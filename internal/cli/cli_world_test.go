package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/VaalaCat/openvaala/www"
)

func TestWorldServeListen(t *testing.T) {
	for _, tc := range []struct {
		name, file, env, flag, want string
	}{
		{name: "default", want: "0.0.0.0:8791"},
		{name: "file", file: "127.0.0.1:8801", want: "127.0.0.1:8801"},
		{name: "environment", file: "127.0.0.1:8801", env: "127.0.0.1:8802", want: "127.0.0.1:8802"},
		{name: "flag", file: "127.0.0.1:8801", env: "127.0.0.1:8802", flag: "--listen=127.0.0.1:8803", want: "127.0.0.1:8803"},
		{name: "removed flag", flag: "--server=http://127.0.0.1:8803"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("OPENVAALA_WORLD_SERVER_LISTEN", tc.env)
			if tc.env == "" {
				if err := os.Unsetenv("OPENVAALA_WORLD_SERVER_LISTEN"); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(home, "config.yaml")
			writeTestFiles(t, map[string]string{path: fmt.Sprintf("world:\n  server:\n    enabled: true\n    domain: example.test\n    listen: %q\n", tc.file)})
			root := NewCLI(&CLIOpt{Stdout: io.Discard, Stderr: io.Discard})
			serve, _, err := root.Find([]string{"world", "serve"})
			if err != nil {
				t.Fatal(err)
			}
			var worldListen, runtimeListen string
			serve.RunE = BuildCLIHandler(func(_ context.Context, args *AppArgs) error {
				worldListen = args.Config.World.Server.Listen
				runtimeListen = args.Config.Server.Listen
				return nil
			})
			arguments := []string{"--config-file", path, "world", "serve"}
			if tc.flag != "" {
				arguments = append(arguments, tc.flag)
			}
			root.SetArgs(arguments)
			err = root.ExecuteContext(t.Context())
			if tc.want == "" {
				if err == nil || worldListen != "" {
					t.Fatalf("removed --server: listen = %q, error = %v", worldListen, err)
				}
				return
			}
			if err != nil || worldListen != tc.want || runtimeListen != "127.0.0.1:8790" {
				t.Fatalf("world listen = %q, runtime listen = %q, want %q and runtime default, error = %v", worldListen, runtimeListen, tc.want, err)
			}
		})
	}
}

func TestWorldServeTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name, logFile string
	}{
		{name: "default under data directory"},
		{name: "explicit log file", logFile: "logs/control.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, directory := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(directory, "config.yaml")
			writeTestFiles(t, map[string]string{path: fmt.Sprintf("world:\n  server:\n    enabled: true\n    domain: example.test\ntelemetry:\n  log_file: %q\n", tc.logFile)})
			listen := getFreeServeAddress(t)
			ctx, cancel := context.WithCancel(t.Context())
			server := &testServe{cancel: cancel, done: make(chan error, 1)}
			go func() {
				_, err := runCLIWithContext(ctx, path, "--data-dir", "instance", "world", "serve", "--listen", listen)
				server.done <- err
			}()
			t.Cleanup(func() { _ = server.stop(t) })
			endpoint := "http://" + listen + "/world/v1/healthz"
			for deadline := time.Now().Add(10 * time.Second); ; {
				if exited, err := server.getExit(); exited {
					t.Fatalf("control plane exited before startup: %v", err)
				}
				response, err := http.Get(endpoint)
				if err == nil {
					response.Body.Close()
					if response.StatusCode == http.StatusOK {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("control plane did not start: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := server.stop(t); err != nil {
				t.Fatal(err)
			}
			dataDirectory := filepath.Join(directory, "instance")
			if _, err := os.Stat(filepath.Join(dataDirectory, "worldserver", "world.db")); err != nil {
				t.Fatal(err)
			}
			logFile := filepath.Join(dataDirectory, "openvaala.log")
			if tc.logFile != "" {
				if _, err := os.Stat(logFile); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("default log exists, stat = %v", err)
				}
				logFile = filepath.Join(directory, tc.logFile)
			}
			log, err := os.ReadFile(logFile)
			if err != nil || !strings.Contains(string(log), "world server listening") {
				t.Fatalf("log %s = %s, error = %v", logFile, log, err)
			}
			assertWorldDefaultDirectoriesAbsent(t, home)
		})
	}
}

func assertWorldDefaultDirectoriesAbsent(t *testing.T, home string) {
	t.Helper()
	for _, kind := range []string{"share", "state"} {
		path := filepath.Join(home, ".local", kind, "openvaala")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("default directory %s was used: %v", path, err)
		}
	}
}

// TestWorldNodeProcess 用独立的用户目录和进程运行第二台节点。
func TestWorldNodeProcess(t *testing.T) {
	// 1. 由双节点测试提供配置，普通测试运行时跳过。
	path := os.Getenv("OPENVAALA_TEST_WORLD_NODE_CONFIG")
	if path == "" {
		t.Skip("requires the second node's configuration")
	}
	if _, err := runCLIWithContext(t.Context(), path, "serve"); err != nil {
		t.Fatal(err)
	}
}

// TestWorldCLI 经真实 CLI 检查可选装配、入网、投递、配置重载和服务退出。
func TestWorldCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"disabled", func(t *testing.T) {
			// 1. 普通 serve 只提供 Runtime 路由和数据目录。
			home := t.TempDir()
			t.Setenv("HOME", home)
			listen := getFreeServeAddress(t)
			path := newServeConfig(t, "http://127.0.0.1:1", listen, serveTestToken)
			startTestServe(t, path, listen)
			var status world.NodeStatus
			worldCommandResult(t, path, &status, "status")
			if status.Enabled || status.AdminEnabled {
				t.Fatal(status)
			}
			for _, path := range []string{"/world/v1/direct", "/world/v1/healthz"} {
				checkWorldHTTP(t, "http://"+listen+path, http.StatusNotFound)
			}
			for _, dir := range []string{"world", "worldserver"} {
				if _, err := os.Stat(filepath.Join(config.GetDataDirectory(home), dir)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s: %v", dir, err)
				}
			}
		}},
		{"standalone_public_relay", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, "config.yaml")
			writeTestFiles(t, map[string]string{path: "{}\n"})
			listen := getFreeServeAddress(t)
			ctx, cancel := context.WithCancel(t.Context())
			relay := &testServe{cancel: cancel, done: make(chan error, 1)}
			go func() {
				_, err := runCLIWithContext(ctx, path, "world", "serve", "--relay-mode", "public", "--listen", listen)
				relay.done <- err
			}()
			t.Cleanup(func() {
				if err := relay.stop(t); err != nil {
					t.Error(err)
				}
			})
			waitWorldHTTP(t, "http://"+listen+"/world/v1/healthz")
		}},
		{"standalone_join_combined_delivery", func(t *testing.T) { testWorldDelivery(t, false) }},
		{"custom_data_directory", func(t *testing.T) { testWorldDelivery(t, true) }},
	} {
		t.Run(tc.name, tc.run)
	}
}

func testWorldDelivery(t *testing.T, customDataDirectory bool) {
	// 1. 从无节点配置开始，独立运行控制面并生成私有管理凭据。
	home := t.TempDir()
	t.Setenv("HOME", home)
	model := &scriptedModel{requests: make(chan map[string]any, 8), reply: func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, modelTestResponse)
	}}
	modelServer := httptest.NewServer(model)
	t.Cleanup(modelServer.Close)
	listen := getFreeServeAddress(t)
	base := "http://" + listen
	path := newServeConfig(t, modelServer.URL, listen, serveTestToken)
	dataDirectory := config.GetDataDirectory(home)
	if customDataDirectory {
		appendConfigText(t, path, "data_dir: ./world-instance\n")
		dataDirectory = filepath.Join(filepath.Dir(path), "world-instance")
	}
	appendConfigText(t, path, "world:\n  server:\n    enabled: true\n    listen: "+listen+"\n    domain: example.test\n")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	standalone := &testServe{cancel: cancel, done: make(chan error, 1)}
	go func() { _, err := runCLIWithContext(ctx, path, "world", "serve"); standalone.done <- err }()
	t.Cleanup(func() {
		if err := standalone.stop(t); err != nil {
			t.Error(err)
		}
	})
	waitWorldHTTP(t, base+"/world/v1/healthz")
	checkWorldHTTP(t, base+"/api/v1/healthz", 404)
	for _, name := range []string{"metadata.db", "runtime.lock", "world"} {
		if _, err := os.Stat(filepath.Join(dataDirectory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("standalone %s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		path     string
		status   int
		location string
	}{
		{"/", http.StatusFound, "/web/v1/world/admin"},
		{"/web/v1/world/admin", http.StatusOK, ""},
		{"/web/v1/unknown.js", http.StatusNotFound, ""},
		{"/world/v1/unknown", http.StatusNotFound, ""},
		{"/api/v1/openvaala", http.StatusNotFound, ""},
	} {
		if www.GetAssets() == nil && (tc.path == "/" || tc.path == "/web/v1/world/admin") {
			tc.status, tc.location = http.StatusNotFound, ""
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Get(base + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status || resp.Header.Get("Location") != tc.location {
			t.Fatalf("%s: status %d, location %q", tc.path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 || cfg.World.Server.AdminToken == "" {
		t.Fatalf("world admin credential not saved privately: %v", err)
	}
	adminToken := cfg.World.Server.AdminToken
	// 2. 用 CLI 签发入网码，在两个互相独立的用户目录下注册本机身份。
	var ticket worldserver.CreateJoinTokenResponse
	worldCommandResult(t, path, &ticket, "token", "create", "--owner", "team", "--tags", "a,b", "--ttl", "1h", "--reusable")
	if !ticket.Reusable || !slices.Equal(ticket.Tags, []string{"a", "b"}) {
		t.Fatalf("token flags were lost")
	}
	var tokens []worldserver.JoinTokenInfo
	worldCommandResult(t, path, &tokens, "token", "ls", "--server", base, "--admin-token", adminToken)
	if len(tokens) != 1 || tokens[0].ID != ticket.ID {
		t.Fatalf("tokens = %+v", tokens)
	}
	var alice, bob worldserver.RegisterNodeResponse
	worldCommandResult(t, path, &alice, "join", ticket.Token, "--name", "alice")
	bobHome := t.TempDir()
	bobListen := getFreeServeAddress(t)
	bobConfig := filepath.Join(bobHome, ".config", "openvaala", "config.yaml")
	t.Setenv("HOME", bobHome)
	worldCommandResult(t, "", &bob, "join", ticket.Token, "--name", "bob")
	appendConfigText(t, bobConfig, fmt.Sprintf("models: {directory: %q}\n", filepath.Join(filepath.Dir(path), "models")))
	appendServerConfig(t, bobConfig, bobListen, serveTestToken)
	t.Setenv("HOME", home)
	if alice.Domain != "example.test" || alice.NodeID == bob.NodeID {
		t.Fatal("unexpected registration identities")
	}
	for _, directory := range []string{dataDirectory, config.GetDataDirectory(bobHome)} {
		info, err := os.Stat(filepath.Join(directory, "world", "identity.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("identity permissions: %v", err)
		}
	}
	http.DefaultClient.CloseIdleConnections()
	if err := standalone.stop(t); err != nil {
		t.Fatal(err)
	}
	// 3. 共享端口启动 Alice 和控制面，Bob 以独立进程连接，核对鉴权和在线投递。
	serve := startTestServe(t, path, listen, "--idle-timeout", "50ms")
	for _, filename := range []string{"world/world.db", "worldserver/world.db"} {
		if _, err := os.Stat(filepath.Join(dataDirectory, filename)); err != nil {
			t.Fatal(err)
		}
	}
	if customDataDirectory {
		if _, err := os.Stat(filepath.Join(dataDirectory, "runtime-server.yaml")); err != nil {
			t.Fatal(err)
		}
		assertWorldDefaultDirectoriesAbsent(t, home)
	}
	checkWorldHTTP(t, base+"/api/v1/world/status", http.StatusNotFound)
	waitWorldHTTP(t, base+"/world/v1/healthz")
	proc := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWorldNodeProcess$")
	proc.Env = append(os.Environ(), "HOME="+bobHome, "OPENVAALA_TEST_WORLD_NODE_CONFIG="+bobConfig)
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	bobDone := make(chan error, 1)
	go func() { bobDone <- proc.Wait() }()
	bobExited := false
	t.Cleanup(func() {
		if !bobExited {
			_ = proc.Process.Kill()
			<-bobDone
		}
	})
	waitWorldHTTP(t, "http://"+bobListen+"/api/v1/healthz")
	waitWorldPeer(t, path, true)
	var status struct {
		Connected bool `json:"connected"`
	}
	worldCommandResult(t, path, &status, "status")
	if !status.Connected {
		t.Fatal("node disconnected")
	}
	var reply world.WorldMessage
	worldCommandResult(t, path, &reply, "send", "bob/default", "hello", "--wait=true", "--thread", "cross-node")
	if reply.Status != world.WorldMessageStatusReplied || reply.ReplyBody != "你好。" || reply.ThreadID != "cross-node" {
		t.Fatalf("online send = %+v", reply)
	}
	select {
	case <-model.requests:
	case <-time.After(time.Second):
		t.Fatal("no model request from Bob")
	}
	// 4. 真实 reload 后发起新对话，在模型请求上检查三个联邦工具。
	if _, err := runCLI(t, path, "serve", "reload"); err != nil {
		t.Fatal(err)
	}
	client, stop := connectServeClient(t, listen, serveTestToken)
	var conversation string
	for _, operation := range []string{"reload", "setDefaultModel"} {
		if operation == "setDefaultModel" {
			if _, err := client.SetDefaultModel(t.Context(), defs.ModelRef{ProviderID: "local", ModelID: "default"}); err != nil {
				t.Fatal(err)
			}
		}
		finished := runClientPrompts(t, client, "reply")
		conversation = string(finished[0].ConversationID)
		select {
		case request := <-model.requests:
			tools := getACPModelToolNames(request)
			for _, name := range []string{"list_world_peers", "send_world_message", "get_world_message"} {
				if !slices.Contains(tools, name) {
					t.Fatalf("after %s tools = %v", operation, tools)
				}
			}
		case <-time.After(time.Second):
			t.Fatalf("no model request after %s", operation)
		}
	}
	stop()
	// 5. Bob 停机后，CLI 可转发真实回复、过滤发件箱并取消待投递消息。
	shutdown, err := requestRuntimeServerShutdown(t.Context(), "http://"+bobListen+"/api/v1/shutdown", serveTestToken)
	if err != nil {
		t.Fatal(err)
	}
	shutdown.Body.Close()
	select {
	case err := <-bobDone:
		bobExited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second node did not stop")
	}
	waitWorldPeer(t, path, false)
	var forward world.WorldMessage
	worldCommandResult(t, path, &forward, "forward", conversation, "bob/default", "--note", "FYI")
	if forward.Body != "你好。\n\nFYI" || forward.Status != world.WorldMessageStatusPendingOffline {
		t.Fatalf("forward = %+v", forward)
	}
	var outbox struct {
		Items      []world.WorldMessage
		NextCursor dao.RecordCursor[string] `json:"next_cursor"`
	}
	worldCommandResult(t, path, &outbox, "messages", "--conversation", conversation, "--direction", "outbound", "--peer", "bob@example.test/default", "--status", "pending_offline", "--kind", "message")
	if len(outbox.Items) != 1 || outbox.Items[0].MessageID != forward.MessageID || outbox.NextCursor.ID != "" {
		t.Fatalf("outbox = %+v", outbox)
	}
	firstPage, secondPage := outbox, outbox
	worldCommandResult(t, path, &firstPage, "messages", "--direction", "outbound", "--limit", "1")
	if len(firstPage.Items) != 1 || firstPage.Items[0].MessageID != forward.MessageID || firstPage.NextCursor.ID != forward.MessageID {
		t.Fatalf("outbox first page = %+v", firstPage)
	}
	output, err := runCLI(t, path, "world", "messages", "--direction", "outbound", "--limit", "1", "--cursor-id", firstPage.NextCursor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(output), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].MessageID != reply.MessageID || secondPage.NextCursor.ID != "" {
		t.Fatalf("outbox second page = %+v", secondPage)
	}
	var cancelled world.WorldMessage
	worldCommandResult(t, path, &cancelled, "cancel", forward.MessageID)
	if cancelled.Status != world.WorldMessageStatusCancelled {
		t.Fatalf("cancel = %+v", cancelled)
	}
	// 6. 经管理命令读写 ACL、吊销凭据和节点。
	aclFile := filepath.Join(t.TempDir(), "acl.yaml")
	writeTestFiles(t, map[string]string{aclFile: "- from: ['owner:*']\n  to: ['owner:*']\n  action: allow\n"})
	worldCommandResult(t, path, nil, "acl", "set", "--file", aclFile)
	var rules []worldserver.ACLRule
	worldCommandResult(t, path, &rules, "acl", "ls")
	if len(rules) != 1 || rules[0].Action != "allow" {
		t.Fatalf("acl = %+v", rules)
	}
	worldCommandResult(t, path, nil, "token", "revoke", ticket.ID)
	worldCommandResult(t, path, nil, "node", "revoke", bob.NodeID)
	var nodes []worldserver.NodeRecord
	worldCommandResult(t, path, &nodes, "node", "ls")
	if !slices.ContainsFunc(nodes, func(n worldserver.NodeRecord) bool { return n.NodeID == bob.NodeID && n.RevokedAt != 0 }) {
		t.Fatalf("node revocation missing")
	}
	worldCommandResult(t, path, nil, "node", "revoke", "--owner", "team")
	// 保持一个 World 订阅，验证 serve 退出时能结束连接。
	subscriber, stopSubscriber := connectServeClient(t, listen, serveTestToken)
	defer stopSubscriber()
	if err := subscriber.SubscribeWorld(t.Context()); err != nil {
		t.Fatal(err)
	}
	serve.cancel()
	if exited, err := serve.wait(t, 3*time.Second); !exited || err != nil {
		t.Fatalf("world subscription shutdown: exited %v, error %v", exited, err)
	}
}

func worldCommandResult(t *testing.T, path string, target any, arguments ...string) {
	t.Helper()
	// 1. 经命令树和配置文件发送请求，解析机器可读输出。
	output, err := runCLI(t, path, append([]string{"world", "--json"}, arguments...)...)
	if err != nil {
		t.Fatalf("world %s: %v", arguments[0], err)
	}
	if target != nil {
		if err := json.Unmarshal([]byte(output), target); err != nil {
			t.Fatalf("world %s: %v", arguments[0], err)
		}
	}
}

func waitWorldPeer(t *testing.T, path string, online bool) {
	t.Helper()
	// 1. 等待真实通讯录推送反映 Bob 的上下线。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var result struct {
			Peers []worldserver.DirectoryNode `json:"peers"`
		}
		worldCommandResult(t, path, &result, "peers")
		if slices.ContainsFunc(result.Peers, func(peer worldserver.DirectoryNode) bool { return peer.NodeName == "bob" && peer.Online == online }) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("bob online did not become %v", online)
}

func checkWorldHTTP(t *testing.T, endpoint string, status int) {
	t.Helper()
	// 1. 检查该模式下路由的公开响应。
	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("GET %s = %d, want %d", endpoint, resp.StatusCode, status)
	}
}

func waitWorldHTTP(t *testing.T, endpoint string) {
	t.Helper()
	// 1. 等待服务完成启动。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(endpoint)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("service did not start: %s", endpoint)
}

func TestWorldServeAdminRoutes(t *testing.T) {
	const adminToken = "world-admin-test-token"
	for _, tc := range []struct {
		name         string
		node, server bool
	}{
		{"both disabled", false, false},
		{"node only", true, false},
		{"server only", false, true},
		{"both enabled", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			listen := getFreeServeAddress(t)
			base := "http://" + listen
			configPath := newServeConfig(t, "http://127.0.0.1:1", listen, serveTestToken)
			appendConfigText(t, configPath, fmt.Sprintf("world:\n  node:\n    enabled: %t\n    name: office\n    domain: example.test\n    server_url: %s\n    default_target: assistant\n  server:\n    enabled: %t\n    domain: example.test\n    admin_token: %s\n", tc.node, base, tc.server, adminToken))
			serve := startTestServe(t, configPath, listen)
			defer serve.stop(t)
			if tc.server {
				waitWorldHTTP(t, base+"/world/v1/healthz")
			}
			client, stop := connectServeClient(t, listen, serveTestToken)
			defer stop()
			status, err := client.GetWorldStatus(t.Context())
			if err != nil || status.Enabled != tc.node || status.AdminEnabled != tc.server {
				t.Fatalf("status = %+v, %v", status, err)
			}
			if !tc.server {
				return
			}
			const aclPath = "/world/v1/admin/acl"
			for _, request := range []struct {
				method, token string
				status        int
			}{
				{http.MethodOptions, "", http.StatusNoContent},
				{http.MethodGet, "", http.StatusUnauthorized},
				{http.MethodGet, serveTestToken, http.StatusUnauthorized},
				{http.MethodGet, adminToken, http.StatusOK},
			} {
				req, err := http.NewRequestWithContext(t.Context(), request.method, base+aclPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Origin", "http://admin.example.test")
				if request.token != "" {
					req.Header.Set("Authorization", "Bearer "+request.token)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != request.status || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
					resp.Header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" ||
					!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "PUT") {
					t.Fatalf("%s: response = %d, headers = %v", request.method, resp.StatusCode, resp.Header)
				}
			}
		})
	}
}
