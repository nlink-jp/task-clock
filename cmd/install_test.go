package cmd

import (
	"strings"
	"testing"
)

func TestGeneratePlist(t *testing.T) {
	plist := GeneratePlist("/opt/task-clock/bin/task-clock")
	for _, want := range []string{
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
