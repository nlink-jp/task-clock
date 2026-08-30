package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig creates a config dir with the given config.toml body and
// tasks.d files, all mode 0600/0700 so the permission gate passes.
func writeConfig(t *testing.T, daemon string, tasks map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(tasks) > 0 {
		td := filepath.Join(dir, TasksDirName)
		if err := os.Mkdir(td, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range tasks {
			if err := os.WriteFile(filepath.Join(td, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func noEnv(string) (string, bool) { return "", false }

const validDaemon = `
[daemon]
api_key = "test-key"
`

func TestLoadAppliesDefaults(t *testing.T) {
	dir := writeConfig(t, validDaemon, nil)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Listen != DefaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Daemon.Listen, DefaultListen)
	}
	if cfg.Daemon.TickInterval.Value() != DefaultTickInterval {
		t.Errorf("TickInterval = %v, want %v", cfg.Daemon.TickInterval.Value(), DefaultTickInterval)
	}
	if cfg.Daemon.RetentionDays != DefaultRetentionDays {
		t.Errorf("RetentionDays = %d, want %d", cfg.Daemon.RetentionDays, DefaultRetentionDays)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	// A typo like "api_keys" must be an immediate error, not a silent fallback.
	dir := writeConfig(t, "[daemon]\napi_keys = \"x\"\n", nil)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("expected unknown-keys error, got %v", err)
	}
}

func TestLoadRejectsUnknownTaskKeys(t *testing.T) {
	dir := writeConfig(t, validDaemon, map[string]string{
		"a.toml": "[[task]]\nname = \"x\"\ncron = \"* * * * *\"\ncommand = [\"/bin/true\"]\ncatchup = false\n",
	})
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("expected unknown-keys error for 'catchup' (correct: catch_up), got %v", err)
	}
}

func TestLoadRejectsWorldReadableKeyFile(t *testing.T) {
	dir := writeConfig(t, validDaemon, nil)
	path := filepath.Join(dir, ConfigFileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected permission error advising chmod 600, got %v", err)
	}
}

func TestLoadAllowsOpenPermissionsWithoutKey(t *testing.T) {
	dir := writeConfig(t, "[daemon]\n", nil)
	if err := os.Chmod(filepath.Join(dir, ConfigFileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("no api_key present — open permissions should load: %v", err)
	}
}

func TestLoadMergesTasksDSorted(t *testing.T) {
	dir := writeConfig(t, validDaemon, map[string]string{
		"20-b.toml": "[[task]]\nname = \"b\"\ncron = \"* * * * *\"\ncommand = [\"/bin/true\"]\n",
		"10-a.toml": "[[task]]\nname = \"a\"\ncron = \"* * * * *\"\ncommand = [\"/bin/true\"]\n",
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tasks) != 2 || cfg.Tasks[0].Name != "a" || cfg.Tasks[1].Name != "b" {
		t.Fatalf("tasks not merged in filename order: %+v", cfg.Tasks)
	}
	if !strings.HasSuffix(cfg.Tasks[0].Source, "10-a.toml") {
		t.Errorf("Source not recorded: %q", cfg.Tasks[0].Source)
	}
}

func TestTaskFieldsAndPolicyDefaults(t *testing.T) {
	dir := writeConfig(t, validDaemon, map[string]string{
		"a.toml": `
[[task]]
name    = "full"
cron    = "*/30 * * * *"
command = ["/usr/bin/foo", "--out", "${OUT}"]
workdir = "${OUT}"
timeout = "10m"
overlap = "skip"
catch_up = false
enabled = false
[task.env]
OUT = "/tmp/o"

[[task]]
name    = "defaults"
cron    = "@hourly"
command = ["/bin/true"]

[[task]]
name    = "sh"
cron    = "@daily"
shell   = true
command = "echo hi | wc -l"
`,
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if errs := cfg.Validate(noEnv); len(errs) != 0 {
		t.Fatalf("unexpected validation errors: %v", errs)
	}

	full := cfg.Tasks[0]
	if full.OverlapPolicy() != OverlapSkip || full.CatchUpEnabled() || full.IsEnabled() {
		t.Errorf("explicit fields not honored: %+v", full)
	}
	if full.Timeout.Value() != 10*time.Minute {
		t.Errorf("Timeout = %v, want 10m", full.Timeout.Value())
	}
	argv, err := full.ResolvedArgv(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if argv[2] != "/tmp/o" {
		t.Errorf("task env not used for expansion: %v", argv)
	}
	wd, err := full.ResolvedWorkdir(noEnv)
	if err != nil || wd != "/tmp/o" {
		t.Errorf("workdir expansion: %q, %v", wd, err)
	}

	def := cfg.Tasks[1]
	if def.OverlapPolicy() != OverlapQueueOne || !def.CatchUpEnabled() || !def.IsEnabled() {
		t.Errorf("defensive defaults not applied: %+v", def)
	}

	sh := cfg.Tasks[2]
	argv, err = sh.ResolvedArgv(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != "echo hi | wc -l" {
		t.Errorf("shell argv = %v", argv)
	}
}

func TestEnvLookupOrderTaskFirst(t *testing.T) {
	task := TaskConfig{Env: map[string]string{"X": "task"}}
	outer := lookupMap(map[string]string{"X": "process", "Y": "outer"})
	lookup := task.LookupWith(outer)
	if v, _ := lookup("X"); v != "task" {
		t.Errorf("task env must win: got %q", v)
	}
	if v, _ := lookup("Y"); v != "outer" {
		t.Errorf("fallthrough to process env broken: got %q", v)
	}
}

func TestValidateFindings(t *testing.T) {
	dir := writeConfig(t, "[daemon]\nlisten = \"0.0.0.0:17282\"\napi_key = \"k\"\n", map[string]string{
		"a.toml": `
[[task]]
name    = "dup"
cron    = "* * * * *"
command = ["/bin/true"]

[[task]]
name    = "dup"
cron    = "bad cron"
command = ["${UNDEFINED_VAR_XYZ}"]

[[task]]
name    = "bad/name"
cron    = "* * * * *"
command = "echo hi"

[[task]]
name    = "noshell"
cron    = "* * * * *"
command = ["/bin/true"]
overlap = "sometimes"
`,
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	errs := cfg.Validate(noEnv)
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{
		"not loopback",
		"duplicate name",
		"invalid cron expression",
		"undefined variable ${UNDEFINED_VAR_XYZ}",
		"invalid name",
		"string command requires shell = true",
		"invalid overlap \"sometimes\"",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("validation missing finding %q in:\n%s", want, joined)
		}
	}
}

func TestValidateAcceptsLocalhostForms(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:1", "localhost:17282", "[::1]:9"} {
		if err := validateLoopbackListen(listen); err != nil {
			t.Errorf("listen %q should be accepted: %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:1", "192.168.1.2:80", "example.com:80"} {
		if err := validateLoopbackListen(listen); err == nil {
			t.Errorf("listen %q should be rejected", listen)
		}
	}
}

// Both HOME and XDG_CONFIG_HOME are overridden — with only one, the test
// picks up the developer's real config (org lesson, actually hit before).
func TestSearchDirsAndResolve(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	explicit := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TASK_CLOCK_CONFIG_DIR", explicit)

	dirs := SearchDirs()
	want := []string{
		explicit,
		filepath.Join(xdg, "task-clock"),
		filepath.Join(home, ".config", "task-clock"),
		filepath.Join(home, "Library", "Application Support", "task-clock"),
	}
	if len(dirs) != len(want) {
		t.Fatalf("SearchDirs = %v, want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("SearchDirs[%d] = %q, want %q", i, dirs[i], want[i])
		}
	}

	// Nothing exists yet: canonical fallback, found=false.
	dir, found := ResolveDir()
	if found || dir != filepath.Join(home, ".config", "task-clock") {
		t.Errorf("ResolveDir (none) = %q, %v", dir, found)
	}

	// A config in a later search dir is still found.
	appSupport := filepath.Join(home, "Library", "Application Support", "task-clock")
	if err := os.MkdirAll(appSupport, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appSupport, ConfigFileName), []byte("[daemon]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, found = ResolveDir()
	if !found || dir != appSupport {
		t.Errorf("ResolveDir (app support) = %q, %v", dir, found)
	}

	// An earlier search dir wins over a later one.
	if err := os.WriteFile(filepath.Join(explicit, ConfigFileName), []byte("[daemon]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, found = ResolveDir()
	if !found || dir != explicit {
		t.Errorf("ResolveDir (explicit) = %q, %v", dir, found)
	}
}
