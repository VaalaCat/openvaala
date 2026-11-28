package world

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func TestDialPeerHandshakeBudget(t *testing.T) {
	peer := testNodeIdentity(t)
	for _, tc := range []struct {
		name     string
		budget   time.Duration
		fallback bool
	}{
		{"direct budget", 5 * time.Second, true}, {"caller cancellation", 100 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var direct, relay atomic.Int32
			stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				direct.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if r.URL.Query().Get("purpose") != "rpc" || r.URL.Query().Get("target") == "" {
					t.Error("missing direct routing hints")
				}
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
				}
			}))
			t.Cleanup(stall.Close)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/world/v1/relay/dial" {
					http.NotFound(w, r)
					return
				}
				relay.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				accepted, err := AcceptSecureChannel(r.Context(), &directFrameTransport{conn}, peer)
				if err != nil {
					return
				}
				defer accepted.Channel.Close()
				_ = accepted.Channel.WriteMessage(r.Context(), []byte("ready"))
			}))
			t.Cleanup(endpoint.Close)
			ctx, cancel := context.WithTimeout(t.Context(), tc.budget)
			defer cancel()
			ch, err := new(PeerDialer).DialPeer(ctx, "bob@home", peer.SignPublic, worldserver.PeerEntries{DirectURLs: []string{stall.URL, stall.URL}, Relays: []string{endpoint.URL}}, nil, ChannelPurposeRPC)
			if !tc.fallback {
				if !errors.Is(err, context.DeadlineExceeded) || relay.Load() != 0 {
					t.Fatalf("dial=%v relays=%d", err, relay.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			got, err := ch.ReadMessage(ctx)
			if err != nil || string(got) != "ready" || direct.Load() != 1 || relay.Load() != 1 {
				t.Fatalf("got=%q direct=%d relay=%d, %v", got, direct.Load(), relay.Load(), err)
			}
		})
	}
}

func TestDialPeerCachesFailedRelayInstances(t *testing.T) {
	peer := testNodeIdentity(t)
	var lookups atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/world/v1/relay/instances":
			lookups.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case "/world/v1/relay/dial":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			accepted, err := AcceptSecureChannel(r.Context(), &directFrameTransport{conn}, peer)
			if err != nil {
				return
			}
			defer accepted.Channel.Close()
			_ = accepted.Channel.WriteMessage(r.Context(), []byte("ready"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(endpoint.Close)
	dialer := new(PeerDialer)
	for range 2 {
		ch, err := dialer.DialPeer(t.Context(), "bob@home", peer.SignPublic, worldserver.PeerEntries{Relays: []string{endpoint.URL}}, nil, ChannelPurposeRPC)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ch.ReadMessage(t.Context())
		ch.Close()
		if err != nil || string(got) != "ready" {
			t.Fatalf("got=%q, %v", got, err)
		}
	}
	if lookups.Load() != 1 {
		t.Fatalf("instance lookups=%d, want 1", lookups.Load())
	}
}

func TestGetRelayInstances(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       []string
	}{
		{"published", `{"instances":["wss://one.test","ws://two.test/base"]}`, 200, []string{"wss://one.test", "ws://two.test/base"}},
		{"single", `{"instances":[]}`, 200, nil},
		{"unpublished", "404", 404, nil},
		{"timeout", "", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/world/v1/relay/instances" || r.Method != http.MethodGet {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				if tc.status == 0 {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			got, err := getRelayInstances(t.Context(), "ws"+strings.TrimPrefix(s.URL, "http")+"/prefix")
			if tc.status == 0 {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("timeout: %v", err)
				}
			} else if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("instances=%v, err=%v", got, err)
			}
		})
	}
}

// TestRelayAttachmentRefresh 验证重连时保留旧挂靠，收到新实例的挂靠确认后才切换。
func TestRelayAttachmentRefresh(t *testing.T) {
	f := newTestFederation(t, "off")
	instances := make([]string, 3)
	cancelRelay := make([]context.CancelFunc, len(instances))
	replacing := make(chan struct{})
	allowAttach := make(chan struct{})
	for i := range instances {
		relay := worldserver.NewRelay(nil)
		router := gin.New()
		relay.RegisterRoutes(router)
		var started sync.Once
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i == 2 && r.URL.Path == "/world/v1/relay/attach" {
				started.Do(func() { close(replacing) })
				select {
				case <-allowAttach:
				case <-r.Context().Done():
					return
				}
			}
			router.ServeHTTP(w, r)
		}))
		t.Cleanup(s.Close)
		instances[i] = "ws" + strings.TrimPrefix(s.URL, "http")
		ctx, cancel := context.WithCancel(t.Context())
		cancelRelay[i] = cancel
		done := make(chan struct{})
		go func() { defer close(done); _ = relay.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	var published atomic.Pointer[[]string]
	discoveryFailed := make(chan struct{}, 1)
	initial := instances[:2]
	published.Store(&initial)
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/world/v1/relay/instances" {
			http.NotFound(w, r)
			return
		}
		if list := published.Load(); list != nil {
			_ = json.NewEncoder(w).Encode(worldserver.RelayInstancesResult{Instances: *list})
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			select {
			case discoveryFailed <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(entry.Close)
	node := f.node(t, "receiver", "", entry.URL)
	node.start(t)
	target := node.opt.Identity.SignPublic
	probe := func(base string) bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		ch, err := dialPeerChannel(ctx, base, "/world/v1/relay/dial", "target="+base64.RawURLEncoding.EncodeToString(target), nil, target, ChannelPurposeRPC)
		if err != nil {
			return false
		}
		ch.Close()
		return true
	}
	awaitWorld(t, func() bool { return probe(instances[0]) && probe(instances[1]) })
	// 查询暂时失败，另一个旧实例仍可配对。重试发现新列表时暂缓它的挂靠。
	published.Store(nil)
	cancelRelay[0]()
	select {
	case <-discoveryFailed:
	case <-time.After(8 * time.Second):
		t.Fatal("reconnection did not refresh the instance list")
	}
	awaitWorld(t, func() bool { return !probe(instances[0]) && probe(instances[1]) })
	next := instances[2:]
	published.Store(&next)
	select {
	case <-replacing:
	case <-time.After(8 * time.Second):
		t.Fatal("new instance did not receive attach")
	}
	if !probe(instances[1]) {
		t.Fatal("previous attachment closed before replacement")
	}
	close(allowAttach)
	awaitWorld(t, func() bool { return probe(instances[2]) && !probe(instances[1]) })
}
