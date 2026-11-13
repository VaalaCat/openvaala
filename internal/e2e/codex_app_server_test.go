package e2e

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
	"gopkg.in/yaml.v3"
)

// e2eTimeout 是等待子进程启动、响应和退出的上限；带 -race 编译的进程明显更慢。
const e2eTimeout = 30 * time.Second

// TestCodexHandshake 检查两种传输下的握手、启动查询和连接错误。
func TestCodexHandshake(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix socket 两种传输下运行。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 启动 app-server，检查初始化前的请求被拒、initialize 的回答和重复初始化被拒。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			if mode == "unix" {
				// 上次异常退出留下的 socket 文件：连不上，启动时当作残留删掉，下面检查新 socket 的权限和路径。
				leaveStaleSocket(t, filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "app-server.sock"))
			}
			wire := startAppServer(t, binary, config, mode)

			assertCodexError(t, wire.response(t, wire.request(t, "model/list", map[string]any{})), -32600, "Not initialized")
			var initialized struct {
				UserAgent      string `json:"userAgent"`
				CodexHome      string `json:"codexHome"`
				PlatformFamily string `json:"platformFamily"`
				PlatformOs     string `json:"platformOs"`
			}
			wire.call(t, "initialize", codexClientInfo, &initialized)
			if !strings.HasPrefix(initialized.UserAgent, "openvaala/") || initialized.PlatformOs != runtime.GOOS ||
				initialized.PlatformFamily != "unix" || initialized.CodexHome != filepath.Dir(config) {
				t.Fatalf("initialize = %+v, want an openvaala user agent on %s with codexHome %s", initialized, runtime.GOOS, filepath.Dir(config))
			}
			wire.notify(t, "initialized", nil)
			assertCodexError(t, wire.response(t, wire.request(t, "initialize", codexClientInfo)), -32600, "Already initialized")

			// 2. 逐项把启动时的查询结果整体和期望的 JSON 比较，必填字段一个不少。
			for _, query := range []struct {
				method string
				params any
				want   string
			}{
				// account/read 与 getAuthStatus 均报告不需要 OpenAI 登录。
				{
					method: "account/read",
					params: map[string]any{"refreshToken": false},
					want:   `{"account":null,"requiresOpenaiAuth":false}`,
				},
				// Codex app 每隔几秒查一次登录状态：和 Codex 对不需要 OpenAI 登录的自定义 provider 的回答一样。
				{
					method: "getAuthStatus",
					params: map[string]any{"includeToken": true, "refreshToken": false},
					want:   `{"authMethod":null,"authToken":null,"requiresOpenaiAuth":false}`,
				},
				{
					method: "model/list",
					params: map[string]any{},
					want: `{"data":[
					{"id":"local/default","model":"local/default","displayName":"default","description":"default","hidden":false,"isDefault":true,"supportedReasoningEfforts":[],"defaultReasoningEffort":"none"},
					{"id":"local/second","model":"local/second","displayName":"Second","description":"Second","hidden":false,"isDefault":false,
					 "supportedReasoningEfforts":[{"reasoningEffort":"low","description":""},{"reasoningEffort":"high","description":""}],"defaultReasoningEffort":"low"}
				],"nextCursor":null}`,
				},
				{
					method: "configRequirements/read",
					want:   `{"requirements":null}`,
				},
				{
					method: "config/read",
					params: map[string]any{"includeLayers": false},
					want:   `{"config":{"model":"local/default","model_provider":"local","model_providers":{"local":{"name":"local"}},"approval_policy":"never","sandbox_mode":"danger-full-access"},"origins":{}}`,
				},
				// codex --remote -C <目录> 先用 config/read 查这个目录的信任决定，没有时弹出信任确认并调 config/batchWrite。
				{
					method: "config/read",
					params: map[string]any{"includeLayers": true, "cwd": "/work/project"},
					want: `{"config":{"model":"local/default","model_provider":"local","model_providers":{"local":{"name":"local"}},"approval_policy":"never","sandbox_mode":"danger-full-access",
					"projects":{"/work/project":{"trust_level":"trusted"}}},"origins":{}}`,
				},
				// TUI 在 /model 里按回车、app 在 composer 里选模型时把它设为默认：回成功，codexHome 作为 filePath；
				// 选择只保存在本连接，后续 config/read 返回本连接的模型和强度。
				{
					method: "config/batchWrite",
					params: map[string]any{"edits": []map[string]any{
						{"keyPath": "model", "value": "local/second", "mergeStrategy": "upsert"},
						{"keyPath": "model_reasoning_effort", "value": "high", "mergeStrategy": "upsert"},
					}, "filePath": nil, "expectedVersion": nil, "reloadUserConfig": true},
					want: fmt.Sprintf(`{"status":"ok","version":"","filePath":%q,"overriddenMetadata":null}`, filepath.Dir(config)),
				},
				{
					method: "config/read",
					params: map[string]any{"includeLayers": false},
					want:   `{"config":{"model":"local/second","model_reasoning_effort":"high","model_provider":"local","model_providers":{"local":{"name":"local"}},"approval_policy":"never","sandbox_mode":"danger-full-access"},"origins":{}}`,
				},
				// Codex 只想让服务端重读配置时发空批次，同样回成功，记住的选择不变。
				{
					method: "config/batchWrite",
					params: map[string]any{"edits": []any{}, "reloadUserConfig": true},
					want:   fmt.Sprintf(`{"status":"ok","version":"","filePath":%q,"overriddenMetadata":null}`, filepath.Dir(config)),
				},
				// 值为 null 时清掉，config/read 回到 Agent 的默认模型、不带强度。
				{
					method: "config/batchWrite",
					params: map[string]any{"edits": []map[string]any{
						{"keyPath": "model", "value": nil, "mergeStrategy": "upsert"},
						{"keyPath": "model_reasoning_effort", "value": nil, "mergeStrategy": "upsert"},
					}},
					want: fmt.Sprintf(`{"status":"ok","version":"","filePath":%q,"overriddenMetadata":null}`, filepath.Dir(config)),
				},
				{
					method: "config/read",
					params: map[string]any{"includeLayers": false},
					want:   `{"config":{"model":"local/default","model_provider":"local","model_providers":{"local":{"name":"local"}},"approval_policy":"never","sandbox_mode":"danger-full-access"},"origins":{}}`,
				},
				{
					method: "collaborationMode/list",
					params: map[string]any{},
					want:   `{"data":[{"name":"Default","mode":"default"}]}`,
				},
				{
					method: "mcpServerStatus/list",
					params: map[string]any{"detail": "toolsAndAuthOnly"},
					want:   `{"data":[],"nextCursor":null}`,
				},
				// Codex 要求重载 MCP server 配置时，serve 重新加载整份配置，回空对象。
				{
					method: "config/mcpServer/reload",
					params: map[string]any{},
					want:   `{}`,
				},
				// 没配置 hook：每个目录一项、hooks 为空；没带目录时一项，目录是新建 Thread 默认用的目录（serve 的启动目录）。
				{
					method: "hooks/list",
					params: map[string]any{"cwds": []string{}},
					want:   fmt.Sprintf(`{"data":[{"cwd":%q,"hooks":[],"warnings":[],"errors":[]}]}`, filepath.Dir(config)),
				},
				{
					method: "hooks/list",
					params: map[string]any{"cwds": []string{"/work/a", "/work/b"}},
					want:   `{"data":[{"cwd":"/work/a","hooks":[],"warnings":[],"errors":[]},{"cwd":"/work/b","hooks":[],"warnings":[],"errors":[]}]}`,
				},
				{
					method: "experimentalFeature/list",
					params: map[string]any{"limit": 100},
					want:   `{"data":[],"nextCursor":null}`,
				},
				// 插件、App、权限配置、侧栏分组和可导入历史查询均返回空结果。
				{
					method: "plugin/list",
					params: map[string]any{"cwds": []string{}},
					want:   `{"marketplaces":[],"marketplaceLoadErrors":[],"featuredPluginIds":[]}`,
				},
				{
					method: "plugin/installed",
					params: map[string]any{"cwds": []string{}},
					want:   `{"marketplaces":[],"marketplaceLoadErrors":[]}`,
				},
				{
					method: "app/installed",
					params: map[string]any{},
					want:   `{"apps":[]}`,
				},
				{
					method: "permissionProfile/list",
					params: map[string]any{},
					want:   `{"data":[],"nextCursor":null}`,
				},
				{
					method: "threadSection/list",
					params: map[string]any{},
					want:   `{"data":[],"nextCursor":null}`,
				},
				{
					method: "externalAgentConfig/import/readHistories",
					want:   `{"data":[],"connectors":[]}`,
				},
			} {
				t.Run(query.method, func(t *testing.T) {
					var got, want any
					wire.call(t, query.method, query.params, &got)
					if err := json.Unmarshal([]byte(query.want), &want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s = %v, want %v", query.method, got, want)
					}
				})
			}

			// 3. skills/list 用 cwds 的第一个目录查草稿 Agent 的 Skills，检查返回这个目录和内置的 openvaala。
			type skillMetadata struct {
				Name    string `json:"name"`
				Scope   string `json:"scope"`
				Enabled bool   `json:"enabled"`
			}
			var skills struct {
				Data []struct {
					Cwd    string          `json:"cwd"`
					Errors []any           `json:"errors"`
					Skills []skillMetadata `json:"skills"`
				} `json:"data"`
			}
			workspace := t.TempDir()
			wire.call(t, "skills/list", map[string]any{"cwds": []string{workspace}, "forceReload": true}, &skills)
			isBuiltinSkill := func(skill skillMetadata) bool {
				return skill.Name == "openvaala" && skill.Scope == "user" && skill.Enabled
			}
			if len(skills.Data) != 1 || skills.Data[0].Cwd != workspace || skills.Data[0].Errors == nil || !slices.ContainsFunc(skills.Data[0].Skills, isBuiltinSkill) {
				t.Fatalf("skills/list = %+v, want one entry for %s with the builtin openvaala skill", skills, workspace)
			}

			// 4. 检查未实现方法返回 -32601 和 status 404，并记录一条 Warn。
			unimplemented := wire.response(t, wire.request(t, "foo/bar", map[string]any{}))
			assertCodexError(t, unimplemented, -32601, "method not found: foo/bar")
			if unimplemented.Error.Status != 404 {
				t.Fatalf("unimplemented method error status = %d, want 404", unimplemented.Error.Status)
			}
			records := slices.DeleteFunc(getE2ELogRecords(t, config, "codex request failed"), func(record map[string]any) bool { return record["method"] != "foo/bar" })
			if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["code"] != float64(-32601) {
				t.Fatalf("log records for an unimplemented method = %v, want one WARN record with method foo/bar and code -32601", records)
			}
			// 5. 改动里有模型以外的配置项（这里是 /experimental 写的功能开关）时整批拒绝；模型名不是 provider_id/model_id 时也拒绝。
			// 被拒的批次不改记住的选择，config/read 仍是 Agent 的默认模型。
			for _, rejected := range []struct {
				edits   []map[string]any
				message string
			}{
				{
					edits: []map[string]any{
						{"keyPath": "model", "value": "local/second", "mergeStrategy": "replace"},
						{"keyPath": "features.goals", "value": true, "mergeStrategy": "replace"},
					},
					message: "OpenVaala does not save Codex settings: features.goals",
				},
				{
					edits:   []map[string]any{{"keyPath": "model", "value": "gpt-6.1-sol", "mergeStrategy": "upsert"}},
					message: `model "gpt-6.1-sol" must be provider_id/model_id`,
				},
			} {
				response := wire.response(t, wire.request(t, "config/batchWrite", map[string]any{"edits": rejected.edits}))
				if response.Error == nil || response.Error.Code != -32602 || !strings.Contains(response.Error.Message, rejected.message) {
					t.Fatalf("config/batchWrite %v = %+v, want -32602 containing %q", rejected.edits, response.Error, rejected.message)
				}
			}
			var configRead struct {
				Config struct {
					Model string `json:"model"`
				} `json:"config"`
			}
			wire.call(t, "config/read", map[string]any{}, &configRead)
			if configRead.Config.Model != "local/default" {
				t.Fatalf("config/read after rejected writes = %+v, want the agent default local/default", configRead)
			}

			// 6. 非法 JSON 记 Warn 后丢弃，不回错误：紧接着的请求照常应答，之前没有多出 id 为 null 的错误响应。
			wire.writeRaw(t, []byte("{bad"))
			wire.call(t, "account/read", map[string]any{}, nil)
			if index := slices.IndexFunc(wire.pending, func(message codexWireMessage) bool { return string(message.ID) == "null" }); index >= 0 {
				t.Fatalf("invalid JSON got a response %+v, want it dropped", wire.pending[index])
			}

			// 7. 超过 16 MiB 的消息回 id 为 null 的 -32600，连接继续服务。
			wire.request(t, "model/list", map[string]any{"padding": strings.Repeat("x", 17<<20)})
			assertCodexError(t, wire.response(t, json.RawMessage("null")), -32600, "codex message exceeds the 16 MiB limit")
			var models struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			wire.call(t, "model/list", map[string]any{}, &models)
			if len(models.Data) != 2 || models.Data[0].ID != "local/default" {
				t.Fatalf("model/list after an oversized message = %+v, want the two configured models", models)
			}

			// 8. stdio 模式下客户端写完请求就关闭 stdin（printf ... | openvaala app-server）：输入结束前读到的请求照常应答，进程正常退出。
			if mode == "stdio" {
				piped := startAppServer(t, binary, config, mode)
				initializeID := piped.request(t, "initialize", codexClientInfo)
				modelListID := piped.request(t, "model/list", map[string]any{})
				piped.close(t)
				if response := piped.response(t, initializeID); response.Error != nil {
					t.Fatalf("initialize before stdin closed: %+v", response.Error)
				}
				response := piped.response(t, modelListID)
				if response.Error != nil || json.Unmarshal(response.Result, &models) != nil || len(models.Data) != 2 {
					t.Fatalf("model/list before stdin closed = %s (error %+v), want the two configured models", response.Result, response.Error)
				}
				piped.process.stop(t)
				return
			}
			// 9. Unix 模式检查 socket 的默认路径和 0600 权限、同一路径上的第二个 app-server、serve 停掉后的新连接，以及退出时删除 socket。
			wantPath := filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "app-server.sock")
			info, err := os.Stat(wire.process.socketPath)
			if err != nil {
				t.Fatal(err)
			}
			if wire.process.socketPath != wantPath || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
				t.Fatalf("socket %s mode %v, want a 0600 socket at the default path %s", wire.process.socketPath, info.Mode(), wantPath)
			}
			// 同一路径上的第二个 app-server 以非零状态退出，不删掉正在使用的 socket。
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancel()
			second := exec.CommandContext(ctx, binary, "--config-file", config, "app-server", "--listen", "unix://")
			second.Env = getE2EEnvironment(filepath.Dir(config))
			output, err := second.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || !strings.Contains(string(output), "app-server is already listening on "+wire.process.socketPath) {
				t.Fatalf("second app-server: %v\n%s\nwant a non-zero exit saying the socket is in use", err, output)
			}
			wire.call(t, "account/read", map[string]any{}, nil)
			// 每条连接开始时确认 serve：第一条连接关闭后停掉 serve，新连接重新拉起它，照常握手和查询。
			wire.close(t)
			stopServe(t, binary, config)
			next := connectCodexWire(t, wire.process)
			next.call(t, "initialize", codexClientInfo, nil)
			next.call(t, "model/list", map[string]any{}, &models)
			if len(models.Data) != 2 || models.Data[0].ID != "local/default" {
				t.Fatalf("model/list after serve stop = %+v, want the two configured models", models)
			}
			// 退出时删除 socket 文件。
			wire.process.stop(t)
			if _, err := os.Stat(wire.process.socketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("socket after exit: %v, want it removed", err)
			}
		})
	}
}

// TestCodexNoModelConfigured 检查未配置模型时的握手、查询和创建错误。
func TestCodexNoModelConfigured(t *testing.T) {
	// 1. 启动没有 Provider、模型和 Agent 文件的服务，完成握手。
	const wantMessage = "no model is configured; run openvaala to set up a model provider"
	binary := buildOpenVaala(t)
	config := writeE2EConfigWithoutModels(t)
	wire := startAppServer(t, binary, config, "stdio")
	wire.call(t, "initialize", codexClientInfo, nil)
	wire.notify(t, "initialized", nil)

	// 2. 查模型列表和各种 thread/start 都返回没有模型的错误，之后连接照常应答。
	for _, tt := range []struct {
		name   string
		method string
		params map[string]any
	}{
		// Codex 客户端启动时先查模型列表。
		{
			name:   "model_list",
			method: "model/list",
			params: map[string]any{},
		},
		{
			name:   "default_model",
			method: "thread/start",
			params: map[string]any{"cwd": t.TempDir()},
		},
		// 客户端指定的模型不在（空的）目录里时忽略它，同样得到没有模型的错误。
		{
			name:   "requested_model",
			method: "thread/start",
			params: map[string]any{"cwd": t.TempDir(), "model": "local/default"},
		},
		{
			name:   "ephemeral",
			method: "thread/start",
			params: map[string]any{"ephemeral": true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertCodexError(t, wire.response(t, wire.request(t, tt.method, tt.params)), -32603, wantMessage)
			wire.call(t, "account/read", map[string]any{}, nil)
		})
	}
}

// TestCodexListen 检查监听入口、Origin 拒绝和忽略参数日志。
func TestCodexListen(t *testing.T) {
	// 1. 编译程序，分别用回环 WebSocket 和 Unix socket 两种 --listen 地址运行。
	binary := buildOpenVaala(t)
	tests := []struct {
		name string
		// listen 是 --listen 的取值；scheme 是 stderr 里 codex --remote 之后的地址前缀。
		listen string
		scheme string
	}{
		{
			name:   "websocket_loopback",
			listen: "ws://127.0.0.1:0",
			scheme: "ws://",
		},
		{
			name:   "unix_socket",
			listen: "unix://",
			scheme: "unix://",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 启动 app-server，从 stderr 读出实际监听地址，按地址建 HTTP 客户端。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			appendE2EConfig(t, config, "telemetry:\n  log_level: debug\n")
			stopServeOnCleanup(t, binary, config)
			process, _ := startE2EProcess(t, binary, config, []string{"app-server", "--listen", tt.listen, "--analytics-default-enabled"}, false)
			address := process.waitRemoteAddress(t, tt.scheme)
			network, baseURL := "tcp", "http://"+address
			if tt.scheme == "unix://" {
				process.socketPath = address
				network, baseURL = "unix", "http://localhost"
			} else {
				process.webSocketAddress = address
				if !strings.HasPrefix(address, "127.0.0.1:") || address == "127.0.0.1:0" {
					t.Fatalf("codex --remote address = %q, want the loopback address with the port the system chose", address)
				}
			}
			client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, network, address)
			}}}
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancel()

			// 2. 检查 healthz、readyz 回 200，带 Origin 的请求回 403。
			for _, tc := range []struct {
				name       string
				path       string
				origin     string
				wantStatus int
			}{
				{
					name:       "healthz",
					path:       "/healthz",
					wantStatus: http.StatusOK,
				},
				{
					name:       "readyz",
					path:       "/readyz",
					wantStatus: http.StatusOK,
				},
				{
					name:       "healthz_from_a_web_page",
					path:       "/healthz",
					origin:     "http://example.com",
					wantStatus: http.StatusForbidden,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+tc.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if tc.origin != "" {
						request.Header.Set("Origin", tc.origin)
					}
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					if response.StatusCode != tc.wantStatus {
						t.Fatalf("GET %s (Origin %q) = %d, want %d", tc.path, tc.origin, response.StatusCode, tc.wantStatus)
					}
				})
			}

			// 3. 带 Origin 的升级请求回 403，不升级；路径不检查（Codex app 的 ws+unix:// 写法会带 /rpc）。
			for _, path := range []string{"/", "/rpc"} {
				_, response, err := websocket.Dial(ctx, "ws://localhost"+path, &websocket.DialOptions{
					HTTPClient: client,
					HTTPHeader: http.Header{"Origin": {"http://example.com"}},
				})
				if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
					t.Fatalf("websocket upgrade of %s with an Origin header = %v, %v; want 403", path, response, err)
				}
			}

			// 4. 拒绝之后连接照常服务，忽略的参数记一条日志。
			wire := connectCodexWire(t, process)
			var initialized struct {
				UserAgent string `json:"userAgent"`
			}
			wire.call(t, "initialize", codexClientInfo, &initialized)
			if !strings.HasPrefix(initialized.UserAgent, "openvaala/") {
				t.Fatalf("initialize userAgent = %q, want an openvaala user agent", initialized.UserAgent)
			}

			records := getE2ELogRecords(t, config, "ignored codex app-server flags")
			if len(records) != 1 || !reflect.DeepEqual(records[0]["flags"], []any{"--analytics-default-enabled"}) {
				t.Fatalf("ignored flags log records = %v, want one record listing --analytics-default-enabled", records)
			}
		})
	}
}

// TestCodexPathMappings 检查对话目录的客户端与服务端路径换算。
func TestCodexPathMappings(t *testing.T) {
	// 1. 配置 Unix 与 Windows 两组前缀，准备共用模型和连接。
	binary := buildOpenVaala(t)
	model := newE2EModel(t)
	config := writeE2EConfig(t, model.server.URL)
	clientRoot, serverRoot := "/openvaala-e2e-client/projects", t.TempDir()
	// 同一份配置里还有一条给 Windows 版 Codex app 的映射，指向 serverRoot 里面的 win：只有发过盘符路径的客户端才用它换算。
	windowsServerRoot := filepath.Join(serverRoot, "win")
	if err := os.Mkdir(windowsServerRoot, 0700); err != nil {
		t.Fatal(err)
	}
	appendE2EConfig(t, config, fmt.Sprintf("client:\n  path_mappings:\n    - client: %q\n      server: %q\n    - client: %q\n      server: %q\n",
		clientRoot, serverRoot, codexWindowsClientRoot, windowsServerRoot))
	fixture := &codexPathMappingFixture{
		binary:      binary,
		config:      config,
		model:       model,
		clientRoot:  clientRoot,
		serverRoot:  serverRoot,
		connections: newCodexConnections(t, binary, config, "stdio"),
	}
	tests := []struct {
		name string
		run  func(t *testing.T, f *codexPathMappingFixture)
	}{
		{
			name: "create_open_resume",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. 创建映射目录中的 Thread，检查服务端目录与客户端列表、读取和恢复结果。
				clientPath, serverPath := f.newProject(t, "create")
				wire := f.connections.open(t)
				turns := &codexTurnFixture{
					wire:  wire,
					model: f.model,
				}
				// startThread 要求回的 Thread cwd 和结果的 cwd 都是客户端路径。
				threadID := turns.startThread(t, clientPath, nil).Thread.ID
				if directory := getE2EConversationDirectory(t, f.config, threadID); directory != serverPath {
					t.Fatalf("conversation directory on the serve = %q, want the serve path %s", directory, serverPath)
				}
				turns.completeTextTurn(t, threadID)
				for _, tc := range []struct {
					name string
					cwd  string
					want []string
				}{
					{
						name: "mapped_project",
						cwd:  clientPath,
						want: []string{threadID},
					},
					{
						name: "other_mapped_project",
						cwd:  f.clientRoot + "/elsewhere",
						want: []string{},
					},
				} {
					var listed codexWireThreadList
					wire.call(t, "thread/list", map[string]any{"cwd": tc.cwd}, &listed)
					if ids := listed.getIDs(); !slices.Equal(ids, tc.want) {
						t.Fatalf("%s: thread/list by %s = %v, want %v", tc.name, tc.cwd, ids, tc.want)
					}
					for _, thread := range listed.Data {
						if thread.Cwd != clientPath {
							t.Fatalf("%s: listed thread cwd = %q, want the client path %s", tc.name, thread.Cwd, clientPath)
						}
					}
				}
				var read codexWireThreadRead
				wire.call(t, "thread/read", map[string]any{"threadId": threadID}, &read)
				if read.Thread.Cwd != clientPath {
					t.Fatalf("thread/read cwd = %q, want the client path %s", read.Thread.Cwd, clientPath)
				}
				// 另一个客户端恢复这个 Thread。
				var resumed codexWireThreadStart
				f.connections.open(t).call(t, "thread/resume", map[string]any{"threadId": threadID}, &resumed)
				if resumed.Thread.ID != threadID || resumed.Thread.Cwd != clientPath || resumed.Cwd != clientPath {
					t.Fatalf("thread/resume = %+v, want thread %s in the client path %s", resumed, threadID, clientPath)
				}
			},
		},
		{
			name: "other_paths_are_not_translated",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. 在映射目录调用 pwd 和 apply_patch，核对工具路径保持服务端写法。
				clientPath, serverPath := f.newProject(t, "tools")
				wire := f.connections.open(t)
				threadID := (&codexTurnFixture{
					wire:  wire,
					model: f.model,
				}).startThread(t, clientPath, nil).Thread.ID
				requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("run and patch")}})
				f.model.next(t).reply <- e2eModelReply{
					tool:      "bash",
					arguments: `{"command":"pwd"}`,
				}
				f.model.next(t).reply <- e2eModelReply{
					tool:      "apply_patch",
					arguments: `{"patch":"*** Begin Patch\n*** Add File: added.txt\n+hello\n*** End Patch"}`,
				}
				f.model.next(t).reply <- e2eModelReply{text: "done"}
				messages := wire.readTurn(t, threadID, requestID)
				assertCodexTurnCompleted(t, messages, "completed")
				command, _ := messages.getItem(t, "item/completed", "commandExecution")
				if command["cwd"] != serverPath || strings.TrimSpace(fmt.Sprint(command["aggregatedOutput"])) != serverPath {
					t.Fatalf("commandExecution = %v, want cwd and pwd output in the serve path %s", command, serverPath)
				}
				fileChange, _ := messages.getItem(t, "item/completed", "fileChange")
				changes, _ := fileChange["changes"].([]any)
				if len(changes) != 1 || changes[0].(map[string]any)["path"] != filepath.Join(serverPath, "added.txt") {
					t.Fatalf("fileChange = %v, want the added file under the serve path %s", fileChange, serverPath)
				}
			},
		},
		{
			name: "linux_client_ignores_windows_mapping",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. Linux 客户端（只发 / 开头的路径）在 Windows 映射覆盖的 serve 目录里建对话，检查列表和恢复回的 cwd 都按 / 开头的映射换算，没有 C:\。
				clientPath, serverPath := f.newProject(t, "win/linux")
				wire := f.connections.open(t)
				turns := &codexTurnFixture{
					wire:  wire,
					model: f.model,
				}
				threadID := turns.startThread(t, clientPath, nil).Thread.ID
				if directory := getE2EConversationDirectory(t, f.config, threadID); directory != serverPath {
					t.Fatalf("conversation directory on the serve = %q, want %s", directory, serverPath)
				}
				turns.completeTextTurn(t, threadID)
				var listed codexWireThreadList
				wire.call(t, "thread/list", map[string]any{"cwd": clientPath}, &listed)
				if ids := listed.getIDs(); !slices.Equal(ids, []string{threadID}) || listed.Data[0].Cwd != clientPath {
					t.Fatalf("thread/list by %s = %+v, want thread %s in the Linux client path", clientPath, listed.Data, threadID)
				}
				var resumed codexWireThreadStart
				f.connections.open(t).call(t, "thread/resume", map[string]any{"threadId": threadID}, &resumed)
				if resumed.Thread.Cwd != clientPath || resumed.Cwd != clientPath {
					t.Fatalf("thread/resume = %+v, want the Linux client path %s", resumed, clientPath)
				}
			},
		},
		{
			name: "windows_client_gets_drive_paths",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. 客户端先在 thread/start 里发盘符路径，检查对话落在映射到的 serve 目录，列表回的 cwd 用 Windows 写法。
				serverPath := filepath.Join(f.serverRoot, "win", "proj")
				if err := os.Mkdir(serverPath, 0700); err != nil {
					t.Fatal(err)
				}
				clientPath := codexWindowsClientRoot + `\proj`
				wire := f.connections.open(t)
				turns := &codexTurnFixture{
					wire:  wire,
					model: f.model,
				}
				threadID := turns.startThread(t, clientPath, nil).Thread.ID
				if directory := getE2EConversationDirectory(t, f.config, threadID); directory != serverPath {
					t.Fatalf("conversation directory on the serve = %q, want %s", directory, serverPath)
				}
				turns.completeTextTurn(t, threadID)
				var listed codexWireThreadList
				wire.call(t, "thread/list", map[string]any{"cwd": clientPath}, &listed)
				if ids := listed.getIDs(); !slices.Equal(ids, []string{threadID}) || listed.Data[0].Cwd != clientPath {
					t.Fatalf("thread/list by %s = %+v, want thread %s in the Windows client path", clientPath, listed.Data, threadID)
				}
			},
		},
		{
			name: "windows_client_resume_gets_drive_paths",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. 新连接第一个带路径的请求是带盘符 cwd 的 thread/resume：这个 cwd 不交给 serve，只用来认出 Windows 客户端，检查回的 cwd 是盘符路径。
				clientPath, _ := f.newProject(t, "win/resume")
				threadID := (&codexTurnFixture{
					wire:  f.connections.open(t),
					model: f.model,
				}).startThread(t, clientPath, nil).Thread.ID
				windowsPath := codexWindowsClientRoot + `\resume`
				var resumed codexWireThreadStart
				f.connections.open(t).call(t, "thread/resume", map[string]any{"threadId": threadID, "cwd": windowsPath}, &resumed)
				if resumed.Thread.ID != threadID || resumed.Thread.Cwd != windowsPath || resumed.Cwd != windowsPath {
					t.Fatalf("thread/resume = %+v, want thread %s in the Windows client path %s", resumed, threadID, windowsPath)
				}
			},
		},
		{
			name: "remote_serve",
			run: func(t *testing.T, f *codexPathMappingFixture) {
				// 1. 用另一个 HOME 启动远端 serve，app-server 用 --url 连它。
				remoteConfig := writeE2EConfig(t, f.model.server.URL)
				remoteWorkspace := t.TempDir()
				// writeE2EConfig 的 server 段在文件末尾，接着写的缩进行属于 server 段。
				appendE2EConfig(t, remoteConfig, fmt.Sprintf("  workspace_directory: %q\n", remoteWorkspace))
				remoteURL := startE2EServe(t, f.binary, remoteConfig)
				appServer, stdout := startE2EProcess(t, f.binary, f.config, []string{"app-server", "--url", remoteURL}, true)
				wire := newStdioWire(t, appServer, stdout)
				wire.call(t, "initialize", codexClientInfo, nil)

				// 2. 没命中映射的 cwd 不带给远端 serve，对话落在它的 workspace_directory。
				const unmappedPath = "/openvaala-e2e-unmapped/app"
				var started codexWireThreadStart
				wire.call(t, "thread/start", map[string]any{"cwd": unmappedPath}, &started)
				threadID := started.Thread.ID
				if started.Thread.Cwd != remoteWorkspace {
					t.Fatalf("thread/start cwd = %q, want the remote workspace directory %s", started.Thread.Cwd, remoteWorkspace)
				}
				if directory := getE2EConversationDirectory(t, remoteConfig, threadID); directory != remoteWorkspace {
					t.Fatalf("conversation directory on the remote serve = %q, want its workspace_directory %s", directory, remoteWorkspace)
				}
				(&codexTurnFixture{
					wire:  wire,
					model: f.model,
				}).completeTextTurn(t, threadID)
				// 3. thread/list 的 cwd 换不了，不按目录筛：对话的目录和请求的不同，照样列出。
				var listed codexWireThreadList
				wire.call(t, "thread/list", map[string]any{"cwd": unmappedPath}, &listed)
				if ids := listed.getIDs(); !slices.Equal(ids, []string{threadID}) {
					t.Fatalf("thread/list by an unmapped cwd = %v, want the unfiltered %v", ids, []string{threadID})
				}

				// 4. 检查日志记下没映射的目录，以及从 --url 连上的 serve 地址（已去掉 token）。
				notMapped := getE2ELogRecords(t, f.config, "workspace directory is not mapped; it is not passed to the runtime server")
				if !slices.ContainsFunc(notMapped, func(record map[string]any) bool { return record["directory"] == unmappedPath }) {
					t.Fatalf("not mapped log records = %v, want one for %s", notMapped, unmappedPath)
				}
				connected := getE2ELogRecords(t, f.config, "connected to runtime server")
				if !slices.ContainsFunc(connected, func(record map[string]any) bool {
					return record["openvaala.runtime_server.source"] == "--url" && record["openvaala.runtime_server.url"] == strings.Split(remoteURL, "?")[0]
				}) {
					t.Fatalf("connected log records = %v, want one from --url with the redacted %s", connected, remoteURL)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, fixture) })
	}
}

