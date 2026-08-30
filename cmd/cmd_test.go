package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// The version subcommand and --version must produce identical output
// (a Homebrew formula's `brew test` runs `--version`).
func TestVersionFormsIdentical(t *testing.T) {
	outputs := make(map[string]string)
	for _, form := range []string{"version", "-v", "--version"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{form}, &stdout, &stderr, "v9.9.9-test"); code != 0 {
			t.Fatalf("%s: exit code %d, want 0 (stderr: %s)", form, code, stderr.String())
		}
		outputs[form] = stdout.String()
	}
	want := "task-clock v9.9.9-test\n"
	for form, got := range outputs {
		if got != want {
			t.Errorf("%s: output %q, want %q", form, got, want)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--help"}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("--help: exit code %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "task-clock <command>") {
		t.Errorf("--help output missing usage line: %q", stdout.String())
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"no-such-command"}, &stdout, &stderr, "dev"); code != 2 {
		t.Fatalf("unknown command: exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("stderr missing diagnosis: %q", stderr.String())
	}
}

func TestNoArgsExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(nil, &stdout, &stderr, "dev"); code != 2 {
		t.Fatalf("no args: exit code %d, want 2", code)
	}
}
