package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nlink-jp/task-clock/internal/api"
	"github.com/nlink-jp/task-clock/internal/clock"
	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/engine"
	"github.com/nlink-jp/task-clock/internal/logrotate"
	"github.com/nlink-jp/task-clock/internal/store"
)

// The daemon's own operational log: 10 MB per generation, 3 generations
// kept (data_dir/daemon.log[.1..3]).
const (
	daemonLogMaxBytes = 10 << 20
	daemonLogKeep     = 3
)

func runServe(args []string, stdout, stderr io.Writer, version string) error {
	var configDir string
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&configDir, "config", "", "config directory (default: search paths)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return err
	}
	cfg, err := loadValidated(dir)
	if err != nil {
		return err
	}
	// Fail-closed: no API key, no daemon (RFP §2).
	if cfg.Daemon.APIKey == "" {
		return fmt.Errorf("api_key is not set in %s — the API refuses to run without one.\nGenerate a key:\n  openssl rand -hex 32\nthen set it under [daemon] and chmod 600 the file", cfg.FilePath)
	}

	st, err := store.Open(cfg.Daemon.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	// Under launchd the daemon's stdout goes nowhere, so operational log
	// lines (reloads, hook failures, prune reports) tee into a rotating
	// file in the data dir — a KeepAlive daemon runs for months, hence the
	// size cap instead of unbounded growth.
	if daemonLog, lerr := logrotate.New(
		filepath.Join(cfg.Daemon.DataDir, "daemon.log"), daemonLogMaxBytes, daemonLogKeep); lerr == nil {
		defer daemonLog.Close()
		stdout = io.MultiWriter(stdout, daemonLog)
		stderr = io.MultiWriter(stderr, daemonLog)
	} else {
		fmt.Fprintf(stderr, "%s daemon log unavailable: %v\n", logTime(), lerr)
	}

	eng := engine.New(clock.Real{}, st, engine.ExecRunner{}, engine.Options{
		Notify:                 makeHookNotifier(cfg.Hooks, stdout, stderr),
		OverrunStreakThreshold: cfg.Hooks.OverrunStreakThreshold,
	})
	if err := eng.Configure(cfg.Tasks); err != nil {
		return err
	}

	// Reload re-reads task definitions, [hooks], and retention_days.
	// Only the structural daemon settings (listen, api_key, data_dir,
	// tick_interval) need a restart — swapping the listener live would
	// tear down the very connection delivering the reload.
	var reloadMu sync.Mutex
	retentionDays := cfg.Daemon.RetentionDays
	reload := func() error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		ncfg, err := loadValidated(dir)
		if err != nil {
			return err
		}
		if err := eng.Configure(ncfg.Tasks); err != nil {
			return err
		}
		eng.SetNotifier(makeHookNotifier(ncfg.Hooks, stdout, stderr),
			ncfg.Hooks.OverrunStreakThreshold)
		retentionDays = ncfg.Daemon.RetentionDays
		fmt.Fprintf(stdout, "%s reloaded config: %d tasks\n", logTime(), len(ncfg.Tasks))
		return nil
	}

	ln, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Daemon.Listen, err)
	}
	httpSrv := &http.Server{Handler: api.New(eng, st, cfg.Daemon.APIKey, reload, version).Handler()}
	go httpSrv.Serve(ln)

	fmt.Fprintf(stdout, "%s task-clock %s serving on %s (%d tasks, tick %s, data %s)\n",
		logTime(), version, cfg.Daemon.Listen, len(cfg.Tasks),
		cfg.Daemon.TickInterval.Value(), cfg.Daemon.DataDir)

	prune := func(now time.Time) {
		reloadMu.Lock()
		days := retentionDays
		reloadMu.Unlock()
		cutoff := now.AddDate(0, 0, -days)
		paths, err := st.Prune(cutoff)
		if err != nil {
			fmt.Fprintf(stderr, "%s prune failed: %v\n", logTime(), err)
			return
		}
		for _, p := range paths {
			os.Remove(p)
		}
		if len(paths) > 0 {
			fmt.Fprintf(stdout, "%s pruned %d run logs older than %d days\n", logTime(), len(paths), days)
		}
	}
	prune(time.Now())
	lastPrune := time.Now()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ticker := time.NewTicker(cfg.Daemon.TickInterval.Value())
	defer ticker.Stop()

	shutdown := func(reason string) {
		fmt.Fprintf(stdout, "%s shutting down (%s)\n", logTime(), reason)
		// Never kill running tasks on daemon stop (field incident
		// 2026-09-02: an in-flight task was killed and work was lost).
		// They live in their own process groups and finish unmanaged
		// (adoptable via the live-run registry written at spawn). Disown
		// FIRST — the scheduler must stop managing before anything that
		// can stretch the shutdown (the HTTP drain below).
		eng.ReleaseAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		httpSrv.Shutdown(ctx)
		cancel()
	}

	for {
		select {
		case now := <-ticker.C:
			eng.Tick(now)
			if now.Sub(lastPrune) >= 24*time.Hour {
				prune(now)
				lastPrune = now
			}
		case <-serveTestStop:
			shutdown("test stop")
			return nil
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				if err := reload(); err != nil {
					fmt.Fprintf(stderr, "%s reload failed: %v\n", logTime(), err)
				}
				continue
			}
			shutdown(sig.String())
			return nil
		}
	}
}

// serveTestStop lets the in-process end-to-end test stop the serve loop.
// It stays nil in production — receiving from a nil channel blocks forever.
var serveTestStop chan struct{}

// loadValidated loads the config and fails on any validation finding.
func loadValidated(dir string) (*config.Config, error) {
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	if errs := cfg.Validate(os.LookupEnv); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, "  - "+e.Error())
		}
		return nil, fmt.Errorf("invalid configuration:\n%s", strings.Join(msgs, "\n"))
	}
	return cfg, nil
}

func logTime() string { return time.Now().Format("2006-01-02T15:04:05") }
