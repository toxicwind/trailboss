// Package serve runs trailboss's web UI and JSON API: `trailboss serve`
// starts a Go net/http server (no new dependencies) with an embedded vanilla-JS
// UI. Selections are listed from lockfiles on disk, select/mirror/scan/apply
// run as background jobs that call the Go packages directly (never by shelling
// out to the trailboss binary), and job logs stream to the browser over
// server-sent events.
//
// Auth: the server reads TRAILBOSS_PAT from its own environment at startup and
// never exposes it — not via any endpoint, not in job logs (child env is never
// logged; argv never carries the token by construction).
package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// JobStatus is a job's lifecycle state.
type JobStatus string

const (
	JobQueued  JobStatus = "queued"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobError   JobStatus = "error"
)

// terminal reports whether no further log lines or status changes will come.
func (s JobStatus) terminal() bool { return s == JobDone || s == JobError }

// Job is one background select/mirror/scan/apply run: an append-only log,
// live subscribers, and a machine-readable result when finished.
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    JobStatus `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Result    any       `json:"result,omitempty"`

	mu   sync.Mutex
	log  []string
	subs []chan string
}

// logLine appends one line to the job log and fans it out to SSE subscribers.
// Lines are capped so a runaway child can't grow the log without bound; the
// cap is recorded in the log itself rather than silently truncating.
func (j *Job) logLine(line string) {
	if len(line) > 4096 {
		line = line[:4096] + "… (truncated)"
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.log) >= 20000 {
		if j.log[len(j.log)-1] != "… (log truncated at 20000 lines)" {
			j.log = append(j.log, "… (log truncated at 20000 lines)")
		}
		return
	}
	j.log = append(j.log, line)
	for _, sub := range j.subs {
		select {
		case sub <- line:
		default:
			// A slow consumer misses lines rather than blocking the job; the
			// full log is always available from GET /api/jobs/{id}.
		}
	}
}

// logf formats and appends one log line.
func (j *Job) logf(format string, args ...any) {
	j.logLine(fmt.Sprintf(format, args...))
}

// snapshot returns a copy of the log so far.
func (j *Job) snapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.log))
	copy(out, j.log)
	return out
}

// subscribe registers for live log lines. The channel is closed when the job
// reaches a terminal state (after a final status event is delivered).
func (j *Job) subscribe() <-chan string {
	j.mu.Lock()
	defer j.mu.Unlock()
	ch := make(chan string, 256)
	j.subs = append(j.subs, ch)
	return ch
}

// subscribeLive atomically captures the current log length and subscribes for
// new lines — or reports the job already terminal, in which case no
// subscription is created. The caller replays the first n log lines and then
// streams the subscription: lines logged after the capture arrive only via
// the channel, so nothing is missed, duplicated, or stranded by a job that
// goes terminal between the replay and the subscribe.
func (j *Job) subscribeLive() (sub <-chan string, n int, status JobStatus) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status.terminal() {
		return nil, len(j.log), j.Status
	}
	ch := make(chan string, 256)
	j.subs = append(j.subs, ch)
	return ch, len(j.log), j.Status
}

// logPrefix returns a copy of the first n log lines.
func (j *Job) logPrefix(n int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n > len(j.log) {
		n = len(j.log)
	}
	out := make([]string, n)
	copy(out, j.log[:n])
	return out
}

func (j *Job) unsubscribe(ch <-chan string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, sub := range j.subs {
		if (<-chan string)(sub) == ch {
			j.subs = append(j.subs[:i], j.subs[i+1:]...)
			return
		}
	}
}

func (j *Job) setStatus(s JobStatus) {
	j.mu.Lock()
	j.Status = s
	if s.terminal() {
		j.EndedAt = time.Now().UTC()
	}
	subs := j.subs
	j.subs = nil
	j.mu.Unlock()
	// Deliver the terminal event, then close: the SSE handler ends the stream
	// on a closed channel.
	if s.terminal() {
		payload, _ := json.Marshal(sseEvent{Type: "status", Status: string(s)})
		for _, sub := range subs {
			select {
			case sub <- "\x00" + string(payload):
			default:
			}
			close(sub)
		}
	}
}

func (j *Job) setSummary(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Summary = s
}

// getSummary returns the summary under the job lock.
func (j *Job) getSummary() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Summary
}

func (j *Job) setResult(r any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Result = r
}

// sseEvent is the JSON payload of one SSE data frame. A log frame carries
// Line; a status frame carries Status. A payload prefixed with \x00 on the
// subscriber channel is a control frame (terminal status), not a log line.
type sseEvent struct {
	Type   string `json:"type"`
	Line   string `json:"line,omitempty"`
	Status string `json:"status,omitempty"`
}

// jobDTO is an immutable snapshot of a job for JSON encoding: every field is
// copied under the job's lock, so HTTP handlers never encode mutable state
// while the job goroutine is still writing it.
type jobDTO struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    JobStatus `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Result    any       `json:"result,omitempty"`
	Log       []string  `json:"log,omitempty"`
}