// codexWindowsClientRoot 是 TestCodexPathMappings 配置里 Windows 客户端一侧的映射前缀。
const codexWindowsClientRoot = `C:\fake`

// codexPathMappingFixture 是 TestCodexPathMappings 各用例共用的配置、假模型和连接：配置里一条 / 开头的映射，clientRoot 故意不存在，
// serverRoot 是 serve 上的临时目录；另一条 codexWindowsClientRoot 映射到 serverRoot/win。
type codexPathMappingFixture struct {
	binary, config         string
	model                  *e2eModel
	connections            *codexConnections
	clientRoot, serverRoot string
}

// newProject 创建项目目录并返回客户端和服务端路径。
func (f *codexPathMappingFixture) newProject(t *testing.T, name string) (clientPath, serverPath string) {
	t.Helper()
	// 1. 在服务端根目录下创建项目，再拼出客户端路径。
	serverPath = filepath.Join(f.serverRoot, name)
	if err := os.Mkdir(serverPath, 0700); err != nil {
		t.Fatal(err)
	}
	return f.clientRoot + "/" + name, serverPath
}

// getE2EConversationDirectory 读取服务端对话的 local 工作目录。
func getE2EConversationDirectory(t *testing.T, config, threadID string) string {
	t.Helper()
	// 1. 连接配置所在 HOME 的服务，持续读取事件并查询对话记录。
	client, stop := newE2ERuntimeClient(t, config)
	defer stop()
	go func() {
		for range client.Events() {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	record, err := client.GetConversation(ctx, defs.ConversationID(threadID))
	if err != nil {
		t.Fatal(err)
	}
	return record.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID]
}

// startE2EServe 启动服务并返回带 token 的连接地址。
func startE2EServe(t *testing.T, binary, config string) string {
	t.Helper()
	// 1. 运行 serve，轮询它写出的地址文件和 healthz；启动失败或超时则报告失败。
	process, _ := startE2EProcess(t, binary, config, []string{"serve"}, false)
	path := filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "runtime-server.yaml")
	deadline := time.Now().Add(e2eTimeout)
	for {
		var server struct {
			URL   string `yaml:"url"`
			Token string `yaml:"token"`
		}
		if payload, err := os.ReadFile(path); err == nil && yaml.Unmarshal(payload, &server) == nil && server.URL != "" {
			healthz, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			healthz.Scheme, healthz.Path = "http", "/api/v1/healthz"
			if response, err := http.Get(healthz.String()); err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return server.URL + "?token=" + url.QueryEscape(server.Token)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not start\n%s", process.stderr)
		}
		select {
		case <-process.exited:
			t.Fatalf("serve exited before serving: %v\n%s", process.exitErr, process.stderr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestCodexCommand 检查 codex 命令的参数转发、信号与退出结果。
func TestCodexCommand(t *testing.T) {
	// 1. 编译程序；各用例写一个记录 PID 和参数的假 codex，运行 openvaala codex 后按 stop 让它结束。
	binary := buildOpenVaala(t)
	tests := []struct {
		name string
		// codexScript 是假 codex 写完参数之后执行的 shell 命令；为空时 PATH 里没有 codex。
		codexScript string
		// stop 是假 codex 运行起来之后测试怎样让它结束：stdin 是关闭 stdin（假 codex 读到结束后退出），sigterm、sighup 是给 openvaala
		// 发这个信号（sighup 相当于关掉终端窗口）；为空时假 codex 自己退出。
		stop string
		// wantStderr 非空时期望 openvaala 以非零状态退出，stderr 含这段文字；为空时期望正常退出。
		wantStderr string
		// isResumeHintShown 期望 codex 退出后 stderr 里有改用 openvaala codex 重连的提示吗？只有 codex 自己正常退出时才有。
		isResumeHintShown bool
		// codexOptions 是写在 -- 之前的 codex 根命令参数。
		codexOptions []string
		// wantForwarded 是假 codex 应当在 -C <目录> 之后、resume --last 之前收到的参数。
		wantForwarded []string
	}{
		{
			name:              "connects",
			codexScript:       "read -r line; exit 0",
			stop:              "stdin",
			isResumeHintShown: true,
		},
		{
			// -c/--config、--enable、--disable、--strict-config 由 openvaala 转给 codex：--config 写成 -c，其余照原样转发。
			name:              "forwards",
			codexScript:       "read -r line; exit 0",
			stop:              "stdin",
			isResumeHintShown: true,
			codexOptions:      []string{"--config", "k=v", "--enable", "x", "--disable", "y", "--strict-config", "-c", "a.b=c"},
			wantForwarded:     []string{"-c", "k=v", "-c", "a.b=c", "--enable", "x", "--disable", "y", "--strict-config"},
		},
		{
			name:        "sigterm",
			codexScript: "read -r line; exit 0",
			stop:        "sigterm",
		},
		{
			name:        "sighup",
			codexScript: "read -r line; exit 0",
			stop:        "sighup",
		},
		{
			name:        "fails",
			codexScript: "exit 3",
			wantStderr:  "codex: exit status 3",
		},
		{
			name:       "missing",
			wantStderr: "cannot run codex (install the Codex CLI first)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			home := filepath.Dir(config)
			codexDirectory := t.TempDir()
			path := codexDirectory
			if tt.codexScript != "" {
				stopServeOnCleanup(t, binary, config)
				script := "#!/bin/sh\necho $$ > \"$HOME/codex-pid\"\nprintf '%s\\n' \"$@\" > \"$HOME/codex-arguments.partial\"\n" +
					"mv \"$HOME/codex-arguments.partial\" \"$HOME/codex-arguments\"\n" + tt.codexScript + "\n"
				if err := os.WriteFile(filepath.Join(codexDirectory, "codex"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				path += string(os.PathListSeparator) + os.Getenv("PATH")
			}
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancel()
			command := exec.CommandContext(ctx, binary, slices.Concat([]string{"--config-file", config, "codex"}, tt.codexOptions, []string{"--", "resume", "--last"})...)
			command.Env = append(getE2EEnvironment(home), "PATH="+path)
			command.Dir = home
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr := &e2eOutput{}
			command.Stderr = stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			socketPath := filepath.Join(home, ".local", "state", "openvaala", fmt.Sprintf("codex-%d.sock", command.Process.Pid))

			if tt.stop != "" {
				// 假 codex 写出参数时 socket 已经在监听。
				argumentsPath := filepath.Join(home, "codex-arguments")
				for {
					if _, err := os.Stat(argumentsPath); err == nil {
						break
					}
					select {
					case err := <-exited:
						t.Fatalf("openvaala codex exited before starting codex: %v\n%s", err, stderr)
					case <-time.After(50 * time.Millisecond):
					}
				}
				payload, err := os.ReadFile(argumentsPath)
				if err != nil {
					t.Fatal(err)
				}
				// -C 是 openvaala codex 的当前目录，测试把它设成 HOME。
				directory, err := filepath.EvalSymlinks(home)
				if err != nil {
					t.Fatal(err)
				}
				wantArguments := slices.Concat([]string{"--remote", "unix://" + socketPath, "-C", directory}, tt.wantForwarded, []string{"resume", "--last"})
				if got := strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n"); !slices.Equal(got, wantArguments) {
					t.Fatalf("codex arguments = %q, want %q", got, wantArguments)
				}
				switch tt.stop {
				case "stdin":
					// 连上的是本进程的 AppServer，它连着本机 serve：握手和查询模型照常。
					wire := connectCodexWire(t, &e2eProcess{
						socketPath: socketPath,
						stderr:     stderr,
					})
					wire.call(t, "initialize", codexClientInfo, nil)
					var models struct {
						Data []struct {
							ID string `json:"id"`
						} `json:"data"`
					}
					wire.call(t, "model/list", map[string]any{}, &models)
					if len(models.Data) != 2 || models.Data[0].ID != "local/default" {
						t.Fatalf("model/list through openvaala codex = %+v, want the two configured models", models)
					}
					wire.close(t)
					if err := stdin.Close(); err != nil {
						t.Fatal(err)
					}
				case "sigterm":
					if err := command.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				case "sighup":
					if err := command.Process.Signal(syscall.SIGHUP); err != nil {
						t.Fatal(err)
					}
				}
			}

			var exitErr error
			select {
			case exitErr = <-exited:
			case <-ctx.Done():
				t.Fatalf("openvaala codex did not exit\n%s", stderr)
			}
			if tt.wantStderr == "" && exitErr != nil {
				t.Fatalf("openvaala codex exited with %v\n%s", exitErr, stderr)
			}
			if tt.wantStderr != "" && (exitErr == nil || !strings.Contains(stderr.String(), tt.wantStderr)) {
				t.Fatalf("openvaala codex exited with %v\n%s\nwant a non-zero exit saying %q", exitErr, stderr, tt.wantStderr)
			}
			if isShown := strings.Contains(stderr.String(), "To reconnect, run:\n  openvaala codex -- resume <session id>\n"); isShown != tt.isResumeHintShown {
				t.Fatalf("resume hint shown = %v, want %v\n%s", isShown, tt.isResumeHintShown, stderr)
			}
			// 退出时删除 socket 文件。
			if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("socket after exit: %v, want it removed", err)
			}
			// openvaala 等 codex 结束才退出，不留下还在运行的 codex。
			if tt.stop != "" {
				payload, err := os.ReadFile(filepath.Join(home, "codex-pid"))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("codex (pid %d) after openvaala exited: %v, want it gone", pid, err)
				}
			}
		})
	}
}

// TestCodexTurn 检查一轮输入、模型输出、工具调用和中断。
func TestCodexTurn(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 启动 app-server，初始化一条各用例共用的连接。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			wire := startAppServer(t, binary, config, mode)
			wire.call(t, "initialize", codexClientInfo, nil)
			fixture := &codexTurnFixture{
				wire:  wire,
				model: model,
			}
			// 2. 各用例在同一条连接上依次新建自己的 Thread，按 Thread id 读自己的通知。
			for _, test := range []struct {
				name string
				run  func(t *testing.T, f *codexTurnFixture)
			}{
				{
					name: "text_reply",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 提交带客户端 ID 的文本，检查响应、增量、推理与回复完成顺序。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "clientUserMessageId": "client-message-1", "input": []any{codexTextInput("hello")},
						})
						f.model.next(t).reply <- e2eModelReply{
							text:      "hello| world",
							reasoning: "thinking",
						}
						messages := f.wire.readTurn(t, threadID, requestID)
						// 响应先于 turn/started，带同一个 TurnID。
						responseIndex := messages.indexOf(isCodexResponse(requestID))
						startedIndex := messages.indexOf(isCodexMethod("turn/started"))
						var response struct {
							Turn codexWireTurn `json:"turn"`
						}
						if responseIndex < 0 || startedIndex < responseIndex || json.Unmarshal(messages[responseIndex].Result, &response) != nil ||
							response.Turn.Status != "inProgress" || response.Turn.ID != messages[startedIndex].getNotification(t).Turn.ID {
							t.Fatalf("turn/start response at %d (%+v), turn/started at %d, want the inProgress response first with the same turn", responseIndex, response, startedIndex)
						}
						userMessage, _ := messages.getItem(t, "item/completed", "userMessage")
						if userMessage["clientId"] != "client-message-1" || fmt.Sprint(userMessage["content"]) != "[map[text:hello text_elements:[] type:text]]" {
							t.Fatalf("userMessage = %v, want the input with the originating clientId", userMessage)
						}
						started, startedAt := messages.getItem(t, "item/started", "agentMessage")
						completed, completedAt := messages.getItem(t, "item/completed", "agentMessage")
						firstDelta := messages.indexOf(isCodexMethod("item/agentMessage/delta"))
						// 这一轮停在这条回复上，它是最终回答，只完成一次。
						completions := messages.count(func(message codexWireMessage) bool {
							return message.Method == "item/completed" && message.getNotification(t).Item["id"] == completed["id"]
						})
						if started["text"] != "" || !(startedAt < firstDelta && firstDelta < completedAt) || completed["text"] != "hello world" || completed["phase"] != "final_answer" || completions != 1 {
							t.Fatalf("agentMessage started %v at %d, first delta at %d, completed %v at %d (%d times), want started, deltas, completed once as final_answer", started, startedAt, firstDelta, completed, completedAt, completions)
						}
						if text := messages.getDeltaText(t, "item/agentMessage/delta", completed["id"].(string)); text != "hello world" {
							t.Fatalf("agentMessage deltas = %q, want them to concatenate to the final text", text)
						}
						reasoning, _ := messages.getItem(t, "item/completed", "reasoning")
						if summary := messages.getDeltaText(t, "item/reasoning/summaryTextDelta", reasoning["id"].(string)); summary != "thinking" || fmt.Sprint(reasoning["summary"]) != "[thinking]" {
							t.Fatalf("reasoning %v with summary deltas %q, want thinking", reasoning, summary)
						}
						assertCodexTurnCompleted(t, messages, "completed")
					},
				},
				{
					name: "mcp_server_status_of_loaded_thread",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 创建 Thread 后查 mcpServerStatus/list：本连接跟随着的 Thread 经 serve 的 mcpServer/list 回答，没有配置 MCP server 时是空页，不是错误。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						var page json.RawMessage
						f.wire.call(t, "mcpServerStatus/list", map[string]any{"threadId": threadID}, &page)
						if string(page) != `{"data":[],"nextCursor":null}` {
							t.Fatalf("mcpServerStatus/list = %s, want an empty page", page)
						}
					},
				},
				{
					name: "bash_output_and_usage",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 执行分段输出的 bash，检查命令内容、回复阶段、输出增量和 token 用量。
						workspace := newE2EWorkspace(t)
						threadID := f.startThread(t, workspace, nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("run it")}})
						f.model.next(t).reply <- e2eModelReply{
							text:      "running it",
							tool:      "bash",
							arguments: `{"command":"printf FIRST; sleep 0.05; printf SECOND"}`,
							usage:     true,
						}
						f.model.next(t).reply <- e2eModelReply{text: "done"}
						messages := f.wire.readTurn(t, threadID, requestID)
						command, completedAt := messages.getItem(t, "item/completed", "commandExecution")
						if command["exitCode"] != 0.0 || command["aggregatedOutput"] != "FIRSTSECOND" || command["status"] != "completed" || command["cwd"] != workspace ||
							command["command"] != "bash -lc 'printf FIRST; sleep 0.05; printf SECOND'" {
							t.Fatalf("commandExecution = %v, want bash -lc of the command, exit code 0 with output FIRSTSECOND in %s", command, workspace)
						}
						_, startedAt := messages.getItem(t, "item/started", "commandExecution")
						// 同一次模型输出里先回复再调用工具：回复的 item/completed 先于命令的 item/started（和 Codex 服务端一样），之后不再重复；
						// 后面还有工具调用，回复是回合中途的话（commentary）。
						reply, replyCompletedAt := messages.getItem(t, "item/completed", "agentMessage")
						if reply["text"] != "running it" || reply["phase"] != "commentary" || replyCompletedAt > startedAt || messages.count(func(message codexWireMessage) bool {
							return message.Method == "item/completed" && message.getNotification(t).Item["id"] == reply["id"]
						}) != 1 {
							t.Fatalf("reply %v completed at %d, command started at %d, want the commentary reply completed once before the command starts", reply, replyCompletedAt, startedAt)
						}
						firstOutput := messages.indexOf(isCodexMethod("item/commandExecution/outputDelta"))
						output := messages.getDeltaText(t, "item/commandExecution/outputDelta", command["id"].(string))
						if !(startedAt < firstOutput && firstOutput < completedAt) || output != "FIRSTSECOND" {
							t.Fatalf("output deltas %q from %d, command started at %d and completed at %d, want the streamed output inside the item", output, firstOutput, startedAt, completedAt)
						}
						usageAt := messages.indexOf(func(message codexWireMessage) bool {
							return message.Method == "thread/tokenUsage/updated" && message.getNotification(t).TokenUsage.Total.TotalTokens > 0
						})
						if usageAt < 0 || usageAt > messages.indexOf(isCodexMethod("turn/completed")) {
							t.Fatalf("thread/tokenUsage/updated with total tokens at %d, want it before turn/completed", usageAt)
						}
						assertCodexTurnCompleted(t, messages, "completed")
					},
				},
				{
					name: "reply_before_hidden_tool",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 不显示的工具（list_agents）前的回复也是回合中途的话：暂停模型输出，检查参数收齐、这次输出还没结束时回复就带 commentary 完成，
						// 之后只有回复的那次输出是最终回答。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("who is there")}})
						finish := make(chan struct{})
						f.model.next(t).reply <- e2eModelReply{
							text:      "checking",
							tool:      "list_agents",
							arguments: `{}`,
							finish:    finish,
						}
						commentary := f.wire.next(t, "reply completed before list_agents", func(message codexWireMessage) bool {
							return message.Method == "item/completed" && message.getThreadID() == threadID && message.getNotification(t).Item["type"] == "agentMessage"
						}).getNotification(t).Item
						if commentary["text"] != "checking" || commentary["phase"] != "commentary" {
							t.Fatalf("reply completed while the model output is open = %v, want checking as commentary", commentary)
						}
						close(finish)
						f.model.next(t).reply <- e2eModelReply{text: "nobody"}
						finalAnswer, _ := f.wire.readTurn(t, threadID, requestID).getItem(t, "item/completed", "agentMessage")
						if finalAnswer["text"] != "nobody" || finalAnswer["phase"] != "final_answer" {
							t.Fatalf("last reply = %v, want nobody as final_answer", finalAnswer)
						}
					},
				},
				{
					name: "apply_patch_diff",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 准备文件并执行新增与更新补丁，检查开始和完成通知中的改动与行号。
						workspace := newE2EWorkspace(t)
						if err := os.WriteFile(filepath.Join(workspace, "lines.txt"), []byte("a\nb\nc\n"), 0600); err != nil {
							t.Fatal(err)
						}
						threadID := f.startThread(t, workspace, nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("patch it")}})
						f.model.next(t).reply <- e2eModelReply{
							text:      "patching",
							tool:      "apply_patch",
							arguments: `{"patch":"*** Begin Patch\n*** Add File: added.txt\n+hello\n*** Update File: lines.txt\n@@\n-b\n+B\n*** End Patch"}`,
						}
						f.model.next(t).reply <- e2eModelReply{text: "patched"}
						messages := f.wire.readTurn(t, threadID, requestID)
						fileChange, _ := messages.getItem(t, "item/completed", "fileChange")
						// 照 Codex 的约定：新增是文件内容，更新是带原文件行号的 unified diff（行号来自 apply_patch 结果里的 change_start_lines）。
						want := []any{
							map[string]any{"path": filepath.Join(workspace, "added.txt"), "kind": map[string]any{"type": "add"}, "diff": "hello\n"},
							map[string]any{"path": filepath.Join(workspace, "lines.txt"), "kind": map[string]any{"type": "update"}, "diff": "@@ -2,1 +2,1 @@\n-b\n+B\n"},
						}
						if fileChange["status"] != "completed" || !reflect.DeepEqual(fileChange["changes"], want) {
							t.Fatalf("fileChange = %v, want completed changes %v", fileChange, want)
						}
						// Codex TUI 按 item/started 里的 changes 画出改动，开始时就要带行号；同一次模型输出里的回复先完成。
						started, startedAt := messages.getItem(t, "item/started", "fileChange")
						_, replyCompletedAt := messages.getItem(t, "item/completed", "agentMessage")
						if !reflect.DeepEqual(started["changes"], want) || replyCompletedAt > startedAt {
							t.Fatalf("started fileChange %v at %d, reply completed at %d, want changes %v after the reply", started, startedAt, replyCompletedAt, want)
						}
						assertCodexTurnCompleted(t, messages, "completed")
					},
				},
				{
					name: "read_file_server",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 读取工作目录文件，检查下一次模型请求含文件内容及工具完成通知。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("read it")}})
						f.model.next(t).reply <- e2eModelReply{
							tool:      "read_file",
							arguments: `{"path":"notes.txt"}`,
						}
						second := f.model.next(t)
						if !strings.Contains(fmt.Sprint(second.body), "WORKSPACE_CONTENT") {
							t.Fatalf("model request after read_file = %v, want the file content", second.body)
						}
						second.reply <- e2eModelReply{text: "read"}
						messages := f.wire.readTurn(t, threadID, requestID)
						toolCall, _ := messages.getItem(t, "item/completed", "mcpToolCall")
						if toolCall["server"] != "local" || toolCall["tool"] != "read_file" || toolCall["status"] != "completed" {
							t.Fatalf("mcpToolCall = %v, want read_file completed on the local executor", toolCall)
						}
						assertCodexTurnCompleted(t, messages, "completed")
					},
				},
				{
					name: "interrupt",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 开始暂停的 Turn，发送中断并核对空响应与 interrupted 终态。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						turnID := f.startHeldTurn(t, threadID)
						var result map[string]any
						f.wire.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, &result)
						if len(result) != 0 {
							t.Fatalf("turn/interrupt = %v, want {}", result)
						}
						completed := f.wire.nextThreadNotification(t, threadID, "turn/completed").getNotification(t).Turn
						if completed.ID != turnID || completed.Status != "interrupted" {
							t.Fatalf("turn/completed = %+v, want turn %s interrupted", completed, turnID)
						}
					},
				},
				{
					name: "steer",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 开始流式 Turn，检查错误目标被拒、正确插话被读入且携带客户端 ID。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("first")}})
						stream := make(chan string)
						f.model.next(t).reply <- e2eModelReply{
							text:   "working",
							stream: stream,
						}
						var started struct {
							Turn codexWireTurn `json:"turn"`
						}
						response := f.wire.response(t, requestID)
						if response.Error != nil || json.Unmarshal(response.Result, &started) != nil {
							t.Fatalf("turn/start = %s (error %+v)", response.Result, response.Error)
						}
						assertCodexError(t, f.wire.response(t, f.wire.request(t, "turn/steer", map[string]any{
							"threadId": threadID, "expectedTurnId": "wrong-turn", "input": []any{codexTextInput("ignored")},
						})), -32600, "expected turn wrong-turn is not active")
						var steered struct {
							TurnID string `json:"turnId"`
						}
						f.wire.call(t, "turn/steer", map[string]any{
							"threadId": threadID, "expectedTurnId": started.Turn.ID, "clientUserMessageId": "steer-1", "input": []any{codexTextInput("steer text")},
						}, &steered)
						if steered.TurnID != started.Turn.ID {
							t.Fatalf("turn/steer = %+v, want turn %s", steered, started.Turn.ID)
						}
						close(stream)
						second := f.model.next(t)
						if !strings.Contains(fmt.Sprint(second.body), "steer text") {
							t.Fatalf("model request after steer = %v, want the steer text", second.body)
						}
						second.reply <- e2eModelReply{text: "steered"}
						messages := f.wire.readTurn(t, threadID, nil)
						steerMessage := messages.getItemWhere(t, "item/completed", func(item map[string]any) bool {
							return item["type"] == "userMessage" && strings.Contains(fmt.Sprint(item["content"]), "steer text")
						})
						if steerMessage["clientId"] != "steer-1" {
							t.Fatalf("steer userMessage = %v, want clientId steer-1", steerMessage)
						}
						assertCodexTurnCompleted(t, messages, "completed")
					},
				},
				{
					name: "busy",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 暂停一轮并再次开轮，检查忙碌错误后中断原轮。
						threadID := f.startThread(t, newE2EWorkspace(t), nil).Thread.ID
						turnID := f.startHeldTurn(t, threadID)
						assertCodexError(t, f.wire.response(t, f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "input": []any{codexTextInput("again")},
						})), -32600, "session is busy")
						f.wire.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
						f.wire.nextThreadNotification(t, threadID, "turn/completed")
					},
				},
				{
					name: "settings",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 检查动态工具拒绝、未知模型回落和本轮模型与强度选择。
						assertCodexError(t, f.wire.response(t, f.wire.request(t, "thread/start", map[string]any{
							"dynamicTools": []any{map[string]any{"name": "lookup", "description": "Look up", "inputSchema": map[string]any{"type": "object"}}},
						})), -32602, "dynamicTools are not supported")
						// Codex TUI 带的是自己配置里的模型名：不在目录里时用 Agent 默认模型。
						started := f.startThread(t, newE2EWorkspace(t), map[string]any{"model": "gpt-6.1-sol"})
						if started.Model != "local/default" || started.ModelProvider != "local" || started.Thread.Model != "local/default" {
							t.Fatalf("thread/start with an unknown model = %+v, want the agent default local/default", started)
						}
						threadID := started.Thread.ID
						response := f.wire.response(t, f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "model": "local/missing", "input": []any{codexTextInput("hi")},
						}))
						if response.Error == nil || response.Error.Code != -32602 {
							t.Fatalf("turn/start with an unknown model = %+v, want -32602", response.Error)
						}
						requestID := f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "model": "local/second", "effort": "high", "input": []any{codexTextInput("switch")},
						})
						request := f.model.next(t)
						if request.body["model"] != "second" || request.body["reasoning_effort"] != "high" {
							t.Fatalf("model request = %v, want local/second with high effort", request.body)
						}
						request.reply <- e2eModelReply{text: "switched"}
						assertCodexTurnCompleted(t, f.wire.readTurn(t, threadID, requestID), "completed")
						// Codex app 每次发消息都带 collaborationMode，model、effort 是 null；带了它就用它的模型和强度，不看 model、effort。
						requestID = f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "model": "local/missing", "effort": "low", "input": []any{codexTextInput("back")},
							"collaborationMode": map[string]any{"mode": "default", "settings": map[string]any{"model": "local/default", "reasoning_effort": nil, "developer_instructions": nil}},
						})
						request = f.model.next(t)
						if _, hasEffort := request.body["reasoning_effort"]; request.body["model"] != "default" || hasEffort {
							t.Fatalf("model request = %v, want local/default without effort", request.body)
						}
						request.reply <- e2eModelReply{text: "back"}
						assertCodexTurnCompleted(t, f.wire.readTurn(t, threadID, requestID), "completed")
						// Codex app 新建对话时把 composer 选的强度放在 config.model_reasoning_effort。
						started = f.startThread(t, newE2EWorkspace(t), map[string]any{"model": "local/second", "config": map[string]any{"model_reasoning_effort": "high"}})
						if started.Model != "local/second" || started.ReasoningEffort != "high" {
							t.Fatalf("thread/start with local/second and high effort = %+v, want them on the new thread", started)
						}
					},
				},
				{
					name: "inputs",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 检查不支持的音频被拒，再提交远程和本地图片并核对模型请求。
						workspace := newE2EWorkspace(t)
						threadID := f.startThread(t, workspace, nil).Thread.ID
						assertCodexError(t, f.wire.response(t, f.wire.request(t, "turn/start", map[string]any{
							"threadId": threadID, "input": []any{map[string]any{"type": "audio", "url": "https://example.com/a.wav"}},
						})), -32602, "unsupported input: audio")
						imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(newE2EPNG(t, color.White))
						localImage := filepath.Join(workspace, "local.png")
						if err := os.WriteFile(localImage, newE2EPNG(t, color.Black), 0600); err != nil {
							t.Fatal(err)
						}
						localImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(newE2EPNG(t, color.Black))
						requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{
							codexTextInput("look"),
							map[string]any{"type": "image", "url": imageURL},
							map[string]any{"type": "localImage", "path": localImage},
						}})
						request := f.model.next(t)
						if body := fmt.Sprint(request.body); !strings.Contains(body, imageURL) || !strings.Contains(body, localImageURL) {
							t.Fatalf("model request = %v, want both image data URLs", request.body)
						}
						request.reply <- e2eModelReply{text: "seen"}
						assertCodexTurnCompleted(t, f.wire.readTurn(t, threadID, requestID), "completed")
					},
				},
				{
					name: "skills_from_loaded_thread",
					run: func(t *testing.T, f *codexTurnFixture) {
						// 1. 建立项目 Skill 并加载 Thread，检查 Skills 来自已加载对话而不是请求目录。
						workspace := newE2EWorkspace(t)
						skillPath := filepath.Join(workspace, ".agents", "skills", "thread-skill", "SKILL.md")
						if err := os.MkdirAll(filepath.Dir(skillPath), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(skillPath, []byte("---\nname: thread-skill\ndescription: Skill of the thread workspace\n---\nbody\n"), 0600); err != nil {
							t.Fatal(err)
						}
						f.startThread(t, workspace, nil)
						// 加载着根 Thread 时用它的对话查 Skills，cwds 里的目录不影响结果。
						var skills struct {
							Data []struct {
								Skills []struct {
									Name string `json:"name"`
								} `json:"skills"`
							} `json:"data"`
						}
						f.wire.call(t, "skills/list", map[string]any{"cwds": []string{t.TempDir()}}, &skills)
						if len(skills.Data) != 1 || !strings.Contains(fmt.Sprint(skills.Data[0].Skills), "thread-skill") {
							t.Fatalf("skills/list = %+v, want the project skill of the loaded thread", skills)
						}
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, fixture) })
			}
		})
	}
}

