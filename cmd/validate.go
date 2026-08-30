package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/schedule"
)

func runValidate(args []string, stdout, stderr io.Writer) error {
	var configDir string
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.StringVar(&configDir, "config", "", "config directory (default: search paths)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	// Always show which file was actually read — the one-glance diagnosis
	// for "my config exists but is not picked up".
	fmt.Fprintf(stdout, "config: %s\n", cfg.FilePath)

	if cfg.Daemon.APIKey == "" {
		fmt.Fprintln(stdout, "warning: api_key is not set — `task-clock serve` will refuse to start")
	}

	errs := cfg.Validate(os.LookupEnv)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(stderr, "error: %v\n", e)
		}
		return fmt.Errorf("%d problem(s) found", len(errs))
	}

	now := time.Now()
	fmt.Fprintf(stdout, "%d task(s), all valid\n", len(cfg.Tasks))
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		if !t.IsEnabled() {
			fmt.Fprintf(stdout, "  %-20s disabled (%s)\n", t.Name, relPathish(t.Source, dir))
			continue
		}
		spec, _ := schedule.Parse(t.Cron)
		next := spec.Next(now)
		fmt.Fprintf(stdout, "  %-20s next fire %s (in %s)  overlap=%s catch_up=%t\n",
			t.Name, fmtLocal(next), fmtDur(next.Sub(now)), t.OverlapPolicy(), t.CatchUpEnabled())
	}
	return nil
}

// relPathish trims the config dir prefix for compact display.
func relPathish(path, dir string) string {
	return strings.TrimPrefix(strings.TrimPrefix(path, dir), "/")
}
