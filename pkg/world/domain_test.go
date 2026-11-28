package world

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDomainLookupResponseDeadline(t *testing.T) {
	home, peer := testNodeIdentity(t), testNodeIdentity(t)
	local := worldserver.DomainIdentity{SignPublic: home.SignPublic, SignPrivate: home.SignPrivate, BoxPublic: home.BoxPublic, BoxPrivate: home.BoxPrivate}
	for _, tc := range []struct {
		name          string
		callerTimeout time.Duration
		callerExpired bool
	}{
		{"query deadline", 2 * inboundChannelTimeout, false},
		{"caller deadline", time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := make(chan domainQuery, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				accepted, err := AcceptSecureChannel(r.Context(), &directFrameTransport{socket}, peer)
				if err != nil {
					return
				}
				defer accepted.Channel.Close()
				data, err := accepted.Channel.ReadMessage(r.Context())
				var query domainQuery
				if err == nil && json.Unmarshal(data, &query) == nil {
					queries <- query
					<-socket.CloseRead(r.Context()).Done()
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), tc.callerTimeout)
			defer cancel()
			card := worldserver.PeerDomain{Domain: "peer.test", PublicKey: peer.SignPublic, Entries: worldserver.PeerEntries{DirectURLs: []string{server.URL}}}
			_, err := NewDomainChannels().Lookup(ctx, local, card, "bob", nil)
			if !errors.Is(err, context.DeadlineExceeded) || (ctx.Err() != nil) != tc.callerExpired {
				t.Fatalf("lookup error = %v, caller error = %v", err, ctx.Err())
			}
			select {
			case got := <-queries:
				if got.Op != "lookup" || got.Node != "bob" {
					t.Fatal(got)
				}
			default:
				t.Fatal("peer did not receive lookup")
			}
		})
	}
}

// TestDomainLookupRequests 在同一条控制连接上交错两个代查，断线时两个等待者都应返回。
func TestDomainLookupRequests(t *testing.T) {
	for _, mode := range []string{"reversed responses", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			remote := make(chan *websocket.Conn, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err == nil {
					remote <- socket
				}
			}))
			defer server.Close()
			socket, _, err := websocket.Dial(ctx, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.CloseNow()
			control := <-remote
			defer control.CloseNow()
			card := worldserver.PeerDomain{Domain: "peer.test", PublicKey: make([]byte, 32)}
			node := &worldNodeImpl{control: socket, controlDone: make(chan struct{}), domains: map[string]worldserver.PeerDomain{card.Domain: card}}
			type lookup struct {
				node   string
				member worldserver.DomainMember
				err    error
			}
			results := make(chan lookup, 2)
			done := node.controlDone
			for _, name := range []string{"one", "two"} {
				go func() {
					member, err := node.lookupMember(ctx, Address{Node: name, Domain: card.Domain}, card, socket, done, false)
					results <- lookup{name, member, err}
				}()
			}
			requests := make([]worldserver.DomainLookup, 2)
			for i := range requests {
				var frame worldserver.ControlEnvelope
				if err = wsjson.Read(ctx, control, &frame); err != nil || frame.Type != worldserver.ControlTypeDomainLookup || json.Unmarshal(frame.Payload, &requests[i]) != nil {
					t.Fatal(frame, err)
				}
			}
			if requests[0].RequestID == "" || requests[0].RequestID == requests[1].RequestID {
				t.Fatal(requests)
			}
			if mode == "disconnect" {
				control.CloseNow()
				if err = node.handleControl(ctx, controlReceived{socket: socket}); err != nil {
					t.Fatal(err)
				}
			} else {
				for i := len(requests) - 1; i >= 0; i-- {
					req := requests[i]
					reply := worldserver.DomainLookupResult{RequestID: req.RequestID, DomainKey: card.PublicKey, Member: &worldserver.DomainMember{PublicKey: make([]byte, 32), Owner: req.Node}}
					if err = writeControl(ctx, control, worldserver.ControlTypeDomainLookupResult, reply); err != nil {
						t.Fatal(err)
					}
					var frame worldserver.ControlEnvelope
					if err = wsjson.Read(ctx, socket, &frame); err != nil {
						t.Fatal(err)
					}
					if err = node.handleControl(ctx, controlReceived{socket, frame}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range requests {
				select {
				case got := <-results:
					if mode == "disconnect" {
						if ErrorCode(got.err) != "world_identity_unverified" || got.err.Error() != fmt.Sprintf("cannot verify %q: its domain control plane is unreachable", got.node+"@peer.test") {
							t.Fatal(got)
						}
					} else if got.err != nil || got.member.Owner != got.node {
						t.Fatal(got)
					}
				case <-ctx.Done():
					t.Fatal("lookup waiter did not return")
				}
			}
		})
	}
}