// codexTurnFixture 是 TestCodexTurn 各用例共用的一条 Codex 连接和假模型。
type codexTurnFixture struct {
	wire  *codexWire
	model *e2eModel
}

// codexWireThreadStart 是 thread/start 结果里测试关心的字段。
type codexWireThreadStart struct {
	Thread struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Model     string `json:"model"`
		Source    string `json:"source"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
	} `json:"thread"`
	Model           string `json:"model"`
	ModelProvider   string `json:"modelProvider"`
	ReasoningEffort string `json:"reasoningEffort"`
	Cwd             string `json:"cwd"`
	ApprovalPolicy  string `json:"approvalPolicy"`
	Sandbox         struct {
		Type string `json:"type"`
	} `json:"sandbox"`
}

// startThread 创建 Thread 并检查响应和启动通知。
func (f *codexTurnFixture) startThread(t *testing.T, workspace string, params map[string]any) codexWireThreadStart {
	t.Helper()
	// 1. 合并工作目录与额外参数，发出 thread/start。
	request := map[string]any{"cwd": workspace}
	for key, value := range params {
		request[key] = value
	}
	requestID := f.wire.request(t, "thread/start", request)
	// 2. 检查响应先于通知，Thread 空闲且目录匹配，不请求命令审批。
	first := f.wire.next(t, "thread/start response", func(message codexWireMessage) bool {
		return isCodexResponse(requestID)(message) || message.Method == "thread/started"
	})
	var result codexWireThreadStart
	if first.Method != "" || first.Error != nil || json.Unmarshal(first.Result, &result) != nil {
		t.Fatalf("first message after thread/start = %+v (error %+v), want the response before thread/started", first, first.Error)
	}
	thread := result.Thread
	if thread.ID == "" || thread.SessionID != thread.ID || thread.Status.Type != "idle" || thread.Source != "appServer" || thread.Cwd != workspace ||
		result.Cwd != workspace || result.ApprovalPolicy != "never" || result.Sandbox.Type != "dangerFullAccess" {
		t.Fatalf("thread/start = %+v, want an idle appServer thread in %s that never asks for approval", result, workspace)
	}
	// 3. 核对启动通知指向刚创建的 Thread。
	if started := f.wire.nextNotification(t, "thread/started").getNotification(t); started.Thread.ID != thread.ID {
		t.Fatalf("thread/started for %s, want %s", started.Thread.ID, thread.ID)
	}
	return result
}

// startHeldTurn 开始一轮并暂停模型输出，返回 TurnID。
func (f *codexTurnFixture) startHeldTurn(t *testing.T, threadID string) string {
	t.Helper()
	// 1. 提交 hold 输入，让模型写出 partial 后等待取消。
	requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("hold")}})
	f.model.next(t).reply <- e2eModelReply{
		text: "partial",
		hold: true,
	}
	var started struct {
		Turn codexWireTurn `json:"turn"`
	}
	response := f.wire.response(t, requestID)
	if response.Error != nil || json.Unmarshal(response.Result, &started) != nil {
		t.Fatalf("turn/start = %s (error %+v)", response.Result, response.Error)
	}
	f.wire.nextThreadNotification(t, threadID, "item/agentMessage/delta")
	return started.Turn.ID
}

// completeTextTurn 在 Thread 中完成一轮纯文本输入。
func (f *codexTurnFixture) completeTextTurn(t *testing.T, threadID string) {
	t.Helper()
	// 1. 提交 save，回复 saved，再读取并检查本轮完成消息。
	requestID := f.wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("save")}})
	f.model.next(t).reply <- e2eModelReply{text: "saved"}
	assertCodexTurnCompleted(t, f.wire.readTurn(t, threadID, requestID), "completed")
}

// newE2EWorkspace 创建测试工作目录。
func newE2EWorkspace(t *testing.T) string {
	t.Helper()
	// 1. 在临时目录的 notes.txt 写入 WORKSPACE_CONTENT 并返回目录。
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("WORKSPACE_CONTENT"), 0600); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// openE2EFIFOWriter 打开 FIFO 写端并登记关闭。
func openE2EFIFOWriter(t *testing.T, path string) *os.File {
	t.Helper()
	// 1. 在后台等待读端打开；成功时返回写端，超时或打开失败时报告失败。
	type openResult struct {
		writer *os.File
		err    error
	}
	opened := make(chan openResult, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		opened <- openResult{
			writer,
			err,
		}
	}()
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		t.Cleanup(func() { _ = result.writer.Close() })
		return result.writer
	case <-time.After(e2eTimeout):
		t.Fatalf("nobody opened %s for reading", path)
		return nil
	}
}

// newE2EPNG 生成指定颜色的测试 PNG。
func newE2EPNG(t *testing.T, fill color.Color) []byte {
	t.Helper()
	// 1. 填充一像素画面，编码为 PNG 并返回字节。
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, fill)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// codexTextInput 构造 Codex 文本输入。
func codexTextInput(text string) map[string]any {
	// 1. 返回含文本、类型和空 text_elements 的输入对象。
	return map[string]any{"type": "text", "text": text, "text_elements": []any{}}
}

// assertCodexTurnCompleted 检查 Turn 终态和 Item 通知顺序。
func assertCodexTurnCompleted(t *testing.T, messages codexTurnMessages, status string, startedItemIDs ...string) {
	t.Helper()
	// 1. 核对末尾 turn/completed 的状态，再登记调用方给出的、在这批消息之前已开始的 Item。
	last := messages[len(messages)-1]
	if last.Method != "turn/completed" || last.getNotification(t).Turn.Status != status {
		t.Fatalf("turn ended with %s %s, want turn/completed %s", last.Method, last.Params, status)
	}
	startedAt, completedAt := map[string]int{}, map[string]int{}
	var startedReplies []string
	for _, itemID := range startedItemIDs {
		startedAt[itemID] = -1
	}
	// 2. 检查 Item 只开始和完成一次，完成及增量均在开始之后。
	for index, message := range messages {
		notification := message.getNotification(t)
		itemID, _ := notification.Item["id"].(string)
		switch {
		case message.Method == "item/started":
			if _, duplicated := startedAt[itemID]; duplicated {
				t.Fatalf("item/started %s twice", itemID)
			}
			startedAt[itemID] = index
			if itemType := notification.Item["type"]; itemType == "agentMessage" || itemType == "reasoning" {
				startedReplies = append(startedReplies, itemID)
			}
		case message.Method == "item/completed":
			if _, duplicated := completedAt[itemID]; duplicated {
				t.Fatalf("item/completed %s twice", itemID)
			}
			if started, found := startedAt[itemID]; !found || started > index {
				t.Fatalf("item/completed %s at %d without an earlier item/started", itemID, index)
			}
			completedAt[itemID] = index
		case strings.HasSuffix(message.Method, "Delta") || strings.HasSuffix(message.Method, "/delta"):
			if _, found := startedAt[notification.ItemID]; !found {
				t.Fatalf("%s for %s before its item/started", message.Method, notification.ItemID)
			}
		}
	}
	// 3. 核对已开始的回复和推理在 Turn 结束前全部完成。
	for _, itemID := range startedReplies {
		if _, found := completedAt[itemID]; !found {
			t.Fatalf("reply or reasoning %s started but not completed before turn/completed", itemID)
		}
	}
}

// TestCodexAutomaticThreadTitle 检查自动标题、空标题和显式改名通知。
func TestCodexAutomaticThreadTitle(t *testing.T) {
	// 1. 在两种传输上分别提交文本、图片和空白输入，核对保存标题与观察者通知。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			connections := newCodexConnections(t, binary, config, mode)
			wire := connections.open(t)
			for _, tt := range []struct {
				name      string
				input     []any
				wantTitle string
			}{
				{
					name:      "text",
					input:     []any{codexTextInput("\n 修复\t登录\n附加说明")},
					wantTitle: "修复 登录",
				},
				{
					name:  "image",
					input: []any{map[string]any{"type": "image", "url": "data:image/png;base64,aGVsbG8="}},
				},
				{
					name:  "blank",
					input: []any{codexTextInput(" \n\t")},
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					threadID := (&codexTurnFixture{
						wire:  wire,
						model: model,
					}).startThread(t, newE2EWorkspace(t), nil).Thread.ID
					observer := connections.open(t)
					observer.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, nil)
					requestID := wire.request(t, "turn/start", map[string]any{
						"threadId": threadID, "input": tt.input,
					})
					model.next(t).reply <- e2eModelReply{text: `{}`}
					messages := wire.readTurn(t, threadID, requestID)
					assertCodexTurnCompleted(t, messages, "completed")
					observerMessages := observer.readTurn(t, threadID, nil)
					if tt.wantTitle != "" {
						for _, client := range []struct {
							wire     *codexWire
							messages codexTurnMessages
						}{{
							wire,
							messages,
						}, {
							observer,
							observerMessages,
						}} {
							isAutomaticTitle := func(message codexWireMessage) bool {
								return message.Method == "thread/name/updated" && message.getThreadID() == threadID && message.getNotification(t).ThreadName == tt.wantTitle
							}
							if client.messages.indexOf(isAutomaticTitle) < 0 {
								client.wire.next(t, "automatic title", isAutomaticTitle)
							}
						}
					}
					var read codexWireThreadRead
					wire.call(t, "thread/read", map[string]any{"threadId": threadID}, &read)
					var title string
					if read.Thread.Name != nil {
						title = *read.Thread.Name
					}
					if title != tt.wantTitle {
						t.Fatalf("saved title = %q, want %q", title, tt.wantTitle)
					}
					// 没有用户文本时仍然可以显式改名；所有跟随这个对话的连接都收到同一个已保存标题。
					wire.call(t, "thread/name/set", map[string]any{"threadId": threadID, "name": "用户指定的标题"}, nil)
					for _, client := range []*codexWire{wire, observer} {
						client.next(t, "explicit title change", func(message codexWireMessage) bool {
							return message.Method == "thread/name/updated" && message.getThreadID() == threadID && message.getNotification(t).ThreadName == "用户指定的标题"
						})
					}
					observer.close(t)
				})
			}
		})
	}
}

// TestCodexThreadTitle 检查临时 Thread 生成标题和退出后的数据。
func TestCodexThreadTitle(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行。
	binary := buildOpenVaala(t)
	const titleSchema = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			wire := startAppServer(t, binary, config, mode)
			wire.call(t, "initialize", codexClientInfo, nil)
			// 1. 建立含项目 Skill 的原 Thread，完成一轮并记录标题、列表和 Skills。
			// 原 Thread 的工作目录里有一个项目 Skill，skills/list 据此看出用的是不是原 Thread 的对话。
			workspace := newE2EWorkspace(t)
			skillPath := filepath.Join(workspace, ".agents", "skills", "thread-skill", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skillPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skillPath, []byte("---\nname: thread-skill\ndescription: Skill of the thread workspace\n---\nbody\n"), 0600); err != nil {
				t.Fatal(err)
			}
			originalID := (&codexTurnFixture{
				wire:  wire,
				model: model,
			}).startThread(t, workspace, nil).Thread.ID
			requestID := wire.request(t, "turn/start", map[string]any{"threadId": originalID, "input": []any{codexTextInput("修复登录")}})
			model.next(t).reply <- e2eModelReply{text: "好的"}
			originalMessages := wire.readTurn(t, originalID, requestID)
			assertCodexTurnCompleted(t, originalMessages, "completed")
			isAutomaticTitle := func(message codexWireMessage) bool {
				return message.Method == "thread/name/updated" && message.getThreadID() == originalID && message.getNotification(t).ThreadName == "修复登录"
			}
			if originalMessages.indexOf(isAutomaticTitle) < 0 {
				wire.next(t, "automatic title from the first user request", isAutomaticTitle)
			}
			var threadsBefore codexWireThreadList
			wire.call(t, "thread/list", map[string]any{}, &threadsBefore)
			var skillsBefore json.RawMessage
			wire.call(t, "skills/list", map[string]any{"cwds": []string{}}, &skillsBefore)
			if !strings.Contains(string(skillsBefore), "thread-skill") {
				t.Fatalf("skills/list = %s, want the project skill of the original thread", skillsBefore)
			}

			// 2. 创建只读且不带 Executor 的临时标题 Thread，核对临时标记。
			// titleThread 是临时 Thread 对象里测试关心的字段；Codex TUI 只在 ephemeral 和 threadSource 都对上时忽略它的 thread/started。
			type titleThread struct {
				ID           string  `json:"id"`
				Ephemeral    bool    `json:"ephemeral"`
				ThreadSource *string `json:"threadSource"`
				Path         *string `json:"path"`
				Cwd          string  `json:"cwd"`
			}
			isTitleThread := func(thread titleThread) bool {
				return thread.ID != "" && thread.Ephemeral && thread.ThreadSource != nil && *thread.ThreadSource == "thread_title" && thread.Path == nil && thread.Cwd == workspace
			}
			var started struct {
				Thread  titleThread `json:"thread"`
				Sandbox struct {
					Type string `json:"type"`
				} `json:"sandbox"`
			}
			wire.call(t, "thread/start", map[string]any{
				"cwd": workspace, "model": "local/default", "modelProvider": "local",
				"ephemeral": true, "threadSource": "thread_title", "sandbox": "read-only", "approvalPolicy": "never",
				"environments": []any{}, "dynamicTools": []any{}, "config": map[string]any{"features.shell_tool": false, "web_search": "disabled"},
			}, &started)
			titleThreadID := started.Thread.ID
			if !isTitleThread(started.Thread) || started.Sandbox.Type != "readOnly" {
				t.Fatalf("ephemeral thread/start = %+v, want an ephemeral thread_title thread without a path in %s with a readOnly sandbox", started, workspace)
			}
			var startedNotification struct {
				Thread titleThread `json:"thread"`
			}
			if err := json.Unmarshal(wire.nextThreadNotification(t, titleThreadID, "thread/started").Params, &startedNotification); err != nil || !isTitleThread(startedNotification.Thread) {
				t.Fatalf("thread/started = %+v (%v), want the same ephemeral thread_title thread", startedNotification, err)
			}
			if loaded := getLoadedThreads(t, wire); !slices.Contains(loaded, titleThreadID) {
				t.Fatalf("thread/loaded/list = %v, want the ephemeral thread %s", loaded, titleThreadID)
			}

			// 3. 带输出 schema 请求标题，核对模型请求中没有 Executor 工具。
			requestID = wire.request(t, "turn/start", map[string]any{
				"threadId":     titleThreadID,
				"input":        []any{codexTextInput("Generate a concise title…\n\nUser prompt:\n修复登录")},
				"outputSchema": json.RawMessage(titleSchema),
			})
			request := model.next(t)
			if text := getModelLastUserText(request.body); !strings.HasSuffix(text, titleSchema) || !strings.Contains(text, "Respond with only a JSON object") {
				t.Fatalf("last user message = %q, want the JSON format instruction followed by the schema", text)
			}
			if tools := getModelToolNames(request.body); slices.ContainsFunc(tools, func(name string) bool {
				return name == "bash" || name == "read_file" || name == "apply_patch"
			}) {
				t.Fatalf("model tools = %v, want no executor tools in an ephemeral thread", tools)
			}
			request.reply <- e2eModelReply{text: `{"title":"修复登录故障"}`}
			messages := wire.readTurn(t, titleThreadID, requestID)
			if reply, _ := messages.getItem(t, "item/completed", "agentMessage"); reply["text"] != `{"title":"修复登录故障"}` {
				t.Fatalf("agentMessage = %v, want the JSON title", reply)
			}
			assertCodexTurnCompleted(t, messages, "completed")

			// 4. 退订临时 Thread，将标题写回原 Thread，再检查列表和 Skills 保持不变。
			var unsubscribed struct {
				Status string `json:"status"`
			}
			wire.call(t, "thread/unsubscribe", map[string]any{"threadId": titleThreadID}, &unsubscribed)
			if unsubscribed.Status != "unsubscribed" {
				t.Fatalf("thread/unsubscribe = %+v, want unsubscribed", unsubscribed)
			}
			wire.call(t, "thread/name/set", map[string]any{"threadId": originalID, "name": "修复登录故障"}, nil)
			if updated := wire.nextThreadNotification(t, originalID, "thread/name/updated").getNotification(t); updated.ThreadName != "修复登录故障" {
				t.Fatalf("thread/name/updated = %+v, want 修复登录故障", updated)
			}

			var threadsAfter codexWireThreadList
			wire.call(t, "thread/list", map[string]any{}, &threadsAfter)
			if before, after := slices.Sorted(slices.Values(threadsBefore.getIDs())), slices.Sorted(slices.Values(threadsAfter.getIDs())); !slices.Equal(after, before) {
				t.Fatalf("thread/list after the title = %v, want the threads before it %v", after, before)
			}
			var skillsAfter json.RawMessage
			wire.call(t, "skills/list", map[string]any{"cwds": []string{}}, &skillsAfter)
			if string(skillsAfter) != string(skillsBefore) {
				t.Fatalf("skills/list after the title = %s, want the result before it %s", skillsAfter, skillsBefore)
			}

			// 5. 检查数据目录没有临时 Thread 的文件或目录。
			// serve 的数据目录里有原 Thread 的日志，没有任何名字带临时 Thread id 的文件或目录。
			var names []string
			dataDirectory := filepath.Join(filepath.Dir(config), ".local", "share", "openvaala")
			if err := filepath.WalkDir(dataDirectory, func(_ string, entry fs.DirEntry, err error) error {
				if err == nil {
					names = append(names, entry.Name())
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			containing := func(id string) func(string) bool {
				return func(name string) bool { return strings.Contains(name, id) }
			}
			if !slices.ContainsFunc(names, containing(originalID)) || slices.ContainsFunc(names, containing(titleThreadID)) {
				t.Fatalf("data directory entries = %v, want the original thread %s and nothing of the ephemeral thread %s", names, originalID, titleThreadID)
			}
		})
	}
}

// getModelLastUserText 返回模型请求最后一条用户消息的文字。
func getModelLastUserText(body map[string]any) string {
	// 1. 倒序查找 user 消息；字符串直接返回，分段内容按顺序拼接 text。
	messages, _ := body["messages"].([]any)
	for _, message := range slices.Backward(messages) {
		fields, _ := message.(map[string]any)
		if fields["role"] != "user" {
			continue
		}
		if text, isText := fields["content"].(string); isText {
			return text
		}
		var text strings.Builder
		parts, _ := fields["content"].([]any)
		for _, part := range parts {
			partFields, _ := part.(map[string]any)
			partText, _ := partFields["text"].(string)
			text.WriteString(partText)
		}
		return text.String()
	}
	return ""
}

// getModelToolNames 返回模型请求中的工具名称。
func getModelToolNames(body map[string]any) []string {
	// 1. 依次读取 tools 中各 function 的 name。
	tools, _ := body["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool.(map[string]any)["function"].(map[string]any)
		name, _ := function["name"].(string)
		names = append(names, name)
	}
	return names
}

// TestCodexResume 检查恢复、运行中接入、重配、压缩和断线。
func TestCodexResume(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行各用例。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			fixture := &codexResumeFixture{
				connections: newCodexConnections(t, binary, config, mode),
				model:       model,
				config:      config,
			}
			// 每个用例新开自己的连接和 Thread：unix 模式是同一个 app-server 进程上的多条连接，stdio 模式是同一个 serve 上的多个进程。
			for _, test := range []struct {
				name string
				run  func(t *testing.T, f *codexResumeFixture)
			}{
				{
					name: "resume_after_exit",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 完成一轮后断开，再恢复并检查历史、跟随列表、改名及分页。
						first := f.connections.open(t)
						threadID := f.startThread(t, first)
						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("hello")}})
						f.model.next(t).reply <- e2eModelReply{text: "hi there"}
						messages := first.readTurn(t, threadID, requestID)
						assertCodexTurnCompleted(t, messages, "completed")
						// 这一轮跑起来时状态变成 active，结束后回到 idle。
						first.waitThreadStatus(t, threadID, "active", messages)
						first.waitThreadStatus(t, threadID, "idle", messages)
						first.close(t)

						second := f.connections.open(t)
						var resumed codexWireThreadResume
						second.call(t, "thread/resume", map[string]any{"threadId": threadID}, &resumed)
						if resumed.Thread.ID != threadID || resumed.Thread.Status.Type != "idle" || len(resumed.Thread.Turns) != 1 {
							t.Fatalf("thread/resume = %+v, want the idle thread %s with its one turn", resumed.Thread, threadID)
						}
						turn := resumed.Thread.Turns[0]
						if turn.Status != "completed" || turn.ItemsView != "full" || len(turn.Items) != 2 ||
							turn.Items[0]["type"] != "userMessage" || turn.Items[1]["type"] != "agentMessage" || turn.Items[1]["text"] != "hi there" {
							t.Fatalf("resumed turn = %+v, want the completed turn with the user message and the reply", turn)
						}
						var loaded codexWireLoadedList
						second.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if !slices.Equal(loaded.Data, []string{threadID}) || loaded.NextCursor != nil {
							t.Fatalf("thread/loaded/list = %+v, want only %s", loaded, threadID)
						}
						// 已经跟随时直接按现在的状态回复，不重新打开。
						var again codexWireThreadResume
						second.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &again)
						if again.Thread.ID != threadID || again.Thread.Status.Type != "idle" || len(again.Thread.Turns) != 0 {
							t.Fatalf("thread/resume of a loaded thread = %+v, want the idle thread without turns", again.Thread)
						}
						// 改名：先回 {}，再在这条连接上发 thread/name/updated。
						renameID := second.request(t, "thread/name/set", map[string]any{"threadId": threadID, "name": "renamed"})
						renamed := second.next(t, "thread/name/set response", func(message codexWireMessage) bool {
							return isCodexResponse(renameID)(message) || (message.Method == "thread/name/updated" && message.getNotification(t).ThreadName == "renamed")
						})
						if renamed.Method != "" || renamed.Error != nil || string(renamed.Result) != "{}" {
							t.Fatalf("first message after thread/name/set = %+v (error %+v), want the {} response", renamed, renamed.Error)
						}
						if updated := second.nextThreadNotification(t, threadID, "thread/name/updated").getNotification(t); updated.ThreadName != "renamed" {
							t.Fatalf("thread/name/updated = %+v, want renamed", updated)
						}
						// 两个 Thread 时按 id 排序分页。
						otherID := f.startThread(t, second)
						sorted := slices.Sorted(slices.Values([]string{threadID, otherID}))
						var page codexWireLoadedList
						second.call(t, "thread/loaded/list", map[string]any{"limit": 1}, &page)
						if !slices.Equal(page.Data, sorted[:1]) || page.NextCursor == nil || *page.NextCursor != sorted[0] {
							t.Fatalf("thread/loaded/list page 1 = %+v, want %s with a cursor", page, sorted[0])
						}
						second.call(t, "thread/loaded/list", map[string]any{"limit": 1, "cursor": *page.NextCursor}, &page)
						if !slices.Equal(page.Data, sorted[1:]) || page.NextCursor != nil {
							t.Fatalf("thread/loaded/list page 2 = %+v, want %s and no cursor", page, sorted[1])
						}
					},
				},
				{
					name: "join_mid_output",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 写出部分回复后接入第二连接，检查累计增量与完成通知。
						first := f.connections.open(t)
						threadID := f.startThread(t, first)
						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("stream it")}})
						stream := make(chan string)
						f.model.next(t).reply <- e2eModelReply{stream: stream}
						stream <- "partial"
						// 发起方看到了第一段：serve 里这条回复已经生成到 partial。
						started := first.readThreadUntil(t, threadID, requestID, isCodexMethod("item/agentMessage/delta"))
						var startResult struct {
							Turn codexWireTurn `json:"turn"`
						}
						if err := json.Unmarshal(started[started.indexOf(isCodexResponse(requestID))].Result, &startResult); err != nil {
							t.Fatal(err)
						}
						first.waitThreadStatus(t, threadID, "active", started)

						second := f.connections.open(t)
						resumeID := second.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						joined := second.readThreadUntil(t, threadID, resumeID, isCodexMethod("item/agentMessage/delta"))
						var resumed codexWireThreadResume
						if joined.indexOf(isCodexResponse(resumeID)) != 0 || json.Unmarshal(joined[0].Result, &resumed) != nil || resumed.Thread.Status.Type != "active" {
							t.Fatalf("first message after thread/resume = %+v, want the response with an active thread", joined[0])
						}
						// 2. 核对响应之后依次收到开轮、回复开始和累计内容增量。
						turnStartedAt := joined.indexOf(isCodexMethod("turn/started"))
						_, itemStartedAt := joined.getItem(t, "item/started", "agentMessage")
						delta := joined[len(joined)-1].getNotification(t)
						if turnStartedAt < 0 || joined[turnStartedAt].getNotification(t).Turn.ID != startResult.Turn.ID || !(turnStartedAt < itemStartedAt) || delta.Delta != "partial" {
							t.Fatalf("after joining: turn/started at %d, item/started at %d, then delta %q, want turn %s started, then the agentMessage with the cumulative delta partial",
								turnStartedAt, itemStartedAt, delta.Delta, startResult.Turn.ID)
						}

						stream <- " and the rest"
						close(stream)
						messages := append(joined, second.readTurn(t, threadID, nil)...)
						assertCodexTurnCompleted(t, messages, "completed")
						reply, _ := messages.getItem(t, "item/completed", "agentMessage")
						if text := messages.getDeltaText(t, "item/agentMessage/delta", reply["id"].(string)); reply["text"] != "partial and the rest" || text != reply["text"] {
							t.Fatalf("joined deltas %q, final reply %q, want the deltas to concatenate to the final text without repeats", text, reply["text"])
						}
						isThisTurn := func(method string) func(codexWireMessage) bool {
							return func(message codexWireMessage) bool {
								return message.Method == method && message.getNotification(t).Turn.ID == startResult.Turn.ID
							}
						}
						if started, completed := messages.count(isThisTurn("turn/started")), messages.count(isThisTurn("turn/completed")); started != 1 || completed != 1 {
							t.Fatalf("joined connection got turn/started %d times and turn/completed %d times, want each once", started, completed)
						}
						second.waitThreadStatus(t, threadID, "idle", messages)
						assertCodexTurnCompleted(t, append(started, first.readTurn(t, threadID, nil)...), "completed")

						// 接入之后两条连接对等：后接入的连接发消息，双方都收到同一条 userMessage，只有发起方的带 clientId。
						requestID = second.request(t, "turn/start", map[string]any{
							"threadId": threadID, "clientUserMessageId": "from-second", "input": []any{codexTextInput("from the second connection")},
						})
						f.model.next(t).reply <- e2eModelReply{text: "seen by both"}
						sent, observed := second.readTurn(t, threadID, requestID), first.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, sent, "completed")
						assertCodexTurnCompleted(t, observed, "completed")
						sentInput, _ := sent.getItem(t, "item/completed", "userMessage")
						observedInput, _ := observed.getItem(t, "item/completed", "userMessage")
						observedReply, _ := observed.getItem(t, "item/completed", "agentMessage")
						if sentInput["clientId"] != "from-second" || observedInput["id"] != sentInput["id"] || observedInput["clientId"] != nil || observedReply["text"] != "seen by both" {
							t.Fatalf("sender's userMessage %v, other connection's userMessage %v and reply %v, want the same input with clientId only on the sender",
								sentInput, observedInput, observedReply)
						}
					},
				},
				{
					name: "join_while_tool_runs",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 让 bash 等待 FIFO，再接入并检查分页中的运行命令及后续实时结果。
						first := f.connections.open(t)
						workspace := newE2EWorkspace(t)
						threadID := (&codexTurnFixture{
							wire:  first,
							model: f.model,
						}).startThread(t, workspace, nil).Thread.ID
						fifo := filepath.Join(workspace, "release.fifo")
						if err := syscall.Mkfifo(fifo, 0600); err != nil {
							t.Fatal(err)
						}
						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("wait for it")}})
						f.model.next(t).reply <- e2eModelReply{
							tool:      "bash",
							arguments: `{"command":"echo before; cat release.fifo; echo after"}`,
						}
						// 写端打开时 bash 已经输出 before、在读 FIFO：命令在执行，ToolCall 在执行前已经保存，不属于生成中的 Item。
						release := openE2EFIFOWriter(t, fifo)

						// 带 excludeTurns 接入（Codex TUI）：客户端自己翻页，还在执行的命令由翻页给出（inProgress），接入时不再发它的 item/started。
						second := f.connections.open(t)
						resumeID := second.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						joined := second.readThreadUntil(t, threadID, resumeID, isCodexMethod("turn/started"))
						var resumed codexWireThreadResume
						if joined.indexOf(isCodexResponse(resumeID)) != 0 || json.Unmarshal(joined[0].Result, &resumed) != nil || resumed.Thread.Status.Type != "active" {
							t.Fatalf("first message after thread/resume = %+v, want the response with an active thread", joined[0])
						}
						var page codexWireItemPage
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage", "commandExecution"}) {
							t.Fatalf("items paged while the command runs = %v, want the user message and the running command", got)
						}
						command := page.Data[1].Item
						if command["status"] != "inProgress" || command["command"] != "bash -lc 'echo before; cat release.fifo; echo after'" {
							t.Fatalf("paged command = %v, want the running command", command)
						}

						if _, err := release.WriteString("released\n"); err != nil {
							t.Fatal(err)
						}
						if err := release.Close(); err != nil {
							t.Fatal(err)
						}
						f.model.next(t).reply <- e2eModelReply{text: "done"}
						messages := append(joined, second.readTurn(t, threadID, nil)...)
						assertCodexTurnCompleted(t, messages, "completed", command["id"].(string))
						completed, completedAt := messages.getItem(t, "item/completed", "commandExecution")
						if completed["id"] != command["id"] || completed["exitCode"] != 0.0 || completed["aggregatedOutput"] != "before\nreleased\nafter\n" || completedAt >= len(messages)-1 {
							t.Fatalf("item/completed %v at %d of %d, want the command completed with exit code 0 and its whole output before turn/completed", completed, completedAt, len(messages))
						}
						if started := messages.count(func(message codexWireMessage) bool {
							return message.Method == "item/started" && message.getNotification(t).Item["id"] == command["id"]
						}); started != 0 {
							t.Fatalf("the paged command got item/started %d times live, want none", started)
						}
						// 2. 核对接入后的输出按 CallID 追加增量；接入前的 before 可能不补发。
						if output := messages.getDeltaText(t, "item/commandExecution/outputDelta", command["id"].(string)); !strings.Contains(output, "released\nafter\n") {
							t.Fatalf("output deltas after joining = %q, want the output produced after the join", output)
						}
						assertCodexTurnCompleted(t, first.readTurn(t, threadID, requestID), "completed")
					},
				},
				{
					name: "resume_followed_thread_before_the_output_is_saved",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 工具调用的参数收齐时回复提前完成，但要到这次模型输出结束才保存：暂停模型输出，检查这期间再 resume 时翻页里没有它，
						// 回复之后重新发出它的 item/started、累计内容的增量和 item/completed，之后保存时不再完成一次。
						wire := f.connections.open(t)
						threadID := (&codexTurnFixture{
							wire:  wire,
							model: f.model,
						}).startThread(t, newE2EWorkspace(t), nil).Thread.ID
						requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("run it")}})
						finish := make(chan struct{})
						f.model.next(t).reply <- e2eModelReply{
							text:      "running it",
							tool:      "bash",
							arguments: `{"command":"printf done"}`,
							finish:    finish,
						}
						live := wire.readThreadUntil(t, threadID, requestID, func(message codexWireMessage) bool {
							return message.Method == "item/started" && message.getNotification(t).Item["type"] == "commandExecution"
						})
						reply, _ := live.getItem(t, "item/completed", "agentMessage")
						command, _ := live.getItem(t, "item/started", "commandExecution")
						replyID := fmt.Sprint(reply["id"])

						resumeID := wire.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						var page codexWireItemPage
						wire.call(t, "thread/items/list", map[string]any{"threadId": threadID}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage"}) {
							t.Fatalf("items paged before the model output ends = %v, want only the saved user message", got)
						}
						close(finish)
						f.model.next(t).reply <- e2eModelReply{text: "ran"}
						rest := wire.readTurn(t, threadID, resumeID)
						isReply := func(method string) func(codexWireMessage) bool {
							return func(message codexWireMessage) bool {
								return message.Method == method && message.getNotification(t).Item["id"] == replyID
							}
						}
						startedAt, completedAt := rest.indexOf(isReply("item/started")), rest.indexOf(isReply("item/completed"))
						if rest.indexOf(isCodexResponse(resumeID)) != 0 || startedAt != 1 || completedAt < startedAt ||
							rest.getDeltaText(t, "item/agentMessage/delta", replyID) != "running it" {
							t.Fatalf("after resuming: %+v, want the completed unsaved reply %s started, filled and completed right after the response", rest, replyID)
						}
						assertCodexTurnCompleted(t, rest, "completed", fmt.Sprint(command["id"]))
					},
				},
				{
					name: "resume_followed_thread_again",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. Codex TUI 切换到一个已经跟随的 Thread 时再 resume 它，丢掉之前收到的通知，按 resume 加翻页重建视图。流式回复期间再 resume：
						// 翻页读到回响应时已经实时推送的位置；回复之后重新发出没有保存的生成中回复（item/started 加一条累计内容的增量），不重发 turn/started
						// 和还在执行的工具调用（翻页里有，inProgress），之后的实时推送照常。
						wire := f.connections.open(t)
						workspace := newE2EWorkspace(t)
						threadID := (&codexTurnFixture{
							wire:  wire,
							model: f.model,
						}).startThread(t, workspace, nil).Thread.ID
						requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("stream it")}})
						stream := make(chan string)
						f.model.next(t).reply <- e2eModelReply{stream: stream}
						stream <- "partial"
						live := wire.readThreadUntil(t, threadID, requestID, isCodexMethod("item/agentMessage/delta"))
						original, _ := live.getItem(t, "item/started", "agentMessage")

						resumeID := wire.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						again := wire.readThreadUntil(t, threadID, resumeID, isCodexMethod("item/agentMessage/delta"))
						var resumed codexWireThreadResume
						if again.indexOf(isCodexResponse(resumeID)) != 0 || json.Unmarshal(again[0].Result, &resumed) != nil || resumed.Thread.Status.Type != "active" {
							t.Fatalf("first message after resuming the followed thread = %+v, want the response with an active thread", again[0])
						}
						restarted, _ := again.getItem(t, "item/started", "agentMessage")
						if restarted["id"] != original["id"] || again[len(again)-1].getNotification(t).Delta != "partial" || again.count(isCodexMethod("turn/started")) != 0 {
							t.Fatalf("after resuming the followed thread: %+v, want the streaming reply %v started again with the cumulative delta and no turn/started", again, original["id"])
						}
						var page codexWireItemPage
						wire.call(t, "thread/items/list", map[string]any{"threadId": threadID}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage"}) {
							t.Fatalf("items paged while the reply streams = %v, want only the saved user message", got)
						}
						stream <- " and the rest"
						close(stream)
						rest := wire.readTurn(t, threadID, nil)
						reply, _ := rest.getItem(t, "item/completed", "agentMessage")
						if reply["id"] != original["id"] || reply["text"] != "partial and the rest" || rest.getDeltaText(t, "item/agentMessage/delta", fmt.Sprint(reply["id"])) != " and the rest" {
							t.Fatalf("reply after resuming = %v, want the whole text with only the new delta pushed", reply)
						}

						// 第二轮命令在执行时再 resume：命令在翻页里是 inProgress，不重发它的 item/started，结果照常推送。
						fifo := filepath.Join(workspace, "release.fifo")
						if err := syscall.Mkfifo(fifo, 0600); err != nil {
							t.Fatal(err)
						}
						requestID = wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("wait for it")}})
						f.model.next(t).reply <- e2eModelReply{
							tool:      "bash",
							arguments: `{"command":"cat release.fifo"}`,
						}
						release := openE2EFIFOWriter(t, fifo)
						started := wire.readThreadUntil(t, threadID, requestID, func(message codexWireMessage) bool {
							return message.Method == "item/started" && message.getNotification(t).Item["type"] == "commandExecution"
						})
						command := started[len(started)-1].getNotification(t).Item
						resumeID = wire.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						wire.response(t, resumeID)
						wire.call(t, "thread/items/list", map[string]any{"threadId": threadID, "sortDirection": "desc", "limit": 1}, &page)
						if len(page.Data) != 1 || page.Data[0].Item["id"] != command["id"] || page.Data[0].Item["status"] != "inProgress" {
							t.Fatalf("newest paged item while the command runs = %+v, want the running command %v", page.Data, command["id"])
						}
						if _, err := release.WriteString("released\n"); err != nil {
							t.Fatal(err)
						}
						if err := release.Close(); err != nil {
							t.Fatal(err)
						}
						f.model.next(t).reply <- e2eModelReply{text: "done"}
						finished := wire.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, finished, "completed", command["id"].(string))
						completed, _ := finished.getItem(t, "item/completed", "commandExecution")
						if completed["id"] != command["id"] || completed["aggregatedOutput"] != "released\n" || finished.count(isCodexMethod("item/started")) != 1 {
							t.Fatalf("after resuming during the command: %+v, want only the reply started and the command completed with its output", finished)
						}
					},
				},
				{
					name: "resume_then_page_stops_at_resume",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. Codex TUI 切换 Thread 时丢掉缓冲的实时通知、按 resume 响应加翻页重建视图，之后到达的通知在重建之后才处理。
						// 回复流式输出时 resume 并记下游标，回复保存后检查用这个游标翻页只读到 resume 时已保存的输入，回复只走实时推送；
						// 没跟随的连接普通分页仍然读到日志末尾。
						wire := f.connections.open(t)
						threadID := f.startThread(t, wire)
						requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("stream it")}})
						stream := make(chan string)
						f.model.next(t).reply <- e2eModelReply{stream: stream}
						stream <- "partial"
						live := wire.readThreadUntil(t, threadID, requestID, isCodexMethod("item/agentMessage/delta"))
						original, _ := live.getItem(t, "item/started", "agentMessage")
						replyID := fmt.Sprint(original["id"])

						var resumed codexWireThreadResume
						wire.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed)
						if resumed.ItemsBackwardsCursor == "" {
							t.Fatal("thread/resume must return the history cursor")
						}
						stream <- " and the rest"
						close(stream)
						rest := wire.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, rest, "completed")

						var page codexWireItemPage
						wire.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage"}) {
							t.Fatalf("items paged after the reply was saved = %v, want only the user message saved before the resume response", got)
						}
						isReplyCompleted := func(message codexWireMessage) bool {
							return message.Method == "item/completed" && message.getNotification(t).Item["id"] == replyID
						}
						if completed := append(live, rest...).count(isReplyCompleted); completed != 1 {
							t.Fatalf("reply %s got item/completed %d times live, want once", replyID, completed)
						}
						if reply, _ := rest.getItem(t, "item/completed", "agentMessage"); reply["id"] != replyID || reply["text"] != "partial and the rest" {
							t.Fatalf("reply pushed live = %v, want %s with the whole text", reply, replyID)
						}

						// 没有跟随这个 Thread 的连接翻页读到日志末尾，存盘的回复在里面。
						var unfollowed codexWireItemPage
						f.connections.open(t).call(t, "thread/items/list", map[string]any{"threadId": threadID}, &unfollowed)
						if got := unfollowed.getTypes(); !slices.Equal(got, []string{"userMessage", "agentMessage"}) {
							t.Fatalf("items paged by a connection that does not follow the thread = %v, want the user message and the saved reply", got)
						}
					},
				},
				{
					name: "reconfigure_keeps_connection",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 通过 Runtime 客户端改目录，检查原 Codex 连接继续执行且命令使用新目录。
						first := f.connections.open(t)
						threadID := f.startThread(t, first)
						// 像 OpenVaala TUI 的 /cd 一样改对话的目录：serve 让同一个对话原地换上新设置，关掉旧订阅，app-server 的 Client 重新同步。
						client, _ := newE2ERuntimeClient(t, f.config)
						go func() {
							for range client.Events() {
							}
						}()
						ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
						defer cancel()
						record, err := client.GetConversation(ctx, defs.ConversationID(threadID))
						if err != nil {
							t.Fatal(err)
						}
						moved := newE2EWorkspace(t)
						settings := record.ExecutorSettings
						settings.WorkspaceDirectories = map[string]string{defs.LocalExecutorID: moved}
						if _, err := client.SetConversationExecutorSettings(ctx, record.ID, settings); err != nil {
							t.Fatal(err)
						}

						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("where am I")}})
						f.model.next(t).reply <- e2eModelReply{
							tool:      "bash",
							arguments: `{"command":"pwd"}`,
						}
						f.model.next(t).reply <- e2eModelReply{text: "moved"}
						messages := first.readTurn(t, threadID, requestID)
						assertCodexTurnCompleted(t, messages, "completed")
						if responseAt, startedAt := messages.indexOf(isCodexResponse(requestID)), messages.indexOf(isCodexMethod("turn/started")); responseAt < 0 || startedAt < responseAt {
							t.Fatalf("turn/start response at %d, turn/started at %d, want the response first", responseAt, startedAt)
						}
						// 重新同步之后按新的目录翻译：commandExecution 的 cwd 是新目录。
						command, _ := messages.getItem(t, "item/completed", "commandExecution")
						if command["cwd"] != moved || !strings.Contains(fmt.Sprint(command["aggregatedOutput"]), filepath.Base(moved)) {
							t.Fatalf("commandExecution after reconfigure = %v, want it to run in %s", command, moved)
						}
						// 输入不论在重新同步之前还是之后保存，都显示一次。
						messages.getItem(t, "item/completed", "userMessage")
					},
				},
				{
					name: "compact",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 完成一轮再手动压缩，检查压缩通知顺序及运行中压缩的忙碌错误。
						wire := f.connections.open(t)
						threadID := f.startThread(t, wire)
						requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("remember this")}})
						f.model.next(t).reply <- e2eModelReply{text: "noted"}
						assertCodexTurnCompleted(t, wire.readTurn(t, threadID, requestID), "completed")

						var result map[string]any
						wire.call(t, "thread/compact/start", map[string]any{"threadId": threadID}, &result)
						if len(result) != 0 {
							t.Fatalf("thread/compact/start = %v, want {}", result)
						}
						f.model.next(t).reply <- e2eModelReply{text: "summary of the conversation"}
						messages := wire.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, messages, "completed")
						_, compactionStartedAt := messages.getItem(t, "item/started", "contextCompaction")
						_, compactionCompletedAt := messages.getItem(t, "item/completed", "contextCompaction")
						turnStartedAt, compactedAt := messages.indexOf(isCodexMethod("turn/started")), messages.indexOf(isCodexMethod("thread/compacted"))
						if !(turnStartedAt >= 0 && turnStartedAt < compactionStartedAt && compactionStartedAt < compactionCompletedAt && compactionCompletedAt < compactedAt) {
							t.Fatalf("turn/started at %d, contextCompaction started at %d and completed at %d, thread/compacted at %d, want them in this order before turn/completed",
								turnStartedAt, compactionStartedAt, compactionCompletedAt, compactedAt)
						}

						turnFixture := &codexTurnFixture{
							wire:  wire,
							model: f.model,
						}
						turnID := turnFixture.startHeldTurn(t, threadID)
						assertCodexError(t, wire.response(t, wire.request(t, "thread/compact/start", map[string]any{"threadId": threadID})), -32600, "session is busy")
						wire.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
						wire.nextThreadNotification(t, threadID, "turn/completed")
					},
				},
				{
					name: "disconnect_keeps_turn",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 发起方断开后让观察者收完整回复，再检查退订期间的开轮与重新接入。
						first := f.connections.open(t)
						threadID := f.startThread(t, first)
						second := f.connections.open(t)
						var resumed codexWireThreadResume
						second.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed)
						if resumed.Thread.Status.Type != "idle" {
							t.Fatalf("thread/resume = %+v, want an idle thread", resumed.Thread)
						}
						// 发起的连接在拿到 turn/start 的响应之前断开：这一轮照常跑完，跟随的连接收到 completed。
						first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("keep going")}})
						request := f.model.next(t)
						first.close(t)
						request.reply <- e2eModelReply{text: "finished"}
						messages := second.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, messages, "completed")
						if reply, _ := messages.getItem(t, "item/completed", "agentMessage"); reply["text"] != "finished" {
							t.Fatalf("reply = %v, want finished", reply)
						}

						// 在等 TurnID 时退订：turn/start 回 thread not loaded，退订回 unsubscribed，这一轮照常执行。
						startID := second.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("again")}})
						unsubscribeID := second.request(t, "thread/unsubscribe", map[string]any{"threadId": threadID})
						assertCodexError(t, second.response(t, startID), -32600, "thread not loaded: "+threadID)
						var unsubscribed struct {
							Status string `json:"status"`
						}
						if response := second.response(t, unsubscribeID); response.Error != nil || json.Unmarshal(response.Result, &unsubscribed) != nil || unsubscribed.Status != "unsubscribed" {
							t.Fatalf("thread/unsubscribe = %s (error %+v), want unsubscribed", response.Result, response.Error)
						}
						request = f.model.next(t)
						second.call(t, "thread/unsubscribe", map[string]any{"threadId": threadID}, &unsubscribed)
						var loaded codexWireLoadedList
						second.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if unsubscribed.Status != "notLoaded" || len(loaded.Data) != 0 {
							t.Fatalf("after unsubscribing: thread/unsubscribe %q, thread/loaded/list %+v, want notLoaded and nothing loaded", unsubscribed.Status, loaded)
						}
						// 重新接入：这一轮还在跑，响应是 active，随后收到它的 turn/started 和结束。
						resumeID := second.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						joined := second.readThreadUntil(t, threadID, resumeID, isCodexMethod("turn/started"))
						if response := joined[joined.indexOf(isCodexResponse(resumeID))]; json.Unmarshal(response.Result, &resumed) != nil || resumed.Thread.Status.Type != "active" {
							t.Fatalf("thread/resume during the turn = %s, want an active thread", response.Result)
						}
						request.reply <- e2eModelReply{text: "again done"}
						messages = second.readTurn(t, threadID, nil)
						assertCodexTurnCompleted(t, messages, "completed")
						if reply, _ := messages.getItem(t, "item/completed", "agentMessage"); reply["text"] != "again done" {
							t.Fatalf("reply = %v, want again done", reply)
						}
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, fixture) })
			}
		})
	}
}

// codexResumeFixture 是 TestCodexResume 各用例共用的连接来源和假模型；config 用来让 runtimeclient 连上同一个 serve。
type codexResumeFixture struct {
	connections *codexConnections
	model       *e2eModel
	config      string
}

// startThread 创建新工作目录中的 Thread 并返回 ID。
func (f *codexResumeFixture) startThread(t *testing.T, wire *codexWire) string {
	t.Helper()
	// 1. 使用共用模型和给定连接调用 Thread 创建夹具。
	return (&codexTurnFixture{
		wire:  wire,
		model: f.model,
	}).startThread(t, newE2EWorkspace(t), nil).Thread.ID
}

// codexWireThreadResume 是 thread/resume 结果里测试关心的字段。
type codexWireThreadResume struct {
	Thread struct {
		ID     string `json:"id"`
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
		Turns []struct {
			ID        string           `json:"id"`
			Status    string           `json:"status"`
			ItemsView string           `json:"itemsView"`
			Items     []map[string]any `json:"items"`
		} `json:"turns"`
	} `json:"thread"`
	ItemsBackwardsCursor string `json:"itemsBackwardsCursor"`
}

// codexWireLoadedList 是 thread/loaded/list 的结果。
type codexWireLoadedList struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

// TestCodexHistory 检查 Thread、Turn 和 Item 的历史查询与分页。
func TestCodexHistory(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行各用例。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			fixture := &codexResumeFixture{
				connections: newCodexConnections(t, binary, config, mode),
				model:       model,
				config:      config,
			}
			// 每个用例新开自己的连接和 Thread：unix 模式是同一个 app-server 进程上的多条连接，stdio 模式是同一个 serve 上的多个进程。
			for _, test := range []struct {
				name string
				run  func(t *testing.T, f *codexResumeFixture)
			}{
				{
					name: "backwards_cursors_from_resume",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 按 Codex app 恢复分页历史 Thread 的做法读六轮历史：从 thread/resume 响应取两个倒序游标（缺了它就报错、停在加载中），
						// 用 turnsBackwardsCursor 倒序取最新的 5 个 Turn，再对每个 Turn 用 itemsBackwardsCursor 倒序取它的 Item。
						threadID := f.startHistoryThread(t, f.connections.open(t), 6)
						var read codexWireThreadRead
						wire := f.connections.open(t)
						wire.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read)
						var resumed struct {
							TurnsBackwardsCursor *string `json:"turnsBackwardsCursor"`
							ItemsBackwardsCursor *string `json:"itemsBackwardsCursor"`
						}
						wire.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed)
						if resumed.TurnsBackwardsCursor == nil || resumed.ItemsBackwardsCursor == nil {
							t.Fatalf("thread/resume backwards cursors = %+v, want both strings", resumed)
						}
						var turns codexWireTurnPage
						wire.call(t, "thread/turns/list", map[string]any{"threadId": threadID, "cursor": *resumed.TurnsBackwardsCursor, "limit": 5, "itemsView": "notLoaded", "sortDirection": "desc"}, &turns)
						want := read.Thread.Turns
						if len(turns.Data) != 5 || turns.Data[0].ID != want[5].ID || turns.Data[4].ID != want[1].ID {
							t.Fatalf("turns from the backwards cursor = %+v, want the five newest turns, newest first", turns.Data)
						}
						for index, turn := range turns.Data {
							var items codexWireItemPage
							wire.call(t, "thread/items/list", map[string]any{"threadId": threadID, "turnId": turn.ID, "cursor": *resumed.ItemsBackwardsCursor, "limit": 100, "sortDirection": "desc"}, &items)
							var gotIDs, wantIDs []any
							for _, entry := range slices.Backward(items.Data) {
								gotIDs = append(gotIDs, entry.Item["id"])
							}
							for _, item := range want[5-index].Items {
								wantIDs = append(wantIDs, item["id"])
							}
							if len(wantIDs) == 0 || !slices.Equal(gotIDs, wantIDs) {
								t.Fatalf("items of turn %s from the backwards cursor = %v, want %v", turn.ID, gotIDs, wantIDs)
							}
						}
					},
				},
				{
					name: "turns_paging",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 创建六轮历史，检查升降序、完整和未加载 Item 的 Turn 分页。
						wire := f.connections.open(t)
						threadID := f.startHistoryThread(t, wire, 6)
						var read codexWireThreadRead
						wire.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read)
						want := read.Thread.Turns
						if len(want) != 6 || want[0].ItemsView != "full" || want[0].Status != "completed" || len(want[0].Items) != 3 {
							t.Fatalf("thread/read turns = %+v, want six completed turns with their items (user message, reasoning, reply in the first)", want)
						}
						wantIDs := make([]string, 0, len(want))
						for _, turn := range want {
							wantIDs = append(wantIDs, turn.ID)
						}
						// 本连接一直跟随着这个 Thread，翻页读到最近一次 thread/resume 响应时已经实时推送的位置：先 resume 一次，像 Codex TUI 切换 Thread 那样。
						wire.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, nil)

						// 三页翻完：TurnID 不重不漏、倒序，没有 Item，最后一页没有下一页。
						var gotIDs []string
						params := map[string]any{"threadId": threadID, "limit": 2, "sortDirection": "desc", "itemsView": "notLoaded"}
						for page := range 3 {
							var turns codexWireTurnPage
							wire.call(t, "thread/turns/list", params, &turns)
							if len(turns.Data) != 2 || turns.BackwardsCursor != nil {
								t.Fatalf("turns page %d = %+v, want two turns and no backwards cursor", page, turns)
							}
							for _, turn := range turns.Data {
								if turn.ItemsView != "notLoaded" || turn.Items == nil || len(turn.Items) != 0 {
									t.Fatalf("turn %s = %+v, want no items and itemsView notLoaded", turn.ID, turn)
								}
								gotIDs = append(gotIDs, turn.ID)
							}
							if wantNext := page < 2; (turns.NextCursor != nil) != wantNext || (wantNext && *turns.NextCursor != turns.Data[1].ID) {
								t.Fatalf("turns page %d next cursor = %v, want the last turn id of the page except on the last page", page, turns.NextCursor)
							}
							if turns.NextCursor != nil {
								params["cursor"] = *turns.NextCursor
							}
						}
						if !slices.Equal(gotIDs, reversedStrings(wantIDs)) {
							t.Fatalf("paged turn ids = %v, want every turn once, newest first: %v", gotIDs, reversedStrings(wantIDs))
						}

						// 省略 sortDirection：thread/turns/list 默认倒序，第一页是最新的两个 Turn。
						var defaultTurns codexWireTurnPage
						wire.call(t, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 2, "itemsView": "notLoaded"}, &defaultTurns)
						if len(defaultTurns.Data) != 2 || defaultTurns.Data[0].ID != wantIDs[5] || defaultTurns.Data[1].ID != wantIDs[4] {
							t.Fatalf("first page of thread/turns/list without sortDirection = %+v, want the newest turns %s and %s", defaultTurns.Data, wantIDs[5], wantIDs[4])
						}

						// 正序同样不重不漏。
						var ascending codexWireTurnPage
						wire.call(t, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 4, "sortDirection": "asc", "itemsView": "notLoaded"}, &ascending)
						if len(ascending.Data) != 4 || ascending.Data[0].ID != wantIDs[0] || ascending.Data[3].ID != wantIDs[3] || ascending.NextCursor == nil || *ascending.NextCursor != wantIDs[3] {
							t.Fatalf("ascending turns = %+v, want the first four turns with a cursor", ascending)
						}

						// full 和默认的 summary：每个 Turn 的 items 和 thread/read 里的一致。
						for _, itemsView := range []any{"full", nil} {
							var pagedTurns []codexWireHistoryTurn
							params := map[string]any{"threadId": threadID, "limit": 2}
							if itemsView != nil {
								params["itemsView"] = itemsView
							}
							for {
								var turns codexWireTurnPage
								wire.call(t, "thread/turns/list", params, &turns)
								pagedTurns = append(pagedTurns, turns.Data...)
								if turns.NextCursor == nil {
									break
								}
								params["cursor"] = *turns.NextCursor
							}
							slices.Reverse(pagedTurns)
							if !reflect.DeepEqual(pagedTurns, want) {
								t.Fatalf("turns with itemsView %v = %+v, want the turns of thread/read: %+v", itemsView, pagedTurns, want)
							}
						}

						assertCodexError(t, wire.response(t, wire.request(t, "thread/turns/list", map[string]any{"threadId": threadID, "cursor": "not-a-turn"})), -32602, "invalid cursor: not-a-turn")
						assertCodexError(t, wire.response(t, wire.request(t, "thread/turns/list", map[string]any{"threadId": threadID, "itemsView": "detailed"})), -32602, "invalid itemsView: detailed")
					},
				},
				{
					name: "items_paging",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 创建混合工具历史，用不同页长与方向读取并与完整历史比较。
						first := f.connections.open(t)
						threadID := f.startHistoryThread(t, first, 6)
						// 另一条连接没有跟随这个 Thread，翻页读到日志末尾。
						reader := f.connections.open(t)
						var read codexWireThreadRead
						reader.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read)
						var want []codexWireItemEntry
						for _, turn := range read.Thread.Turns {
							for _, item := range turn.Items {
								want = append(want, codexWireItemEntry{
									TurnID: turn.ID,
									Item:   item,
								})
							}
						}
						if len(want) < 15 {
							t.Fatalf("thread/read has %d items, want the whole conversation", len(want))
						}
						for _, test := range []struct {
							name      string
							limit     int
							direction string
						}{
							{
								name:      "desc_one_at_a_time",
								limit:     1,
								direction: "desc",
							},
							{
								name:      "desc_three_at_a_time",
								limit:     3,
								direction: "desc",
							},
							{
								name:  "default_direction_is_asc",
								limit: 4,
							},
							{
								name:      "asc_one_at_a_time",
								limit:     1,
								direction: "asc",
							},
							{
								name:      "asc_three_at_a_time",
								limit:     3,
								direction: "asc",
							},
						} {
							t.Run(test.name, func(t *testing.T) {
								params := map[string]any{"threadId": threadID, "limit": test.limit}
								if test.direction != "" {
									params["sortDirection"] = test.direction
								}
								pages := pageThreadItems(t, reader, params)
								var got []codexWireItemEntry
								for _, page := range pages {
									if page.BackwardsCursor != nil {
										t.Fatalf("backwardsCursor = %v, want null", *page.BackwardsCursor)
									}
									got = append(got, page.Data...)
								}
								// 检查跨页的 ToolCall 与 ToolResult 已配对，工具调用均显示完成。
								for _, entry := range got {
									if status, hasStatus := entry.Item["status"].(string); hasStatus && status != "completed" {
										t.Fatalf("%s item %v has status %s, want every tool call completed with its result", entry.Item["type"], entry.Item["id"], status)
									}
									if entry.StartedAtMs == nil || (entry.Item["type"] == "commandExecution" && (entry.CompletedAtMs == nil || *entry.CompletedAtMs < *entry.StartedAtMs)) {
										t.Fatalf("entry %v times = %v..%v, want the save time of the first item and, for a tool call, of its result", entry.Item["id"], entry.StartedAtMs, entry.CompletedAtMs)
									}
								}
								if test.direction == "desc" {
									slices.Reverse(got)
								}
								if !equalItemEntries(got, want) {
									t.Fatalf("%d paged items concatenate to %v, want the %d items of thread/read: %v", len(got), getEntryIDs(got), len(want), getEntryIDs(want))
								}
							})
						}

						// 省略 sortDirection：thread/items/list 照 Codex 0.163.0 默认正序，第一页从对话的第一条 Item 开始。
						var defaultItems codexWireItemPage
						reader.call(t, "thread/items/list", map[string]any{"threadId": threadID, "limit": 2}, &defaultItems)
						if len(defaultItems.Data) == 0 || defaultItems.Data[0].Item["id"] != want[0].Item["id"] {
							t.Fatalf("first page of thread/items/list without sortDirection = %v, want it to start with the first item %v", getEntryIDs(defaultItems.Data), want[0].Item["id"])
						}

						// turnId 只保留这个 Turn 的 Item，按页读也是。
						turn := read.Thread.Turns[3]
						var wantTurn []codexWireItemEntry
						for _, item := range turn.Items {
							wantTurn = append(wantTurn, codexWireItemEntry{
								TurnID: turn.ID,
								Item:   item,
							})
						}
						var gotTurn []codexWireItemEntry
						for _, page := range pageThreadItems(t, reader, map[string]any{"threadId": threadID, "turnId": turn.ID, "limit": 1, "sortDirection": "asc"}) {
							gotTurn = append(gotTurn, page.Data...)
						}
						if !equalItemEntries(gotTurn, wantTurn) {
							t.Fatalf("items of turn %s = %v, want %v", turn.ID, getEntryIDs(gotTurn), getEntryIDs(wantTurn))
						}
					},
				},
				{
					name: "thread_without_turn_is_not_listed",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. thread/start 只在内存里建对话：检查第一轮之前另一条连接的 thread/list 里没有它，第一轮之后才有。
						first, reader := f.connections.open(t), f.connections.open(t)
						directory := newE2EWorkspace(t)
						threadID := (&codexTurnFixture{
							wire:  first,
							model: f.model,
						}).startThread(t, directory, nil).Thread.ID
						var before codexWireThreadList
						reader.call(t, "thread/list", map[string]any{"cwd": directory}, &before)
						if len(before.getIDs()) != 0 {
							t.Fatalf("thread/list before the first turn = %v, want empty", before.getIDs())
						}
						f.runTurn(t, first, threadID, "question 1", historyTurnScripts[0]...)
						var after codexWireThreadList
						reader.call(t, "thread/list", map[string]any{"cwd": directory}, &after)
						if !slices.Equal(after.getIDs(), []string{threadID}) {
							t.Fatalf("thread/list after the first turn = %v, want [%s]", after.getIDs(), threadID)
						}
					},
				},
				{
					name: "list_filters",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 创建并改名三个 Thread，检查目录、标题、顺序、归档和子 Agent 筛选。
						wire := f.connections.open(t)
						turnFixture := &codexTurnFixture{
							wire:  wire,
							model: f.model,
						}
						firstDirectory, secondDirectory := newE2EWorkspace(t), newE2EWorkspace(t)
						fixBug := turnFixture.startThread(t, firstDirectory, nil).Thread.ID
						writeDocs := turnFixture.startThread(t, firstDirectory, nil).Thread.ID
						fixPager := turnFixture.startThread(t, secondDirectory, nil).Thread.ID
						for _, threadID := range []string{fixBug, writeDocs, fixPager} {
							turnFixture.completeTextTurn(t, threadID)
						}
						// 2. 按创建顺序改名，列表按最近更新倒序返回 fixPager、writeDocs、fixBug。
						wire.call(t, "thread/name/set", map[string]any{"threadId": fixBug, "name": "Fix login bug"}, nil)
						wire.call(t, "thread/name/set", map[string]any{"threadId": writeDocs, "name": "Write docs"}, nil)
						wire.call(t, "thread/name/set", map[string]any{"threadId": fixPager, "name": "Fix pager"}, nil)
						reader := f.connections.open(t)

						for _, test := range []struct {
							name   string
							params map[string]any
							want   []string
						}{
							{
								name:   "one_cwd",
								params: map[string]any{"cwd": firstDirectory},
								want:   []string{writeDocs, fixBug},
							},
							{
								name:   "several_cwds",
								params: map[string]any{"cwd": []string{firstDirectory, secondDirectory}},
								want:   []string{fixPager, writeDocs, fixBug},
							},
							{
								name:   "search_term_is_case_sensitive",
								params: map[string]any{"cwd": []string{firstDirectory, secondDirectory}, "searchTerm": "FIX"},
								want:   []string{},
							},
							{
								name:   "search_term_matches",
								params: map[string]any{"cwd": []string{firstDirectory, secondDirectory}, "searchTerm": "Fix"},
								want:   []string{fixPager, fixBug},
							},
							{
								name:   "search_term_within_a_cwd",
								params: map[string]any{"cwd": firstDirectory, "searchTerm": "docs"},
								want:   []string{writeDocs},
							},
							{
								name:   "oldest_update_first",
								params: map[string]any{"cwd": firstDirectory, "sortDirection": "asc"},
								want:   []string{fixBug, writeDocs},
							},
							{
								name:   "archived_is_empty",
								params: map[string]any{"cwd": firstDirectory, "archived": true},
								want:   []string{},
							},
							{
								name:   "sub_agent_kinds_without_a_filter_are_empty",
								params: map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}},
								want:   []string{},
							},
							{
								name:   "sub_agents_of_a_thread_without_any",
								params: map[string]any{"sourceKinds": []string{"subAgent", "subAgentThreadSpawn"}, "ancestorThreadId": fixBug},
								want:   []string{},
							},
							{
								name:   "direct_sub_agents_of_a_thread_without_any",
								params: map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}, "parentThreadId": fixBug},
								want:   []string{},
							},
						} {
							t.Run(test.name, func(t *testing.T) {
								var listed codexWireThreadList
								wire.call(t, "thread/list", test.params, &listed)
								if got := listed.getIDs(); !slices.Equal(got, test.want) || listed.BackwardsCursor != nil {
									t.Fatalf("thread/list %v = %v, want %v", test.params, got, test.want)
								}
							})
						}

						// 不带过滤：最近更新的三个对话就是刚建的这三个，本连接跟随着它们（空闲），另一条连接没有（notLoaded）。
						var all, others codexWireThreadList
						wire.call(t, "thread/list", map[string]any{"limit": 3}, &all)
						reader.call(t, "thread/list", map[string]any{"limit": 3}, &others)
						if !slices.Equal(all.getIDs(), []string{fixPager, writeDocs, fixBug}) || !slices.Equal(others.getIDs(), all.getIDs()) {
							t.Fatalf("thread/list = %v and %v, want the three new threads newest first on both connections", all.getIDs(), others.getIDs())
						}
						for index, thread := range all.Data {
							if thread.Status.Type != "idle" || others.Data[index].Status.Type != "notLoaded" || thread.Turns == nil || len(thread.Turns) != 0 {
								t.Fatalf("thread %s status %q (following) and %q (not following), turns %v, want idle, notLoaded and an empty turns list", thread.ID, thread.Status.Type, others.Data[index].Status.Type, thread.Turns)
							}
						}
						fixBugThread := all.Data[2]
						if fixBugThread.Name == nil || *fixBugThread.Name != "Fix login bug" || fixBugThread.Preview != "Fix login bug" || fixBugThread.Cwd != firstDirectory ||
							fixBugThread.ModelProvider != "local" || fixBugThread.Model != "local/default" || fixBugThread.SessionID != fixBug ||
							string(fixBugThread.Source) != `"appServer"` || fixBugThread.HistoryMode != "paginated" {
							t.Fatalf("thread/list row = %+v, want the stored thread", fixBugThread)
						}

						// 一页一个：游标接着读，最后一页没有游标。
						var walked []string
						params := map[string]any{"cwd": []string{firstDirectory, secondDirectory}, "limit": 1}
						for pages := 0; ; pages++ {
							var listed codexWireThreadList
							wire.call(t, "thread/list", params, &listed)
							walked = append(walked, listed.getIDs()...)
							if listed.NextCursor == nil {
								break
							}
							if pages > 3 {
								t.Fatalf("thread/list did not end: %v", walked)
							}
							params["cursor"] = *listed.NextCursor
						}
						if !slices.Equal(walked, []string{fixPager, writeDocs, fixBug}) {
							t.Fatalf("thread/list pages = %v, want every thread once", walked)
						}
						assertCodexError(t, wire.response(t, wire.request(t, "thread/list", map[string]any{"cursor": "???"})), -32602, "invalid cursor: ???")
					},
				},
				{
					name: "read_unfollowed",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 比较跟随与未跟随连接的历史和状态，再检查接入后的变化。
						first := f.connections.open(t)
						threadID := f.startHistoryThread(t, first, 2)
						var followed codexWireThreadRead
						first.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &followed)
						if followed.Thread.Status.Type != "idle" || len(followed.Thread.Turns) != 2 {
							t.Fatalf("thread/read on the following connection = %+v, want an idle thread with two turns", followed.Thread)
						}

						// 另起连接 B，不 resume：notLoaded，turns 和 A 看到的一致，读完 B 仍没有跟随它。
						second := f.connections.open(t)
						var unfollowed, withoutTurns codexWireThreadRead
						second.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &unfollowed)
						second.call(t, "thread/read", map[string]any{"threadId": threadID}, &withoutTurns)
						if unfollowed.Thread.Status.Type != "notLoaded" || !reflect.DeepEqual(unfollowed.Thread.Turns, followed.Thread.Turns) {
							t.Fatalf("thread/read on another connection = %+v, want a notLoaded thread with the same turns", unfollowed.Thread)
						}
						if withoutTurns.Thread.Turns == nil || len(withoutTurns.Thread.Turns) != 0 || withoutTurns.Thread.Cwd != followed.Thread.Cwd || withoutTurns.Thread.Model != "local/default" {
							t.Fatalf("thread/read without includeTurns = %+v, want no turns and the stored settings", withoutTurns.Thread)
						}
						var loaded codexWireLoadedList
						second.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if len(loaded.Data) != 0 {
							t.Fatalf("thread/loaded/list after thread/read = %v, want nothing: reading does not follow", loaded.Data)
						}
						assertCodexError(t, second.response(t, second.request(t, "thread/read", map[string]any{"threadId": "missing"})), -32600, "thread not loaded: missing")

						// 在跑的 Turn：跟随的连接是 active，没跟随的是 notLoaded，两边都看到 inProgress 的 Turn。
						turnID := (&codexTurnFixture{
							wire:  first,
							model: f.model,
						}).startHeldTurn(t, threadID)
						waitThreadReadStatus(t, first, threadID, "active")
						var activeFollowed, activeUnfollowed codexWireThreadRead
						first.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &activeFollowed)
						second.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &activeUnfollowed)
						running := activeUnfollowed.Thread.Turns[len(activeUnfollowed.Thread.Turns)-1]
						if activeFollowed.Thread.Status.Type != "active" || activeUnfollowed.Thread.Status.Type != "notLoaded" || running.ID != turnID || running.Status != "inProgress" {
							t.Fatalf("during a turn: following %q, other %q, last turn %+v, want active, notLoaded and turn %s inProgress", activeFollowed.Thread.Status.Type, activeUnfollowed.Thread.Status.Type, running, turnID)
						}
						first.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
						first.nextThreadNotification(t, threadID, "turn/completed")

						// B resume 之后跟随它：thread/read 的状态随之变成这个 Session 的状态，不再是 notLoaded。
						waitThreadReadStatus(t, first, threadID, "idle")
						second.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, nil)
						waitThreadReadStatus(t, second, threadID, "idle")
					},
				},
				{
					name: "join_mid_message_then_page",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 部分回复期间接入并翻页，检查历史与实时内容不重复且游标位置固定。
						first := f.connections.open(t)
						threadID := f.startThread(t, first)
						firstLive := f.runTurn(t, first, threadID, "first question", e2eModelReply{text: "first answer"})
						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("second question")}})
						stream := make(chan string)
						f.model.next(t).reply <- e2eModelReply{stream: stream}
						stream <- "partial"
						firstLive = append(firstLive, first.readThreadUntil(t, threadID, requestID, isCodexMethod("item/agentMessage/delta"))...)

						second := f.connections.open(t)
						resumeID := second.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						secondLive := second.readThreadUntil(t, threadID, resumeID, isCodexMethod("item/agentMessage/delta"))
						var resumed codexWireThreadResume
						resumeResponse := secondLive.indexOf(isCodexResponse(resumeID))
						if resumeResponse < 0 {
							t.Fatal("thread/resume response must precede the replayed delta")
						}
						if err := json.Unmarshal(secondLive[resumeResponse].Result, &resumed); err != nil || resumed.ItemsBackwardsCursor == "" {
							t.Fatalf("thread/resume history cursor = %q, error %v", resumed.ItemsBackwardsCursor, err)
						}
						// resume 之后先翻页：跟随起点之前保存的是前一轮和这一轮的输入，生成中的回复只有实时推送。
						var page codexWireItemPage
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor, "limit": 100, "sortDirection": "desc"}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage", "agentMessage", "userMessage"}) || page.Data[0].Item["content"] == nil ||
							!strings.Contains(fmt.Sprint(page.Data[0].Item["content"]), "second question") {
							t.Fatalf("items paged right after joining = %v, want the second question, the first answer and the first question", got)
						}

						stream <- " and the rest"
						close(stream)
						secondLive = append(secondLive, second.readTurn(t, threadID, nil)...)
						firstLive = append(firstLive, first.readTurn(t, threadID, nil)...)
						reply, _ := secondLive.getItem(t, "item/completed", "agentMessage")
						if reply["text"] != "partial and the rest" {
							t.Fatalf("live reply = %v, want the whole text", reply)
						}
						// 2. 检查跟随起点之前的分页 Item ID 没有出现在实时通知里。
						liveIDs := secondLive.getItemIDs(t)
						for _, id := range page.getIDs() {
							if liveIDs[id] {
								t.Fatalf("item %s was both paged and pushed live to the joining connection", id)
							}
						}
						// 轮结束之后再翻页：仍读到接入时回 resume 响应的位置，这一轮的回复只有实时推送，翻页里没有。
						var after codexWireItemPage
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor, "limit": 100, "sortDirection": "desc"}, &after)
						if !slices.Equal(after.getIDs(), page.getIDs()) {
							t.Fatalf("items paged after the turn = %v, want what was paged right after joining: %v", after.getIDs(), page.getIDs())
						}
						// 再 resume 一次（Codex TUI 切换 Thread）：位置推进到已经实时推送的内容，这一轮的回复也在翻页里，和 Codex 读 rollout 一样。
						second.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed)
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor, "limit": 100, "sortDirection": "desc"}, &after)
						if want := append([]string{fmt.Sprint(reply["id"])}, page.getIDs()...); !slices.Equal(after.getIDs(), want) {
							t.Fatalf("items paged after resuming again = %v, want the reply and everything before it: %v", after.getIDs(), want)
						}
						// 发起方 resume 之后：翻页给出全部已保存的内容，正是它实时收到的那些。
						first.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, nil)
						var starter codexWireItemPage
						first.call(t, "thread/items/list", map[string]any{"threadId": threadID, "limit": 100}, &starter)
						if got, want := slices.Sorted(slices.Values(starter.getIDs())), slices.Sorted(maps.Keys(firstLive.getItemIDs(t))); !slices.Equal(got, want) {
							t.Fatalf("items paged by the starting connection = %v, want every item it got live: %v", got, want)
						}
						// thread/read 是一次读取，不截断：这一轮的回复也在里面。
						var read codexWireThreadRead
						second.call(t, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read)
						if len(read.Thread.Turns) != 2 || len(read.Thread.Turns[1].Items) != 2 || read.Thread.Turns[1].Items[1]["text"] != "partial and the rest" {
							t.Fatalf("thread/read after the turn = %+v, want both turns with the whole reply", read.Thread.Turns)
						}
					},
				},
				{
					name: "join_while_tool_runs_then_page",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 命令运行中接入并翻页，再完成命令并检查历史游标与实时结果。
						first := f.connections.open(t)
						workspace := newE2EWorkspace(t)
						threadID := (&codexTurnFixture{
							wire:  first,
							model: f.model,
						}).startThread(t, workspace, nil).Thread.ID
						fifo := filepath.Join(workspace, "release.fifo")
						if err := syscall.Mkfifo(fifo, 0600); err != nil {
							t.Fatal(err)
						}
						requestID := first.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("wait for it")}})
						f.model.next(t).reply <- e2eModelReply{
							tool:      "bash",
							arguments: `{"command":"echo before; cat release.fifo; echo after"}`,
						}
						release := openE2EFIFOWriter(t, fifo)

						second := f.connections.open(t)
						resumeID := second.request(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
						secondLive := second.readThreadUntil(t, threadID, resumeID, isCodexMethod("turn/started"))
						var resumed codexWireThreadResume
						resumeResponse := secondLive.indexOf(isCodexResponse(resumeID))
						if resumeResponse < 0 {
							t.Fatal("thread/resume response must precede the replayed turn")
						}
						if err := json.Unmarshal(secondLive[resumeResponse].Result, &resumed); err != nil || resumed.ItemsBackwardsCursor == "" {
							t.Fatalf("thread/resume history cursor = %q, error %v", resumed.ItemsBackwardsCursor, err)
						}
						// 翻页给出全部已保存的内容：输入和还在执行的命令（inProgress）；命令不再由实时推送发 item/started。
						var page codexWireItemPage
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor}, &page)
						if got := page.getTypes(); !slices.Equal(got, []string{"userMessage", "commandExecution"}) || page.Data[1].Item["status"] != "inProgress" {
							t.Fatalf("items paged while the command runs = %v, want the user message and the running command", page.Data)
						}
						command := page.Data[1].Item

						if _, err := release.WriteString("released\n"); err != nil {
							t.Fatal(err)
						}
						if err := release.Close(); err != nil {
							t.Fatal(err)
						}
						f.model.next(t).reply <- e2eModelReply{text: "done"}
						secondLive = append(secondLive, second.readTurn(t, threadID, nil)...)
						assertCodexTurnCompleted(t, secondLive, "completed", command["id"].(string))
						assertCodexTurnCompleted(t, first.readTurn(t, threadID, requestID), "completed")
						// 实时推送照常：命令按 id 完成。
						completed, _ := secondLive.getItem(t, "item/completed", "commandExecution")
						if completed["id"] != command["id"] || completed["status"] != "completed" {
							t.Fatalf("live command = %v, want %v completed", completed, command["id"])
						}
						// 翻页仍读到接入时回 resume 响应的位置：命令的结果和回复只有实时推送，翻页里的命令还是 inProgress。
						var after codexWireItemPage
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor}, &after)
						if got := after.getTypes(); !slices.Equal(got, []string{"userMessage", "commandExecution"}) || after.Data[1].Item["status"] != "inProgress" {
							t.Fatalf("items paged after the command finished = %v, want what was paged while it ran: the user message and the running command", after.Data)
						}
						// 再 resume 一次（Codex TUI 切换 Thread）：位置推进到已经实时推送的内容，翻页给出完成的命令和回复。
						second.call(t, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &resumed)
						second.call(t, "thread/items/list", map[string]any{"threadId": threadID, "cursor": resumed.ItemsBackwardsCursor}, &after)
						if got := after.getTypes(); !slices.Equal(got, []string{"userMessage", "commandExecution", "agentMessage"}) || after.Data[1].Item["status"] != "completed" {
							t.Fatalf("items paged after resuming again = %v, want the user message, the finished command and the reply", after.Data)
						}
						// 另一条没跟随的连接翻页能读到命令的完整结果，和 thread/read 一致。
						reader := f.connections.open(t)
						var all codexWireItemPage
						reader.call(t, "thread/items/list", map[string]any{"threadId": threadID, "sortDirection": "asc"}, &all)
						if got := all.getTypes(); !slices.Equal(got, []string{"userMessage", "commandExecution", "agentMessage"}) || all.Data[1].Item["aggregatedOutput"] != "before\nreleased\nafter\n" {
							t.Fatalf("items paged by an unfollowing connection = %v, want the user message, the finished command and the reply", got)
						}
					},
				},
				{
					name: "invalid_requests",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 创建历史后用新连接提交无效游标、方向和过滤条件，核对错误。
						threadID := f.startHistoryThread(t, f.connections.open(t), 1)
						// 用另一条连接检查普通分页的无效请求，不先 resume。
						wire := f.connections.open(t)
						for _, test := range []struct {
							name    string
							method  string
							params  map[string]any
							code    int
							message string
						}{
							{
								name:    "items_cursor_not_a_number",
								method:  "thread/items/list",
								params:  map[string]any{"threadId": threadID, "cursor": "abc"},
								code:    -32602,
								message: "invalid cursor: abc",
							},
							{
								name:    "items_cursor_zero",
								method:  "thread/items/list",
								params:  map[string]any{"threadId": threadID, "cursor": "0"},
								code:    -32602,
								message: "invalid cursor: 0",
							},
							{
								name:    "items_cursor_past_the_log",
								method:  "thread/items/list",
								params:  map[string]any{"threadId": threadID, "cursor": "99999"},
								code:    -32602,
								message: "invalid cursor: 99999",
							},
							{
								name:   "items_cursor_not_a_string",
								method: "thread/items/list",
								params: map[string]any{"threadId": threadID, "cursor": 3},
								code:   -32602,
							},
							{
								name:    "items_sort_direction",
								method:  "thread/items/list",
								params:  map[string]any{"threadId": threadID, "sortDirection": "up"},
								code:    -32602,
								message: "invalid sortDirection: up",
							},
							{
								name:    "items_unknown_thread",
								method:  "thread/items/list",
								params:  map[string]any{"threadId": "missing"},
								code:    -32600,
								message: "thread not loaded: missing",
							},
							{
								name:    "turns_unknown_thread",
								method:  "thread/turns/list",
								params:  map[string]any{"threadId": "missing"},
								code:    -32600,
								message: "thread not loaded: missing",
							},
							{
								name:    "turns_sort_direction",
								method:  "thread/turns/list",
								params:  map[string]any{"threadId": threadID, "sortDirection": "up"},
								code:    -32602,
								message: "invalid sortDirection: up",
							},
							{
								name:    "list_sort_direction",
								method:  "thread/list",
								params:  map[string]any{"sortDirection": "up"},
								code:    -32602,
								message: "invalid sortDirection: up",
							},
							{
								name:    "list_both_filters",
								method:  "thread/list",
								params:  map[string]any{"sourceKinds": []string{"subAgent"}, "ancestorThreadId": threadID, "parentThreadId": threadID},
								code:    -32602,
								message: "ancestorThreadId and parentThreadId are mutually exclusive",
							},
							{
								name:    "list_unknown_ancestor",
								method:  "thread/list",
								params:  map[string]any{"sourceKinds": []string{"subAgent"}, "ancestorThreadId": "missing"},
								code:    -32600,
								message: "thread not loaded: missing",
							},
						} {
							t.Run(test.name, func(t *testing.T) {
								response := wire.response(t, wire.request(t, test.method, test.params))
								if response.Error == nil || response.Error.Code != test.code || (test.message != "" && response.Error.Message != test.message) {
									t.Fatalf("%s %v = %+v, want error %d %q", test.method, test.params, response.Error, test.code, test.message)
								}
							})
						}
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, fixture) })
			}
		})
	}
}

// historyTurnScripts 是 startHistoryThread 里每一轮假模型的输出：文本加推理、bash、纯文本、read_file、bash、apply_patch，各轮的回复是 reply-N。
var historyTurnScripts = [][]e2eModelReply{
	{{
		text:      "reply-1",
		reasoning: "thinking-1",
	}},
	{{
		tool:      "bash",
		arguments: `{"command":"echo out-2"}`,
	}, {text: "reply-2"}},
	{{text: "reply|-3"}},
	{{
		tool:      "read_file",
		arguments: `{"path":"notes.txt"}`,
	}, {text: "reply-4"}},
	{{
		tool:      "bash",
		arguments: `{"command":"echo out-5"}`,
	}, {text: "reply-5"}},
	{{
		tool:      "apply_patch",
		arguments: `{"patch":"*** Begin Patch\n*** Add File: added.txt\n+hello\n*** End Patch"}`,
	}, {text: "reply-6"}},
}

// startHistoryThread 创建 Thread 并依次运行指定轮数的历史脚本。
func (f *codexResumeFixture) startHistoryThread(t *testing.T, wire *codexWire, turns int) string {
	t.Helper()
	// 1. 新建 Thread，逐轮发送 question N 并使用对应脚本回复。
	threadID := f.startThread(t, wire)
	for index := range turns {
		f.runTurn(t, wire, threadID, fmt.Sprintf("question %d", index+1), historyTurnScripts[index]...)
	}
	return threadID
}

// runTurn 提交一轮输入并收集到完成消息。
func (f *codexResumeFixture) runTurn(t *testing.T, wire *codexWire, threadID, text string, replies ...e2eModelReply) codexTurnMessages {
	t.Helper()
	// 1. 发出 turn/start，依次回复模型请求，再检查本轮正常结束。
	requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput(text)}})
	for _, reply := range replies {
		f.model.next(t).reply <- reply
	}
	messages := wire.readTurn(t, threadID, requestID)
	assertCodexTurnCompleted(t, messages, "completed")
	return messages
}

// pageThreadItems 逐页读取全部 Item。
func pageThreadItems(t *testing.T, wire *codexWire, params map[string]any) []codexWireItemPage {
	t.Helper()
	// 1. 复制参数，读取每页并推进游标；游标不动或超过五百页时失败。
	params = maps.Clone(params)
	var pages []codexWireItemPage
	for {
		var page codexWireItemPage
		wire.call(t, "thread/items/list", params, &page)
		pages = append(pages, page)
		if page.NextCursor == nil {
			return pages
		}
		if len(pages) > 500 || params["cursor"] == *page.NextCursor {
			t.Fatalf("thread/items/list did not end: cursor %v after %d pages", *page.NextCursor, len(pages))
		}
		params["cursor"] = *page.NextCursor
	}
}

// waitThreadReadStatus 轮询 Thread 记录直到指定状态出现。
func waitThreadReadStatus(t *testing.T, wire *codexWire, threadID, statusType string) {
	t.Helper()
	// 1. 反复调用 thread/read，未到目标状态时短暂停顿，超时报告实际状态。
	deadline := time.Now().Add(e2eTimeout)
	for {
		var read codexWireThreadRead
		wire.call(t, "thread/read", map[string]any{"threadId": threadID}, &read)
		if read.Thread.Status.Type == statusType {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread/read status of %s is %q, want %q", threadID, read.Thread.Status.Type, statusType)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reversedStrings 返回倒序排列的字符串副本。
func reversedStrings(values []string) []string {
	// 1. 复制切片并反转。
	reversed := slices.Clone(values)
	slices.Reverse(reversed)
	return reversed
}

// equalItemEntries 两组条目的 TurnID 和 Item 是否逐项相同？
func equalItemEntries(got, want []codexWireItemEntry) bool {
	// 1. 逐项比较 TurnID 和 Item，不比较时间字段。
	return slices.EqualFunc(got, want, func(a, b codexWireItemEntry) bool { return a.TurnID == b.TurnID && reflect.DeepEqual(a.Item, b.Item) })
}

// getEntryIDs 返回 Item 条目中的 ID 列表。
func getEntryIDs(entries []codexWireItemEntry) []string {
	// 1. 按条目顺序读取 Item 的 id 并转成字符串。
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, fmt.Sprint(entry.Item["id"]))
	}
	return ids
}

// TestCodexConversationManagement 检查分叉、归档、删除和对应列表。
func TestCodexConversationManagement(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上运行各用例。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			fixture := &codexResumeFixture{
				connections: newCodexConnections(t, binary, config, mode),
				model:       model,
				config:      config,
			}
			// 每个用例新开自己的连接和 Thread：unix 模式是同一个 app-server 进程上的多条连接，stdio 模式是同一个 serve 上的多个进程。
			for _, test := range []struct {
				name string
				run  func(t *testing.T, f *codexResumeFixture)
			}{
				{
					name: "paseo_rewind",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 在第三轮之前分叉，核对响应字段、复制轮次、后续输入和原历史不变。
						wire := f.connections.open(t)
						sourceID := f.startHistoryThread(t, wire, 3)
						var source codexWireThreadRead
						wire.call(t, "thread/read", map[string]any{"threadId": sourceID, "includeTurns": true}, &source)
						sourceTurns := source.Thread.Turns
						// Paseo 的 rewind 照抄这些参数：在第 3 轮之前分叉，带当前的目录和模型，要完整的轮次。
						workspace := newE2EWorkspace(t)
						requestID := wire.request(t, "thread/fork", map[string]any{
							"threadId": sourceID, "beforeTurnId": sourceTurns[2].ID, "cwd": workspace, "model": "local/second", "serviceTier": nil,
							"config": map[string]any{"model_reasoning_effort": "high"}, "excludeTurns": false, "persistExtendedHistory": true,
						})
						first := wire.next(t, "thread/fork response", func(message codexWireMessage) bool {
							return isCodexResponse(requestID)(message) || message.Method == "thread/started"
						})
						var forked struct {
							codexWireThreadRead
							ReasoningEffort *string `json:"reasoningEffort"`
						}
						if first.Method != "" || first.Error != nil || json.Unmarshal(first.Result, &forked) != nil {
							t.Fatalf("first message after thread/fork = %+v (error %+v), want the response before thread/started", first, first.Error)
						}
						// Paseo 校验 thread/fork 响应时要求这些字段都在，serviceTier 可以是 null。
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(first.Result, &fields); err != nil {
							t.Fatalf("thread/fork result %s: %v", first.Result, err)
						}
						for _, key := range []string{"model", "modelProvider", "serviceTier", "cwd"} {
							if _, ok := fields[key]; !ok {
								t.Fatalf("thread/fork result %s has no %s, Paseo rejects it", first.Result, key)
							}
						}
						thread := forked.Thread
						if thread.ID == "" || thread.ID == sourceID || thread.ForkedFromID == nil || *thread.ForkedFromID != sourceID || thread.Cwd != workspace ||
							thread.Model != "local/second" || forked.ReasoningEffort == nil || *forked.ReasoningEffort != "high" {
							t.Fatalf("thread/fork = %+v reasoning %v, want a new thread forked from %s in %s on local/second high", thread, forked.ReasoningEffort, sourceID, workspace)
						}
						if len(thread.Turns) != 2 || thread.Turns[0].ID != sourceTurns[0].ID || thread.Turns[1].ID != sourceTurns[1].ID {
							t.Fatalf("forked turns = %+v, want the first two turns of the source", thread.Turns)
						}
						// 复制来的回复 From 还是来源的根 Session，仍按 ModelCallID 显示成 agentMessage。
						for index, turn := range thread.Turns {
							wantReply := fmt.Sprintf("reply-%d", index+1)
							if !slices.ContainsFunc(turn.Items, func(item map[string]any) bool { return item["type"] == "agentMessage" && item["text"] == wantReply }) {
								t.Fatalf("forked turn %d items = %v, want the agentMessage %s", index+1, turn.Items, wantReply)
							}
						}
						if started := wire.nextNotification(t, "thread/started").getNotification(t); started.Thread.ID != thread.ID {
							t.Fatalf("thread/started for %s, want the forked thread %s", started.Thread.ID, thread.ID)
						}
						messages := f.runTurn(t, wire, thread.ID, "question 3 again", e2eModelReply{text: "forked reply"})
						if reply, _ := messages.getItem(t, "item/completed", "agentMessage"); reply["text"] != "forked reply" {
							t.Fatalf("reply on the forked thread = %v, want the agentMessage forked reply", reply)
						}
						var unchanged codexWireThreadRead
						wire.call(t, "thread/read", map[string]any{"threadId": sourceID, "includeTurns": true}, &unchanged)
						if !reflect.DeepEqual(unchanged.Thread.Turns, sourceTurns) {
							t.Fatalf("source turns after the fork = %+v, want them unchanged: %+v", unchanged.Thread.Turns, sourceTurns)
						}
					},
				},
				{
					name: "items_paging_across_fork_boundary",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 运行触发压缩的两次 bash，再分叉并检查跨来源日志的工具调用与结果。
						wire := f.connections.open(t)
						sourceID := f.startThread(t, wire)
						// 第二次 bash 的输出超过压缩阈值（上下文窗口 32000 的 80%），下一次模型调用之前自动压缩；保留预算只够留下第二次调用和结果，
						// 摘要覆盖到第一次 bash 的结果。分叉从这条结果开始复制，它的调用留在来源里。
						messages := f.runTurn(t, wire, sourceID, "question 1",
							e2eModelReply{
								tool:      "bash",
								arguments: `{"command":"echo first"}`,
							},
							e2eModelReply{
								tool:      "bash",
								arguments: `{"command":"printf '%090000d' 0"}`,
							},
							e2eModelReply{text: "summary of the first command"},
							e2eModelReply{text: "done-1"},
						)
						messages.getItem(t, "item/completed", "contextCompaction")
						var forked codexWireThreadRead
						wire.call(t, "thread/fork", map[string]any{"threadId": sourceID, "excludeTurns": true}, &forked)
						forkedID := forked.Thread.ID
						log := readE2ERootSessionLog(t, f.config, forkedID)
						if len(log) < 2 || log[0].Kind != defs.ItemSessionFork || log[1].Kind != defs.ItemToolResult {
							t.Fatalf("forked root session log starts with %+v, want the continuation marker and then the first tool result", log[:min(len(log), 2)])
						}

						reader := f.connections.open(t)
						var sourceEntries, forkedEntries []codexWireItemEntry
						for _, page := range pageThreadItems(t, reader, map[string]any{"threadId": sourceID, "limit": 1, "sortDirection": "asc"}) {
							sourceEntries = append(sourceEntries, page.Data...)
						}
						for _, page := range pageThreadItems(t, reader, map[string]any{"threadId": forkedID, "limit": 1, "sortDirection": "asc"}) {
							forkedEntries = append(forkedEntries, page.Data...)
						}
						if !equalItemEntries(forkedEntries, sourceEntries) {
							t.Fatalf("forked items %v, want the items of the source %v", getEntryIDs(forkedEntries), getEntryIDs(sourceEntries))
						}
						// 跨来源与本地的一轮，用 full Turn 范围和指定 Turn 分页都能配上工具结果；再次 fork 不累加序号。
						var sourceRead codexWireThreadRead
						reader.call(t, "thread/read", map[string]any{"threadId": sourceID, "includeTurns": true}, &sourceRead)
						var secondFork codexWireThreadRead
						wire.call(t, "thread/fork", map[string]any{"threadId": forkedID, "excludeTurns": true}, &secondFork)
						for _, targetID := range []string{forkedID, secondFork.Thread.ID} {
							var turns codexWireTurnPage
							reader.call(t, "thread/turns/list", map[string]any{"threadId": targetID, "itemsView": "full", "limit": 1, "sortDirection": "asc"}, &turns)
							if len(turns.Data) != 1 || !reflect.DeepEqual(turns.Data[0], sourceRead.Thread.Turns[0]) {
								t.Fatalf("fork full range = %+v, want %+v", turns.Data, sourceRead.Thread.Turns)
							}
							var entries []codexWireItemEntry
							for _, page := range pageThreadItems(t, reader, map[string]any{"threadId": targetID, "turnId": turns.Data[0].ID, "limit": 1, "sortDirection": "desc"}) {
								entries = append(entries, page.Data...)
							}
							slices.Reverse(entries)
							if !equalItemEntries(entries, sourceEntries) {
								t.Fatalf("fork specified Turn = %v, want %v", getEntryIDs(entries), getEntryIDs(sourceEntries))
							}
						}
						var firstCommands []map[string]any
						for _, entry := range forkedEntries {
							if entry.Item["type"] == "commandExecution" && strings.Contains(fmt.Sprint(entry.Item["command"]), "echo first") {
								firstCommands = append(firstCommands, entry.Item)
							}
						}
						if len(firstCommands) != 1 || firstCommands[0]["status"] != "completed" || firstCommands[0]["aggregatedOutput"] != "first\n" {
							t.Fatalf("first bash call on the forked thread = %v, want one completed commandExecution with its output", firstCommands)
						}
					},
				},
				{
					name: "archive_unarchive",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 完成一轮后重复归档，检查列表、跟随状态和取消归档结果。
						wire := f.connections.open(t)
						workspace := newE2EWorkspace(t)
						turnFixture := &codexTurnFixture{
							wire:  wire,
							model: f.model,
						}
						threadID := turnFixture.startThread(t, workspace, nil).Thread.ID
						turnFixture.completeTextTurn(t, threadID)
						listIDs := func(params map[string]any) []string {
							var listed codexWireThreadList
							wire.call(t, "thread/list", params, &listed)
							return listed.getIDs()
						}

						// 已归档时再归档同样成功；归档不停止跟随。
						for range 2 {
							if result := callThenThreadNotification(t, wire, "thread/archive", threadID, "thread/archived"); string(result) != "{}" {
								t.Fatalf("thread/archive = %s, want {}", result)
							}
						}
						if got := listIDs(map[string]any{"cwd": workspace, "archived": true}); !slices.Equal(got, []string{threadID}) {
							t.Fatalf("archived thread/list = %v, want %s", got, threadID)
						}
						if got := listIDs(map[string]any{"cwd": workspace}); len(got) != 0 {
							t.Fatalf("thread/list = %v, want the archived thread left out", got)
						}
						if !slices.Contains(getLoadedThreads(t, wire), threadID) {
							t.Fatalf("thread/loaded/list after archiving = %v, want %s still followed", getLoadedThreads(t, wire), threadID)
						}

						var unarchived codexWireThreadRead
						if err := json.Unmarshal(callThenThreadNotification(t, wire, "thread/unarchive", threadID, "thread/unarchived"), &unarchived); err != nil {
							t.Fatal(err)
						}
						if unarchived.Thread.ID != threadID || unarchived.Thread.Cwd != workspace || unarchived.Thread.Turns == nil || len(unarchived.Thread.Turns) != 0 {
							t.Fatalf("thread/unarchive = %+v, want the thread without turns", unarchived.Thread)
						}
						if got := listIDs(map[string]any{"cwd": workspace}); !slices.Equal(got, []string{threadID}) {
							t.Fatalf("thread/list after unarchiving = %v, want %s", got, threadID)
						}
						if got := listIDs(map[string]any{"cwd": workspace, "archived": true}); len(got) != 0 {
							t.Fatalf("archived thread/list after unarchiving = %v, want none", got)
						}
					},
				},
				{
					name: "delete",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 先检查运行中不能删除，再中断并删除，核对跟随列表和后续查询错误。
						wire := f.connections.open(t)
						workspace := newE2EWorkspace(t)
						turnFixture := &codexTurnFixture{
							wire:  wire,
							model: f.model,
						}
						threadID := turnFixture.startThread(t, workspace, nil).Thread.ID
						turnFixture.completeTextTurn(t, threadID)

						// 另一条连接在 Turn 跑着时删除：serve 拒绝。
						turnID := turnFixture.startHeldTurn(t, threadID)
						other := f.connections.open(t)
						assertCodexError(t, other.response(t, other.request(t, "thread/delete", map[string]any{"threadId": threadID})), -32600, "cannot delete thread "+threadID+" while a turn is running")
						wire.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
						wire.waitThreadStatus(t, threadID, "idle", wire.readTurn(t, threadID, nil))

						// 跟随着这个 Thread 的连接删除：先停止跟随，再删除。
						if result := callThenThreadNotification(t, wire, "thread/delete", threadID, "thread/deleted"); string(result) != "{}" {
							t.Fatalf("thread/delete = %s, want {}", result)
						}
						if slices.Contains(getLoadedThreads(t, wire), threadID) {
							t.Fatalf("thread/loaded/list after deleting = %v, want %s no longer followed", getLoadedThreads(t, wire), threadID)
						}
						// thread/read 对 serve 上找不到的 Thread 回 thread not loaded，和 Codex 一致；只认根 Thread 的方法回 thread not found。
						assertCodexError(t, wire.response(t, wire.request(t, "thread/read", map[string]any{"threadId": threadID})), -32600, "thread not loaded: "+threadID)
						assertCodexError(t, wire.response(t, wire.request(t, "thread/delete", map[string]any{"threadId": threadID})), -32600, "thread not found: "+threadID)
						for _, archived := range []bool{false, true} {
							var listed codexWireThreadList
							wire.call(t, "thread/list", map[string]any{"cwd": workspace, "archived": archived}, &listed)
							if len(listed.Data) != 0 {
								t.Fatalf("thread/list archived %v after deleting = %v, want none", archived, listed.getIDs())
							}
						}
					},
				},
				{
					name: "fork_errors",
					run: func(t *testing.T, f *codexResumeFixture) {
						// 1. 创建历史并暂停下一轮，检查分叉参数、缺失轮次和临时 Thread 的错误。
						wire := f.connections.open(t)
						threadID := f.startHistoryThread(t, wire, 1)
						turnFixture := &codexTurnFixture{
							wire:  wire,
							model: f.model,
						}
						runningTurnID := turnFixture.startHeldTurn(t, threadID)
						const missingTurnID = "00000000-0000-7000-8000-000000000999"
						for _, test := range []struct {
							name    string
							params  map[string]any
							code    int
							message string
						}{
							{
								name:    "both_boundaries",
								params:  map[string]any{"threadId": threadID, "beforeTurnId": runningTurnID, "lastTurnId": missingTurnID},
								code:    -32600,
								message: "`beforeTurnId` cannot be combined with `lastTurnId`",
							},
							{
								name:    "missing_last_turn",
								params:  map[string]any{"threadId": threadID, "lastTurnId": missingTurnID},
								code:    -32600,
								message: "lastTurnId '" + missingTurnID + "' was not found in the source thread",
							},
							{
								name:    "missing_before_turn",
								params:  map[string]any{"threadId": threadID, "beforeTurnId": missingTurnID},
								code:    -32600,
								message: "beforeTurnId '" + missingTurnID + "' was not found in the source thread",
							},
							{
								name:    "missing_thread",
								params:  map[string]any{"threadId": "missing"},
								code:    -32600,
								message: "thread not found: missing",
							},
						} {
							t.Run(test.name, func(t *testing.T) {
								assertCodexError(t, wire.response(t, wire.request(t, "thread/fork", test.params)), test.code, test.message)
							})
						}
						wire.call(t, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": runningTurnID}, nil)
						wire.readTurn(t, threadID, nil)
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, fixture) })
			}
		})
	}
}

// callThenThreadNotification 调用 Thread 方法并检查响应先于通知。
func callThenThreadNotification(t *testing.T, wire *codexWire, method, threadID, notification string) json.RawMessage {
	t.Helper()
	// 1. 发起请求，核对首条匹配消息是成功响应，再读取指定通知。
	requestID := wire.request(t, method, map[string]any{"threadId": threadID})
	first := wire.next(t, method+" response", func(message codexWireMessage) bool {
		return isCodexResponse(requestID)(message) || (message.Method == notification && message.getThreadID() == threadID)
	})
	if first.Method != "" || first.Error != nil {
		t.Fatalf("first message after %s = %+v (error %+v), want the response before %s", method, first, first.Error, notification)
	}
	wire.nextThreadNotification(t, threadID, notification)
	return first.Result
}

// readE2ERootSessionLog 读取根 Session 的原始日志 Item。
func readE2ERootSessionLog(t *testing.T, config, conversationID string) []*defs.Item {
	t.Helper()
	// 1. 查找对话目录下非 metadata 的唯一日志，逐行解码 payload。
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(config), ".local", "share", "openvaala", "conversations", conversationID, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	paths = slices.DeleteFunc(paths, func(path string) bool { return filepath.Base(path) == "metadata.jsonl" })
	if len(paths) != 1 {
		t.Fatalf("session logs of %s = %v, want only the root session", conversationID, paths)
	}
	payload, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var items []*defs.Item
	for line := range strings.SplitSeq(strings.TrimSpace(string(payload)), "\n") {
		var entry struct {
			Payload *defs.Item `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		items = append(items, entry.Payload)
	}
	return items
}

