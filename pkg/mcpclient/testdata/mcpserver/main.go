// Command mcpserver 是多个包的测试共用的假 MCP server，各包的测试从这里编译：默认走 stdio。
//
// 只认下面几个参数，其余参数原样留给 argv 工具返回：
//
//	-http <addr>      改用 streamable HTTP 监听 addr，开始监听后往 stdout 写一行 http://<实际地址>
//	-sse <addr>       改用 SSE 监听 addr，同样写出地址
//	-stderr <text>    往 stderr 写 text 后以退出码 1 结束
//	-junk             先往 stdout 写一行非 JSON
//	-hang             读 stdin 但从不回应，stdin 结束时退出
//	-empty            不注册任何工具
//	-exit-after-list  回答完 tools/list 后很快结束进程，模拟每次连上就断开的 server
//	-pidfile <path>   启动时先把进程号写进 path（写临时文件再改名），测试据此确认进程的启停
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverOptions 是从命令行认出的参数。
type serverOptions struct {
	httpAddress   string
	sseAddress    string
	stderrText    string
	junk          bool
	hang          bool
	empty         bool
	exitAfterList bool
	pidFile       string
}

// main 按测试参数启动 stdio、HTTP 或 SSE MCP server。
func main() {
	// 1. 读取选项，按要求写 PID、输出错误退出或只读取 stdin。
	options := parseServerOptions(os.Args[1:])
	if options.pidFile != "" {
		writePIDFile(options.pidFile)
	}
	if options.stderrText != "" {
		fmt.Fprintln(os.Stderr, options.stderrText)
		os.Exit(1)
	}
	if options.hang {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	// 2. 创建 server，按选项注册工具和退出通知处理。
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake",
		Version: "1.0.0",
	}, nil)
	if !options.empty {
		addTools(server)
	}
	if options.exitAfterList {
		server.AddReceivingMiddleware(exitAfterToolsList)
	}
	// 3. 按传输类型开始服务，stdio 可先写入非 JSON 测试内容。
	getServer := func(*http.Request) *mcp.Server { return server }
	switch {
	case options.httpAddress != "":
		serveHTTP(options.httpAddress, mcp.NewStreamableHTTPHandler(getServer, nil))
	case options.sseAddress != "":
		serveHTTP(options.sseAddress, mcp.NewSSEHandler(getServer, nil))
	default:
		if options.junk {
			fmt.Println("this line is not JSON")
		}
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// parseServerOptions 读取测试 server 支持的命令参数，忽略其他参数。
func parseServerOptions(arguments []string) serverOptions {
	// 1. 逐项匹配开关和后续值，填入启动选项。
	var options serverOptions
	for index := 0; index < len(arguments); index++ {
		value := func() string {
			if index+1 >= len(arguments) {
				return ""
			}
			index++
			return arguments[index]
		}
		switch arguments[index] {
		case "-http":
			options.httpAddress = value()
		case "-sse":
			options.sseAddress = value()
		case "-stderr":
			options.stderrText = value()
		case "-junk":
			options.junk = true
		case "-hang":
			options.hang = true
		case "-empty":
			options.empty = true
		case "-exit-after-list":
			options.exitAfterList = true
		case "-pidfile":
			options.pidFile = value()
		}
	}
	return options
}

// exitAfterToolsList 给 MCP 处理函数包一层，回答完 tools/list 后很快杀掉自己的进程。
func exitAfterToolsList(next mcp.MethodHandler) mcp.MethodHandler {
	// 1. 调用原处理函数，tools/list 完成后延迟 100 毫秒发送 SIGKILL。
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		if method == "tools/list" {
			time.AfterFunc(100*time.Millisecond, func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) })
		}
		return result, err
	}
}

// writePIDFile 把进程号写入文件。
func writePIDFile(path string) {
	// 1. 先写临时文件，再改名为目标路径。
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Rename(temporary, path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// serveHTTP 监听地址并持续提供 HTTP 服务。
func serveHTTP(address string, handler http.Handler) {
	// 1. 创建 TCP 监听，输出实际地址，再运行 HTTP 服务。
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("http://" + listener.Addr().String())
	if err := http.Serve(listener, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// addTools 注册供连接和调用测试使用的 MCP 工具。
func addTools(server *mcp.Server) {
	// 1. 注册回显、错误、等待、图片、大文本、结构化内容、参数、进程号和列表变化工具。
	server.AddTool(newTool("echo", `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
				return nil, &jsonrpc.Error{
					Code:    jsonrpc.CodeInvalidParams,
					Message: "invalid echo arguments: " + err.Error(),
				}
			}
			return newTextResult(arguments.Text), nil
		})
	server.AddTool(newTool("fail", `{"type":"object","properties":{"message":{"type":"string"}}}`),
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
			result := newTextResult(arguments.Message)
			result.IsError = true
			return result, nil
		})
	server.AddTool(newTool("slow", `{"type":"object","properties":{"seconds":{"type":"number"}}}`),
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Seconds float64 `json:"seconds"`
			}
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
			select {
			case <-time.After(time.Duration(arguments.Seconds * float64(time.Second))):
				return newTextResult("slept"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	server.AddTool(newTool("image", `{"type":"object"}`),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			data := make([]byte, 1024)
			copy(data, "\x89PNG\r\n\x1a\n")
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "here"},
				&mcp.ImageContent{
					MIMEType: "image/png",
					Data:     data,
				},
			}}, nil
		})
	server.AddTool(newTool("big", `{"type":"object","properties":{"bytes":{"type":"integer"}}}`),
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Bytes int `json:"bytes"`
			}
			_ = json.Unmarshal(request.Params.Arguments, &arguments)
			return newTextResult(strings.Repeat("x", arguments.Bytes)), nil
		})
	server.AddTool(newTool("structured", `{"type":"object"}`),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
		})
	server.AddTool(newTool("argv", `{"type":"object"}`),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			encoded, err := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Getenv("MCP_TEST_ENV")})
			if err != nil {
				return nil, err
			}
			return newTextResult(string(encoded)), nil
		})
	server.AddTool(newTool("pid", `{"type":"object"}`),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return newTextResult(strconv.Itoa(os.Getpid())), nil
		})
	server.AddTool(newTool("change", `{"type":"object"}`),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// 新增工具后 SDK 向会话发送 notifications/tools/list_changed。
			server.AddTool(newTool("added", `{"type":"object"}`),
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return newTextResult("added"), nil
				})
			return newTextResult("changed"), nil
		})
}

// newTool 创建带给定输入 schema 的测试工具定义。
func newTool(name, inputSchema string) *mcp.Tool {
	// 1. 填入名称、说明和原始 JSON schema。
	return &mcp.Tool{
		Name:        name,
		Description: "fake " + name,
		InputSchema: json.RawMessage(inputSchema),
	}
}

// newTextResult 创建包含一个文本块的 MCP 结果。
func newTextResult(text string) *mcp.CallToolResult {
	// 1. 把文本放进结果的内容列表。
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
