package store

import (
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ts(t *testing.T, v string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestRunLifecycle(t *testing.T) {
	s := openTest(t)
	sched := ts(t, "2026-08-31T10:30:00Z")
	started := sched.Add(5 * time.Second)

	id, err := s.StartRun("analyze", sched, OutcomeOnTime, "/logs/analyze/x.log", started)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(id, 0, "", started.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	run, err := s.LastRun("analyze")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("LastRun returned nil")
	}
	if !run.ScheduledFor.Equal(sched) || run.StartedAt == nil || !run.StartedAt.Equal(started) {
		t.Errorf("scheduled/started mismatch: %+v", run)
	}
	if run.FinishedAt == nil || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Errorf("finish not recorded: %+v", run)
	}
	if run.Outcome != OutcomeOnTime || run.LogPath != "/logs/analyze/x.log" {
		t.Errorf("outcome/log mismatch: %+v", run)
	}
}

func TestRecordMissedAndHistoryOrder(t *testing.T) {
	s := openTest(t)
	base := ts(t, "2026-08-31T10:00:00Z")

	if _, err := s.RecordMissed("a", base, MissedOverlap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMissed("a", base.Add(30*time.Minute), MissedCoalesced); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMissed("b", base, MissedCatchUpDisabled); err != nil {
		t.Fatal(err)
	}

	runs, err := s.History("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("History(a) = %d rows, want 2", len(runs))
	}
	if !runs[0].ScheduledFor.After(runs[1].ScheduledFor) {
		t.Errorf("history not newest-first: %+v", runs)
	}
	if runs[0].Outcome != OutcomeMissed || runs[0].MissedReason != MissedCoalesced {
		t.Errorf("missed row wrong: %+v", runs[0])
	}
	if runs[0].StartedAt != nil || runs[0].ExitCode != nil {
		t.Errorf("missed row must have no start/exit: %+v", runs[0])
	}

	all, err := s.History("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("History(all) = %d rows, want 3", len(all))
	}
}

func TestLastSuccessSkipsFailures(t *testing.T) {
	s := openTest(t)
	base := ts(t, "2026-08-31T10:00:00Z")

	id1, _ := s.StartRun("a", base, OutcomeOnTime, "", base)
	s.FinishRun(id1, 0, "", base.Add(time.Minute))
	id2, _ := s.StartRun("a", base.Add(time.Hour), OutcomeOnTime, "", base.Add(time.Hour))
	s.FinishRun(id2, 1, "boom", base.Add(61*time.Minute))

	run, err := s.LastSuccess("a")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.ID != id1 {
		t.Errorf("LastSuccess = %+v, want id %d", run, id1)
	}

	if run, err := s.LastSuccess("never-ran"); err != nil || run != nil {
		t.Errorf("LastSuccess(no rows) = %+v, %v; want nil, nil", run, err)
	}
}

func TestPrune(t *testing.T) {
	s := openTest(t)
	old := ts(t, "2026-07-01T00:00:00Z")
	recent := ts(t, "2026-08-30T00:00:00Z")

	idOld, _ := s.StartRun("a", old, OutcomeOnTime, "/logs/a/old.log", old)
	s.FinishRun(idOld, 0, "", old.Add(time.Minute))
	s.RecordMissed("a", old.Add(time.Hour), MissedOverlap)
	idNew, _ := s.StartRun("a", recent, OutcomeOnTime, "/logs/a/new.log", recent)
	s.FinishRun(idNew, 0, "", recent.Add(time.Minute))

	paths, err := s.Prune(ts(t, "2026-08-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/logs/a/old.log" {
		t.Errorf("pruned paths = %v, want [/logs/a/old.log]", paths)
	}
	runs, err := s.History("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != idNew {
		t.Errorf("after prune: %+v, want only id %d", runs, idNew)
	}
}
