// Package store persists the scheduled-vs-actual run history in SQLite
// (pure Go driver, no CGO). The schema is an internal implementation
// detail: the external contract is the HTTP API only (RFP §2), so nothing
// outside the daemon reads this database directly.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Outcome values for a firing (RFP §2). Every planned fire ends up as
// exactly one row, so skips and drift are observable facts.
const (
	OutcomeOnTime = "on_time" // started at its scheduled fire
	OutcomeQueued = "queued"  // ran later than scheduled (overlap queue or catch-up)
	OutcomeMissed = "missed"  // never ran; missed_reason says why
	OutcomeManual = "manual"  // started by explicit trigger
)

// Missed reasons.
const (
	MissedOverlap         = "overlap"           // dropped by overlap policy
	MissedCoalesced       = "coalesced"         // older fire folded into one catch-up run
	MissedCatchUpDisabled = "catch_up_disabled" // fire was late and catch_up = false
)

// Run is one row of the history.
type Run struct {
	ID           int64      `json:"id"`
	Task         string     `json:"task"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Outcome      string     `json:"outcome"`
	MissedReason string     `json:"missed_reason,omitempty"`
	LogPath      string     `json:"log_path,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// Store wraps the SQLite database under the data directory.
type Store struct {
	db      *sql.DB
	dataDir string
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	task          TEXT NOT NULL,
	scheduled_for TEXT NOT NULL,
	started_at    TEXT,
	finished_at   TEXT,
	exit_code     INTEGER,
	outcome       TEXT NOT NULL,
	missed_reason TEXT NOT NULL DEFAULT '',
	log_path      TEXT NOT NULL DEFAULT '',
	error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task, id);
CREATE TABLE IF NOT EXISTS paused_tasks (
	task TEXT PRIMARY KEY
);
`

// Open opens (creating if needed) the database under dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		filepath.Join(dataDir, "task-clock.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection sidesteps SQLite writer contention entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DataDir returns the directory this store lives in.
func (s *Store) DataDir() string { return s.dataDir }

// LogDir is where run stdout/stderr files are written.
func (s *Store) LogDir() string { return filepath.Join(s.dataDir, "logs") }

func encodeTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func decodeTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// RecordMissed records a fire that never ran.
func (s *Store) RecordMissed(task string, scheduledFor time.Time, reason string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO runs (task, scheduled_for, outcome, missed_reason) VALUES (?, ?, ?, ?)`,
		task, encodeTime(scheduledFor), OutcomeMissed, reason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// StartRun records a run starting now and returns its row id.
func (s *Store) StartRun(task string, scheduledFor time.Time, outcome, logPath string, now time.Time) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO runs (task, scheduled_for, started_at, outcome, log_path) VALUES (?, ?, ?, ?, ?)`,
		task, encodeTime(scheduledFor), encodeTime(now), outcome, logPath)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun records a run's completion.
func (s *Store) FinishRun(id int64, exitCode int, errMsg string, now time.Time) error {
	_, err := s.db.Exec(
		`UPDATE runs SET finished_at = ?, exit_code = ?, error = ? WHERE id = ?`,
		encodeTime(now), exitCode, errMsg, id)
	return err
}

// History returns runs newest-first. An empty task means all tasks.
func (s *Store) History(task string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, task, scheduled_for, started_at, finished_at, exit_code,
			outcome, missed_reason, log_path, error FROM runs `
	args := []any{}
	if task != "" {
		query += `WHERE task = ? `
		args = append(args, task)
	}
	query += `ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// LastRun returns the newest history row for a task, or nil if none.
func (s *Store) LastRun(task string) (*Run, error) {
	runs, err := s.History(task, 1)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// LastSuccess returns the newest run that finished with exit code 0, or nil.
// (The Phase 2 watermark trigger keys off this.)
func (s *Store) LastSuccess(task string) (*Run, error) {
	rows, err := s.db.Query(
		`SELECT id, task, scheduled_for, started_at, finished_at, exit_code,
			outcome, missed_reason, log_path, error FROM runs
		 WHERE task = ? AND exit_code = 0 ORDER BY id DESC LIMIT 1`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	r, err := scanRun(rows)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetPaused persists a task's paused state. Pause is the operational
// per-task off switch (GUI/API); it survives daemon restarts, while the
// config's `enabled` stays the declarative baseline in tasks.d.
func (s *Store) SetPaused(task string, paused bool) error {
	if paused {
		_, err := s.db.Exec(`INSERT OR IGNORE INTO paused_tasks (task) VALUES (?)`, task)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM paused_tasks WHERE task = ?`, task)
	return err
}

// PausedTasks returns the set of persistently paused task names.
func (s *Store) PausedTasks() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT task FROM paused_tasks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		set[t] = true
	}
	return set, rows.Err()
}

// Prune deletes history rows older than cutoff and returns the log paths of
// the deleted rows so the caller can remove the files.
func (s *Store) Prune(cutoff time.Time) ([]string, error) {
	enc := encodeTime(cutoff)
	where := `COALESCE(finished_at, started_at, scheduled_for) < ?`
	rows, err := s.db.Query(`SELECT log_path FROM runs WHERE `+where+` AND log_path != ''`, enc)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM runs WHERE `+where, enc); err != nil {
		return nil, err
	}
	return paths, nil
}

type rowScanner interface{ Scan(...any) error }

func scanRun(rows rowScanner) (Run, error) {
	var (
		r                              Run
		scheduledFor                   string
		startedAt, finishedAt          sql.NullString
		exitCode                       sql.NullInt64
	)
	if err := rows.Scan(&r.ID, &r.Task, &scheduledFor, &startedAt, &finishedAt,
		&exitCode, &r.Outcome, &r.MissedReason, &r.LogPath, &r.Error); err != nil {
		return Run{}, err
	}
	var err error
	if r.ScheduledFor, err = decodeTime(scheduledFor); err != nil {
		return Run{}, err
	}
	if startedAt.Valid {
		t, err := decodeTime(startedAt.String)
		if err != nil {
			return Run{}, err
		}
		r.StartedAt = &t
	}
	if finishedAt.Valid {
		t, err := decodeTime(finishedAt.String)
		if err != nil {
			return Run{}, err
		}
		r.FinishedAt = &t
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}
	return r, nil
}
