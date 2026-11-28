package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	sdk "github.com/coder/acp-go-sdk"
)

// SDK 和 stdio 对端按行读取 JSON-RPC。
const maxACPFrameBytes = 10 << 20

type acpConnection struct {
	// promptWritten 在 SDK 把当前 Prompt 帧写进 stdin 后通知 StartTurn。
	promptWritten atomic.Pointer[chan struct{}]
	stopInput     func() bool
	client        *sdk.ClientSideConnection
	capabilities  sdk.AgentCapabilities
	process       workspace.Process
	stdin         *os.File
	stdout        *io.PipeReader
	exited        chan struct{}
	exitErr       error
}

func (s *acpAgentSession) startACPConnection(ctx context.Context, ws workspace.Workspace, cwd string, env map[string]string) (*acpConnection, error) {
	command := s.command
	if ws == nil || command.Command == "" {
		return nil, errors.New("ACP requires workspace and command")
	}
	initialize, err := mergeParamOverwrite(sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber}, sdk.AgentMethodInitialize, s.getParamOverwrites()...)
	if err != nil {
		return nil, err
	}
	words := []string{"exec", "env", "--"}
	variables := maps.Clone(command.Env)
	if variables == nil {
		variables = make(map[string]string)
	}
	maps.Copy(variables, env)
	token, hasToken := variables["OPENVAALA_TOKEN"]
	if hasToken {
		if strings.ContainsRune(token, 0) {
			return nil, errors.New("invalid ACP session token")
		}
		delete(variables, "OPENVAALA_TOKEN")
		// bash 从非 TTY stdin 消费一个 NUL 结尾的私有值，再让后端独占余下的 ACP 流。
		words = append([]string{"IFS= read -r -d '' OPENVAALA_TOKEN && export OPENVAALA_TOKEN &&"}, words...)
	}
	for _, key := range slices.Sorted(maps.Keys(variables)) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, errors.New("invalid ACP environment variable name")
		}
		words = append(words, workspace.QuoteShellWord(key+"="+variables[key]))
	}
	for _, word := range append([]string{command.Command}, command.Args...) {
		words = append(words, workspace.QuoteShellWord(word))
	}
	// 本机直接把文件描述符交给子进程，进程退出不依赖 Go 的 stdin 复制协程。
	input, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	reader, output := io.Pipe()
	process, err := ws.StartProcess(ctx, &workspace.ProcessRequest{Directory: cwd, Command: strings.Join(words, " "), Stdin: input, Stdout: output, Stderr: processLog{ctx}})
	if err != nil {
		input.Close()
		writer.Close()
		reader.Close()
		output.Close()
		return nil, err
	}
	c := &acpConnection{process: process, stdin: writer, stdout: reader, exited: make(chan struct{})}
	c.stopInput = context.AfterFunc(ctx, func() { writer.Close() })
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxACPFrameBytes)
	c.client = sdk.NewClientSideConnection(s, c, &notificationReader{scanner: scanner, session: s})
	go func() {
		code, err := process.Wait()
		if err == nil {
			err = fmt.Errorf("ACP process exited with code %d", code)
		}
		c.exitErr = err
		input.Close()
		close(c.exited)
		output.Close()
	}()
	if hasToken {
		if _, err := io.WriteString(writer, token+"\x00"); err != nil {
			c.close()
			return nil, fmt.Errorf("initialize ACP process environment: %w", err)
		}
	}
	initialized, err := c.client.Initialize(ctx, initialize)
	if err != nil {
		select {
		case <-c.exited:
			err = c.exitErr
		default:
			err = rpcError(err)
		}
		c.close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if initialized.ProtocolVersion != sdk.ProtocolVersionNumber {
		c.close()
		return nil, fmt.Errorf("unsupported ACP protocol version %d", initialized.ProtocolVersion)
	}
	c.capabilities = initialized.AgentCapabilities
	return c, nil
}

// Write 是 SDK 的 stdio 写端。SDK 每次写一整帧，method 是顶层字段；只读取信封头，避免再次解析大图片。
func (c *acpConnection) Write(frame []byte) (int, error) {
	n, err := c.stdin.Write(frame)
	if err == nil && isPromptFrame(frame) {
		close(*c.promptWritten.Load())
	}
	return n, err
}

func (c *acpConnection) close() {
	c.stopInput()
	c.stdin.Close()
	c.stdout.Close()
	select {
	case <-c.exited:
	default:
		if err := c.process.Terminate(); err != nil {
			_ = c.process.Close()
		}
		<-c.exited
	}
	<-c.client.Done()
}

// notificationReader 在读取下一帧前交付本次通知，让实时和回放消费者都能向 stdout 施加回压。
// 请求和应答交给 SDK；其默认的异步通知队列满 1024 条会主动断连。
type notificationReader struct {
	scanner *bufio.Scanner
	session *acpAgentSession
	pending []byte
}

func (r *notificationReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scanner.Bytes()
		var frame struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		decoded := json.Unmarshal(line, &frame) == nil
		if decoded && frame.ID == nil && frame.Method == "auth/status_update" {
			continue
		}
		if !decoded || frame.Method != sdk.ClientMethodSessionUpdate || frame.ID != nil {
			r.pending = append(line, '\n')
			continue
		}
		var kind struct {
			Update struct{ SessionUpdate string }
		}
		if err := json.Unmarshal(frame.Params, &kind); err != nil {
			return 0, err
		}
		switch kind.Update.SessionUpdate {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update",
			"available_commands_update", "current_mode_update", "config_option_update", "usage_update":
		default:
			telemetry.GetLogger(r.session.ctx).Debug("ignoring ACP session update", "session_update", kind.Update.SessionUpdate)
			continue
		}
		var n sdk.SessionNotification
		if err := json.Unmarshal(frame.Params, &n); err != nil {
			return 0, err
		}
		if err := n.Validate(); err != nil {
			return 0, err
		}
		if err := r.session.SessionUpdate(r.session.ctx, n); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
