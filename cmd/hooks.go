package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/engine"
)

// hookTimeout bounds a notification hook run; a stuck hook must not pile
// up forever (each event runs on its own goroutine).
const hookTimeout = 60 * time.Second

// makeHookNotifier turns the [hooks] config into the engine's Notify
// callback. Event details reach the hook as TASK_CLOCK_* environment
// variables, so any command works without argument templating.
func makeHookNotifier(hooks config.HooksConfig, stdout, stderr io.Writer) func(engine.Event) {
	if len(hooks.OnMissed) == 0 && len(hooks.OnFailure) == 0 && len(hooks.OnOverrunStreak) == 0 {
		return nil
	}
	return func(ev engine.Event) {
		var argv []string
		switch ev.Type {
		case engine.EventMissed:
			argv = hooks.OnMissed
		case engine.EventFailure:
			argv = hooks.OnFailure
		case engine.EventOverrunStreak:
			argv = hooks.OnOverrunStreak
		}
		if len(argv) == 0 {
			return
		}

		scheduledFor := ""
		if !ev.ScheduledFor.IsZero() {
			scheduledFor = ev.ScheduledFor.UTC().Format(time.RFC3339)
		}
		ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(),
			"TASK_CLOCK_EVENT="+ev.Type,
			"TASK_CLOCK_TASK="+ev.Task,
			"TASK_CLOCK_SCHEDULED_FOR="+scheduledFor,
			"TASK_CLOCK_REASON="+ev.Reason,
			"TASK_CLOCK_EXIT_CODE="+strconv.Itoa(ev.ExitCode),
			"TASK_CLOCK_ERROR="+ev.Error,
			"TASK_CLOCK_STREAK="+strconv.Itoa(ev.Streak),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(stderr, "%s hook %s (task %s) failed: %v: %s\n",
				logTime(), ev.Type, ev.Task, err, bytes.TrimSpace(out))
			return
		}
		fmt.Fprintf(stdout, "%s hook %s ran (task %s)\n", logTime(), ev.Type, ev.Task)
	}
}
