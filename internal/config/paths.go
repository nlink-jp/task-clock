package config

import (
	"os"
	"path/filepath"
	"slices"
)

const appDirName = "task-clock"

// SearchDirs returns the config directories in resolution order. macOS CLI
// users put configs in ~/.config as often as Application Support, and a
// config that exists but is silently not read is the worst failure mode —
// so every location is searched (org lesson: sensor-lens doctor incident).
func SearchDirs() []string {
	var dirs []string
	add := func(d string) {
		if d == "" || slices.Contains(dirs, d) {
			return
		}
		dirs = append(dirs, d)
	}
	if v := os.Getenv("TASK_CLOCK_CONFIG_DIR"); v != "" {
		add(v)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		add(filepath.Join(x, appDirName))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".config", appDirName))
		add(filepath.Join(home, "Library", "Application Support", appDirName))
	}
	return dirs
}

// ResolveDir returns the first search directory that contains config.toml.
// When none does, it returns the canonical location (~/.config/task-clock,
// RFP §2) and found=false so callers can report the searched paths.
func ResolveDir() (dir string, found bool) {
	dirs := SearchDirs()
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, ConfigFileName)); err == nil {
			return d, true
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", appDirName), false
	}
	if len(dirs) > 0 {
		return dirs[0], false
	}
	return "", false
}

// DefaultDataDir is where the SQLite history and run logs live
// (~/Library/Application Support/task-clock — RFP §2). Config and data
// deliberately diverge: only config gets the multi-path search.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return appDirName
	}
	return filepath.Join(home, "Library", "Application Support", appDirName)
}
