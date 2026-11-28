package world

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

const (
	bridgeProtocolVersion = 1
	maxBridgeFrameBytes   = 8 << 20
	bridgeNotifyTimeout   = 30 * time.Second
)

var errBridgeFrameTooLarge = errors.New("JSON-RPC frame exceeds 8 MiB")

type worldBridge struct {
	node    *worldNodeImpl
	writes  chan bridgeWrite
	pending syncmap.SyncMap[string, chan bridgeFrame]
	nextID  atomic.Uint64
	ready   chan struct{}
}

// RunBridge 在可由 Close 中断的输入输出流上运行 World 节点与宿主的 JSON-RPC 会话。
func RunBridge(ctx context.Context, opt *WorldNodeOpt, input io.ReadCloser, output io.WriteCloser) error {
	node, err := NewWorldNode(opt)
	if err != nil {
		return err
	}
	bridge := &worldBridge{
		node: node.(*worldNodeImpl), ready: make(chan struct{}), writes: make(chan bridgeWrite),
	}
	return bridge.run(ctx, input, output)
}

func (b *worldBridge) run(ctx context.Context, input io.ReadCloser, output io.WriteCloser) error {
	ctx, stop := context.WithCancelCause(ctx)
	streamsClosed := make(chan struct{})
	context.AfterFunc(ctx, func() {
		input.Close()
		output.Close()
		close(streamsClosed)
	})
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case write := <-b.writes:
				if _, err := output.Write(write.payload); err != nil {
					stop(fmt.Errorf("write bridge frame: %w", err))
					return
				}
				if write.flushed != nil {
					close(write.flushed)
				}
			}
		}
	})

	// 握手确认写出后才启动节点。首个握手后的请求等待同一个就绪信号。
	initialized := false
	reader := bufio.NewReader(input)
	for ctx.Err() == nil {
		line, err := readBridgeFrame(reader)
		if err != nil {
			stop(err)
			break
		}
		var frame bridgeFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			rpcErr := &bridgeError{Code: -32700, Message: "invalid JSON-RPC JSON"}
			if json.Valid(line) {
				rpcErr = &bridgeError{Code: -32600, Message: "invalid JSON-RPC request"}
			}
			tasks.Go(func() { b.respond(ctx, nil, nil, rpcErr) })
			continue
		}
		if frame.Method == nil && (frame.Result != nil || frame.Error != nil) {
			if frame.Version == "2.0" {
				if reply, ok := b.pending.Load(string(frame.ID)); ok {
					select {
					case reply <- frame:
					default:
					}
				}
			}
			continue
		}
		var method string
		if frame.Version != "2.0" || !validBridgeID(frame.ID) || json.Unmarshal(frame.Method, &method) != nil || method == "" {
			if frame.Method == nil || frame.ID != nil {
				tasks.Go(func() { b.respond(ctx, nil, nil, &bridgeError{Code: -32600, Message: "invalid JSON-RPC request"}) })
			}
			continue
		}
		if method == "world/initialize" && frame.ID == nil {
			continue
		}
		if method == "world/initialize" && !initialized {
			var params struct {
				ProtocolVersion int    `json:"protocol_version"`
				HostName        string `json:"host_name"`
			}
			if err := decodeBridgeParams(frame.Params, &params); err != nil || params.ProtocolVersion != bridgeProtocolVersion {
				tasks.Go(func() {
					b.respond(ctx, frame.ID, nil, &bridgeError{Code: -32602, Message: "world/initialize requires protocol_version 1"})
				})
				continue
			}
			initialized = true
			tasks.Go(func() {
				if err := b.startNode(ctx, frame.ID); err != nil {
					stop(err)
				}
			})
			continue
		}
		initializedForRequest := initialized
		tasks.Go(func() {
			if !initializedForRequest {
				if frame.ID != nil {
					b.respond(ctx, frame.ID, nil, &bridgeError{Code: -32001, Message: "world/initialize is required"})
				}
				return
			}
			select {
			case <-b.ready:
			case <-ctx.Done():
				return
			}
			result, err := b.handleRequest(ctx, method, frame.Params)
			if frame.ID != nil {
				b.respond(ctx, frame.ID, result, bridgeRequestError(err))
			}
		})
	}
	<-streamsClosed
	tasks.Wait()
	if err := context.Cause(ctx); !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (b *worldBridge) startNode(ctx context.Context, id json.RawMessage) error {
	result := struct {
		ProtocolVersion int                            `json:"protocol_version"`
		NodeAddr        string                         `json:"node_addr"`
		DefaultTarget   string                         `json:"default_target"`
		Targets         []worldserver.AdvertisedTarget `json:"targets"`
	}{
		bridgeProtocolVersion, b.node.address.String(), b.node.opt.DefaultTarget,
		append([]worldserver.AdvertisedTarget{}, b.node.opt.AdvertiseTargets...),
	}
	if err := b.write(ctx, bridgeResponse{Version: "2.0", ID: id, Result: result}, true); err != nil {
		return err
	}
	initialSub := make(chan NodeNotification, 32)
	b.node.subscribers[initialSub] = struct{}{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var tasks sync.WaitGroup
	tasks.Go(func() {
		var sub <-chan NodeNotification = initialSub
		stopSubscription := func() {}
		defer func() { stopSubscription() }()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-sub:
				if !ok {
					// 恢复订阅，宿主可查询记录补齐积压时断开的事件。
					stopSubscription()
					subCtx, stop := context.WithCancel(ctx)
					nextSub, err := b.node.Subscribe(subCtx)
					if err != nil {
						stop()
						return
					}
					sub, stopSubscription = nextSub, stop
					continue
				}
				if err := b.write(ctx, bridgeRequest{Version: "2.0", Method: "world/event", Params: event}, false); err != nil {
					if !errors.Is(err, errBridgeFrameTooLarge) {
						return
					}
				}
			}
		}
	})
	close(b.ready)
	err := b.node.Run(ctx, b, nil)
	cancel()
	tasks.Wait()
	return err
}

