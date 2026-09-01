package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon binary must live at a path nothing else owns. An .app bundle
// interior dies with the app (field incident: quitting the GUI killed the
// daemon and its tasks), dist/ gets re-signed underneath, Homebrew's
// Cellar is deleted on upgrade — install therefore copies the executable
// into the daemon home under data_dir and points the plist there.
func TestDaemonBinaryPathIsTheStableHome(t *testing.T) {
	home := "/data/task-clock/bin/task-clock"

	for _, unstable := range []string{
		"/Applications/TaskClock.app/Contents/Resources/task-clock", // bundle
		"/opt/homebrew/Cellar/task-clock/0.3.0/bin/task-clock",      // brew cellar
		"/Users/x/works/task-clock/dist/task-clock",                 // dev build
		"/opt/homebrew/bin/task-clock",                              // symlink target changes
	} {
		bin, needsCopy := daemonBinaryPath(unstable, "/data/task-clock")
		if bin != home || !needsCopy {
			t.Errorf("daemonBinaryPath(%q) = (%q, %v), want (%q, true)",
				unstable, bin, needsCopy, home)
		}
	}

	// Already the home copy (a daemon-home binary re-running install):
	// no self-copy.
	bin, needsCopy := daemonBinaryPath(home, "/data/task-clock")
	if bin != home || needsCopy {
		t.Errorf("home binary must not re-copy itself: (%q, %v)", bin, needsCopy)
	}
}

func TestCopyExecutablePreservesBytesAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "nested", "bin", "task-clock")
	payload := []byte("#!/bin/sh\necho fake-binary\n")
	if err := os.WriteFile(src, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutable(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Error("copy altered the bytes")
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", info.Mode().Perm())
	}
	if _, err := os.Stat(dst + ".tmp"); err == nil {
		t.Error("temp file left behind")
	}
}

// interceptLaunchctl replaces the launchctl hook for one test, recording
// every call as "verb rest-of-args" and answering via respond.
func interceptLaunchctl(t *testing.T, respond func(args []string) error) *[]string {
	t.Helper()
	var calls []string
	prev := launchctl
	launchctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", respond(args)
	}
	t.Cleanup(func() { launchctl = prev })
	return &calls
}

func installFakePlist(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := plistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start is run-state only: it must refuse when the daemon was never
// installed instead of half-registering something.
func TestStartRequiresInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := interceptLaunchctl(t, func([]string) error { return nil })
	if err := runStart(io.Discard); err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("err = %v, want install guidance", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("launchctl touched before the install check: %v", *calls)
	}
}

// start must clear the persistent disable record BEFORE bootstrapping —
// launchd refuses to bootstrap a disabled service, so the order is the
// whole correctness of resuming after a stop.
func TestStartEnablesThenBootstraps(t *testing.T) {
	installFakePlist(t)
	calls := interceptLaunchctl(t, func(args []string) error {
		if args[0] == "print" {
			return errNotLoaded // not currently loaded
		}
		return nil
	})
	if err := runStart(io.Discard); err != nil {
		t.Fatal(err)
	}
	got := *calls
	if len(got) != 3 ||
		!strings.HasPrefix(got[0], "enable gui/") ||
		!strings.HasPrefix(got[1], "print gui/") ||
		!strings.HasPrefix(got[2], "bootstrap gui/") {
		t.Fatalf("call sequence = %v", got)
	}
}

var errNotLoaded = os.ErrNotExist

// A start against an already-loaded daemon must not double-bootstrap.
func TestStartWhenAlreadyLoaded(t *testing.T) {
	installFakePlist(t)
	calls := interceptLaunchctl(t, func([]string) error { return nil })
	var out strings.Builder
	if err := runStart(&out); err != nil {
		t.Fatal(err)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "bootstrap") {
			t.Fatalf("bootstrapped an already-loaded daemon: %v", *calls)
		}
	}
	if !strings.Contains(out.String(), "already running") {
		t.Errorf("output = %q", out.String())
	}
}

// stop = disable (survive logins: RunAtLoad would otherwise revive a
// deliberately stopped daemon) + bootout, in that order, and idempotent —
// stopping a stopped daemon reports success.
func TestStopDisablesThenBootsOutAndIsIdempotent(t *testing.T) {
	calls := interceptLaunchctl(t, func([]string) error {
		return os.ErrNotExist // launchd: nothing loaded — must be tolerated
	})
	if err := runStop(io.Discard); err != nil {
		t.Fatal(err)
	}
	got := *calls
	if len(got) != 2 ||
		!strings.HasPrefix(got[0], "disable gui/") ||
		!strings.HasPrefix(got[1], "bootout gui/") {
		t.Fatalf("call sequence = %v", got)
	}
}

// install must lift a lingering stop (enable before bootstrap): installing
// is an explicit "run it", whatever the previous run state was.
func TestInstallEnablesBeforeBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	calls := interceptLaunchctl(t, func([]string) error { return nil })
	if err := runInstall(io.Discard); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, call := range *calls {
		seq = append(seq, strings.Fields(call)[0])
	}
	want := []string{"bootout", "enable", "bootstrap"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("verb sequence = %v, want %v", seq, want)
	}
}

func TestGeneratePlist(t *testing.T) {
	plist := GeneratePlist("/opt/task-clock/bin/task-clock", "/data/task-clock/daemon.err")
	for _, want := range []string{
		"<key>StandardErrorPath</key>",
		"<string>/data/task-clock/daemon.err</string>",
		"<string>jp.nlink.task-clock</string>",
		"<string>/opt/task-clock/bin/task-clock</string>",
		"<string>serve</string>",
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
		// App Nap countermeasure: never Background (RFP §3).
		"<key>ProcessType</key>",
		"<string>Interactive</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	for _, banned := range []string{"StartInterval", "StartCalendarInterval"} {
		if strings.Contains(plist, banned) {
			t.Errorf("plist must not use launchd timers (%s present)", banned)
		}
	}
}
