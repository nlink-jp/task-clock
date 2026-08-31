package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/task-clock/internal/config"
)

// A task flooding its log past log_max_mb is killed with the reason
// recorded — tick-checked, same mechanism as the timeout.
func TestLogSizeCapKillsRunawayTask(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, st := newTestEngine(t, start,
		task("a", "0 0 1 1 *", func(c *config.TaskConfig) { c.LogMaxMB = 1 }))

	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	logPath := runner.spec(0).LogPath
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Under the cap: no kill.
	if err := os.WriteFile(logPath, []byte(strings.Repeat("x", 1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	if runner.handle(0).wasKilled() {
		t.Fatal("killed below the cap")
	}

	// Past the cap (>1 MB): killed on the next tick.
	if err := os.WriteFile(logPath, []byte(strings.Repeat("x", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(t, "2026-08-31T10:00:20Z"))
	e.Tick(clk.Now())
	if !runner.handle(0).wasKilled() {
		t.Fatal("not killed past the cap")
	}
	runner.handle(0).finish(Result{ExitCode: -1, Err: "signal: killed"})
	waitFor(t, "finish recorded", func() bool {
		r := history(t, st, "a")
		return len(r) == 1 && r[0].FinishedAt != nil
	})
	if got := history(t, st, "a")[0].Error; got != "killed: log size cap exceeded (log_max_mb)" {
		t.Errorf("error = %q", got)
	}
}

// The default (log_max_mb unset) never kills, whatever the log size.
func TestLogSizeCapOffByDefault(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, task("a", "0 0 1 1 *"))
	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	logPath := runner.spec(0).LogPath
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	os.WriteFile(logPath, []byte(strings.Repeat("x", 2<<20)), 0o600)
	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	if runner.handle(0).wasKilled() {
		t.Fatal("default must be no cap — killing a task by surprise is worse than disk growth")
	}
}