// TestCodexSubAgents 检查子 Thread 的发现、通知、历史和只读操作。
func TestCodexSubAgents(t *testing.T) {
	// 1. 编译程序，分别在 stdio 和 Unix 传输上跑一遍子 Agent 剧本，再逐用例检查它留下的结果。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			f := runSubAgentScenario(t, newCodexConnections(t, binary, config, mode), model)
			// 各用例检查同一段剧本（runSubAgentScenario）留下的结果，需要新连接的用例各自新开。
			for _, test := range []struct {
				name string
				run  func(t *testing.T, f *codexSubAgentFixture)
			}{
				{
					name: "child_thread_started_first",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 检查三条跟随了子 Thread 的连接（自动跟随、空闲变在跑、打开时在跑）上，子 Thread 的第一条消息都是它唯一的 thread/started，
						// 再检查派生信息与时间。
						for name, wire := range map[string]*codexWire{"spawning": f.parent, "idle_at_open": f.idleJoiner, "running_at_open": f.runningJoiner} {
							isChildMessage := func(message codexWireMessage) bool { return message.Method != "" && message.getThreadID() == f.childID }
							first := slices.IndexFunc(wire.arrivals, isChildMessage)
							started := codexTurnMessages(wire.arrivals).count(func(message codexWireMessage) bool {
								return isChildMessage(message) && message.Method == "thread/started"
							})
							if first < 0 || wire.arrivals[first].Method != "thread/started" || started != 1 {
								t.Fatalf("%s connection: first sub-agent message at %d, %d thread/started; want exactly one thread/started before the rest", name, first, started)
							}
						}
						thread := f.childStarted
						var source codexWireThreadSpawn
						if err := json.Unmarshal(thread.Source, &source); err != nil {
							t.Fatal(err)
						}
						spawn := source.SubAgent.ThreadSpawn
						if thread.SessionID != f.childID || spawn.ParentThreadID != f.rootID || spawn.Depth != 1 || spawn.AgentNickname != "review" || spawn.AgentRole != "assistant" ||
							thread.ParentThreadID == nil || *thread.ParentThreadID != f.rootID || thread.AgentNickname == nil || *thread.AgentNickname != "review" ||
							thread.AgentRole == nil || *thread.AgentRole != "assistant" || thread.CanAcceptDirectInput || thread.Model != "local/default" ||
							thread.CreatedAt == 0 || thread.UpdatedAt < thread.CreatedAt {
							t.Fatalf("sub-agent thread/started = %+v with source %s, want the review task spawned by %s with its times", thread, thread.Source, f.rootID)
						}
					},
				},
				{
					name: "parent_activities",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 按到达顺序检查父 Thread 上的子 Agent 活动：依次是 spawn 的 started、子 Agent 结束的 completed、send_message 的 interacted、interrupt_agent 的 interrupted、
						// 被中断那一轮的 completed；都指向子 Thread，路径是任务名（发现子 Session 后路径才取得到）；每条都先 item/started 再 item/completed。
						var kinds []string
						startedAt := map[string]int{}
						for index, message := range f.parent.arrivals {
							notification := message.getNotification(t)
							if message.getThreadID() != f.rootID || notification.Item["type"] != "subAgentActivity" {
								continue
							}
							itemID := fmt.Sprint(notification.Item["id"])
							if message.Method == "item/started" {
								startedAt[itemID] = index
								continue
							}
							if started, found := startedAt[itemID]; !found || started > index || notification.Item["agentThreadId"] != f.childID || notification.Item["agentPath"] != "/root/review" {
								t.Fatalf("subAgentActivity %v completed at %d (started at %d, %v), want it started first, on %s at /root/review", notification.Item, index, started, found, f.childID)
							}
							kinds = append(kinds, fmt.Sprint(notification.Item["kind"]))
						}
						if want := []string{"started", "completed", "interacted", "interrupted", "completed"}; !slices.Equal(kinds, want) {
							t.Fatalf("parent subAgentActivity kinds = %v, want %v", kinds, want)
						}
					},
				},
				{
					name: "child_stream",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 检查子 Thread 首轮的回复按增量推出、拼起来就是子 Agent 的回复，send_message 开始的第二轮在各连接上都是中断。
						assertCodexTurnCompleted(t, f.childFirstTurn, "completed")
						reply, _ := f.childFirstTurn.getItem(t, "item/completed", "agentMessage")
						if text := f.childFirstTurn.getDeltaText(t, "item/agentMessage/delta", fmt.Sprint(reply["id"])); text != "child reply" || reply["text"] != "child reply" {
							t.Fatalf("sub-agent reply %v with deltas %q, want child reply", reply, text)
						}
						for _, secondTurn := range []codexTurnMessages{f.childSecondTurn, f.idleJoinerChildTurn, f.runningJoinerChildTurn} {
							assertCodexTurnCompleted(t, secondTurn, "interrupted")
						}
					},
				},
				{
					name: "loaded_threads",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 比较派生、空闲接入、再次运行和运行中接入时的跟随列表。
						both := slices.Sorted(slices.Values([]string{f.rootID, f.childID}))
						for _, tt := range []struct {
							name string
							got  []string
							want []string
						}{
							{
								name: "spawning_connection",
								got:  f.parentLoaded,
								want: both,
							},
							{
								name: "idle_at_open",
								got:  f.idleJoinerLoadedAtOpen,
								want: []string{f.rootID},
							},
							{
								name: "idle_then_running",
								got:  f.idleJoinerLoadedAfterRunning,
								want: both,
							},
							{
								name: "running_at_open",
								got:  f.runningJoinerLoaded,
								want: both,
							},
						} {
							if !slices.Equal(tt.got, tt.want) {
								t.Errorf("%s: thread/loaded/list = %v, want %v", tt.name, tt.got, tt.want)
							}
						}
					},
				},
				{
					name: "sub_agent_list",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 跟随着子 Thread 的连接上 status 是它的运行状态。第二轮被中断后的 idle 来自对话订阅，和这一轮的 turn/completed
						// 先后不固定：先等到第二轮开始之后的 idle，再按祖先和父 Thread 两种条件查询列表。
						f.parent.waitThreadStatus(t, f.childID, "idle", f.childSecondTurn[f.childSecondTurn.indexOf(isCodexMethod("turn/started")):])
						for _, filter := range []string{"ancestorThreadId", "parentThreadId"} {
							var listed codexWireThreadList
							f.parent.call(t, "thread/list", map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}, filter: f.rootID}, &listed)
							if !slices.Equal(listed.getIDs(), []string{f.childID}) || listed.Data[0].Status.Type != "idle" || listed.Data[0].AgentNickname == nil || *listed.Data[0].AgentNickname != "review" {
								t.Fatalf("thread/list by %s = %+v, want the idle review sub-agent", filter, listed.Data)
							}
						}
					},
				},
				{
					name: "history_after_fork",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 打开了根 Thread、没有跟随子 Thread 的连接：检查读到子 Thread 自己的全部内容，没有 fork_turns 复制来的根 Thread 第一轮；
						// 状态是子 Session 的运行状态（空闲），不是 notLoaded（Codex TUI 把 notLoaded 的子 Agent 当成已关闭，不让切进去）。
						wire := f.connections.open(t)
						wire.call(t, "thread/resume", map[string]any{"threadId": f.rootID, "excludeTurns": true}, nil)
						var read codexWireThreadRead
						wire.call(t, "thread/read", map[string]any{"threadId": f.childID, "includeTurns": true}, &read)
						var childTurnIDs []string
						for _, turn := range read.Thread.Turns {
							childTurnIDs = append(childTurnIDs, turn.ID)
							for _, item := range turn.Items {
								if f.rootFirstTurnItemIDs[fmt.Sprint(item["id"])] {
									t.Fatalf("sub-agent turn %s has the forked item %v", turn.ID, item)
								}
							}
						}
						if read.Thread.ID != f.childID || read.Thread.Status.Type != "idle" || len(read.Thread.Turns) != 2 ||
							read.Thread.Turns[0].Status != "completed" || read.Thread.Turns[1].Status != "interrupted" {
							t.Fatalf("thread/read of the sub-agent = %+v, want its two turns, idle", read.Thread)
						}
						for _, direction := range []string{"asc", "desc"} {
							var entries []codexWireItemEntry
							for _, page := range pageThreadItems(t, wire, map[string]any{"threadId": f.childID, "sortDirection": direction, "limit": 1}) {
								entries = append(entries, page.Data...)
							}
							hasReply := slices.ContainsFunc(entries, func(entry codexWireItemEntry) bool { return entry.Item["text"] == "child reply" })
							for _, entry := range entries {
								if f.rootFirstTurnItemIDs[fmt.Sprint(entry.Item["id"])] || !slices.Contains(childTurnIDs, entry.TurnID) {
									t.Fatalf("%s thread/items/list of the sub-agent has %+v, want only items of its own turns %v", direction, entry, childTurnIDs)
								}
							}
							if !hasReply {
								t.Fatalf("%s thread/items/list of the sub-agent = %+v, want the child reply", direction, entries)
							}
						}
						var listed codexWireThreadList
						wire.call(t, "thread/list", map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}, "ancestorThreadId": f.rootID}, &listed)
						if !slices.Equal(listed.getIDs(), []string{f.childID}) || listed.Data[0].Status.Type != "idle" {
							t.Fatalf("thread/list on a connection not following the sub-agent = %+v, want it idle", listed.Data)
						}
						// thread/loaded/list 仍只列本连接跟随着的 Thread。
						if loaded := getLoadedThreads(t, wire); !slices.Equal(loaded, []string{f.rootID}) {
							t.Fatalf("thread/loaded/list = %v, want only the followed root", loaded)
						}
					},
				},
				{
					name: "followed_sub_agent_history_without_resume",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 自动跟随的子 Thread 已经实时收到过子 Agent 的回复：不先 resume，直接翻页检查仍能读到它。
						for _, test := range []struct {
							name string
							wire *codexWire
						}{
							{
								name: "spawning_connection",
								wire: f.parent,
							},
							{
								name: "idle_then_running",
								wire: f.idleJoiner,
							},
							{
								name: "running_at_open",
								wire: f.runningJoiner,
							},
						} {
							t.Run(test.name, func(t *testing.T) {
								var read codexWireThreadRead
								test.wire.call(t, "thread/read", map[string]any{"threadId": f.childID, "includeTurns": false}, &read)
								for _, direction := range []string{"asc", "desc"} {
									var turns codexWireTurnPage
									test.wire.call(t, "thread/turns/list", map[string]any{"threadId": f.childID, "itemsView": "full", "sortDirection": direction}, &turns)
									hasReply := slices.ContainsFunc(turns.Data, func(turn codexWireHistoryTurn) bool {
										return slices.ContainsFunc(turn.Items, func(item map[string]any) bool { return item["text"] == "child reply" })
									})
									if !hasReply {
										t.Errorf("%s thread/turns/list = %+v, want the child reply", direction, turns.Data)
									}
									var entries []codexWireItemEntry
									for _, page := range pageThreadItems(t, test.wire, map[string]any{"threadId": f.childID, "sortDirection": direction, "limit": 1}) {
										entries = append(entries, page.Data...)
									}
									if !slices.ContainsFunc(entries, func(entry codexWireItemEntry) bool { return entry.Item["text"] == "child reply" }) {
										t.Errorf("%s thread/items/list = %+v, want the child reply", direction, entries)
									}
								}
							})
						}
					},
				},
				{
					name: "historic_sub_agent_follows_on_resume",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 打开根 Thread 时早已空闲的子 Agent 不自动跟随，对它 thread/resume 之后才跟随；再退订根 Thread。
						wire := f.connections.open(t)
						wire.call(t, "thread/resume", map[string]any{"threadId": f.rootID, "excludeTurns": true}, nil)
						var loaded codexWireLoadedList
						wire.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if !slices.Equal(loaded.Data, []string{f.rootID}) {
							t.Fatalf("thread/loaded/list after resuming the root = %v, want only the root", loaded.Data)
						}
						var resumed codexWireThreadRead
						wire.call(t, "thread/resume", map[string]any{"threadId": f.childID}, &resumed)
						if resumed.Thread.ID != f.childID || resumed.Thread.Status.Type != "idle" || resumed.Thread.CanAcceptDirectInput || len(resumed.Thread.Turns) != 2 ||
							resumed.Thread.ParentThreadID == nil || *resumed.Thread.ParentThreadID != f.rootID {
							t.Fatalf("thread/resume of the sub-agent = %+v, want the idle sub-agent with its two turns", resumed.Thread)
						}
						wire.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if want := slices.Sorted(slices.Values([]string{f.rootID, f.childID})); !slices.Equal(loaded.Data, want) {
							t.Fatalf("thread/loaded/list after resuming the sub-agent = %v, want %v", loaded.Data, want)
						}
						// 退订根 Thread 时，本连接跟随的同一对话里的子 Thread 一起停止。
						var unsubscribed struct {
							Status string `json:"status"`
						}
						wire.call(t, "thread/unsubscribe", map[string]any{"threadId": f.rootID}, &unsubscribed)
						wire.call(t, "thread/loaded/list", map[string]any{}, &loaded)
						if unsubscribed.Status != "unsubscribed" || len(loaded.Data) != 0 {
							t.Fatalf("thread/unsubscribe of the root = %+v, then thread/loaded/list = %v, want both threads stopped", unsubscribed, loaded.Data)
						}
					},
				},
				{
					name: "unopened_connection_finds_child_thread",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 没有打开过根 Thread 的连接照 Codex app 的顺序找子 Agent：先按 parentThreadId 逐层列，再直接读子 Thread。
						// 认不出的 id 按 SessionID 去 serve 找，检查子 Thread 能读、翻页、resume；子 Thread 只读，不接受直接输入。
						wire := f.connections.open(t)
						startChildTurn := func() codexWireMessage {
							return wire.response(t, wire.request(t, "turn/start", map[string]any{"threadId": f.childID, "input": []any{codexTextInput("hi")}}))
						}
						// 还没认出子 Thread 时 turn/start 回 thread not loaded：只有读历史的请求才去 serve 找。
						assertCodexError(t, startChildTurn(), -32600, "thread not loaded: "+f.childID)
						var listed codexWireThreadList
						wire.call(t, "thread/list", map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}, "parentThreadId": f.rootID}, &listed)
						if !slices.Equal(listed.getIDs(), []string{f.childID}) {
							t.Fatalf("thread/list by the root = %+v, want the review sub-agent", listed.Data)
						}
						wire.call(t, "thread/list", map[string]any{"sourceKinds": []string{"subAgentThreadSpawn"}, "parentThreadId": f.childID}, &listed)
						if listed.Data == nil || len(listed.Data) != 0 {
							t.Fatalf("thread/list by the sub-agent = %+v, want an empty list", listed.Data)
						}
						hasChildReply := func(items []map[string]any) bool {
							return slices.ContainsFunc(items, func(item map[string]any) bool { return item["type"] == "agentMessage" && item["text"] == "child reply" })
						}
						var read codexWireThreadRead
						wire.call(t, "thread/read", map[string]any{"threadId": f.childID, "includeTurns": true}, &read)
						if read.Thread.ID != f.childID || read.Thread.CanAcceptDirectInput || len(read.Thread.Turns) != 2 || !hasChildReply(read.Thread.Turns[0].Items) {
							t.Fatalf("thread/read of the sub-agent = %+v, want its two turns with the child reply", read.Thread)
						}
						var turns codexWireTurnPage
						wire.call(t, "thread/turns/list", map[string]any{"threadId": f.childID, "itemsView": "full", "sortDirection": "asc"}, &turns)
						if len(turns.Data) != 2 || !hasChildReply(turns.Data[0].Items) {
							t.Fatalf("thread/turns/list of the sub-agent = %+v, want its two turns with the child reply", turns.Data)
						}
						var items []map[string]any
						for _, page := range pageThreadItems(t, wire, map[string]any{"threadId": f.childID}) {
							for _, entry := range page.Data {
								items = append(items, entry.Item)
							}
						}
						if !hasChildReply(items) {
							t.Fatalf("thread/items/list of the sub-agent = %v, want the child reply", items)
						}
						var resumed codexWireThreadRead
						wire.call(t, "thread/resume", map[string]any{"threadId": f.childID, "excludeTurns": true}, &resumed)
						if resumed.Thread.ID != f.childID || resumed.Thread.Status.Type != "idle" || resumed.Thread.CanAcceptDirectInput {
							t.Fatalf("thread/resume of the sub-agent = %+v, want the idle sub-agent", resumed.Thread)
						}
						if loaded := getLoadedThreads(t, wire); !slices.Equal(loaded, []string{f.childID}) {
							t.Fatalf("thread/loaded/list after resuming the sub-agent = %v, want only the sub-agent", loaded)
						}
						// resume 之后和跟随根 Thread 时认出的子 Thread 一样拒绝直接输入，和 Codex 对 v2 子 Agent 一致。
						assertCodexError(t, startChildTurn(), -32600, "direct app-server input is not allowed for multi-agent v2 sub-agents")
						// serve 上也找不到的 id 仍回 thread not loaded。
						for _, method := range []string{"thread/read", "thread/resume", "thread/turns/list", "thread/items/list"} {
							assertCodexError(t, wire.response(t, wire.request(t, method, map[string]any{"threadId": "missing"})), -32600, "thread not loaded: missing")
						}
					},
				},
				{
					name: "direct_input_rejected",
					run: func(t *testing.T, f *codexSubAgentFixture) {
						// 1. 子 Thread 只读，外部输入只进根 Session：向子 Thread 请求开轮、插话、中断和压缩，逐项检查报错。
						for _, request := range []struct {
							method string
							params map[string]any
						}{
							{
								method: "turn/start",
								params: map[string]any{"threadId": f.childID, "input": []any{codexTextInput("hi")}},
							},
							{
								method: "turn/steer",
								params: map[string]any{"threadId": f.childID, "expectedTurnId": f.childID, "input": []any{codexTextInput("hi")}},
							},
							{
								method: "turn/interrupt",
								params: map[string]any{"threadId": f.childID, "turnId": f.childID},
							},
							{
								method: "thread/compact/start",
								params: map[string]any{"threadId": f.childID},
							},
						} {
							assertCodexError(t, f.parent.response(t, f.parent.request(t, request.method, request.params)), -32600, "direct app-server input is not allowed for multi-agent v2 sub-agents")
						}
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, f) })
			}
		})
	}
}