func (b *worldBridge) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "world/initialize":
		return nil, &bridgeError{Code: -32600, Message: "world/initialize already completed"}
	case "world/status", "world/peers":
		if err := decodeBridgeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		if method == "world/status" {
			return b.node.GetStatus(ctx, "")
		}
		return b.node.ListPeers(ctx)
	case "world/send":
		request := struct {
			SendMessageRequest
			Notify bool `json:"notify"`
		}{SendMessageRequest: SendMessageRequest{Wait: true}}
		if err := decodeBridgeParams(params, &request); err != nil {
			return nil, err
		}
		return b.node.sendMessage(ctx, request.SendMessageRequest, request.Notify)
	case "world/message", "world/cancel":
		var request struct {
			MessageID            string  `json:"message_id"`
			SourceConversationID *string `json:"source_conversation_id"`
		}
		if err := decodeBridgeParams(params, &request); err != nil {
			return nil, err
		}
		if method == "world/cancel" {
			return b.node.CancelMessage(ctx, request.MessageID)
		}
		if request.MessageID == "" {
			return nil, &worldError{"invalid_arguments", "message_id is required"}
		}
		return b.node.getMessage(ctx, request.MessageID, request.SourceConversationID)
	case "world/messages":
		var request ListMessagesParams
		if err := decodeBridgeParams(params, &request); err != nil {
			return nil, err
		}
		page, err := b.node.ListMessages(ctx, request)
		if errors.Is(err, errEmptyClientCursorID) {
			return nil, &worldError{"invalid_arguments", "cursor_id must be nonempty"}
		}
		return page, err
	default:
		return nil, &bridgeError{Code: -32601, Message: "JSON-RPC method not found"}
	}
}

func (b *worldBridge) OpenSession(ctx context.Context, req OpenSessionRequest) (OpenedSession, error) {
	var opened OpenedSession
	err := b.call(ctx, "world/open", req, &opened)
	return opened, err
}

func (b *worldBridge) RunInboundTurn(ctx context.Context, req InboundTurnRequest, _ func()) (string, error) {
	msg := req.Message
	params := struct {
		SessionID string `json:"session_id"`
		MessageID string `json:"message_id"`
		ThreadID  string `json:"thread_id"`
		From      string `json:"from"`
		Target    string `json:"target"`
		Subject   string `json:"subject,omitempty"`
		Body      string `json:"body"`
		HopLimit  int    `json:"hop_limit"`
	}{req.OpenedSession.SessionID, msg.MessageID, msg.ThreadID, msg.PeerAddr, req.Target, msg.Subject, msg.Body, msg.HopLimit}
	var result struct {
		Reply *string `json:"reply"`
	}
	if err := b.call(ctx, "world/deliver", params, &result); err != nil {
		return "", err
	}
	if result.Reply == nil {
		return "", errors.New("world/deliver requires a reply string")
	}
	return *result.Reply, nil
}