// TestDomainLookupCache 统计控制连接实际收到的查人帧，按节点时钟验证缓存期限。
func TestDomainLookupCache(t *testing.T) {
	for _, tc := range []struct {
		name         string
		offlineFirst bool
	}{
		{"one hour", false}, {"offline then online", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			card := worldserver.PeerDomain{Domain: "peer.test", PublicKey: testNodeIdentity(t).SignPublic}
			member := worldserver.DomainMember{Owner: "team", PublicKey: testNodeIdentity(t).SignPublic, Entries: worldserver.PeerEntries{Relays: []string{"ws://relay.test"}}}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				socket, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer socket.CloseNow()
				for {
					var frame worldserver.ControlEnvelope
					if wsjson.Read(ctx, socket, &frame) != nil {
						return
					}
					var req worldserver.DomainLookup
					if frame.Type != worldserver.ControlTypeDomainLookup || json.Unmarshal(frame.Payload, &req) != nil || req.Domain != card.Domain || req.Node != "alice" {
						t.Errorf("lookup frame: %+v", frame)
						return
					}
					found := member
					if requests.Add(1) == 1 && tc.offlineFirst {
						found.Entries = worldserver.PeerEntries{}
					}
					reply := worldserver.DomainLookupResult{RequestID: req.RequestID, DomainKey: card.PublicKey, Member: &found}
					if writeControl(ctx, socket, worldserver.ControlTypeDomainLookupResult, reply) != nil {
						return
					}
				}
			}))
			defer server.Close()
			socket, _, err := websocket.Dial(ctx, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.CloseNow()
			replies := make(chan worldserver.ControlEnvelope)
			go func() {
				for {
					var frame worldserver.ControlEnvelope
					if wsjson.Read(ctx, socket, &frame) != nil {
						return
					}
					select {
					case replies <- frame:
					case <-ctx.Done():
						return
					}
				}
			}()
			synctest.Test(t, func(t *testing.T) {
				node := &worldNodeImpl{}
				node.updateDomains([]worldserver.PeerDomain{card})
				lookup := func(needEntries bool) {
					t.Helper()
					done := make(chan error, 1)
					go func() {
						_, err := node.lookupMember(ctx, Address{Node: "alice", Domain: card.Domain}, card, socket, nil, needEntries)
						done <- err
					}()
					for {
						select {
						case err := <-done:
							if err != nil {
								t.Fatal(err)
							}
							return
						case reply := <-replies:
							node.handleDomainLookupResult(reply.Payload)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
					}
				}
				lookup(true)
				if requests.Load() != 1 {
					t.Fatal("first lookup", requests.Load())
				}
				if tc.offlineFirst {
					lookup(false)
					if requests.Load() != 1 {
						t.Fatal("identity without entries was not cached")
					}
					lookup(true)
					if requests.Load() != 2 {
						t.Fatal("empty entries suppressed lookup", requests.Load())
					}
				}
				warm := requests.Load()
				time.Sleep(59*time.Minute + 59*time.Second)
				lookup(true)
				if requests.Load() != warm {
					t.Fatal("early lookup", requests.Load())
				}
				time.Sleep(time.Second)
				lookup(true)
				if requests.Load() != warm+1 {
					t.Fatal("missing lookup at expiry", requests.Load())
				}
				node = &worldNodeImpl{}
				node.updateDomains([]worldserver.PeerDomain{card})
				lookup(true)
				if requests.Load() != warm+2 {
					t.Fatal("missing lookup after restart", requests.Load())
				}
			})
		})
	}
}