// TestCodexSubAgentParentActivity 检查子 Agent 向父 Agent 发消息时的根 Thread 身份与历史。
func TestCodexSubAgentParentActivity(t *testing.T) {
	// 1. 分别启动 stdio 和 Unix app-server，从模型请求取得根 Session 身份。
	binary := buildOpenVaala(t)
	for _, mode := range []string{"stdio", "unix"} {
		t.Run(mode, func(t *testing.T) {
			model := newE2EModel(t)
			connections := newCodexConnections(t, binary, writeE2EConfig(t, model.server.URL), mode)
			wire := connections.open(t)
			fixture := &codexResumeFixture{connections: connections, model: model}
			rootID := fixture.startThread(t, wire)
			requestID := wire.request(t, "turn/start", map[string]any{"threadId": rootID, "input": []any{codexTextInput("delegate")}})
			root := model.next(t)
			rootSessionID := root.header.Get("Session-Id")
			if rootSessionID == "" || rootSessionID == rootID {
				t.Fatalf("root Session-Id = %q, want a session distinct from thread %s", rootSessionID, rootID)
			}
			root.reply <- e2eModelReply{tool: "spawn_agent", arguments: `{"task_name":"review","message":"check"}`}
			afterSpawn, child := model.nextRootAndTask(t, "review")
			childID := child.header.Get("Session-Id")
			if childID == "" || childID == rootSessionID {
				t.Fatalf("child Session-Id = %q, want a distinct child session", childID)
			}

			// 2. 保持根 Agent 在运行，让子 Agent 用根 Session ID 发送 steer 消息并完成。
			rootReply := make(chan string, 1)
			afterSpawn.reply <- e2eModelReply{stream: rootReply}
			child.reply <- e2eModelReply{
				tool:      "send_message",
				arguments: fmt.Sprintf(`{"session_id":%q,"message":"review update","mode":"steer"}`, rootSessionID),
			}
			afterSend := model.next(t)
			if got := afterSend.header.Get("Session-Id"); got != childID {
				t.Fatalf("request after child send_message belongs to %q, want child %s", got, childID)
			}
			afterSend.reply <- e2eModelReply{text: "child done"}
			childMessages := wire.readTurn(t, childID, nil)
			assertCodexTurnCompleted(t, childMessages, "completed")
			rootReply <- "root done"
			close(rootReply)
			model.replyAll(t, e2eModelReply{text: "received"})
			rootMessages := wire.readTurn(t, rootID, requestID)
			assertCodexTurnCompleted(t, rootMessages, "completed")
			wire.waitThreadStatus(t, childID, "idle", childMessages)
			wire.waitThreadStatus(t, rootID, "idle", rootMessages)

			// 3. 收集实时活动与三种历史查询中的同一个向父消息 Item。
			activity := childMessages.getItemWhere(t, "item/completed", func(item map[string]any) bool {
				return item["type"] == "subAgentActivity" && item["kind"] == "interacted"
			})
			activityID, ok := activity["id"].(string)
			if !ok || activityID == "" {
				t.Fatalf("parent activity id = %v, want a non-empty item id", activity["id"])
			}
			started := childMessages.getItemWhere(t, "item/started", func(item map[string]any) bool { return item["id"] == activityID })
			var read codexWireThreadRead
			wire.call(t, "thread/read", map[string]any{"threadId": childID, "includeTurns": true}, &read)
			var turns codexWireTurnPage
			wire.call(t, "thread/turns/list", map[string]any{"threadId": childID, "itemsView": "full"}, &turns)
			var readItems, turnItems, pagedItems []map[string]any
			for _, turn := range read.Thread.Turns {
				readItems = append(readItems, turn.Items...)
			}
			for _, turn := range turns.Data {
				turnItems = append(turnItems, turn.Items...)
			}
			for _, page := range pageThreadItems(t, wire, map[string]any{"threadId": childID, "limit": 1}) {
				for _, entry := range page.Data {
					pagedItems = append(pagedItems, entry.Item)
				}
			}

			// 4. 每个入口只返回一次该活动，目标都是根 Conversation，而不是内部根 Session。
			for _, test := range []struct {
				name  string
				items []map[string]any
			}{
				{name: "item_started", items: []map[string]any{started}},
				{name: "item_completed", items: []map[string]any{activity}},
				{name: "thread_read", items: readItems},
				{name: "thread_turns_list", items: turnItems},
				{name: "thread_items_list", items: pagedItems},
			} {
				t.Run(test.name, func(t *testing.T) {
					count := 0
					for _, item := range test.items {
						if item["id"] != activityID {
							continue
						}
						count++
						if item["agentThreadId"] != rootID || item["agentThreadId"] == rootSessionID || !reflect.DeepEqual(item, activity) {
							t.Errorf("parent activity = %v, want the live activity targeting root thread %s, not session %s", item, rootID, rootSessionID)
						}
					}
					if count != 1 {
						t.Fatalf("parent activity count = %d, want 1", count)
					}
				})
			}
		})
	}
}

