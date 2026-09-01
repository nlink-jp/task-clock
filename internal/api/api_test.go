package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/engine"
	"github.com/nlink-jp/task-clock/internal/store"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type nullHandle struct{ done chan engine.Result }

func (h nullHandle) Done() <-chan engine.Result { return h.done }
func (h nullHandle) Kill()                      {}
func (h nullHandle) Pid() int                   { return 12345 }

type nullRunner struct {
	mu     sync.Mutex
	starts int
}

func (r *nullRunner) Start(engine.RunSpec) (engine.Handle, error) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return nullHandle{done: make(chan engine.Result, 1)}, nil
}

func (r *nullRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

const testKey = "test-api-key-0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T, reload func() error) (*httptest.Server, *nullRunner, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	clk := fixedClock{t: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)}
	runner := &nullRunner{}
	eng := engine.New(clk, st, runner, engine.Options{
		LookupEnv: func(string) (string, bool) { return "", false },
		BaseEnv:   func() []string { return nil },
	})
	err = eng.Configure([]config.TaskConfig{{
		Name:    "analyze",
		Cron:    "*/30 * * * *",
		Command: config.CommandValue{Argv: []string{"/bin/true"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reload == nil {
		reload = func() error { return nil }
	}
	srv := httptest.NewServer(New(eng, st, testKey, reload, "test").Handler())
	t.Cleanup(srv.Close)
	return srv, runner, st
}

func request(t *testing.T, method, url, key string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	resp, body := request(t, "GET", srv.URL+"/v1/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d (%s)", resp.StatusCode, body)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	for _, key := range []string{"", "wrong-key", testKey + "x"} {
		resp, _ := request(t, "GET", srv.URL+"/v1/tasks", key)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
	}
	resp, _ := request(t, "GET", srv.URL+"/v1/tasks", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct key rejected: %d", resp.StatusCode)
	}
}

func TestNoCORSHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	resp, _ := request(t, "GET", srv.URL+"/v1/healthz", "")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS header present — cross-origin browser calls must stay denied")
	}
}

func TestTasksListShape(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	resp, body := request(t, "GET", srv.URL+"/v1/tasks", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload struct {
		Tasks []struct {
			Name            string  `json:"name"`
			Cron            string  `json:"cron"`
			NextFire        *string `json:"next_fire"`
			NextExpectedRun struct {
				Kind string `json:"kind"`
			} `json:"next_expected_run"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("bad JSON: %v — %s", err, body)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].Name != "analyze" {
		t.Fatalf("tasks = %+v", payload.Tasks)
	}
	if payload.Tasks[0].NextFire == nil || payload.Tasks[0].NextExpectedRun.Kind != "at" {
		t.Errorf("next fire fields missing: %s", body)
	}
}

func TestTaskDetailAndNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	resp, _ := request(t, "GET", srv.URL+"/v1/tasks/analyze", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("detail = %d", resp.StatusCode)
	}
	resp, body := request(t, "GET", srv.URL+"/v1/tasks/nope", testKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown task = %d (%s)", resp.StatusCode, body)
	}
}

func TestTriggerAndConflict(t *testing.T) {
	srv, runner, _ := newTestServer(t, nil)
	resp, _ := request(t, "POST", srv.URL+"/v1/tasks/analyze/trigger", testKey)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger = %d", resp.StatusCode)
	}
	if runner.count() != 1 {
		t.Fatalf("runner starts = %d", runner.count())
	}
	resp, body := request(t, "POST", srv.URL+"/v1/tasks/analyze/trigger", testKey)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second trigger = %d (%s), want 409", resp.StatusCode, body)
	}
	resp, _ = request(t, "POST", srv.URL+"/v1/tasks/nope/trigger", testKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown trigger = %d, want 404", resp.StatusCode)
	}
}

func TestPauseResumeEndpoints(t *testing.T) {
	srv, runner, _ := newTestServer(t, nil)
	resp, body := request(t, "POST", srv.URL+"/v1/tasks/analyze/pause", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d (%s)", resp.StatusCode, body)
	}
	resp, body = request(t, "GET", srv.URL+"/v1/tasks/analyze", testKey)
	if resp.StatusCode != http.StatusOK || !json.Valid(body) {
		t.Fatalf("detail = %d", resp.StatusCode)
	}
	var view struct {
		Paused bool `json:"paused"`
	}
	json.Unmarshal(body, &view)
	if !view.Paused {
		t.Errorf("task not paused: %s", body)
	}
	resp, _ = request(t, "POST", srv.URL+"/v1/tasks/analyze/resume", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume = %d", resp.StatusCode)
	}
	resp, _ = request(t, "POST", srv.URL+"/v1/tasks/nope/pause", testKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("pause unknown = %d, want 404", resp.StatusCode)
	}
	_ = runner
}

func TestHistoryEndpoint(t *testing.T) {
	srv, _, st := newTestServer(t, nil)
	when := time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC)
	st.RecordMissed("analyze", when, store.MissedOverlap)

	resp, body := request(t, "GET", srv.URL+"/v1/tasks/analyze/history?limit=10", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d", resp.StatusCode)
	}
	var payload struct {
		Runs []store.Run `json:"runs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Runs) != 1 || payload.Runs[0].Outcome != store.OutcomeMissed {
		t.Errorf("runs = %+v", payload.Runs)
	}

	resp, _ = request(t, "GET", srv.URL+"/v1/tasks/analyze/history?limit=bogus", testKey)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", resp.StatusCode)
	}
	resp, _ = request(t, "GET", srv.URL+"/v1/tasks/nope/history", testKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown task history = %d, want 404", resp.StatusCode)
	}
}

func TestReload(t *testing.T) {
	calls := 0
	srv, _, _ := newTestServer(t, func() error {
		calls++
		if calls > 1 {
			return errors.New("config.toml: unknown keys: api_keys")
		}
		return nil
	})
	resp, _ := request(t, "POST", srv.URL+"/v1/reload", testKey)
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("reload = %d, calls = %d", resp.StatusCode, calls)
	}
	resp, body := request(t, "POST", srv.URL+"/v1/reload", testKey)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("failed reload = %d (%s), want 422", resp.StatusCode, body)
	}
	var payload map[string]string
	json.Unmarshal(body, &payload)
	if payload["error"] != "reload_failed" || payload["detail"] == "" {
		t.Errorf("reload failure body = %s", body)
	}
}

func TestUnknownPathIsAuthedAndStatic(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	resp, _ := request(t, "GET", srv.URL+"/v1/other", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown path without key = %d, want 401", resp.StatusCode)
	}
	resp, body := request(t, "GET", srv.URL+"/v1/other", testKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d (%s), want 404", resp.StatusCode, body)
	}
}
