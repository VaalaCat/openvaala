package runtimeserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/coder/websocket"
)

// TestGetRPCRequestID 检查请求 ID 的遥测文本。
func TestGetRPCRequestID(t *testing.T) {
	// 1. 准备输入与预期结果。
	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "number",
			id:   `42`,
			want: "42",
		},
		{
			name: "string_unquoted",
			id:   `"req-1"`,
			want: "req-1",
		},
		{
			name: "empty_string",
			id:   `""`,
			want: "",
		},
		{
			name: "string_with_escaped_quote",
			id:   `"a\"b"`,
			want: `a"b`,
		},
		{
			name: "null",
			id:   `null`,
			want: "",
		},
	}
	// 2. 逐项核对返回值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getRPCRequestID(json.RawMessage(tt.id)); got != tt.want {
				t.Fatalf("getRPCRequestID(%s) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// TestIsNormalConnectionClose 检查连接结束错误的正常与异常分类。
func TestIsNormalConnectionClose(t *testing.T) {
	// 1. 准备输入与预期结果。
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "normal_closure_frame",
			err:  fmt.Errorf("read frame: %w", websocket.CloseError{Code: websocket.StatusNormalClosure}),
			want: true,
		},
		{
			name: "going_away_frame",
			err:  fmt.Errorf("read frame: %w", websocket.CloseError{Code: websocket.StatusGoingAway}),
			want: true,
		},
		{
			name: "context_canceled",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped_context_canceled",
			err:  fmt.Errorf("read frame: %w", context.Canceled),
			want: true,
		},
		{
			name: "protocol_error_frame",
			err:  fmt.Errorf("read frame: %w", websocket.CloseError{Code: websocket.StatusProtocolError}),
			want: false,
		},
		{
			name: "message_too_big_frame",
			err:  fmt.Errorf("read frame: %w", websocket.CloseError{Code: websocket.StatusMessageTooBig}),
			want: false,
		},
		{
			name: "connection_dropped",
			err:  fmt.Errorf("read frame: %w", io.EOF),
			want: false,
		},
		{
			name: "ping_timeout",
			err:  fmt.Errorf("ping: %w", context.DeadlineExceeded),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "other_error",
			err:  errors.New("write message: broken pipe"),
			want: false,
		},
	}
	// 2. 逐项核对返回值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNormalConnectionClose(tt.err); got != tt.want {
				t.Fatalf("isNormalConnectionClose(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestGetPeerAddress 检查对端地址的主机与端口。
func TestGetPeerAddress(t *testing.T) {
	// 1. 准备输入与预期结果。
	tests := []struct {
		name       string
		remoteAddr string
		wantHost   string
		wantPort   int
	}{
		{
			name:       "ipv4",
			remoteAddr: "127.0.0.1:54321",
			wantHost:   "127.0.0.1",
			wantPort:   54321,
		},
		{
			name:       "ipv6",
			remoteAddr: "[::1]:8080",
			wantHost:   "::1",
			wantPort:   8080,
		},
		{
			name:       "without_port_kept_as_is",
			remoteAddr: "pipe",
			wantHost:   "pipe",
		},
	}
	// 2. 逐项核对返回值。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if host, port := getPeerAddress(tt.remoteAddr); host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("getPeerAddress(%q) = %q, %d; want %q, %d", tt.remoteAddr, host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestOpenVaalaMethodsMatchProtocol 检查方法表与 runtimeprotocol.Methods 的方法名、参数类型和结果类型一一对应。
func TestOpenVaalaMethodsMatchProtocol(t *testing.T) {
	// 1. 逐个协议方法核对方法表里登记的处理函数。
	for _, method := range runtimeprotocol.Methods {
		t.Run(method.Name, func(t *testing.T) {
			handler, found := openvaalaMethods[method.Name]
			if !found {
				t.Fatalf("method %q is not registered in openvaalaMethods", method.Name)
			}
			if handler.params != method.Params {
				t.Fatalf("params of %q = %v, want %v", method.Name, handler.params, method.Params)
			}
			if handler.result != method.Result {
				t.Fatalf("result of %q = %v, want %v", method.Name, handler.result, method.Result)
			}
		})
	}
	// 2. 方法表里没有协议之外的方法。
	if len(openvaalaMethods) != len(runtimeprotocol.Methods) {
		t.Fatalf("openvaalaMethods has %d methods, runtimeprotocol.Methods has %d", len(openvaalaMethods), len(runtimeprotocol.Methods))
	}
}
