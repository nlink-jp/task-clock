package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/nlink-jp/task-clock/internal/api"
	"github.com/nlink-jp/task-clock/internal/store"
)

// clientFlags are the flags shared by every daemon-querying subcommand.
type clientFlags struct {
	configDir string
	jsonOut   bool
}

func newClientFlagSet(name string, cf *clientFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&cf.configDir, "config", "", "config directory (default: search paths)")
	fs.BoolVar(&cf.jsonOut, "json", false, "print the raw API response")
	return fs
}

func runStatus(args []string, stdout, stderr io.Writer) error {
	var cf clientFlags
	fs := newClientFlagSet("status", &cf)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, code, err := fetchTasks(cf)
	if err != nil {
		return err
	}
	if cf.jsonOut {
		fmt.Fprintln(stdout, string(body))
		return nil
	}
	views, err := decodeTasks(body, code)
	if err != nil {
		return err
	}
	now := time.Now()
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tNEXT FIRE\tNEXT RUN\tLAST RUN")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			v.Name, renderState(v, now), renderNextFire(v), renderNextRun(v), renderLastRun(v))
	}
	return w.Flush()
}

func renderState(v api.TaskView, now time.Time) string {
	if !v.Enabled {
		return "disabled"
	}
	if v.Running != nil {
		s := "running " + fmtDur(now.Sub(v.Running.StartedAt))
		if v.OverrunSeconds > 0 {
			// The overrun is an explicit state word, never a negative countdown.
			s += fmt.Sprintf(" (overrun %s)", fmtDur(time.Duration(v.OverrunSeconds)*time.Second))
		}
		return s
	}
	return "idle"
}

func renderNextFire(v api.TaskView) string {
	if v.NextFire == nil {
		return "-"
	}
	return fmtLocal(*v.NextFire)
}

func renderNextRun(v api.TaskView) string {
	switch v.NextExpectedRun.Kind {
	case "after_current":
		return "after current run"
	case "after_success":
		return "success + " + v.Watermark
	case "at":
		if v.NextExpectedRun.At != nil {
			return fmtLocal(*v.NextExpectedRun.At)
		}
	}
	return "-"
}

// renderTrigger shows what schedules the task: a cron expression or the
// watermark interval.
func renderTrigger(v api.TaskView) string {
	if v.Watermark != "" {
		return "success + " + v.Watermark
	}
	return v.Cron
}

func renderLastRun(v api.TaskView) string {
	r := v.LastRun
	if r == nil {
		return "-"
	}
	switch {
	case r.Outcome == store.OutcomeMissed:
		return fmt.Sprintf("missed(%s) @%s", r.MissedReason, fmtLocal(r.ScheduledFor))
	case r.FinishedAt == nil:
		return "in progress"
	case r.ExitCode != nil && *r.ExitCode == 0:
		return fmt.Sprintf("ok @%s", fmtLocal(*r.FinishedAt))
	default:
		exit := "?"
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		return fmt.Sprintf("exit %s @%s", exit, fmtLocal(*r.FinishedAt))
	}
}

func runList(args []string, stdout, stderr io.Writer) error {
	var cf clientFlags
	fs := newClientFlagSet("list", &cf)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, code, err := fetchTasks(cf)
	if err != nil {
		return err
	}
	if cf.jsonOut {
		fmt.Fprintln(stdout, string(body))
		return nil
	}
	views, err := decodeTasks(body, code)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tENABLED\tTRIGGER\tOVERLAP\tCATCH_UP")
	for _, v := range views {
		overlap, catchUp := v.Overlap, strconv.FormatBool(v.CatchUp)
		if v.Watermark != "" {
			overlap, catchUp = "-", "-"
		}
		fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\n", v.Name, v.Enabled, renderTrigger(v), overlap, catchUp)
	}
	return w.Flush()
}

func fetchTasks(cf clientFlags) ([]byte, int, error) {
	c, err := newClient(cf.configDir)
	if err != nil {
		return nil, 0, err
	}
	return c.call("GET", "/v1/tasks")
}

func decodeTasks(body []byte, code int) ([]api.TaskView, error) {
	if code != http.StatusOK {
		return nil, apiError(body, code)
	}
	var payload struct {
		Tasks []api.TaskView `json:"tasks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload.Tasks, nil
}

func runHistory(args []string, stdout, stderr io.Writer) error {
	var cf clientFlags
	fs := newClientFlagSet("history", &cf)
	limit := fs.Int("limit", 20, "maximum rows")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: task-clock history [flags] <task>")
	}
	name := fs.Arg(0)
	c, err := newClient(cf.configDir)
	if err != nil {
		return err
	}
	body, code, err := c.call("GET", fmt.Sprintf("/v1/tasks/%s/history?limit=%d", name, *limit))
	if err != nil {
		return err
	}
	if cf.jsonOut {
		fmt.Fprintln(stdout, string(body))
		return nil
	}
	if code != http.StatusOK {
		return apiError(body, code)
	}
	var payload struct {
		Runs []store.Run `json:"runs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SCHEDULED\tSTARTED\tDURATION\tOUTCOME\tEXIT\tLOG")
	for _, r := range payload.Runs {
		started, duration, exit := "-", "-", "-"
		if r.StartedAt != nil {
			started = fmtLocal(*r.StartedAt)
			if r.FinishedAt != nil {
				duration = fmtDur(r.FinishedAt.Sub(*r.StartedAt))
			} else {
				duration = "running"
			}
		}
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		outcome := r.Outcome
		if r.MissedReason != "" {
			outcome += "(" + r.MissedReason + ")"
		}
		log := r.LogPath
		if log == "" {
			log = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			fmtLocal(r.ScheduledFor), started, duration, outcome, exit, log)
	}
	return w.Flush()
}

func runTrigger(args []string, stdout, stderr io.Writer) error {
	var cf clientFlags
	fs := newClientFlagSet("trigger", &cf)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: task-clock trigger [flags] <task>")
	}
	name := fs.Arg(0)
	c, err := newClient(cf.configDir)
	if err != nil {
		return err
	}
	body, code, err := c.call("POST", "/v1/tasks/"+name+"/trigger")
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return apiError(body, code)
	}
	fmt.Fprintf(stdout, "triggered: %s\n", name)
	return nil
}

func runReload(args []string, stdout, stderr io.Writer) error {
	var cf clientFlags
	fs := newClientFlagSet("reload", &cf)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient(cf.configDir)
	if err != nil {
		return err
	}
	body, code, err := c.call("POST", "/v1/reload")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return apiError(body, code)
	}
	fmt.Fprintln(stdout, "reloaded")
	return nil
}

// apiError turns an error response into a readable message.
func apiError(body []byte, code int) error {
	var payload struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		if payload.Detail != "" {
			return fmt.Errorf("%s (HTTP %d): %s", payload.Error, code, payload.Detail)
		}
		return fmt.Errorf("%s (HTTP %d)", payload.Error, code)
	}
	return fmt.Errorf("unexpected API response (HTTP %d)", code)
}
