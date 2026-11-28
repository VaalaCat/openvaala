package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// exerciseLocalMCP 用后端收到的 stdio 定义启动服务并调用宿主的真实工具。
func exerciseLocalMCP(params json.RawMessage) ([]string, error) {
	var request struct {
		MCPServers []struct {
			Name, Command string
			Args          []string
			Env           []struct{ Name, Value string }
		}
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, err
	}
	for _, server := range request.MCPServers {
		if server.Name != "openvaala" || server.Command == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, server.Command, server.Args...)
		command.Env = os.Environ()
		for _, variable := range server.Env {
			command.Env = append(command.Env, variable.Name+"="+variable.Value)
		}
		command.Stderr = os.Stderr
		client := mcp.NewClient(&mcp.Implementation{Name: "fakeacp"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
		if err != nil {
			return nil, fmt.Errorf("connect MCP bridge: %w", err)
		}
		defer session.Close()
		var outputs []string
		for _, name := range []string{"list_agents", "terminal_list", "bash"} {
			args := object{}
			if name == "bash" {
				args["command"] = "printf stdio-mcp"
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				return nil, fmt.Errorf("call %s: %w", name, err)
			}
			var content strings.Builder
			for _, part := range result.Content {
				if text, ok := part.(*mcp.TextContent); ok {
					content.WriteString(text.Text)
				}
			}
			if result.IsError {
				return nil, fmt.Errorf("call %s: %s", name, content.String())
			}
			outputs = append(outputs, content.String())
		}
		return outputs, nil
	}
	return nil, errors.New("missing openvaala stdio MCP server")
}
