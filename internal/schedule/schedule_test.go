package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Spec {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("bad test time %q: %v", value, err)
	}
	return ts
}

func TestNextIsPureFunction(t *testing.T) {
	s := mustParse(t, "*/30 * * * *")
	base := at(t, "2026-08-31T10:05:00Z")
	first := s.Next(base)
	second := s.Next(base)
	if !first.Equal(second) {
		t.Errorf("Next is not deterministic: %v vs %v", first, second)
	}
	if want := at(t, "2026-08-31T10:30:00Z"); !first.Equal(want) {
		t.Errorf("Next = %v, want %v", first, want)
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	s := mustParse(t, "0 * * * *")
	onTheHour := at(t, "2026-08-31T10:00:00Z")
	next := s.Next(onTheHour)
	if want := at(t, "2026-08-31T11:00:00Z"); !next.Equal(want) {
		t.Errorf("Next(on-the-hour) = %v, want strictly after: %v", next, want)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, expr := range []string{"", "not a cron", "* * * *", "@reboot"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q): expected error, got nil", expr)
		}
	}
}

func TestParseAcceptsDescriptors(t *testing.T) {
	s := mustParse(t, "@hourly")
	base := at(t, "2026-08-31T10:05:00Z")
	if want := at(t, "2026-08-31T11:00:00Z"); !s.Next(base).Equal(want) {
		t.Errorf("@hourly Next = %v, want %v", s.Next(base), want)
	}
}

// A one-hour delay of a 30-minute schedule must surface both elapsed fires —
// the exact scenario from the originating incident, where launchd silently
// dropped one.
func TestFiresBetweenEnumeratesElapsedFires(t *testing.T) {
	s := mustParse(t, "*/30 * * * *")
	after := at(t, "2026-08-31T10:00:00Z") // last handled fire
	until := at(t, "2026-08-31T11:00:00Z") // woke up an hour later
	fires := FiresBetween(s, after, until, 0)
	want := []time.Time{
		at(t, "2026-08-31T10:30:00Z"),
		at(t, "2026-08-31T11:00:00Z"),
	}
	if len(fires) != len(want) {
		t.Fatalf("got %d fires (%v), want %d", len(fires), fires, len(want))
	}
	for i := range want {
		if !fires[i].Equal(want[i]) {
			t.Errorf("fire[%d] = %v, want %v", i, fires[i], want[i])
		}
	}
}

func TestFiresBetweenEmptyWindow(t *testing.T) {
	s := mustParse(t, "*/30 * * * *")
	base := at(t, "2026-08-31T10:00:00Z")
	if fires := FiresBetween(s, base, base.Add(time.Minute), 0); len(fires) != 0 {
		t.Errorf("expected no fires in a 1-minute window, got %v", fires)
	}
}

func TestFiresBetweenCap(t *testing.T) {
	s := mustParse(t, "* * * * *")
	after := at(t, "2026-08-31T00:00:00Z")
	until := at(t, "2026-08-31T06:00:00Z") // 360 fires uncapped
	if fires := FiresBetween(s, after, until, 5); len(fires) != 5 {
		t.Errorf("cap: got %d fires, want 5", len(fires))
	}
}
