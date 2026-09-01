// Package cmd implements the task-clock CLI dispatch.
//
// Dispatch uses the standard library `flag` package (no third-party CLI
// framework) to keep the tool dependency-light and offline-buildable.
package cmd

import (
	"fmt"
	"io"
)

// Run executes the CLI and returns the process exit code. Streams and
// version are injected so tests can exercise dispatch without a process.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	rest := args[1:]
	var err error
	switch args[0] {
	case "serve":
		err = runServe(rest, stdout, stderr, version)
	case "status":
		err = runStatus(rest, stdout, stderr)
	case "list":
		err = runList(rest, stdout, stderr)
	case "history":
		err = runHistory(rest, stdout, stderr)
	case "trigger":
		err = runTrigger(rest, stdout, stderr)
	case "pause", "resume":
		err = runPauseResume(args[0], rest, stdout, stderr)
	case "reload":
		err = runReload(rest, stdout, stderr)
	case "validate":
		err = runValidate(rest, stdout, stderr)
	case "install":
		err = runInstall(stdout)
	case "uninstall":
		err = runUninstall(stdout)
	case "start":
		err = runStart(stdout)
	case "stop":
		err = runStop(stdout)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, versionString(version))
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		usage(stderr)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// versionString is the single source for version output, so the `version`
// subcommand and `--version` can never drift apart (a Homebrew formula's
// `brew test` runs `--version`).
func versionString(version string) string {
	return "task-clock " + version
}

func usage(w io.Writer) {
	fmt.Fprint(w, `task-clock — a resident scheduler that does not trust launchd's timing engine

Usage:
  task-clock <command> [flags]

Commands:
  serve        run the resident daemon (launched via launchd KeepAlive)
  status       show per-task state: next_fire / next_expected_run / overrun
  list         show task definitions
  history      show scheduled-vs-actual run history for a task
  trigger      trigger a task manually
  pause        suspend a task's scheduling until resume (survives restarts)
  resume       lift a pause (cron restarts from the next future fire)
  reload       reload config and tasks.d
  validate     validate config and preview each task's next fire
  install      generate and register the launchd plist (KeepAlive only)
  uninstall    remove the launchd plist
  start        start the installed daemon
  stop         stop the daemon (running tasks are never killed; survives logins)
  version      print the version

Use "task-clock <command> -h" for command flags.
`)
}
