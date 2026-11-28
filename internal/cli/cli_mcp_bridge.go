package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sync"

	"github.com/spf13/cobra"
)

const maxMCPBridgeFrameBytes = 16 << 20

func newMCPBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-bridge",
		Short: "Relay an Agent's stdio MCP connection to OpenVaala",
		Args:  cobra.NoArgs,
		RunE:  runMCPBridgeCommand,
	}
}

func runMCPBridgeCommand(command *cobra.Command, _ []string) error {
	endpoint, token := os.Getenv("OPENVAALA_MCP_URL"), os.Getenv("OPENVAALA_TOKEN")
	if endpoint == "" || token == "" {
		return errors.New("mcp-bridge requires OPENVAALA_MCP_URL and OPENVAALA_TOKEN")
	}
	input, output, closePipes, err := openBridgeStdio(command)
	if err != nil {
		return err
	}
	defer closePipes()

	ctx, stop := context.WithCancelCause(command.Context())
	streamsClosed := make(chan struct{})
	context.AfterFunc(ctx, func() {
		input.Close()
		output.Close()
		close(streamsClosed)
	})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 并发转发各帧，让等待会话消息的请求可以和其他调用并行。每次输出一条完整的 JSON-RPC 消息。
	var tasks sync.WaitGroup
	var writes sync.Mutex
	write := func(frame []byte) error {
		payload := bytes.TrimSpace(frame)
		if bytes.ContainsRune(payload, '\n') {
			var compact bytes.Buffer
			if err := json.Compact(&compact, payload); err != nil {
				return err
			}
			payload = compact.Bytes()
		}
		writes.Lock()
		defer writes.Unlock()
		_, err := output.Write(append(payload, '\n'))
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(nil, maxMCPBridgeFrameBytes)
	for ctx.Err() == nil && scanner.Scan() {
		frame := bytes.Clone(scanner.Bytes())
		tasks.Go(func() {
			if err := relayMCPFrame(ctx, client, endpoint, token, frame, write); err != nil {
				stop(err)
			}
		})
	}
	stop(scanner.Err())
	<-streamsClosed
	tasks.Wait()
	if err := context.Cause(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// relayMCPFrame 将原始消息投递给 Streamable HTTP 端点，再按响应传输类型剥掉外层。
func relayMCPFrame(ctx context.Context, client *http.Client, endpoint, token string, frame []byte, write func([]byte) error) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	// Streamable HTTP 要求请求头与消息的 MCP 协议版本元数据一致。
	var envelope struct {
		Params struct {
			Meta struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"_meta"`
		}
	}
	if json.Unmarshal(frame, &envelope) == nil && envelope.Params.Meta.ProtocolVersion != "" {
		request.Header.Set("Mcp-Protocol-Version", envelope.Params.Meta.ProtocolVersion)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		return write(body)
	case "text/event-stream":
		return relayMCPEvents(response.Body, write)
	default:
		return fmt.Errorf("openvaala MCP endpoint returned %s", response.Status)
	}
}

// relayMCPEvents 将每个 SSE 事件的数据帧交给标准输出，忽略传输层字段和空事件。
func relayMCPEvents(body io.Reader, write func([]byte) error) error {
	var data bytes.Buffer
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, maxMCPBridgeFrameBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if data.Len() > 0 {
				if err := write(data.Bytes()); err != nil {
					return err
				}
				data.Reset()
			}
			continue
		}
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	return scanner.Err()
}
