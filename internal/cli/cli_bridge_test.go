package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func TestBridgeCLI(t *testing.T) {
	for _, tc := range []struct {
		name, id, visibility, dataDirectory string
		blockOutput                         bool
	}{
		{name: "external target and protocol-only stdout", id: "init", visibility: "domain", dataDirectory: "bridge-instance"},
		{name: "SIGTERM while stdout is blocked", id: strings.Repeat("x", 1<<20), visibility: "domain", blockOutput: true},
		{name: "reject invalid visibility", id: "init", visibility: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := httptest.NewServer(http.NotFoundHandler())
			defer control.Close()
			home := t.TempDir()
			path := filepath.Join(home, "bridge.yaml")
			payload := fmt.Sprintf(`world:
  node:
    enabled: true
    name: external
    domain: example.test
    server_url: %s
    default_target: host-only
    advertise_targets:
      - name: host-only
        visibility: %s
`, control.URL, tc.visibility)
			if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBridgeCLIProcess$")
			cmd.Env = append(os.Environ(), "HOME="+home, "OPENVAALA_TEST_BRIDGE_CONFIG="+path)
			if tc.dataDirectory != "" {
				cmd.Env = append(cmd.Env, "OPENVAALA_DATA_DIR="+tc.dataDirectory)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cmd.Process.Kill()
				cmd.Wait()
			})
			if tc.visibility == "invalid" {
				output, err := io.ReadAll(stdout)
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err == nil || len(output) != 0 || !strings.Contains(stderr.String(), "invalid exposed world target") {
					t.Fatalf("invalid target: exit=%v, stdout=%q, stderr=%q", err, output, stderr.String())
				}
				return
			}
			if _, err = fmt.Fprintf(stdin, "{\"jsonrpc\":\"2.0\",\"id\":%q,\"method\":\"world/initialize\",\"params\":{\"protocol_version\":1}}\n", tc.id); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			if tc.blockOutput {
				// 只读首字节，让大握手响应堵塞在 OS 管道上。
				if got, err := reader.ReadByte(); err != nil || got != '{' {
					t.Fatalf("first byte = %q: %v", got, err)
				}
			} else {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					Version string `json:"jsonrpc"`
					ID      string `json:"id"`
					Result  struct {
						ProtocolVersion int    `json:"protocol_version"`
						DefaultTarget   string `json:"default_target"`
					} `json:"result"`
				}
				if err := json.Unmarshal(line, &response); err != nil || response.Version != "2.0" ||
					response.ID != tc.id || response.Result.ProtocolVersion != 1 || response.Result.DefaultTarget != "host-only" {
					t.Fatalf("handshake stdout = %s, err=%v", line, err)
				}
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("bridge did not shut down on SIGTERM: %v, stderr=%s", err, stderr.String())
			}
			if tc.dataDirectory != "" {
				for _, filename := range []string{"identity.json", "world.db"} {
					if _, err := os.Stat(filepath.Join(home, tc.dataDirectory, "world", filename)); err != nil {
						t.Fatal(err)
					}
				}
				assertWorldDefaultDirectoriesAbsent(t, home)
			}
		})
	}
}

func TestBridgeCLIProcess(t *testing.T) {
	path := os.Getenv("OPENVAALA_TEST_BRIDGE_CONFIG")
	if path == "" {
		t.Skip("requires bridge subprocess configuration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	command := NewCLI(&CLIOpt{Stdout: os.Stdout, Stderr: os.Stderr})
	command.SetArgs([]string{"bridge", "--config-file", path})
	if err := command.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestBridgeCLIJoinExternalTarget(t *testing.T) {
	for _, tc := range []struct{ name, target, defaultSetting string }{
		{"external target", "host-only", "    default_target: host-only\n"},
		{"single default target", "default", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			listen := getFreeServeAddress(t)
			base := "http://" + listen
			path := newServeConfig(t, "http://127.0.0.1:1", listen, serveTestToken)
			appendConfigText(t, path, "world:\n  server: {enabled: true, domain: example.test, admin_token: test-admin}\n")
			serve := startTestServe(t, path, listen)
			waitWorldHTTP(t, base+"/world/v1/healthz")
			var ticket worldserver.CreateJoinTokenResponse
			worldCommandResult(t, path, &ticket, "token", "create", "--server", base, "--admin-token", "test-admin", "--owner", "team", "--reusable")
			worldCommandResult(t, path, nil, "join", ticket.Token, "--name", "sender")

			hostHome := t.TempDir()
			hostPath := filepath.Join(hostHome, "bridge.yaml")
			hostConfig := "world:\n  node:\n" + tc.defaultSetting +
				"    advertise_targets: [{name: " + tc.target + ", visibility: domain}]\n"
			if err := os.WriteFile(hostPath, []byte(hostConfig), 0600); err != nil {
				t.Fatal(err)
			}
			senderHome := os.Getenv("HOME")
			t.Setenv("HOME", hostHome)
			worldCommandResult(t, hostPath, nil, "join", ticket.Token, "--name", "bridge")
			t.Setenv("HOME", senderHome)
			if err := serve.stop(t); err != nil {
				t.Fatal(err)
			}
			startTestServe(t, path, listen)
			waitWorldHTTP(t, base+"/world/v1/healthz")

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBridgeCLIProcess$")
			cmd.Env = append(os.Environ(), "HOME="+hostHome, "OPENVAALA_TEST_BRIDGE_CONFIG="+hostPath)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
			_, err = io.WriteString(stdin, `{"jsonrpc":"2.0","id":"init","method":"world/initialize","params":{"protocol_version":1}}`+"\n")
			if err != nil {
				t.Fatal(err)
			}
			read := json.NewDecoder(stdout)
			type frame struct {
				ID     json.RawMessage
				Method string
				Params struct{ Target string }
				Result struct {
					DefaultTarget string `json:"default_target"`
				}
			}
			var initialized frame
			if err := read.Decode(&initialized); err != nil || initialized.Result.DefaultTarget != tc.target {
				t.Fatalf("initialize = %+v (%v)", initialized, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var peers struct{ Peers []worldserver.DirectoryNode }
				worldCommandResult(t, path, &peers, "peers")
				online := false
				for _, peer := range peers.Peers {
					if peer.NodeName == "bridge" && peer.Online && len(peer.Targets) > 0 {
						online = true
					}
				}
				if online {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bridge did not join directory")
				}
				time.Sleep(20 * time.Millisecond)
			}
			var sent world.WorldMessage
			worldCommandResult(t, path, &sent, "send", "bridge", "hello")
			for _, method := range []string{"world/open", "world/deliver"} {
				var incoming frame
				for incoming.Method != method {
					incoming = frame{}
					if err := read.Decode(&incoming); err != nil {
						t.Fatal(err)
					}
					if incoming.Method != method && incoming.Method != "world/event" {
						t.Fatalf("unexpected callback %q", incoming.Method)
					}
				}
				if incoming.Params.Target != tc.target {
					t.Fatalf("%s target = %q", method, incoming.Params.Target)
				}
				if err := json.NewEncoder(stdin).Encode(map[string]any{
					"jsonrpc": "2.0", "id": incoming.ID, "result": map[string]string{"session_id": "host-session", "reply": "hello back"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
