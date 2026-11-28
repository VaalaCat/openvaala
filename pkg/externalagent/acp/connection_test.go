package acp

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestACPAuthStatusUpdate(t *testing.T) {
	const notification = `{"jsonrpc":"2.0","method":"auth/status_update","params":{}}` + "\n"
	const request = `{"jsonrpc":"2.0","id":1,"method":"auth/status_update","params":{}}` + "\n"
	const response = `{"jsonrpc":"2.0","id":2,"result":{}}` + "\n"
	for _, tc := range []struct{ name, input, want string }{
		{"notification before response", notification + response, response},
		{"request goes to SDK", request, request},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &notificationReader{scanner: bufio.NewScanner(strings.NewReader(tc.input))}
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != tc.want {
				t.Fatalf("SDK input = %q, %v, want %q", data, err, tc.want)
			}
		})
	}
}
