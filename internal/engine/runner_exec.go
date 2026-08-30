package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// killGrace is how long SIGTERM gets before SIGKILL follows.
const killGrace = 5 * time.Second

// ExecRunner is the production Runner: it launches the task in its own
// process group and writes combined stdout+stderr to the run's log file
// (one chronological stream per run — RFP §2).
type ExecRunner struct{}

var _ Runner = ExecRunner{}

type execHandle struct {
	pgid   int
	done   chan Result
	exited chan struct{} // closed once Wait returns
}

func (ExecRunner) Start(spec RunSpec) (Handle, error) {
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Workdir
	cmd.Env = spec.Env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own process group, so Kill reaches the task's children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}

	h := &execHandle{
		pgid:   cmd.Process.Pid,
		done:   make(chan Result, 1),
		exited: make(chan struct{}),
	}
	go func() {
		defer logFile.Close()
		err := cmd.Wait()
		close(h.exited)
		res := Result{ExitCode: cmd.ProcessState.ExitCode()}
		if err != nil && res.ExitCode < 0 {
			res.Err = err.Error() // signaled, not a plain non-zero exit
		}
		h.done <- res
	}()
	return h, nil
}

func (h *execHandle) Done() <-chan Result { return h.done }

// Kill sends SIGTERM to the process group, then SIGKILL after a grace
// period unless the process exits first. Signalling an already-gone group
// is a harmless no-op.
func (h *execHandle) Kill() {
	syscall.Kill(-h.pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-time.After(killGrace):
			syscall.Kill(-h.pgid, syscall.SIGKILL)
		case <-h.exited:
		}
	}()
}
