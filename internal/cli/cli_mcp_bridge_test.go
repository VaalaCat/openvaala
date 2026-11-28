package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRelayMCPFrame(t *testing.T) {
	const notification = `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`
	const response = `{"jsonrpc":"2.0","id":"peer-id","result":{"extension":[1,2]}}`
	const rpcError = `{"jsonrpc":"2.0","id":"peer-id","error":{"code":-32601,"message":"unknown method"}}`
	for _, tc := range []struct {
		name, frame, protocolVersion string
		status                       int
		contentType, body            string
		want                         []string
		wantErr                      bool
	}{
		{
			name: "json", frame: `{"jsonrpc":"2.0","id":"peer-id","method":"tools/call", "params":{"_meta":{"protocolVersion":"2026-07-28"},"name":"custom_tool"}}`,
			protocolVersion: "2026-07-28", status: http.StatusOK, contentType: "application/json; charset=utf-8",
			body: response, want: []string{response},
		},
		{
			name: "event stream", frame: `{"jsonrpc":"2.0","id":"peer-id","method":"tools/call","params":{"name":"custom_tool"}}`,
			status: http.StatusOK, contentType: "text/event-stream",
			body: ": heartbeat\r\n\r\nevent: message\r\ndata: " + notification + "\r\n\r\nid: 1\ndata: " + response + "\n\n",
			want: []string{notification, response},
		},
		{
			name: "notification", frame: `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			status: http.StatusAccepted,
		},
		{
			name: "rpc error", frame: `{"jsonrpc":"2.0","id":"peer-id","method":"custom/request"}`,
			status: http.StatusNotFound, contentType: "application/json", body: rpcError, want: []string{rpcError},
		},
		{
			name: "http error", frame: `{"jsonrpc":"2.0","id":"peer-id","method":"initialize"}`,
			status: http.StatusUnauthorized, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, []byte(tc.frame)) {
					t.Errorf("request body = %q, error = %v", body, err)
				}
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-session-token" ||
					r.Header.Get("Accept") != "application/json, text/event-stream" || r.Header.Get("Content-Type") != "application/json" ||
					r.Header.Get("Mcp-Protocol-Version") != tc.protocolVersion {
					t.Errorf("unexpected HTTP request for %s", tc.name)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()

			var frames []string
			err := relayMCPFrame(t.Context(), server.Client(), server.URL, "test-session-token", []byte(tc.frame), func(payload []byte) error {
				frames = append(frames, string(payload))
				return nil
			})
			if (err != nil) != tc.wantErr || !slices.Equal(frames, tc.want) {
				t.Fatalf("frames = %q, error = %v", frames, err)
			}
		})
	}
}
