// Package config loads and validates the daemon config (config.toml) and
// task definitions (tasks.d/*.toml). Decoding is strict — unknown keys are
// errors, never silently ignored (a typo that silently falls back is far
// harder to debug than an immediate error).
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/nlink-jp/task-clock/internal/schedule"
)

const (
	// ConfigFileName is the daemon config file inside the config directory.
	ConfigFileName = "config.toml"
	// TasksDirName holds the task definition files ([[task]] tables).
	TasksDirName = "tasks.d"

	// DefaultListen binds to loopback only; any non-loopback listen address
	// is rejected at validation (RFP §2: remote exposure is out of scope).
	DefaultListen        = "127.0.0.1:17282"
	DefaultTickInterval  = 10 * time.Second
	DefaultRetentionDays = 30
	DefaultTimeout       = 0 // no timeout unless configured

	// Overlap policies (RFP §2). Defaults are the defensive ones: the
	// originating incident's root cause was never determined, so defaults
	// must recover from either loss path (timer delay or skip-while-running).
	OverlapQueueOne       = "queue-one"
	OverlapSkip           = "skip"
	OverlapKillAndRestart = "kill-and-restart"
)

// Duration wraps time.Duration for TOML text values like "10m".
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("negative duration %q", text)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

// CommandValue accepts either an argv array (the default form, expanded by
// task-clock itself) or a single string (only valid with shell = true,
// interpreted by /bin/sh -c).
type CommandValue struct {
	Argv  []string
	Str   string
	IsStr bool
	set   bool
}

func (c *CommandValue) UnmarshalTOML(v any) error {
	c.set = true
	switch x := v.(type) {
	case string:
		c.Str = x
		c.IsStr = true
		return nil
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("command array elements must be strings, got %T", e)
			}
			c.Argv = append(c.Argv, s)
		}
		return nil
	default:
		return fmt.Errorf("command must be a string array or (with shell = true) a string, got %T", v)
	}
}

// DaemonConfig is the [daemon] table of config.toml.
type DaemonConfig struct {
	Listen        string   `toml:"listen"`
	APIKey        string   `toml:"api_key"`
	TickInterval  Duration `toml:"tick_interval"`
	RetentionDays int      `toml:"retention_days"`
	DataDir       string   `toml:"data_dir"`
}

// TaskConfig is one [[task]] table from tasks.d/*.toml.
//
// Exactly one of Cron and Watermark defines the trigger. A watermark task
// fires when the configured duration has elapsed since its last success
// (RFP Phase 2): the trigger that keeps backlog-driven jobs self-healing —
// however a fire was lost, the elapsed-since-success condition re-arms.
type TaskConfig struct {
	Name      string            `toml:"name"`
	Cron      string            `toml:"cron"`
	Watermark Duration          `toml:"watermark"`
	Command   CommandValue      `toml:"command"`
	Shell     bool              `toml:"shell"`
	Workdir   string            `toml:"workdir"`
	Env       map[string]string `toml:"env"`
	Timeout   Duration          `toml:"timeout"`
	Overlap   string            `toml:"overlap"`
	CatchUp   *bool             `toml:"catch_up"`
	Jitter    Duration          `toml:"jitter"`
	Enabled   *bool             `toml:"enabled"`

	Source string `toml:"-"` // file this task was defined in
}

// IsWatermark reports whether the task is watermark-triggered.
func (t *TaskConfig) IsWatermark() bool { return t.Watermark > 0 }

// OverlapPolicy returns the effective overlap policy (default queue-one).
func (t *TaskConfig) OverlapPolicy() string {
	if t.Overlap == "" {
		return OverlapQueueOne
	}
	return t.Overlap
}

// CatchUpEnabled reports the effective catch-up setting (default true).
func (t *TaskConfig) CatchUpEnabled() bool { return t.CatchUp == nil || *t.CatchUp }

// IsEnabled reports the effective enabled setting (default true).
func (t *TaskConfig) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// LookupWith layers the task's env table over the outer lookup (RFP §2:
// resolution order is the task env, then the daemon's process environment).
func (t *TaskConfig) LookupWith(outer func(string) (string, bool)) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if v, ok := t.Env[name]; ok {
			return v, true
		}
		return outer(name)
	}
}

// ResolvedArgv returns the argv to execute, with ${VAR} expansion applied to
// the array form. The shell form is passed to /bin/sh -c untouched — the
// shell owns its own expansion there.
func (t *TaskConfig) ResolvedArgv(outer func(string) (string, bool)) ([]string, error) {
	if t.Shell {
		return []string{"/bin/sh", "-c", t.Command.Str}, nil
	}
	lookup := t.LookupWith(outer)
	argv := make([]string, 0, len(t.Command.Argv))
	for _, a := range t.Command.Argv {
		expanded, err := Expand(a, lookup)
		if err != nil {
			return nil, err
		}
		argv = append(argv, expanded)
	}
	return argv, nil
}

// ResolvedWorkdir returns the working directory with ${VAR} expansion.
func (t *TaskConfig) ResolvedWorkdir(outer func(string) (string, bool)) (string, error) {
	if t.Workdir == "" {
		return "", nil
	}
	return Expand(t.Workdir, t.LookupWith(outer))
}

// Config is the fully loaded configuration.
type Config struct {
	Daemon DaemonConfig
	Tasks  []TaskConfig

	Dir      string // config directory the files were read from
	FilePath string // config.toml path actually read
}

type daemonFile struct {
	Daemon DaemonConfig `toml:"daemon"`
}

type tasksFile struct {
	Tasks []TaskConfig `toml:"task"`
}

