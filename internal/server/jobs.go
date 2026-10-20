package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/scan"
)

// Job is a background operation the UI can poll and cancel.
type Job struct {
	mu        sync.Mutex
	ID        string
	Kind      string
	Params    map[string]any
	State     string
	progress  float64
	found     int
	logs      []string
	Result    any
	Error     string
	started   time.Time
	cancelled bool
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Check returns scan.ErrCancelled once the user asked to stop.
func (j *Job) Check() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelled {
		return scan.ErrCancelled
	}
	return nil
}

// Log appends a line to the job log (the last 300 are kept).
func (j *Job) Log(m string) {
	j.mu.Lock()
	j.logs = append(j.logs, time.Now().Format("[15:04:05] ")+m)
	if len(j.logs) > 300 {
		j.logs = j.logs[len(j.logs)-300:]
	}
	j.mu.Unlock()
}

// SetProgress records progress between 0 and 1.
func (j *Job) SetProgress(f float64) { j.mu.Lock(); j.progress = f; j.mu.Unlock() }

// SetFound records how many files were found so far.
func (j *Job) SetFound(n int) { j.mu.Lock(); j.found = n; j.mu.Unlock() }

// Cancel asks the job to stop.
func (j *Job) Cancel() { j.mu.Lock(); j.cancelled = true; j.mu.Unlock() }

// Running reports whether the job has not finished.
func (j *Job) Running() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.State == "running" }

// View is the JSON shape the UI polls.
func (j *Job) View() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	logs := j.logs
	if len(logs) > 40 {
		logs = logs[len(logs)-40:]
	}
	var errv any
	if j.Error != "" {
		errv = j.Error
	}
	return map[string]any{"id": j.ID, "kind": j.Kind, "params": j.Params, "state": j.State, "progress": j.progress,
		"found": j.found, "logs": append([]string{}, logs...), "error": errv, "elapsed": time.Since(j.started).Seconds(),
		"result": j.Result}
}

func (s *Server) startJob(kind string, params map[string]any, fn func(*Job) (any, error)) *Job {
	j := &Job{ID: newID(), Kind: kind, Params: params, State: "running", started: time.Now(), logs: []string{}}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				j.mu.Lock()
				j.State, j.Error = "error", fmt.Sprint("internal error: ", r)
				j.mu.Unlock()
			}
		}()
		res, err := fn(j)
		j.mu.Lock()
		defer j.mu.Unlock()
		j.Result = res
		switch {
		case err == scan.ErrCancelled:
			j.State = "cancelled"
		case err != nil:
			j.State, j.Error = "error", err.Error()
		default:
			j.State, j.progress = "done", 1
		}
	}()
	return j
}

func (s *Server) anyRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Running() {
			return true
		}
	}
	return false
}
