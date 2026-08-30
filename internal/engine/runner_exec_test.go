package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerCapturesStdoutAndStderr(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "logs", "a", "run.log")
	h, err := ExecRunner{}.Start(RunSpec{
		Task:    "a",
		Argv:    []string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr 1>&2"},
		Env:     []string{"PATH=/usr/bin:/bin"},
		LogPath: logPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := <-h.Done()
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d (%s)", res.ExitCode, res.Err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Errorf("log must capture both streams, got: %q", out)
	}
}

func TestExecRunnerExitCodeAndWorkdirAndEnv(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	h, err := ExecRunner{}.Start(RunSpec{
		Task:    "a",
		Argv:    []string{"/bin/sh", "-c", "pwd; echo $MARKER; exit 3"},
		Workdir: dir,
		Env:     []string{"PATH=/usr/bin:/bin", "MARKER=xyzzy"},
		LogPath: logPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := <-h.Done()
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), dir) || !strings.Contains(string(data), "xyzzy") {
		t.Errorf("workdir/env not applied, log: %q", string(data))
	}
}

func TestExecRunnerKill(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "run.log")
	h, err := ExecRunner{}.Start(RunSpec{
		Task:    "a",
		Argv:    []string{"/bin/sleep", "30"},
		Env:     []string{"PATH=/usr/bin:/bin"},
		LogPath: logPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Kill()
	select {
	case res := <-h.Done():
		if res.ExitCode >= 0 {
			t.Errorf("killed process reported exit %d, want negative (signaled)", res.ExitCode)
		}
		if res.Err == "" {
			t.Error("signal death not described in Err")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("killed process did not finish within grace")
	}
}

func TestExecRunnerStartErrorOnBadBinary(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "run.log")
	_, err := ExecRunner{}.Start(RunSpec{
		Task:    "a",
		Argv:    []string{"/no/such/binary/anywhere"},
		LogPath: logPath,
	})
	if err == nil {
		t.Fatal("expected start error for missing binary")
	}
}