// Load reads config.toml and every tasks.d/*.toml under dir. It applies
// defaults but does not validate task semantics — call Validate for that,
// so `task-clock validate` can report every problem at once.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir, FilePath: filepath.Join(dir, ConfigFileName)}

	var df daemonFile
	md, err := toml.DecodeFile(cfg.FilePath, &df)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.FilePath, err)
	}
	if err := rejectUnknownKeys(cfg.FilePath, md); err != nil {
		return nil, err
	}
	cfg.Daemon = df.Daemon
	applyDefaults(&cfg.Daemon)

	if cfg.Daemon.APIKey != "" {
		if err := checkKeyFilePermissions(cfg.FilePath); err != nil {
			return nil, err
		}
	}

	taskFiles, err := filepath.Glob(filepath.Join(dir, TasksDirName, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(taskFiles)
	for _, f := range taskFiles {
		var tf tasksFile
		md, err := toml.DecodeFile(f, &tf)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := rejectUnknownKeys(f, md); err != nil {
			return nil, err
		}
		for i := range tf.Tasks {
			tf.Tasks[i].Source = f
		}
		cfg.Tasks = append(cfg.Tasks, tf.Tasks...)
	}
	return cfg, nil
}

func applyDefaults(d *DaemonConfig) {
	if d.Listen == "" {
		d.Listen = DefaultListen
	}
	if d.TickInterval == 0 {
		d.TickInterval = Duration(DefaultTickInterval)
	}
	if d.RetentionDays == 0 {
		d.RetentionDays = DefaultRetentionDays
	}
	if d.DataDir == "" {
		d.DataDir = DefaultDataDir()
	}
}

func rejectUnknownKeys(path string, md toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		keys = append(keys, k.String())
	}
	return fmt.Errorf("%s: unknown keys: %s (typo? strict decoding rejects unrecognized keys)",
		path, strings.Join(keys, ", "))
}

// checkKeyFilePermissions refuses a group/other-readable file that holds
// api_key (org rule: configs holding credentials must check permissions).
func checkKeyFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s holds api_key but is group/other-accessible (mode %o) — run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	return nil
}

var taskNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Validate checks the whole configuration and returns every problem found.
// lookupEnv is the process-environment lookup (injected for tests).
func (c *Config) Validate(lookupEnv func(string) (string, bool)) []error {
	var errs []error
	addf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if err := validateLoopbackListen(c.Daemon.Listen); err != nil {
		errs = append(errs, err)
	}
	if c.Daemon.RetentionDays < 0 {
		addf("[daemon] retention_days must be >= 0, got %d", c.Daemon.RetentionDays)
	}

	seen := map[string]string{}
	for i := range c.Tasks {
		t := &c.Tasks[i]
		where := fmt.Sprintf("%s: task %q", t.Source, t.Name)
		if t.Name == "" {
			addf("%s: task with empty name", t.Source)
			continue
		}
		if !taskNameRe.MatchString(t.Name) {
			addf("%s: invalid name (allowed: letters, digits, . _ -)", where)
		}
		if prev, dup := seen[t.Name]; dup {
			addf("%s: duplicate name (also defined in %s)", where, prev)
		} else {
			seen[t.Name] = t.Source
		}

		switch {
		case t.Cron == "" && !t.IsWatermark():
			addf("%s: a trigger is required — set cron or watermark", where)
		case t.Cron != "" && t.IsWatermark():
			addf("%s: cron and watermark are mutually exclusive", where)
		case t.Cron != "":
			if _, err := schedule.Parse(t.Cron); err != nil {
				addf("%s: %v", where, err)
			}
		}
		if t.IsWatermark() {
			// Overlap/catch-up/jitter describe how discrete cron fires
			// collide; a watermark re-evaluates after every run, so these
			// knobs would silently do nothing — reject them instead.
			if t.Overlap != "" {
				addf("%s: overlap has no effect on a watermark task", where)
			}
			if t.CatchUp != nil {
				addf("%s: catch_up has no effect on a watermark task", where)
			}
			if t.Jitter > 0 {
				addf("%s: jitter has no effect on a watermark task", where)
			}
		}

		switch {
		case !t.Command.set:
			addf("%s: command is required", where)
		case t.Shell && !t.Command.IsStr:
			addf("%s: shell = true requires command to be a string", where)
		case !t.Shell && t.Command.IsStr:
			addf("%s: string command requires shell = true (use an array for direct execution)", where)
		case !t.Shell && len(t.Command.Argv) == 0:
			addf("%s: command array is empty", where)
		case t.Shell && strings.TrimSpace(t.Command.Str) == "":
			addf("%s: shell command is empty", where)
		}

		switch t.OverlapPolicy() {
		case OverlapQueueOne, OverlapSkip, OverlapKillAndRestart:
		default:
			addf("%s: invalid overlap %q (queue-one | skip | kill-and-restart)", where, t.Overlap)
		}

		for k := range t.Env {
			if !validVarName(k) {
				addf("%s: invalid env key %q", where, k)
			}
		}

		// Pre-flight the ${VAR} expansion so an undefined variable is a
		// validate-time finding, not a launch-time failure.
		if t.Command.set && !t.Command.IsStr && !t.Shell {
			if _, err := t.ResolvedArgv(lookupEnv); err != nil {
				addf("%s: %v", where, err)
			}
		}
		if t.Workdir != "" {
			if _, err := t.ResolvedWorkdir(lookupEnv); err != nil {
				addf("%s: workdir: %v", where, err)
			}
		}
	}
	return errs
}

func validateLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("[daemon] listen %q: %v", listen, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("[daemon] listen %q is not loopback — remote exposure is out of scope (RFP §3)", listen)
	}
	return nil
}
