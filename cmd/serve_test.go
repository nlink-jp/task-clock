package cmd

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/store"
)

// freePort grabs an ephemeral port for the test daemon. The tiny window
// between Close and reuse is acceptable in a local test.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func writeServeConfig(t *testing.T, port int, dataDir string) string {
	t.Helper()
	dir := t.TempDir()
	daemon := fmt.Sprintf(`
[daemon]
listen        = "127.0.0.1:%d"
api_key       = "e2e-test-key"
tick_interval = "50ms"
data_dir      = %q
`, port, dataDir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := `
[[task]]
name    = "echo-task"
cron    = "0 0 1 1 *"
command = ["/bin/sh", "-c", "echo e2e-marker"]
`
	if err := os.Mkdir(filepath.Join(dir, "tasks.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.d", "tasks.toml"), []byte(tasks), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// End-to-end through the real wiring: config -> serve (engine + API) ->
// client subcommands -> process execution -> history and captured log.
func TestServeEndToEnd(t *testing.T) {
	port := freePort(t)
	dataDir := t.TempDir()
	dir := writeServeConfig(t, port, dataDir)

	serveTestStop = make(chan struct{})
	defer func() { serveTestStop = nil }()

	serveDone := make(chan int, 1)
	var serveOut, serveErr bytes.Buffer
	go func() {
		serveDone <- Run([]string{"serve", "-config", dir}, &serveOut, &serveErr, "e2e")
	}()

	// Wait for the daemon to accept API calls.
	waitCmd(t, "daemon up", func() bool {
		var out, errBuf bytes.Buffer
		return Run([]string{"status", "-config", dir}, &out, &errBuf, "e2e") == 0
	})

	var out, errBuf bytes.Buffer
	if code := Run([]string{"list", "-config", dir}, &out, &errBuf, "e2e"); code != 0 {
		t.Fatalf("list failed: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "echo-task") {
		t.Fatalf("list output missing task: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"trigger", "-config", dir, "echo-task"}, &out, &errBuf, "e2e"); code != 0 {
		t.Fatalf("trigger failed: %s", errBuf.String())
	}

	waitCmd(t, "run recorded", func() bool {
		var hout, herr bytes.Buffer
		if Run([]string{"history", "-config", dir, "--json", "echo-task"}, &hout, &herr, "e2e") != 0 {
			return false
		}
		s := hout.String()
		return strings.Contains(s, `"manual"`) &&
			(strings.Contains(s, `"exit_code": 0`) || strings.Contains(s, `"exit_code":0`))
	})

	// The task's stdout must have been captured under the data dir.
	waitCmd(t, "log captured", func() bool {
		found := false
		filepath.Walk(filepath.Join(dataDir, "logs"), func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "e2e-marker") {
				found = true
			}
			return nil
		})
		return found
	})

	// Reload picks up an edited task set.
	extra := `
[[task]]
name    = "second"
cron    = "0 0 1 1 *"
command = ["/bin/true"]
`
	if err := os.WriteFile(filepath.Join(dir, "tasks.d", "extra.toml"), []byte(extra), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"reload", "-config", dir}, &out, &errBuf, "e2e"); code != 0 {
		t.Fatalf("reload failed: %s", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	Run([]string{"list", "-config", dir}, &out, &errBuf, "e2e")
	if !strings.Contains(out.String(), "second") {
		t.Fatalf("reloaded task not listed: %s", out.String())
	}

	close(serveTestStop)
	select {
	case code := <-serveDone:
		if code != 0 {
			t.Fatalf("serve exited %d: %s", code, serveErr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// A task still running at shutdown must not stay "running" in the history
// forever: shutdown kills it and drains until the kill is recorded.
func TestShutdownRecordsKilledRuns(t *testing.T) {
	port := freePort(t)
	dataDir := t.TempDir()
	dir := writeServeConfig(t, port, dataDir)
	long := `
[[task]]
name    = "long-task"
cron    = "0 0 1 1 *"
command = ["/bin/sleep", "30"]
`
	if err := os.WriteFile(filepath.Join(dir, "tasks.d", "long.toml"), []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}

	serveTestStop = make(chan struct{})
	defer func() { serveTestStop = nil }()
	serveDone := make(chan int, 1)
	var serveOut, serveErr bytes.Buffer
	go func() {
		serveDone <- Run([]string{"serve", "-config", dir}, &serveOut, &serveErr, "t")
	}()

	waitCmd(t, "daemon up", func() bool {
		var out, errBuf bytes.Buffer
		return Run([]string{"status", "-config", dir}, &out, &errBuf, "t") == 0
	})
	var out, errBuf bytes.Buffer
	if code := Run([]string{"trigger", "-config", dir, "long-task"}, &out, &errBuf, "t"); code != 0 {
		t.Fatalf("trigger failed: %s", errBuf.String())
	}

	close(serveTestStop)
	select {
	case <-serveDone:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}

	// The daemon is gone, so read the history directly (test-only access;
	// the runtime contract stays the HTTP API).
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.LastRun("long-task")
	if err != nil || run == nil {
		t.Fatalf("no history row: %v", err)
	}
	if run.FinishedAt == nil {
		t.Fatalf("killed run left as running forever: %+v", run)
	}
	if run.ExitCode == nil || *run.ExitCode >= 0 {
		t.Errorf("killed run should record a signal exit, got %+v", run.ExitCode)
	}
}

func TestServeRefusesWithoutAPIKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[daemon]\n"), 0o600)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"serve", "-config", dir}, &out, &errBuf, "t"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "api_key") || !strings.Contains(errBuf.String(), "openssl rand") {
		t.Errorf("fail-closed message missing guidance: %s", errBuf.String())
	}
}

func TestClientReportsDaemonDown(t *testing.T) {
	port := freePort(t)
	dir := writeServeConfig(t, port, t.TempDir())
	var out, errBuf bytes.Buffer
	if code := Run([]string{"status", "-config", dir}, &out, &errBuf, "t"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "daemon is not running") {
		t.Errorf("down-daemon diagnosis missing: %s", errBuf.String())
	}
}

func TestValidateReportsConfigAndNextFires(t *testing.T) {
	dir := writeServeConfig(t, 12345, t.TempDir())
	var out, errBuf bytes.Buffer
	if code := Run([]string{"validate", "-config", dir}, &out, &errBuf, "t"); code != 0 {
		t.Fatalf("validate failed: %s", errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "config: "+filepath.Join(dir, "config.toml")) {
		t.Errorf("validate must print the file actually read: %s", s)
	}
	if !strings.Contains(s, "echo-task") || !strings.Contains(s, "next fire") {
		t.Errorf("validate must preview next fires: %s", s)
	}
}

func waitCmd(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