// snapshotDTO copies the job's mutable fields under one lock.
func (j *Job) snapshotDTO(withLog bool) jobDTO {
	j.mu.Lock()
	defer j.mu.Unlock()
	d := jobDTO{
		ID:        j.ID,
		Kind:      j.Kind,
		Status:    j.Status,
		StartedAt: j.StartedAt,
		EndedAt:   j.EndedAt,
		Summary:   j.Summary,
		Result:    j.Result,
	}
	if withLog {
		d.Log = make([]string, len(j.log))
		copy(d.Log, j.log)
	}
	return d
}

// registry is the in-memory job store. Jobs live for the server's lifetime;
// there is no persistence — a restart loses them, which the UI states plainly.
type registry struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	order []string // creation order, oldest first
}

func newRegistry() *registry {
	return &registry{jobs: map[string]*Job{}}
}

func newJobID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func (r *registry) add(kind string) *Job {
	j := &Job{ID: newJobID(), Kind: kind, Status: JobQueued, StartedAt: time.Now().UTC()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.ID] = j
	r.order = append(r.order, j.ID)
	return j
}

func (r *registry) get(id string) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

// list returns jobs newest-first.
func (r *registry) list() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, 0, len(r.order))
	for i := len(r.order) - 1; i >= 0; i-- {
		out = append(out, r.jobs[r.order[i]])
	}
	return out
}

// exec runs name with args/env, streaming combined output into the job log
// line-by-line as it arrives and returning the captured bytes. The environment
// is NEVER logged — it carries mapped token vars.
func (j *Job) exec(ctx context.Context, name string, args, env []string) ([]byte, error) {
	// Log the invocation with argv only. argv never carries the token by
	// construction (apply/mirror pass it via env), so this line is safe.
	j.logf("$ %s %s", name, strings.Join(args, " "))
	// name/args are trailboss's own delegate wiring (git/ghorg/multi-gitter +
	// flags built in-process), never unsanitised external input.
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: controlled delegate invocation, not external input.
	c.Env = env
	var buf bytes.Buffer
	outLW := &lineWriter{job: j}
	errLW := &lineWriter{job: j}
	c.Stdout = io.MultiWriter(&buf, outLW)
	c.Stderr = io.MultiWriter(&buf, errLW)
	err := c.Run()
	// Flush trailing partial lines: a child that ends without a final
	// newline would otherwise lose its last line from the job log.
	outLW.flush()
	errLW.flush()
	return buf.Bytes(), err
}

// lineWriter splits a streaming byte flow into lines and appends each complete
// line to the job log, so the SSE viewer sees output live. A trailing partial
// line is held until flush, which the caller runs after the command exits.
type lineWriter struct {
	job *Job
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.job.logLine(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush emits any trailing partial line held by Write.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.job.logLine(string(w.buf))
		w.buf = nil
	}
}

// applyRunner adapts the job's exec to apply.Runner's signature (captured
// output + error), so apply.Apply / apply.Direct stream into the job log.
func (j *Job) applyRunner() func(ctx context.Context, name string, args, env []string) ([]byte, error) {
	return j.exec
}

// mirrorRunner adapts the job's exec to mirror.Runner's signature.
func (j *Job) mirrorRunner() func(ctx context.Context, name string, args, env []string) error {
	return func(ctx context.Context, name string, args, env []string) error {
		_, err := j.exec(ctx, name, args, env)
		return err
	}
}
