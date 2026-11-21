package workspace_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestLocalWorkspaceWaitWithOpenOutput 验证独立进程组保留输出写端时，Wait 仍返回 Bash 的退出码和已读输出。
func TestLocalWorkspaceWaitWithOpenOutput(t *testing.T) {
	for name, redirect := range map[string]string{
		"stdout": "2>/dev/null",
		"stderr": ">/dev/null",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// 1. 用 Bash 作业控制把 sleep 放进独立进程组，只保留被测管道的写端。
			fixture := newWorkspaceFixture(t, "local")
			var stdout, stderr bytes.Buffer
			process, err := fixture.workspace.StartProcess(t.Context(), &workspace.ProcessRequest{
				Command:   "set -m\nsleep 30 " + redirect + " &\necho $! > sleep.pid\nprintf stdout\nprintf stderr >&2\nexit 7",
				Directory: fixture.root,
				Stdout:    &stdout,
				Stderr:    &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			waitForCondition(t, 5*time.Second, func() bool {
				data, _ := os.ReadFile(filepath.Join(fixture.root, "sleep.pid"))
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				return pid > 0
			})
			sleeper, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = sleeper.Kill()
				_ = sleeper.Release()
			})

			// 2. 在 sleep 自行退出前取得 Bash 的退出码和两路输出。
			if code, err := waitProcess(t, process); code != 7 || err != nil {
				t.Fatalf("Wait() = (%d, %v), want (7, nil)", code, err)
			}
			if stdout.String() != "stdout" || stderr.String() != "stderr" {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}
