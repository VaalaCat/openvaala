package desktop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
)

// TestRPCLogPresentation 检查重复请求 ID 的连接归属与嵌套凭据遮罩。
func TestRPCLogPresentation(t *testing.T) {
	// 1. 相同 ID 在不同连接上独立配对。
	for _, connection := range []uint64{1, 2} {
		t.Run(string(rune('0'+connection)), func(t *testing.T) {
			frames := []runtimeclient.RPCFrame{{ConnectionID: 1, Direction: runtimeclient.RPCSent, Time: time.Unix(1, 0), Payload: json.RawMessage(`{"id":1,"method":"first"}`)}, {ConnectionID: 2, Direction: runtimeclient.RPCSent, Time: time.Unix(2, 0), Payload: json.RawMessage(`{"id":1,"method":"second"}`)}, {ConnectionID: connection, Direction: runtimeclient.RPCReceived, Time: time.Unix(3, 0), Payload: json.RawMessage(`{"id":1,"result":{}}`)}}
			rows := pairRPCFrames(frames)
			want := "first"
			if connection == 2 {
				want = "second"
			}
			if rows[2].Method != want {
				t.Fatalf("method=%s", rows[2].Method)
			}
		})
	}
	// 2. 展示与导出使用同一脱敏数据，不改变原始记录。
	data := []byte(`{"api_key":"secret","child":{"Authorization":"Bearer credential","token":"private","api_key":""},"message":"keep"}`)
	maskedJSON, err := marshalJSONView(maskRPCPayload(data))
	if err != nil {
		t.Fatal(err)
	}
	masked := string(maskedJSON)
	for _, secret := range []string{"secret", "credential", "private"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("secret visible: %s", masked)
		}
	}
	if !strings.Contains(masked, "keep") {
		t.Fatal("normal content lost")
	}
}