// codexSubAgentFixture 是 runSubAgentScenario 跑完的结果：三条跟随了子 Thread 的连接、它们读到的消息和 thread/loaded/list。
type codexSubAgentFixture struct {
	connections *codexConnections
	// parent 是发起 spawn 的连接；idleJoiner 在子 Agent 空闲时打开根 Thread，子 Agent 再次在跑后自动跟随；runningJoiner 在子 Agent 在跑时打开根 Thread。
	parent, idleJoiner, runningJoiner *codexWire
	rootID, childID                   string
	// rootFirstTurnItemIDs 是根 Thread 第一轮的 Item，spawn 时 fork_turns 为 all，子 Session 日志开头复制了它们。
	rootFirstTurnItemIDs map[string]bool
	// childStarted 是 parent 收到的子 Thread 的 thread/started 里的 Thread 对象。
	childStarted codexWireThread
	// childFirstTurn、childSecondTurn 是 parent 上子 Thread 两轮的消息；idleJoinerChildTurn、runningJoinerChildTurn 是另外两条连接上
	// 子 Thread 第二轮的消息。
	childFirstTurn, childSecondTurn, idleJoinerChildTurn, runningJoinerChildTurn codexTurnMessages
	// 各连接在剧本里各个时刻的 thread/loaded/list。
	parentLoaded, idleJoinerLoadedAtOpen, idleJoinerLoadedAfterRunning, runningJoinerLoaded []string
}

