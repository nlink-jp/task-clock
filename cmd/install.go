package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const launchdLabel = "jp.nlink.task-clock"

// GeneratePlist renders the LaunchAgent plist. launchd is used strictly for
// residency: KeepAlive restarts the daemon, ProcessType Interactive keeps
// it out of the background-throttling band, and no launchd timer key
// appears here — timing is the daemon's own job (RFP §3).
func GeneratePlist(execPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`, launchdLabel, execPath)
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func runInstall(stdout io.Writer) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GeneratePlist(execPath)), 0o644); err != nil {
		return err
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// A previous registration blocks bootstrap; boot it out first (the
	// error is expected when nothing is registered).
	exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Fprintf(stdout, "installed: %s (label %s, binary %s)\n", path, launchdLabel, execPath)
	return nil
}

func runUninstall(stdout io.Writer) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(stdout, "uninstalled: %s\n", launchdLabel)
	return nil
}
