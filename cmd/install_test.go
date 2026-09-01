package cmd

import (
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