// runSubAgentScenario 运行子 Agent 派生、消息追加、中断与多连接接入。
func runSubAgentScenario(t *testing.T, connections *codexConnections, model *e2eModel) *codexSubAgentFixture {
	t.Helper()
	// 1. 创建根 Thread，完成首轮（输入 hello、回复 hi）并记录这一轮的 Item ID。
	f := &codexSubAgentFixture{
		connections: connections,
		parent:      connections.open(t),
	}
	history := &codexResumeFixture{
		connections: connections,
		model:       model,
	}
	f.rootID = history.startThread(t, f.parent)
	f.rootFirstTurnItemIDs = history.runTurn(t, f.parent, f.rootID, "hello", e2eModelReply{text: "hi"}).getItemIDs(t)

	// 2. 派生 review 并复制全部历史，暂停子回复，结束根 Agent 本轮。
	spawnRequestID := f.parent.request(t, "turn/start", map[string]any{"threadId": f.rootID, "input": []any{codexTextInput("delegate")}})
	model.next(t).reply <- e2eModelReply{
		tool:      "spawn_agent",
		arguments: `{"task_name":"review","message":"check","fork_turns":"all"}`,
	}
	afterSpawn, childFirstRequest := model.nextRootAndTask(t, "review")
	childReply := make(chan string)
	childFirstRequest.reply <- e2eModelReply{stream: childReply}
	afterSpawn.reply <- e2eModelReply{text: "delegated"}
	assertCodexTurnCompleted(t, f.parent.readTurn(t, f.rootID, spawnRequestID), "completed")
	// 3. 读取子 Thread 启动通知和跟随列表，再分两段完成子回复。
	var started struct {
		Thread codexWireThread `json:"thread"`
	}
	if err := json.Unmarshal(f.parent.nextNotification(t, "thread/started").Params, &started); err != nil {
		t.Fatal(err)
	}
	f.childStarted, f.childID = started.Thread, started.Thread.ID
	f.parentLoaded = getLoadedThreads(t, f.parent)
	childReply <- "child "
	childReply <- "reply"
	close(childReply)
	f.childFirstTurn = f.parent.readTurn(t, f.childID, nil)

	// 4. 根 Agent 收到子结果后，让空闲观察者接入，再发送消息开启子 Agent 第二轮。
	notified := model.next(t)
	f.idleJoiner = connections.open(t)
	f.idleJoiner.call(t, "thread/resume", map[string]any{"threadId": f.rootID, "excludeTurns": true}, nil)
	f.idleJoinerLoadedAtOpen = getLoadedThreads(t, f.idleJoiner)
	notified.reply <- e2eModelReply{
		tool:      "send_message",
		arguments: fmt.Sprintf(`{"session_id":%q,"message":"more"}`, f.childID),
	}
	afterSend, childSecondRequest := model.nextRootAndTask(t, "review")
	childSecondRequest.reply <- e2eModelReply{hold: true}
	f.idleJoiner.nextThreadNotification(t, f.childID, "thread/started")
	f.idleJoinerLoadedAfterRunning = getLoadedThreads(t, f.idleJoiner)
	// 5. 子 Agent 第二轮暂停时接入另一观察者，再由根 Agent 中断子 Agent。
	f.runningJoiner = connections.open(t)
	f.runningJoiner.call(t, "thread/resume", map[string]any{"threadId": f.rootID, "excludeTurns": true}, nil)
	f.runningJoiner.nextThreadNotification(t, f.childID, "thread/started")
	f.runningJoinerLoaded = getLoadedThreads(t, f.runningJoiner)
	afterSend.reply <- e2eModelReply{
		tool:      "interrupt_agent",
		arguments: fmt.Sprintf(`{"session_id":%q}`, f.childID),
	}
	model.replyAll(t, e2eModelReply{text: "ok"})

	// 6. 收齐两次子 Agent 完成活动和各连接的第二轮中断消息。
	completedActivities := 0
	f.parent.readThreadUntil(t, f.rootID, nil, func(message codexWireMessage) bool {
		if item := message.getNotification(t).Item; message.Method == "item/completed" && item["type"] == "subAgentActivity" && item["kind"] == "completed" {
			completedActivities++
		}
		return completedActivities == 2
	})
	f.parent.readTurn(t, f.rootID, nil)
	f.childSecondTurn = f.parent.readTurn(t, f.childID, nil)
	f.runningJoinerChildTurn = f.runningJoiner.readTurn(t, f.childID, nil)
	f.idleJoinerChildTurn = f.idleJoiner.readTurn(t, f.childID, nil)
	return f
}

// getLoadedThreads 查询连接跟随的全部 Thread ID。
func getLoadedThreads(t *testing.T, wire *codexWire) []string {
	t.Helper()
	// 1. 调用 thread/loaded/list 并返回 Data。
	var loaded codexWireLoadedList
	wire.call(t, "thread/loaded/list", map[string]any{}, &loaded)
	return loaded.Data
}

// codexWireThreadRead 是 thread/read 结果里测试关心的字段。
type codexWireThreadRead struct {
	Thread codexWireThread `json:"thread"`
}

// codexWireThread 是 thread/read、thread/list 里的 Thread 对象里测试关心的字段。
type codexWireThread struct {
	ID            string  `json:"id"`
	SessionID     string  `json:"sessionId"`
	Name          *string `json:"name"`
	Preview       string  `json:"preview"`
	Cwd           string  `json:"cwd"`
	ModelProvider string  `json:"modelProvider"`
	Model         string  `json:"model"`
	Status        struct {
		Type string `json:"type"`
	} `json:"status"`
	Source      json.RawMessage        `json:"source"`
	HistoryMode string                 `json:"historyMode"`
	Turns       []codexWireHistoryTurn `json:"turns"`
	CreatedAt   int64                  `json:"createdAt"`
	UpdatedAt   int64                  `json:"updatedAt"`
	// ParentThreadID、AgentNickname、AgentRole 只有子 Thread 有；CanAcceptDirectInput 子 Thread 为 false。
	ParentThreadID       *string `json:"parentThreadId"`
	AgentNickname        *string `json:"agentNickname"`
	AgentRole            *string `json:"agentRole"`
	CanAcceptDirectInput bool    `json:"canAcceptDirectInput"`
	// ForkedFromID 是分叉来源的对话，不是分叉出来的为 null。
	ForkedFromID *string `json:"forkedFromId"`
}

// codexWireThreadSpawn 是子 Thread 的 source（{"subAgent":{"thread_spawn":{...}}}）里测试关心的字段。
type codexWireThreadSpawn struct {
	SubAgent struct {
		ThreadSpawn struct {
			ParentThreadID string `json:"parent_thread_id"`
			Depth          int    `json:"depth"`
			AgentNickname  string `json:"agent_nickname"`
			AgentRole      string `json:"agent_role"`
		} `json:"thread_spawn"`
	} `json:"subAgent"`
}

// codexWireHistoryTurn 是 thread/read、thread/turns/list 里的 Turn 对象。
type codexWireHistoryTurn struct {
	ID        string           `json:"id"`
	Status    string           `json:"status"`
	ItemsView string           `json:"itemsView"`
	Items     []map[string]any `json:"items"`
}

// codexWireTurnPage 是 thread/turns/list 的结果。
type codexWireTurnPage struct {
	Data            []codexWireHistoryTurn `json:"data"`
	NextCursor      *string                `json:"nextCursor"`
	BackwardsCursor *string                `json:"backwardsCursor"`
}

// codexWireItemPage 是 thread/items/list 的结果。
type codexWireItemPage struct {
	Data            []codexWireItemEntry `json:"data"`
	NextCursor      *string              `json:"nextCursor"`
	BackwardsCursor *string              `json:"backwardsCursor"`
}

// codexWireItemEntry 是 thread/items/list 的一项：Item 和它所在的 Turn，加上开始和结束的保存时间。
type codexWireItemEntry struct {
	TurnID        string         `json:"turnId"`
	Item          map[string]any `json:"item"`
	StartedAtMs   *int64         `json:"startedAtMs"`
	CompletedAtMs *int64         `json:"completedAtMs"`
}

// codexWireThreadList 是 thread/list 的结果。
type codexWireThreadList struct {
	Data            []codexWireThread `json:"data"`
	NextCursor      *string           `json:"nextCursor"`
	BackwardsCursor *string           `json:"backwardsCursor"`
}

// getIDs 返回 Thread 列表中的 ID。
func (l codexWireThreadList) getIDs() []string {
	// 1. 按列表顺序收集 Thread ID。
	ids := make([]string, 0, len(l.Data))
	for _, thread := range l.Data {
		ids = append(ids, thread.ID)
	}
	return ids
}

// getIDs 返回 Item 页中的 ID。
func (p codexWireItemPage) getIDs() []string {
	// 1. 从 Data 提取 Item ID。
	return getEntryIDs(p.Data)
}

// getTypes 返回 Item 页中的类型。
func (p codexWireItemPage) getTypes() []string {
	// 1. 按页内顺序读取 Item 的 type。
	types := make([]string, 0, len(p.Data))
	for _, entry := range p.Data {
		types = append(types, fmt.Sprint(entry.Item["type"]))
	}
	return types
}

// getItemIDs 收集通知和增量涉及的 Item ID。
func (messages codexTurnMessages) getItemIDs(t *testing.T) map[string]bool {
	t.Helper()
	// 1. 逐条解码通知，将 Item.id 和非空 itemId 加入集合。
	ids := map[string]bool{}
	for _, message := range messages {
		notification := message.getNotification(t)
		if id, isItem := notification.Item["id"].(string); isItem {
			ids[id] = true
		}
		if notification.ItemID != "" {
			ids[notification.ItemID] = true
		}
	}
	return ids
}

// ── Codex 通知读取 ──

// codexWireNotification 是测试读取的通知参数，字段是各种通知的并集。
type codexWireNotification struct {
	ThreadID string         `json:"threadId"`
	ItemID   string         `json:"itemId"`
	Delta    string         `json:"delta"`
	Item     map[string]any `json:"item"`
	Turn     codexWireTurn  `json:"turn"`
	Thread   struct {
		ID string `json:"id"`
	} `json:"thread"`
	TokenUsage struct {
		Total struct {
			TotalTokens int `json:"totalTokens"`
		} `json:"total"`
	} `json:"tokenUsage"`
	// Status 是 thread/status/changed 的状态，ThreadName 是 thread/name/updated 的新标题。
	Status struct {
		Type string `json:"type"`
	} `json:"status"`
	ThreadName string `json:"threadName"`
}

// codexWireTurn 是 Turn 对象里测试关心的字段。
type codexWireTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// getNotification 解码一条通知参数。
func (m codexWireMessage) getNotification(t *testing.T) codexWireNotification {
	t.Helper()
	// 1. 参数非空时解码，失败则报告方法和原始参数。
	var notification codexWireNotification
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &notification); err != nil {
			t.Fatalf("%s params %s: %v", m.Method, m.Params, err)
		}
	}
	return notification
}

// getThreadID 返回通知所属的 Thread ID。
func (m codexWireMessage) getThreadID() string {
	// 1. 解码参数，优先取 threadId，再取 thread.id；无有效字段时返回空。
	var params struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(m.Params, &params)
	return cmp.Or(params.ThreadID, params.Thread.ID)
}

// isCodexResponse 构造指定响应 ID 的匹配函数。
func isCodexResponse(id json.RawMessage) func(codexWireMessage) bool {
	// 1. 返回同时检查空方法名与响应 ID 的判断函数。
	return func(message codexWireMessage) bool { return message.Method == "" && bytes.Equal(message.ID, id) }
}

// isCodexMethod 构造指定通知方法的匹配函数。
func isCodexMethod(method string) func(codexWireMessage) bool {
	// 1. 返回检查方法名的判断函数。
	return func(message codexWireMessage) bool { return message.Method == method }
}

// nextThreadNotification 等待指定 Thread 的下一条方法通知。
func (w *codexWire) nextThreadNotification(t *testing.T, threadID, method string) codexWireMessage {
	t.Helper()
	// 1. 按方法名和 Thread ID 匹配消息，其余消息留待后续读取。
	return w.next(t, "notification "+method+" of "+threadID, func(message codexWireMessage) bool {
		return message.Method == method && message.getThreadID() == threadID
	})
}

// readTurn 按顺序读取响应和 Thread 通知直到本轮完成。
func (w *codexWire) readTurn(t *testing.T, threadID string, requestID json.RawMessage) codexTurnMessages {
	t.Helper()
	// 1. 将 turn/completed 作为停止条件；requestID 为空时不收集响应。
	return w.readThreadUntil(t, threadID, requestID, isCodexMethod("turn/completed"))
}

// readThreadUntil 读取响应和 Thread 通知直到指定通知出现。
func (w *codexWire) readThreadUntil(t *testing.T, threadID string, requestID json.RawMessage, isLast func(codexWireMessage) bool) codexTurnMessages {
	t.Helper()
	// 1. 逐条取出匹配响应或本 Thread 通知，收下满足停止条件的通知后返回。
	var messages codexTurnMessages
	for {
		message := w.next(t, "messages of "+threadID, func(message codexWireMessage) bool {
			return (requestID != nil && isCodexResponse(requestID)(message)) || (message.Method != "" && message.getThreadID() == threadID)
		})
		messages = append(messages, message)
		if message.Method != "" && isLast(message) {
			return messages
		}
	}
}

// waitThreadStatus 等待指定 Thread 的状态通知。
func (w *codexWire) waitThreadStatus(t *testing.T, threadID, statusType string, messages codexTurnMessages) {
	t.Helper()
	// 1. 先在已读消息中匹配状态，缺失时继续读取通知。
	isStatus := func(message codexWireMessage) bool {
		return message.Method == "thread/status/changed" && message.getThreadID() == threadID && message.getNotification(t).Status.Type == statusType
	}
	if messages.indexOf(isStatus) < 0 {
		w.next(t, "thread/status/changed "+statusType+" of "+threadID, isStatus)
	}
}

// codexTurnMessages 是一轮里按到达顺序读到的响应和通知。
type codexTurnMessages []codexWireMessage

// count 返回符合条件的消息条数。
func (messages codexTurnMessages) count(isMatch func(codexWireMessage) bool) int {
	// 1. 逐条判断并累计匹配数量。
	count := 0
	for _, message := range messages {
		if isMatch(message) {
			count++
		}
	}
	return count
}

// indexOf 返回首条匹配消息的位置。
func (messages codexTurnMessages) indexOf(isMatch func(codexWireMessage) bool) int {
	// 1. 在消息列表中查找第一个匹配项，未找到时返回 -1。
	return slices.IndexFunc(messages, isMatch)
}

// getItem 返回指定方法和类型的首个 Item 及位置。
func (messages codexTurnMessages) getItem(t *testing.T, method, itemType string) (map[string]any, int) {
	t.Helper()
	// 1. 逐条读取通知，匹配后返回；全部缺失时报告失败。
	for index, message := range messages {
		if item := message.getNotification(t).Item; message.Method == method && item["type"] == itemType {
			return item, index
		}
	}
	t.Fatalf("no %s %s in %d messages", method, itemType, len(messages))
	return nil, -1
}

// getItemWhere 返回指定方法中首个符合条件的 Item。
func (messages codexTurnMessages) getItemWhere(t *testing.T, method string, isMatch func(map[string]any) bool) map[string]any {
	t.Helper()
	// 1. 逐条读取通知并判断 Item，全部缺失时报告失败。
	for _, message := range messages {
		if item := message.getNotification(t).Item; message.Method == method && isMatch(item) {
			return item
		}
	}
	t.Fatalf("no matching %s in %d messages", method, len(messages))
	return nil
}

// getDeltaText 按顺序拼接指定方法和 Item 的增量。
func (messages codexTurnMessages) getDeltaText(t *testing.T, method, itemID string) string {
	t.Helper()
	// 1. 筛选方法名和 itemId，将 delta 写入文本。
	var text strings.Builder
	for _, message := range messages {
		if notification := message.getNotification(t); message.Method == method && notification.ItemID == itemID {
			text.WriteString(notification.Delta)
		}
	}
	return text.String()
}

// leaveStaleSocket 留下没有进程监听的 socket 文件。
func leaveStaleSocket(t *testing.T, path string) {
	t.Helper()
	// 1. 创建父目录并监听 Unix socket，关闭时保留 socket 文件。
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: path,
		Net:  "unix",
	})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// codexClientInfo 是测试客户端 initialize 的参数。
var codexClientInfo = map[string]any{"clientInfo": map[string]any{"name": "openvaala-e2e", "version": "0.0.0"}}

// assertCodexError 检查错误响应的代码和文字。
func assertCodexError(t *testing.T, response codexWireMessage, code int, message string) {
	t.Helper()
	// 1. 同时核对错误非空、错误码和错误消息。
	if response.Error == nil || response.Error.Code != code || response.Error.Message != message {
		t.Fatalf("response = %+v (error %+v), want error %d %q", response, response.Error, code, message)
	}
}

// ── openvaala 进程夹具 ──

// e2eBuildDirectory 是本包共用的编译目录，TestMain 创建，全部测试结束后删除。
var e2eBuildDirectory string

// TestMain 准备共用编译目录并运行测试。
func TestMain(m *testing.M) {
	// 1. 创建临时目录，失败时输出错误并退出。
	directory, err := os.MkdirTemp("", "openvaala-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	e2eBuildDirectory = directory
	// 2. 运行全部测试，删除共用目录后使用测试退出码结束。
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildOpenVaalaOnce 在本包第一次用到时编译 openvaala 到 e2eBuildDirectory，之后的测试共用同一个二进制；
// 测试本身带 -race 时同样带 -race 编译，子进程里的数据竞争让它以非零状态退出。
var buildOpenVaalaOnce = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(e2eBuildDirectory, "openvaala")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	if output, err := exec.Command("go", append(arguments, "../../cmd/openvaala")...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build openvaala: %w\n%s", err, output)
	}
	return binary, nil
})

// buildOpenVaala 返回共用的测试程序。
func buildOpenVaala(t *testing.T) string {
	t.Helper()
	// 1. 调用一次性编译入口，编译失败时报告测试失败。
	binary, err := buildOpenVaalaOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// isRaceEnabled 测试程序是否带 -race 编译？
func isRaceEnabled() bool {
	// 1. 读取构建设置并检查 -race 是否为 true。
	info, ok := debug.ReadBuildInfo()
	return ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	})
}