func (b *worldBridge) NotifyOutbound(ctx context.Context, msg WorldMessage) error {
	ctx, cancel := context.WithTimeout(ctx, bridgeNotifyTimeout)
	defer cancel()
	params := struct {
		SessionID string             `json:"session_id"`
		MessageID string             `json:"message_id"`
		ThreadID  string             `json:"thread_id"`
		PeerAddr  string             `json:"peer_addr"`
		Status    WorldMessageStatus `json:"status"`
		ReplyBody string             `json:"reply_body,omitempty"`
		ErrorText string             `json:"error_text,omitempty"`
	}{msg.ConversationID, msg.MessageID, msg.ThreadID, msg.PeerAddr, msg.Status, msg.ReplyBody, msg.ErrorText}
	return b.call(ctx, "world/notify", params, &struct{}{})
}

func (b *worldBridge) call(ctx context.Context, method string, params, result any) error {
	id := strconv.FormatUint(b.nextID.Add(1), 10)
	reply := make(chan bridgeFrame, 1)
	b.pending.Store(id, reply)
	defer b.pending.Delete(id)
	request := bridgeRequest{Version: "2.0", ID: json.RawMessage(id), Method: method, Params: params}
	if err := b.write(ctx, request, false); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case frame := <-reply:
		if frame.Error != nil {
			var replyError bridgeError
			if err := json.Unmarshal(frame.Error, &replyError); err != nil {
				return fmt.Errorf("%s: invalid JSON-RPC error: %w", method, err)
			}
			return &replyError
		}
		return json.Unmarshal(frame.Result, result)
	}
}

func (b *worldBridge) respond(ctx context.Context, id json.RawMessage, result any, rpcErr *bridgeError) {
	response := bridgeResponse{Version: "2.0", ID: id, Result: result, Error: rpcErr}
	if rpcErr != nil {
		response.Result = nil
	}
	if err := b.write(ctx, response, false); errors.Is(err, errBridgeFrameTooLarge) {
		b.write(ctx, bridgeResponse{Version: "2.0", ID: id, Error: bridgeRequestError(err)}, false)
	}
}

func (b *worldBridge) write(ctx context.Context, frame any, flush bool) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(payload) > maxBridgeFrameBytes {
		return errBridgeFrameTooLarge
	}
	write := bridgeWrite{payload: append(payload, '\n')}
	if flush {
		write.flushed = make(chan struct{})
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.writes <- write:
	}
	if flush {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-write.flushed:
		}
	}
	return nil
}

type bridgeWrite struct {
	payload []byte
	flushed chan struct{}
}

type bridgeFrame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  json.RawMessage `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type bridgeRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params"`
}

type bridgeResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *bridgeError    `json:"error,omitempty"`
}

type bridgeError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *bridgeError) Error() string { return e.Message }

func bridgeRequestError(err error) *bridgeError {
	if err == nil {
		return nil
	}
	var rpcErr *bridgeError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if errors.Is(err, errBridgeFrameTooLarge) {
		return &bridgeError{Code: -32002, Message: err.Error()}
	}
	code := ErrorCode(err)
	data, _ := json.Marshal(map[string]string{"code": code})
	if code == "invalid_arguments" {
		return &bridgeError{Code: -32602, Message: err.Error(), Data: data}
	}
	return &bridgeError{Code: -32000, Message: err.Error(), Data: data}
}

func decodeBridgeParams(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] != '{' {
		return &worldError{"invalid_arguments", "JSON-RPC params must be an object"}
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return &worldError{"invalid_arguments", err.Error()}
	}
	return nil
}

func validBridgeID(id json.RawMessage) bool {
	return len(id) == 0 || id[0] == '"' || id[0] == 'n' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9')
}

func readBridgeFrame(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if err == nil {
			part = part[:len(part)-1]
		}
		if len(line)+len(part) > maxBridgeFrameBytes {
			return nil, errBridgeFrameTooLarge
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return line, nil
	}
}
