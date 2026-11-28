package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type worldSettingsTestClient struct {
	runtimeclient.Client
	files *config.ConfigFiles
}

func (c *worldSettingsTestClient) SetWorldSettings(_ context.Context, p runtimeprotocol.SetWorldParams) (runtimeprotocol.SetWorldResult, error) {
	update, err := c.files.PrepareWorldSettings(p.Settings, false)
	if err == nil {
		err = c.files.SaveWorldSettings(update, nil, "")
	}
	return runtimeprotocol.SetWorldResult{Restarting: err == nil}, err
}

func TestWorldDesktopRemoteAccess(t *testing.T) {
	for _, tc := range []struct {
		server              bool
		initial, mode, want string
	}{
		{true, "off", "domain", "domain"},
		{true, "domain", "off", "off"},
		{true, "domain", "public", "public"},
		{false, "off", "domain", "off"},
		{false, "public", "off", "off"},
		{false, "off", "public", "public"},
	} {
		t.Run(fmt.Sprintf("server_%t_%s_to_%s", tc.server, tc.initial, tc.mode), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("world: {relay_mode: %s, server: {enabled: %t, domain: home.test, admin_token: example}}", tc.initial, tc.server)), 0600); err != nil {
				t.Fatal(err)
			}
			files := config.NewConfigFiles(&config.LoadConfigOpt{Path: path, Home: home})
			saved, err := files.GetWorldSettings()
			if err != nil {
				t.Fatal(err)
			}
			d := testDesktop()
			d.window = &mygo.Window{}
			d.preferences.Language = "zh-CN"
			d.client = &worldSettingsTestClient{files: files}
			d.page = "world"
			d.worldSaved = &runtimeprotocol.GetWorldResult{Settings: saved, Running: runtimeprotocol.WorldRunning{ServerEnabled: tc.server}}
			d.resetWorldDraft()
			tester := ui.NewTester(d.view, 1280, 1450)
			tester.SetLocale("zh-Hans")
			for _, mode := range []string{"off", "domain", "public"} {
				if !tester.HasText(d.t("world.mode_" + mode)) {
					t.Fatalf("missing mode %s", mode)
				}
			}
			if err := tester.Click(d.t("world.mode_" + tc.mode)); err != nil {
				t.Fatal(err)
			}
			if tc.want != tc.initial {
				if err := tester.Click(d.t("world.save")); err != nil {
					t.Fatal(err)
				}
				if !tester.HasText(d.t("world.confirm")) {
					t.Fatal("missing restart confirmation")
				}
				tester.Key(0, ui.KeyEnter)
				d.tasks.Wait()
			} else if tester.HasText(d.t("world.save")) {
				t.Fatal("disabled mode changed the draft")
			}
			got, err := files.GetWorldSettings()
			if err != nil || got.RelayMode != tc.want {
				t.Fatalf("saved relay mode = %q, want %q: %v", got.RelayMode, tc.want, err)
			}
		})
	}
}

func TestWorldRemoteServerProfile(t *testing.T) {
	link, err := world.FormatRemoteAccessURL("", world.RemoteAccessTarget{Node: "b", Domain: "home", PublicKey: make([]byte, 32), Entries: worldserver.PeerEntries{Relays: []string{"https://relay.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	d := testDesktop()
	d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
	d.serverForm.URL, d.serverForm.Token = link, "secret"
	d.saveServerForm(false)
	d.preferences.RemoteServers = nil
	if err := d.loadPreferences(); err != nil {
		t.Fatal(err)
	}
	want := remoteServer{ID: "remote-1", URL: link, Token: "secret"}
	if len(d.preferences.RemoteServers) != 1 || d.preferences.RemoteServers[0] != want {
		t.Fatalf("saved servers: %+v", d.preferences.RemoteServers)
	}
	if name, address := getServerDisplay(want); name != "relay.test" || address != "b@home" {
		t.Fatalf("server display = %q, %q", name, address)
	}

}
