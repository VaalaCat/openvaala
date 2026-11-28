package worldserver

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func TestWorldRelayAttachmentReplacement(t *testing.T) {
	for _, tc := range []struct{ first, second string }{
		{"relay/attach", "relay/attach"}, {"relay/attach", "connect"}, {"connect", "relay/attach"},
	} {
		t.Run(tc.first+" to "+tc.second, func(t *testing.T) {
			server := serveWorldForTest(t)
			id, key := registerWorldNode(t, server, "node", "team", nil)
			attach := func(path string) *websocket.Conn {
				t.Helper()
				s, _, err := websocket.Dial(t.Context(), server+"/world/v1/"+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.CloseNow() })
				purpose := ChallengeRelayAttach
				if path == "connect" {
					purpose = ChallengeConnect
				}
				challenge := receiveWorldPayload[ChallengeMessage](t, s, ControlTypeChallenge)
				sendWorldProof(t, s, purpose, challenge.Challenge, id, key, time.Now().Unix())
				readWorldControl(t, s, ControlTypeRelayAttached)
				return s
			}
			first := attach(tc.first)
			second := attach(tc.second)
			if tc.first == "connect" {
				writeWorldControl(t, first, ControlTypePing, nil)
				readWorldControl(t, first, ControlTypePong)
			} else {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if _, _, err := first.Read(ctx); err == nil || ctx.Err() != nil {
					t.Fatalf("replaced attach: %v", err)
				}
			}
			dial, _, err := websocket.Dial(t.Context(), server+"/world/v1/relay/dial?target="+relayKey(key.Public().(ed25519.PublicKey)), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer dial.CloseNow()
			if got := receiveWorldPayload[RelayIncoming](t, second, ControlTypeRelayIncoming); got.ConnectionID == "" {
				t.Fatal(got)
			}
		})
	}
}

func TestRelayInstances(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []string
		want      string
	}{
		{"single", nil, `{"instances":[]}`},
		{"cluster", []string{"ws://one.test", "wss://two.test/base"}, `{"instances":["ws://one.test","wss://two.test/base"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			NewRelay(tc.instances).RegisterRoutes(router)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/world/v1/relay/instances", nil))
			if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "*" || response.Body.String() != tc.want {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
			}
		})
	}
}
