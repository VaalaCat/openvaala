package world

import (
	"crypto/ed25519"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func TestWorldRemoteAccessLink(t *testing.T) {
	target := RemoteAccessTarget{Node: "b", Domain: "home.test", PublicKey: make(ed25519.PublicKey, 32), Entries: worldserver.PeerEntries{Relays: []string{"wss://relay.test/prefix"}}}
	for _, tc := range []struct{ name, base, want string }{
		{"default", "", "https://relay.test/web/v1/#world="},
		{"custom", "http://example.test:8080", "http://example.test:8080/web/v1/#world="},
		{"page path", "https://example.test/ui", ""},
		{"credentials", "https://user@example.test", ""},
		{"query", "https://example.test/?token=x", ""},
		{"page scheme", "ws://example.test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := FormatRemoteAccessURL(tc.base, target)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid page accepted")
				}
				return
			}
			if err != nil || !strings.HasPrefix(link, tc.want) {
				t.Fatalf("link %q: %v", link, err)
			}
			got, err := ParseRemoteAccessURL(link)
			if err != nil || !reflect.DeepEqual(got, target) {
				t.Fatalf("decoded %+v: %v", got, err)
			}
			other := target
			other.Node = "a"
			second, err := FormatRemoteAccessURL(tc.base, other)
			if err != nil || second == link {
				t.Fatalf("second node link: %q %v", second, err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*RemoteAccessTarget)
	}{
		{"key", func(t *RemoteAccessTarget) { t.PublicKey = nil }},
		{"entries", func(t *RemoteAccessTarget) {
			t.Entries = worldserver.PeerEntries{DirectURLs: []string{"ftp://invalid.test"}}
		}},
		{"node", func(t *RemoteAccessTarget) { t.Node = "b/a" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := target
			tc.change(&v)
			if _, err := FormatRemoteAccessURL("https://page.test", v); err == nil {
				t.Fatal("invalid target accepted")
			}
		})
	}
}

func TestRemoteAccessOnboarding(t *testing.T) {
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for _, tc := range []struct {
		name, page     string
		relays, direct []string
		want           string
		local          bool
	}{
		{"override", "https://web.example", []string{"https://relay.example"}, nil, "https://web.example", false},
		{"public relay", "", []string{"http://127.0.0.1", "https://relay.example/path"}, []string{"https://direct.example"}, "https://relay.example", false},
		{"public direct", "", []string{"https://192.168.0.2"}, []string{"https://direct.example/world/v1/direct"}, "https://direct.example", false},
		{"local HTTPS direct", "", []string{"http://localhost"}, []string{"https://192.168.0.2"}, "https://192.168.0.2", true},
		{"local only", "", []string{"http://localhost:8080"}, nil, "http://localhost:8080", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := RemoteAccessTarget{PublicKey: key, Name: "studio", Entries: worldserver.PeerEntries{Relays: tc.relays, DirectURLs: tc.direct}, Token: "a+&=% b"}
			link, err := FormatRemoteAccessURL(tc.page, target)
			if err != nil || !strings.HasPrefix(link, tc.want+"/web/v1/#world=") || IsLocalPageURL(link) != tc.local {
				t.Fatalf("page selection: %q %v", link, err)
			}
			parsed, err := ParseRemoteAccessURL(link)
			if err != nil || !reflect.DeepEqual(parsed, target) {
				t.Fatalf("target: %+v %v", parsed, err)
			}
		})
	}
}
