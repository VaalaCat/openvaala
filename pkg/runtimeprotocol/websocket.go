package runtimeprotocol

import (
	"context"

	"github.com/coder/websocket"
)

// WebSocketMessages 通过普通 WebSocket 文本帧收发 RPC 消息。
type WebSocketMessages struct{ *websocket.Conn }

func (s WebSocketMessages) ReadMessage(ctx context.Context) ([]byte, error) {
	_, frame, err := s.Read(ctx)
	return frame, err
}
func (s WebSocketMessages) WriteMessage(ctx context.Context, frame []byte) error {
	return s.Write(ctx, websocket.MessageText, frame)
}
func (s WebSocketMessages) Close() error { return s.CloseNow() }