// writeE2EConfig 在临时 HOME 写入 Agent、模型和监听配置。
func writeE2EConfig(t *testing.T, modelURL string) string {
	t.Helper()
	// 1. 准备 assistant、两档模型和空闲监听地址的文件内容。
	home := t.TempDir()
	files := map[string]string{
		"config.yaml":           fmt.Sprintf("run:\n  agent_name: assistant\nserver:\n  listen: %q\n", getFreeAddress(t)),
		"agents/assistant.yaml": "name: assistant\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: default\n",
		"models/local.yaml": fmt.Sprintf("id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n  default:\n    context_window: 32000\n    capabilities:\n      input: [text, image]\n"+
			"  second:\n    display_name: Second\n    context_window: 64000\n    reasoning_efforts: [low, high]\n    default_reasoning_effort: low\n", modelURL),
	}
	// 2. 创建配置父目录并写入文件，返回 HOME 根目录的 config.yaml。
	for name, content := range files {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(home, "config.yaml")
}

// writeE2EConfigWithoutModels 写入只有监听地址的临时配置。
func writeE2EConfigWithoutModels(t *testing.T) string {
	t.Helper()
	// 1. 创建临时 HOME，写入未配置 Provider、模型和 Agent 的文件。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("server:\n  listen: %q\n", getFreeAddress(t))), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// appendE2EConfig 将配置片段追加到文件末尾。
func appendE2EConfig(t *testing.T, config, section string) {
	t.Helper()
	// 1. 以追加模式打开配置，写入片段后关闭文件。
	file, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(section); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// getE2ELogRecords 读取指定消息的日志记录。
func getE2ELogRecords(t *testing.T, config, message string) []map[string]any {
	t.Helper()
	// 1. 读取配置所在 HOME 的日志文件，文件不存在时返回空，否则逐行解码并筛选 msg。
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "openvaala.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(payload, []byte("\n")) {
		var record map[string]any
		if json.Unmarshal(line, &record) == nil && record["msg"] == message {
			records = append(records, record)
		}
	}
	return records
}

// getFreeAddress 返回当前空闲的回环 TCP 地址。
func getFreeAddress(t *testing.T) string {
	t.Helper()
	// 1. 监听系统分配端口，记录地址后关闭监听器。
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

// getE2EEnvironment 返回使用临时 HOME 的子进程环境。
func getE2EEnvironment(home string) []string {
	// 1. 替换 HOME，移除 OPENVAALA_*、XDG_*、CODEX_HOME 和 PASEO_HOME。
	environment := []string{"HOME=" + home}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "CODEX_HOME" || name == "PASEO_HOME" || strings.HasPrefix(name, "OPENVAALA_") || strings.HasPrefix(name, "XDG_") {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

// e2eServeHomes 是已经登记了 serve stop 的 HOME：同一 HOME 只登记一次，登记在第一个调用 stopServeOnCleanup 的测试上。
// 清理按登记的逆序执行：先停止本测试的全部 app-server，再停止 serve。
var e2eServeHomes syncmap.SyncMap[string, struct{}]

// stopServeOnCleanup 为指定 HOME 登记一次服务停止操作。
func stopServeOnCleanup(t *testing.T, binary, config string) {
	t.Helper()
	// 1. 首次遇到这个 HOME 时登记 Cleanup，重复调用不再登记。
	if _, loaded := e2eServeHomes.LoadOrStore(filepath.Dir(config), struct{}{}); !loaded {
		t.Cleanup(func() { stopServe(t, binary, config) })
	}
}

// stopServe 停止配置所在 HOME 的服务。
func stopServe(t *testing.T, binary, config string) {
	t.Helper()
	// 1. 在超时上下文中运行 serve stop，失败时输出命令结果。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--config-file", config, "serve", "stop")
	command.Env = getE2EEnvironment(filepath.Dir(config))
	if output, err := command.CombinedOutput(); err != nil {
		t.Errorf("serve stop: %v\n%s", err, output)
	}
}

// e2eOutput 收集子进程的 stderr，可以在进程运行中读取。
type e2eOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// Write 将字节写入子进程输出缓存。
func (o *e2eOutput) Write(p []byte) (int, error) {
	// 1. 在锁内追加字节。
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(p)
}

// String 返回子进程输出缓存的文本。
func (o *e2eOutput) String() string {
	// 1. 在锁内读取缓存。
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

// ── Codex 线上客户端 ──

// codexWireMessage 是测试客户端收到的一条 Codex 消息。
type codexWireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexWireError `json:"error,omitempty"`
}

type codexWireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// codexWireRequest 是测试客户端发出的请求或通知；通知不带 ID。
type codexWireRequest struct {
	ID     int    `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// e2eProcess 是测试启动的一个 openvaala 子进程：app-server 或 acp。
type e2eProcess struct {
	command *exec.Cmd
	stderr  *e2eOutput
	// exited 在进程退出后关闭，exitErr 是它的退出结果。
	exited  chan struct{}
	exitErr error
	// stdin 是 stdio 收发时的协议输入，关闭它就让进程退出；socketPath 是 unix 监听模式 app-server 的 socket 文件，
	// webSocketAddress 是 ws 监听模式 app-server 的回环地址（host:port）。
	stdin            io.WriteCloser
	socketPath       string
	webSocketAddress string
	stopOnce         sync.Once
}

// codexWire 是测试内的 Codex 线上客户端：到一个 app-server 进程的一条连接，经 stdio、Unix socket 或回环 TCP 端口上的 WebSocket 收发。
// acpWire 也用它经 stdio 收发 ACP 的 JSON-RPC。
type codexWire struct {
	process *e2eProcess
	// conn 是监听模式（Unix socket 或回环 TCP 端口）的 WebSocket 连接；stdio 模式为 nil，经 process.stdin 发送。
	conn *websocket.Conn
	// messages 是读任务按到达顺序解码出的消息，连接结束时关闭；readerDone 在读任务结束后关闭。
	messages   chan codexWireMessage
	readerDone chan struct{}
	// readingAllowed 里有令牌时读任务照常读取；pauseReading 取走令牌，读任务读完手上这一条就停下，不再从连接读取，对端的写随之阻塞。
	// isReadingPaused 只由测试的 goroutine 读写。
	readingAllowed  chan struct{}
	isReadingPaused bool
	// pending 是已经读到、还没被 response 或 nextNotification 取走的消息，按到达顺序；arrivals 是读到过的全部消息，按到达顺序，
	// 测试据此检查不同 Thread 的消息之间的先后。
	pending   []codexWireMessage
	arrivals  []codexWireMessage
	nextID    int
	closeOnce sync.Once
}

// startE2EProcess 在临时 HOME 启动程序并登记停止。
func startE2EProcess(t *testing.T, binary, config string, arguments []string, isStdio bool) (*e2eProcess, io.Reader) {
	t.Helper()
	// 1. 设置参数、环境、工作目录和 stderr 缓存。
	home := filepath.Dir(config)
	command := exec.Command(binary, append([]string{"--config-file", config}, arguments...)...)
	command.Env = getE2EEnvironment(home)
	command.Dir = home
	process := &e2eProcess{
		command: command,
		stderr:  &e2eOutput{},
		exited:  make(chan struct{}),
	}
	command.Stderr = process.stderr
	var stdout *io.PipeReader
	var stdoutWriter *io.PipeWriter
	// 2. 标准流模式接上输入与输出管道，再启动进程。
	if isStdio {
		var err error
		if process.stdin, err = command.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		stdout, stdoutWriter = io.Pipe()
		command.Stdout = stdoutWriter
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// 3. 后台等待退出，关闭输出管道和退出信号，登记测试清理。
	go func() {
		process.exitErr = command.Wait()
		if stdoutWriter != nil {
			_ = stdoutWriter.Close()
		}
		close(process.exited)
	}()
	t.Cleanup(func() { process.stop(t) })
	return process, stdout
}

// startAppServer 启动 app-server 并返回第一条连接。
func startAppServer(t *testing.T, binary, config, mode string) *codexWire {
	t.Helper()
	// 1. 登记服务停止；Unix 模式监听 socket 后连接，stdio 模式接上标准流。
	stopServeOnCleanup(t, binary, config)
	if mode == "unix" {
		process, _ := startE2EProcess(t, binary, config, []string{"app-server", "--listen", "unix://"}, false)
		process.socketPath = process.waitSocketPath(t)
		return connectCodexWire(t, process)
	}
	process, stdout := startE2EProcess(t, binary, config, []string{"app-server"}, true)
	return newStdioWire(t, process, stdout)
}

// newStdioWire 创建标准流连接并启动读取。
func newStdioWire(t *testing.T, process *e2eProcess, stdout io.Reader) *codexWire {
	t.Helper()
	// 1. 创建客户端，登记关闭，再启动 stdout 读取任务。
	wire := newCodexWire(process)
	t.Cleanup(func() { wire.close(t) })
	go wire.readStdio(stdout)
	return wire
}

// connectCodexWire 连接 Unix socket 或回环 TCP 上的 WebSocket。
func connectCodexWire(t *testing.T, appServer *e2eProcess) *codexWire {
	t.Helper()
	// 1. 选择网络和地址，在超时上下文中拨号。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	network, address := "unix", appServer.socketPath
	if appServer.webSocketAddress != "" {
		network, address = "tcp", appServer.webSocketAddress
	}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
	}}})
	if err != nil {
		t.Fatalf("dial %s %s: %v\n%s", network, address, err, appServer.stderr)
	}
	conn.SetReadLimit(-1)
	wire := newCodexWire(appServer)
	wire.conn = conn
	t.Cleanup(func() { wire.close(t) })
	go wire.readWebSocket()
	return wire
}

// newCodexWire 创建测试客户端的消息通道和读取令牌。
func newCodexWire(process *e2eProcess) *codexWire {
	// 1. 建立消息、读取结束与读取放行通道，再放入初始令牌。
	wire := &codexWire{
		process:        process,
		messages:       make(chan codexWireMessage, 1024),
		readerDone:     make(chan struct{}),
		readingAllowed: make(chan struct{}, 1),
	}
	wire.readingAllowed <- struct{}{}
	return wire
}

// codexConnections 打开连到同一个 HOME、同一个 serve 的多条 Codex 连接：unix 模式在同一个 app-server 进程上新开 WebSocket 连接
// （像多个 codex --remote），stdio 模式每条连接是一个新的 app-server 进程（像多个 Paseo agent）。
type codexConnections struct {
	binary, config, mode string
	// appServer 是 unix 模式共用的 app-server 进程，stdio 模式为 nil。
	appServer *e2eProcess
	// openedCount 是 open 打开过的连接数。
	openedCount int
}

// newCodexConnections 创建连接来源并按传输方式准备共用进程。
func newCodexConnections(t *testing.T, binary, config, mode string) *codexConnections {
	t.Helper()
	// 1. 登记 HOME 的服务停止，Unix 模式启动共用 app-server。
	stopServeOnCleanup(t, binary, config)
	connections := &codexConnections{
		binary: binary,
		config: config,
		mode:   mode,
	}
	if mode == "unix" {
		connections.appServer = startAppServer(t, binary, config, mode).process
	}
	return connections
}

// open 打开一条已初始化的连接。
func (c *codexConnections) open(t *testing.T) *codexWire {
	t.Helper()
	// 1. 连接共用 app-server 或启动 stdio 进程，递增连接数并完成 initialize。
	var wire *codexWire
	if c.appServer != nil {
		wire = connectCodexWire(t, c.appServer)
	} else {
		wire = startAppServer(t, c.binary, c.config, c.mode)
	}
	c.openedCount++
	wire.call(t, "initialize", codexClientInfo, nil)
	return wire
}

// newE2ERuntimeClient 连接配置所在 HOME 的 Runtime 服务并返回停止函数。
func newE2ERuntimeClient(t *testing.T, config string) (runtimeclient.Client, func()) {
	t.Helper()
	// 1. 读取已运行服务的地址与 token，创建客户端。
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(config), ".local", "state", "openvaala", "runtime-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var server struct {
		URL   string `yaml:"url"`
		Token string `yaml:"token"`
	}
	if err := yaml.Unmarshal(payload, &server); err != nil {
		t.Fatal(err)
	}
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(server.URL, server.Token))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 后台运行客户端，返回只取消和等待一次的停止函数；调用方持续读取 Events。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Run(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return client, stop
}

// waitSocketPath 等待并返回 app-server 打印的 Unix socket 路径。
func (s *e2eProcess) waitSocketPath(t *testing.T) string {
	t.Helper()
	// 1. 读取 unix:// 前缀后的远程地址。
	return s.waitRemoteAddress(t, "unix://")
}

// waitRemoteAddress 等待并返回 app-server 打印的监听地址。
func (s *e2eProcess) waitRemoteAddress(t *testing.T, scheme string) string {
	t.Helper()
	// 1. 轮询 stderr 中完整的 codex --remote 行，进程提前退出或超时则报告失败。
	deadline := time.Now().Add(e2eTimeout)
	for {
		for _, line := range strings.SplitAfter(s.stderr.String(), "\n") {
			if address, found := strings.CutPrefix(line, "codex --remote "+scheme); found && strings.HasSuffix(address, "\n") {
				return strings.TrimSuffix(address, "\n")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("app-server did not start listening\n%s", s.stderr)
		}
		select {
		case <-s.exited:
			t.Fatalf("app-server exited before listening: %v\n%s", s.exitErr, s.stderr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stop 停止测试进程。
func (s *e2eProcess) stop(t *testing.T) {
	t.Helper()
	// 1. 只执行一次：标准流模式关闭 stdin，其余模式发送 SIGTERM。
	s.stopOnce.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		} else {
			_ = s.command.Process.Signal(syscall.SIGTERM)
		}
		// 2. 等待退出，超时则强制结束并等待；非零退出也报告失败。
		select {
		case <-s.exited:
		case <-time.After(e2eTimeout):
			_ = s.command.Process.Kill()
			<-s.exited
			t.Errorf("%v did not exit\n%s", s.command.Args[3:], s.stderr)
		}
		if s.exitErr != nil {
			t.Errorf("%v exited with %v\n%s", s.command.Args[3:], s.exitErr, s.stderr)
		}
	})
}

// readStdio 从标准输出逐行读取 JSON 消息。
func (w *codexWire) readStdio(stdout io.Reader) {
	// 1. 登记关闭消息与结束通道，每次读取前等待放行，解码成功后交付消息。
	defer close(w.readerDone)
	defer close(w.messages)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for w.waitReadingAllowed() && scanner.Scan() {
		var message codexWireMessage
		if json.Unmarshal(scanner.Bytes(), &message) == nil {
			w.messages <- message
		}
	}
}

// readWebSocket 从 WebSocket 读取 JSON 消息。
func (w *codexWire) readWebSocket() {
	// 1. 登记关闭消息与结束通道，每次读取前等待放行，读取失败则结束。
	defer close(w.readerDone)
	defer close(w.messages)
	for w.waitReadingAllowed() {
		_, payload, err := w.conn.Read(context.Background())
		if err != nil {
			return
		}
		var message codexWireMessage
		if json.Unmarshal(payload, &message) == nil {
			w.messages <- message
		}
	}
}

// waitReadingAllowed 读取任务是否已取得放行令牌？
func (w *codexWire) waitReadingAllowed() bool {
	// 1. 等待并放回令牌后返回 true。
	token := <-w.readingAllowed
	w.readingAllowed <- token
	return true
}

// pauseReading 暂停后续连接读取。
func (w *codexWire) pauseReading() {
	// 1. 尚未暂停时取走令牌并记录暂停状态。
	if !w.isReadingPaused {
		<-w.readingAllowed
		w.isReadingPaused = true
	}
}

// resumeReading 恢复后续连接读取。
func (w *codexWire) resumeReading() {
	// 1. 已暂停时放回令牌并清除暂停状态。
	if w.isReadingPaused {
		w.readingAllowed <- struct{}{}
		w.isReadingPaused = false
	}
}

// close 关闭测试连接。
func (w *codexWire) close(t *testing.T) {
	t.Helper()
	// 1. 只执行一次：先恢复读取，再关闭 WebSocket 或标准输入。
	w.closeOnce.Do(func() {
		w.resumeReading()
		if w.conn != nil {
			_ = w.conn.CloseNow()
		} else {
			_ = w.process.stdin.Close()
		}
		// 2. 等待读取任务结束，超时则记录测试错误。
		select {
		case <-w.readerDone:
		case <-time.After(e2eTimeout):
			t.Errorf("codex connection reader did not stop\n%s", w.process.stderr)
		}
	})
}

// request 发送 Codex 请求并返回 ID。
func (w *codexWire) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	// 1. 递增 ID，写出带参数的请求，再返回 JSON ID。
	w.nextID++
	w.write(t, codexWireRequest{
		ID:     w.nextID,
		Method: method,
		Params: params,
	})
	return json.RawMessage(strconv.Itoa(w.nextID))
}

// notify 发送不带 ID 的 Codex 通知。
func (w *codexWire) notify(t *testing.T, method string, params any) {
	t.Helper()
	// 1. 构造方法与参数并写出。
	w.write(t, codexWireRequest{
		Method: method,
		Params: params,
	})
}

// call 发送 Codex 请求并解码成功结果。
func (w *codexWire) call(t *testing.T, method string, params, result any) {
	t.Helper()
	// 1. 等待响应，协议错误时失败；结果目标非空时解码。
	response := w.response(t, w.request(t, method, params))
	if response.Error != nil {
		t.Fatalf("%s: %+v", method, response.Error)
	}
	if result == nil {
		return
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		t.Fatalf("%s result %s: %v", method, response.Result, err)
	}
}

// response 等待指定 ID 的响应。
func (w *codexWire) response(t *testing.T, id json.RawMessage) codexWireMessage {
	t.Helper()
	// 1. 匹配空方法名和响应 ID，其余消息留待后续读取。
	return w.next(t, "response "+string(id), func(message codexWireMessage) bool {
		return message.Method == "" && bytes.Equal(message.ID, id)
	})
}

// nextNotification 等待指定方法的下一条通知。
func (w *codexWire) nextNotification(t *testing.T, method string) codexWireMessage {
	t.Helper()
	// 1. 匹配无 ID 且方法名相同的消息，其余消息留待后续读取。
	return w.next(t, "notification "+method, func(message codexWireMessage) bool {
		return len(message.ID) == 0 && message.Method == method
	})
}

// next 等待第一条匹配消息。
func (w *codexWire) next(t *testing.T, description string, isMatch func(codexWireMessage) bool) codexWireMessage {
	t.Helper()
	// 1. 调用可返回关闭状态的读取方法，连接结束时输出子进程日志并报告测试失败。
	message, ok := w.nextUnlessClosed(t, description, isMatch)
	if !ok {
		t.Fatalf("connection closed while waiting for %s\n%s", description, w.process.stderr)
	}
	return message
}

// nextUnlessClosed 取出第一条匹配消息或返回连接关闭状态。
func (w *codexWire) nextUnlessClosed(t *testing.T, description string, isMatch func(codexWireMessage) bool) (codexWireMessage, bool) {
	t.Helper()
	// 1. 先查找 pending，命中时取出并删除。
	if index := slices.IndexFunc(w.pending, isMatch); index >= 0 {
		message := w.pending[index]
		w.pending = slices.Delete(w.pending, index, index+1)
		return message, true
	}
	// 2. 继续读取并记录到达顺序，不匹配的消息存入 pending，关闭时返回 false。
	timeout := time.After(e2eTimeout)
	for {
		select {
		case message, ok := <-w.messages:
			if !ok {
				return codexWireMessage{}, false
			}
			w.arrivals = append(w.arrivals, message)
			if isMatch(message) {
				return message, true
			}
			w.pending = append(w.pending, message)
		case <-timeout:
			t.Fatalf("timed out waiting for %s\n%s", description, w.process.stderr)
		}
	}
}

// write 编码并发送一条 Codex 请求。
func (w *codexWire) write(t *testing.T, message codexWireRequest) {
	t.Helper()
	// 1. 将请求编码为 JSON，再调用原样发送方法。
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	w.writeRaw(t, payload)
}

// writeRaw 原样发送消息字节。
func (w *codexWire) writeRaw(t *testing.T, payload []byte) {
	t.Helper()
	// 1. WebSocket 使用超时上下文写入，stdio 在末尾追加换行再写入。
	var err error
	if w.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
		defer cancel()
		err = w.conn.Write(ctx, websocket.MessageText, payload)
	} else {
		_, err = w.process.stdin.Write(append(payload, '\n'))
	}
	if err != nil {
		t.Fatalf("write: %v\n%s", err, w.process.stderr)
	}
}

// ── ACP 线上客户端 ──

// acpWire 是测试内的 ACP 编辑器：到一个 openvaala acp 进程的 stdio 连接。读取和按 id 配对响应与 Codex 的 stdio 连接相同，
// 由 codexWire 负责；发出的请求另带 ACP 要求的 jsonrpc 字段。
type acpWire struct {
	wire *codexWire
}

// startACPWire 启动 ACP 进程并检查初始化结果。
func startACPWire(t *testing.T, binary, config string) *acpWire {
	t.Helper()
	// 1. 登记服务停止，启动 stdio 进程并创建 ACP 客户端。
	stopServeOnCleanup(t, binary, config)
	process, stdout := startE2EProcess(t, binary, config, []string{"acp"}, true)
	editor := &acpWire{wire: newStdioWire(t, process, stdout)}
	var initialized struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	editor.call(t, "initialize", map[string]any{"protocolVersion": 1}, &initialized)
	if initialized.ProtocolVersion != 1 || !initialized.AgentCapabilities.LoadSession {
		t.Fatalf("ACP initialize = %+v, want protocol 1 with session/load", initialized)
	}
	return editor
}

// request 发送 ACP 请求并返回 ID。
func (w *acpWire) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	// 1. 递增 ID，编码带 jsonrpc 字段的请求并原样发送。
	w.wire.nextID++
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": w.wire.nextID, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	w.wire.writeRaw(t, payload)
	return json.RawMessage(strconv.Itoa(w.wire.nextID))
}

// call 发送 ACP 请求并解码成功结果。
func (w *acpWire) call(t *testing.T, method string, params, result any) {
	t.Helper()
	// 1. 等待对应响应，协议错误时失败，结果目标非空时解码。
	response := w.wire.response(t, w.request(t, method, params))
	if response.Error != nil {
		t.Fatalf("ACP %s: %+v", method, response.Error)
	}
	if result != nil {
		if err := json.Unmarshal(response.Result, result); err != nil {
			t.Fatalf("ACP %s result %s: %v", method, response.Result, err)
		}
	}
}

// getACPUpdate 解码 session/update 中的会话更新。
func getACPUpdate(t *testing.T, message codexWireMessage) acp.SessionUpdate {
	t.Helper()
	// 1. 解码通知参数并返回 Update，失败时输出原始参数。
	var notification acp.SessionNotification
	if err := json.Unmarshal(message.Params, &notification); err != nil {
		t.Fatalf("ACP session/update %s: %v", message.Params, err)
	}
	return notification.Update
}

// ── runtimeclient 观察者 ──

// runtimeObserver 像 OpenVaala TUI 一样用 runtimeclient 打开对话、跟随根 Session：收事件的任务按到达顺序收下 Client 的全部事件
// （Client 要求调用方持续读取），测试用 next 依次取出，取出过的按顺序留在 received 里。
type runtimeObserver struct {
	client runtimeclient.Client
	// conversation 是 OpenConversation 返回的对话记录。
	conversation defs.Conversation
	stopClient   func()
	// arrivals 是收事件的任务收下的事件，由 mu 保护；arrived 在收下新事件时得到一个信号（多次合并成一个）；
	// collectorDone 在 Events 关闭、收事件的任务结束后关闭。
	mu            sync.Mutex
	arrivals      []runtimeclient.ClientEvent
	arrived       chan struct{}
	collectorDone chan struct{}
	// received 是测试已经取走的事件，只由测试的 goroutine 读写。
	received []runtimeclient.ClientEvent
}

// openRuntimeObserver 打开对话并启动 Runtime 事件收集。
func openRuntimeObserver(t *testing.T, config string, conversationID defs.ConversationID) *runtimeObserver {
	t.Helper()
	// 1. 连接服务，创建观察者及事件到达和收集结束通道。
	client, stopClient := newE2ERuntimeClient(t, config)
	observer := &runtimeObserver{
		client:        client,
		stopClient:    stopClient,
		arrived:       make(chan struct{}, 1),
		collectorDone: make(chan struct{}),
	}
	// 2. 持续收集客户端事件，在锁内追加并合并到达信号。
	go func() {
		defer close(observer.collectorDone)
		for event := range client.Events() {
			observer.mu.Lock()
			observer.arrivals = append(observer.arrivals, event)
			observer.mu.Unlock()
			select {
			case observer.arrived <- struct{}{}:
			default:
			}
		}
	}()
	// 3. 登记停止并打开对话，保存打开结果。
	t.Cleanup(observer.stop)
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	record, err := client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{ConversationID: conversationID})
	if err != nil {
		t.Fatal(err)
	}
	observer.conversation = record
	return observer
}

// next 取出下一个 Runtime 事件。
func (o *runtimeObserver) next(t *testing.T) runtimeclient.ClientEvent {
	t.Helper()
	// 1. 在锁内读取尚未取出的事件，记录并返回；没有时等待到达、结束或超时。
	timeout := time.After(e2eTimeout)
	for {
		o.mu.Lock()
		var event runtimeclient.ClientEvent
		if len(o.arrivals) > len(o.received) {
			event = o.arrivals[len(o.received)]
		}
		o.mu.Unlock()
		if event != nil {
			o.received = append(o.received, event)
			return event
		}
		select {
		case <-o.arrived:
		case <-o.collectorDone:
			o.mu.Lock()
			isDrained := len(o.arrivals) == len(o.received)
			o.mu.Unlock()
			if isDrained {
				t.Fatal("runtime client stopped while waiting for an event")
			}
		case <-timeout:
			t.Fatal("timed out waiting for a runtime client event")
		}
	}
}

// stop 停止客户端并等待事件收集结束。
func (o *runtimeObserver) stop() {
	// 1. 调用可重复的客户端停止函数，再等待收集结束通道。
	o.stopClient()
	<-o.collectorDone
}

// ── 假模型 ──

// e2eModelRequest 是假模型收到的一次请求（body 为请求体，header 为请求头），测试用 reply 决定这次的输出；请求被取消时不阻塞测试服务器。
type e2eModelRequest struct {
	body   map[string]any
	header http.Header
	reply  chan e2eModelReply
}

// e2eModelReply 是假模型的一次输出：text 按 | 分成多个增量；tool 非空时调用这个工具；hold 时不结束、停在输出一半；
// status 非 0 时直接以这个 HTTP 状态拒绝；usage 时附带用量。
type e2eModelReply struct {
	text, reasoning, tool, arguments string
	hold                             bool
	status                           int
	usage                            bool
	// stream 非 nil 时，在 text 之后把收到的每段文本作为一个增量写出并立即发送，通道关闭后正常结束。
	stream <-chan string
	// finish 非 nil 时，写出 finish_reason 之后等它关闭才结束输出：工具调用的参数已经收齐，这次模型输出还没结束。
	finish <-chan struct{}
}

// e2eModel 是可控的 OpenAI Chat Completions 兼容假模型：每个请求交给测试决定输出。
type e2eModel struct {
	server   *httptest.Server
	requests chan e2eModelRequest
}

// newE2EModel 启动由测试逐请求控制回复的模型服务。
func newE2EModel(t *testing.T) *e2eModel {
	t.Helper()
	// 1. 创建请求队列和工具调用计数器，启动 HTTP 服务。
	model := &e2eModel{requests: make(chan e2eModelRequest, 128)}
	// 工具调用计数器为每次调用生成不同的 ID。
	var toolCalls atomic.Int32
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解码请求，交给测试决定回复；等待期间请求取消则结束。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := e2eModelRequest{
			body:   body,
			header: r.Header.Clone(),
			reply:  make(chan e2eModelReply, 1),
		}
		model.requests <- request
		var reply e2eModelReply
		select {
		case reply = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 2. 预设 HTTP 错误时直接拒绝，否则先写推理和文本增量。
		if reply.status != 0 {
			http.Error(w, "model refused request", reply.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if reply.reasoning != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", reply.reasoning)
		}
		for _, part := range strings.Split(reply.text, "|") {
			if part != "" {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
			}
		}
		// 3. 逐段写出通道中的流式文字，通道关闭或请求取消后停止。
	stream:
		for reply.stream != nil {
			select {
			case part, ok := <-reply.stream:
				if !ok {
					break stream
				}
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
		// 4. 写出带唯一 ID 的工具调用；hold 模式刷新后等待取消。
		if reply.tool != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-%d\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n", toolCalls.Add(1), reply.tool, reply.arguments)
		}
		if reply.hold {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		// 5. 写出结束原因后按需等待放行，再追加用量和流结束标记。
		if reply.finish != nil {
			w.(http.Flusher).Flush()
			select {
			case <-reply.finish:
			case <-r.Context().Done():
				return
			}
		}
		if reply.usage {
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":4,\"total_tokens\":124}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(model.server.Close)
	return model
}

// nextRootAndTask 收取根 Agent 与指定任务各一次模型请求。
func (m *e2eModel) nextRootAndTask(t *testing.T, task string) (root, child e2eModelRequest) {
	t.Helper()
	// 1. 接收两个请求，按 TaskName 分开，再核对两类请求都已收到。
	for range 2 {
		request := m.next(t)
		if strings.Contains(fmt.Sprint(request.body), "TaskName: "+task) {
			child = request
		} else {
			root = request
		}
	}
	if root.reply == nil || child.reply == nil {
		t.Fatalf("model requests are not one from the root agent and one from task %s", task)
	}
	return root, child
}

// replyAll 在后台回复后续全部模型请求直到测试结束。
func (m *e2eModel) replyAll(t *testing.T, reply e2eModelReply) {
	t.Helper()
	// 1. 启动回复循环，测试清理时发出停止信号并等待循环结束。
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case request := <-m.requests:
				request.reply <- reply
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// next 等待假模型收到下一个请求。
func (m *e2eModel) next(t *testing.T) e2eModelRequest {
	t.Helper()
	// 1. 读取请求队列，超时时报告测试失败。
	select {
	case request := <-m.requests:
		return request
	case <-time.After(e2eTimeout):
		t.Fatal("model request timed out")
	}
	return e2eModelRequest{}
}
