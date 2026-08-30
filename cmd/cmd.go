// Package cmd implements the task-clock CLI dispatch.
//
// Dispatch uses the standard library `flag` package (no third-party CLI
// framework) to keep the scaffold dependency-free and offline-buildable.
// Subcommands are stubs until Phase 3 (see docs/ja/task-clock-rfp.ja.md).
package cmd

import (
	"errors"
	"fmt"
	"io"
)

var errNotImplemented = errors.New("not implemented yet (scaffold — see docs/ja/task-clock-rfp.ja.md)")

// Run executes the CLI and returns the process exit code. Streams and
// version are injected so tests can exercise dispatch without a process.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	rest := args[1:]
	_ = rest
	var err error
	switch args[0] {
	case "serve":
		err = errNotImplemented
	case "status":
		err = errNotImplemented
	case "list":
		err = errNotImplemented
	case "history":
		err = errNotImplemented
	case "trigger":
		err = errNotImplemented
	case "reload":
		err = errNotImplemented
	case "validate":
		err = errNotImplemented
	case "install":
		err = errNotImplemented
	case "uninstall":
		err = errNotImplemented
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
  reload       reload config and tasks.d
  validate     validate config and preview each task's next fire
  install      generate and register the launchd plist (KeepAlive only)
  uninstall    remove the launchd plist
  version      print the version

Use "task-clock <command> -h" for command flags.
`)
}
