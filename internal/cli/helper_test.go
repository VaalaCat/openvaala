package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGetACPClientMCPServers 检查 MCP 服务名称转换、重复后缀和支持类型筛选。
func TestGetACPClientMCPServers(t *testing.T) {
	// 1. 列出各种传输、名称和重复 ID 场景。
	stdio := func(name string) acp.McpServer {
		return acp.McpServer{Stdio: &acp.McpServerStdio{
			Name:    name,
			Command: "server",
		}}
	}
	http := func(name string) acp.McpServer {
		return acp.McpServer{Http: &acp.McpServerHttpInline{
			Name: name,
			Type: "http",
			Url:  "http://127.0.0.1/mcp",
		}}
	}
	sse := func(name string) acp.McpServer {
		return acp.McpServer{Sse: &acp.McpServerSseInline{
			Name: name,
			Type: "sse",
			Url:  "http://127.0.0.1/sse",
		}}
	}
	acpServer := func(name string) acp.McpServer {
		return acp.McpServer{Acp: &acp.McpServerAcpInline{
			Id:   "editor-1",
			Name: name,
		}}
	}
	tests := []struct {
		name    string
		servers []acp.McpServer
		wantIDs []string
	}{
		{
			name:    "space",
			servers: []acp.McpServer{stdio("My Server")},
			wantIDs: []string{"My_Server"},
		},
		{
			name:    "underscore_run",
			servers: []acp.McpServer{stdio("a__b")},
			wantIDs: []string{"a_b"},
		},
		{
			name:    "invalid_characters",
			servers: []acp.McpServer{stdio("a/b.c"), stdio("中文 名")},
			wantIDs: []string{"a_b_c", "_"},
		},
		{
			name:    "allowed_characters_kept",
			servers: []acp.McpServer{stdio("My-Server_2")},
			wantIDs: []string{"My-Server_2"},
		},
		{
			name:    "same_name",
			servers: []acp.McpServer{stdio("fs"), stdio("fs")},
			wantIDs: []string{"fs", "fs-2"},
		},
		{
			name:    "same_name_three_times",
			servers: []acp.McpServer{stdio("fs"), http("fs"), sse("fs")},
			wantIDs: []string{"fs", "fs-2", "fs-3"},
		},
		{
			name:    "same_id_after_conversion",
			servers: []acp.McpServer{stdio("a b"), stdio("a_b"), stdio("a.b")},
			wantIDs: []string{"a_b", "a_b-2", "a_b-3"},
		},
		{
			name:    "suffix_taken_by_another_name",
			servers: []acp.McpServer{stdio("fs"), stdio("fs-2"), stdio("fs")},
			wantIDs: []string{"fs", "fs-2", "fs-3"},
		},
		{
			name:    "empty_name_skipped",
			servers: []acp.McpServer{stdio(""), stdio("fs")},
			wantIDs: []string{"fs"},
		},
		{
			name:    "acp_type_skipped",
			servers: []acp.McpServer{acpServer("editor"), stdio("fs")},
			wantIDs: []string{"fs"},
		},
		{
			name:    "http_and_sse",
			servers: []acp.McpServer{http("web"), sse("events")},
			wantIDs: []string{"web", "events"},
		},
		{
			name:    "none",
			servers: nil,
			wantIDs: []string{},
		},
	}
	// 2. 整理编辑器 MCP 配置，检查登记 ID 的内容和顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getACPClientMCPServers(context.Background(), tt.servers)
			gotIDs := make([]string, 0, len(got))
			for _, server := range got {
				gotIDs = append(gotIDs, server.ID)
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Fatalf("server ids = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// TestGetACPMCPServerSummary 检查 MCP 日志摘要不含凭据、参数和 URL 私有内容。
func TestGetACPMCPServerSummary(t *testing.T) {
	// 1. 列出 stdio、HTTP、SSE 和无效地址样例。
	http := func(url string) acp.McpServer {
		return acp.McpServer{Http: &acp.McpServerHttpInline{
			Name: "web",
			Type: "http",
			Url:  url,
		}}
	}
	tests := []struct {
		name   string
		server acp.McpServer
		want   string
	}{
		{
			name: "stdio_without_args_and_env",
			server: acp.McpServer{Stdio: &acp.McpServerStdio{
				Name:    "web",
				Command: "npx",
				Args:    []string{"-y", "server", "--api-key", "secret"},
				Env: []acp.EnvVariable{{
					Name:  "TOKEN",
					Value: "secret",
				}},
			}},
			want: "web stdio npx",
		},
		{
			name: "http_without_credentials_path_query_and_headers",
			server: acp.McpServer{Http: &acp.McpServerHttpInline{
				Name: "web",
				Type: "http",
				Url:  "https://user:secret@mcp.context7.com/mcp?key=secret#part",
				Headers: []acp.HttpHeader{{
					Name:  "Authorization",
					Value: "Bearer secret",
				}},
			}},
			want: "web http https://mcp.context7.com",
		},
		{
			name:   "secret_in_path",
			server: http("https://mcp.zapier.com/api/mcp/s/secret/mcp"),
			want:   "web http https://mcp.zapier.com",
		},
		{
			name: "sse",
			server: acp.McpServer{Sse: &acp.McpServerSseInline{
				Name: "web",
				Type: "sse",
				Url:  "http://127.0.0.1:8080/sse",
			}},
			want: "web sse http://127.0.0.1:8080",
		},
		{
			name:   "opaque_credentials",
			server: http("https:user:secret@mcp.example.com/mcp"),
			want:   "web http (invalid url)",
		},
		{
			name:   "unparsable",
			server: http("http://secret\x7f"),
			want:   "web http (invalid url)",
		},
	}
	// 2. 生成摘要，检查仅输出预期服务信息。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getACPMCPServerSummary(acpClientMCPServer{
				ID:     "web",
				Server: tt.server,
			}); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}

// acpGlobalHeaders 是传给 getACPClientMCPTransport 的全局 http.headers：User-Agent 应出现在每个请求里，
// 和编辑器配置的请求头同名的条目应被编辑器的值覆盖。
var acpGlobalHeaders = map[string]string{"User-Agent": acpGlobalUserAgent, acpEditorHeaderName: "global-value"}

// TestGetACPClientMCPTransport 检查本机 MCP 传输的目录、环境和请求头。
func TestGetACPClientMCPTransport(t *testing.T) {
	// 1. 准备工作目录并列出三种传输配置。
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		// getServer 生成编辑器给的配置，http、sse 时 url 是测试 server 的地址。
		getServer func(url string) acp.McpServer
		// transport 是 http、sse 时测试 server 的类型；为空表示 stdio，运行命令检查目录和环境，want 是命令的输出。
		transport string
		want      string
	}{
		{
			name: "stdio",
			want: "from-editor|kept|" + directory,
			getServer: func(string) acp.McpServer {
				return acp.McpServer{Stdio: &acp.McpServerStdio{
					Name:    "fs",
					Command: "sh",
					Args:    []string{"-c", `printf '%s|%s|%s' "$MCP_TEST_ENV" "$KEEP" "$(pwd)"`},
					Env: []acp.EnvVariable{{
						Name:  "MCP_TEST_ENV",
						Value: "from-editor",
					}},
				}}
			},
		},
		{
			name:      "http",
			transport: "http",
			getServer: func(url string) acp.McpServer {
				return acp.McpServer{Http: &acp.McpServerHttpInline{
					Name: "web",
					Type: "http",
					Url:  url,
					Headers: []acp.HttpHeader{{
						Name:  acpEditorHeaderName,
						Value: acpEditorHeaderValue,
					}},
				}}
			},
		},
		{
			name:      "sse",
			transport: "sse",
			getServer: func(url string) acp.McpServer {
				return acp.McpServer{Sse: &acp.McpServerSseInline{
					Name: "events",
					Type: "sse",
					Url:  url,
					Headers: []acp.HttpHeader{{
						Name:  acpEditorHeaderName,
						Value: acpEditorHeaderValue,
					}},
				}}
			},
		},
	}
	// 2. stdio 检查命令输出，HTTP 和 SSE 连接测试服务并检查工具列表及请求头。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.transport == "" {
				environment := append(os.Environ(), "MCP_TEST_ENV=from-acp", "KEEP=kept")
				transport := getACPClientMCPTransport(tt.getServer(""), directory, environment, nil)
				output, err := transport.(*mcp.CommandTransport).Command.Output()
				if err != nil || string(output) != tt.want {
					t.Fatalf("command output = %q (%v), want %q", output, err, tt.want)
				}
				return
			}
			var requests, headed atomic.Int32
			editorServer := newACPEditorMCPServer(t, tt.transport, &requests, &headed)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			session, err := mcp.NewClient(&mcp.Implementation{
				Name:    "test",
				Version: "1.0.0",
			}, nil).Connect(ctx, getACPClientMCPTransport(tt.getServer(editorServer.URL), directory, nil, acpGlobalHeaders), nil)
			if err != nil {
				t.Fatal(err)
			}
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
				t.Fatalf("tools = %+v (%v), want echo", tools, err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if requests.Load() < 2 || requests.Load() != headed.Load() {
				t.Fatalf("editor server got %d requests, %d with the editor header and the global User-Agent, want at least 2 and all of them", requests.Load(), headed.Load())
			}
		})
	}
}

// TestGetACPImageBlock 检查 data URL 图片转换后的数据和 MIME 类型。
func TestGetACPImageBlock(t *testing.T) {
	// 1. 列出 PNG 和 JPEG 图片样例。
	tests := []struct {
		name         string
		dataURL      string
		wantData     string
		wantMimeType string
	}{
		{
			name:         "png",
			dataURL:      "data:image/png;base64,AAAA",
			wantData:     "AAAA",
			wantMimeType: "image/png",
		},
		{
			name:         "jpeg",
			dataURL:      "data:image/jpeg;base64,/9j/4AAQ",
			wantData:     "/9j/4AAQ",
			wantMimeType: "image/jpeg",
		},
	}
	// 2. 转换图片块并检查数据和 MIME 类型。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := getACPImageBlock(&defs.ImageContent{DataURL: tt.dataURL})
			if block.Image == nil || block.Image.Type != "image" || block.Image.Data != tt.wantData || block.Image.MimeType != tt.wantMimeType {
				t.Fatalf("getACPImageBlock(%q) = %+v, want image %q %q", tt.dataURL, block.Image, tt.wantMimeType, tt.wantData)
			}
		})
	}
}
