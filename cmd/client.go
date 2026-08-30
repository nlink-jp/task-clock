package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
)

// resolveConfigDir picks the config directory: explicit flag first, then
// the search paths. When nothing is found the error lists every searched
// location — a config that exists but is silently not read is the worst
// failure mode, so the search must be diagnosable in one glance.
func resolveConfigDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	dir, found := config.ResolveDir()
	if !found {
		return "", fmt.Errorf("no %s found; searched:\n  %s",
			config.ConfigFileName, strings.Join(config.SearchDirs(), "\n  "))
	}
	return dir, nil
}

// client talks to the daemon over the localhost API — the CLI's only
// channel to daemon state (RFP §2: single source of truth).
type client struct {
	base string
	key  string
	http *http.Client
}

func newClient(flagConfigDir string) (*client, error) {
	dir, err := resolveConfigDir(flagConfigDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	if cfg.Daemon.APIKey == "" {
		return nil, fmt.Errorf("api_key is not set in %s — generate one with:\n  openssl rand -hex 32", cfg.FilePath)
	}
	return &client{
		base: "http://" + cfg.Daemon.Listen,
		key:  cfg.Daemon.APIKey,
		http: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// call performs a request and returns the raw body and status code.
// A refused connection becomes a "daemon not running" diagnosis instead of
// a bare network error.
func (c *client) call(method, path string) ([]byte, int, error) {
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, 0, fmt.Errorf("daemon is not running (%s unreachable) — start it with:\n  task-clock serve\nor install the launchd agent:\n  task-clock install", c.base)
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// fmtLocal renders a UTC timestamp in local time for humans.
func fmtLocal(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

// fmtDur renders a duration compactly (e.g. 92s -> 1m32s).
func fmtDur(d time.Duration) string { return d.Round(time.Second).String() }
