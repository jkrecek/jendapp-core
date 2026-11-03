// Package ops runs module commands without Claude: sources (data for views)
// and command operations (the module's own write actions).
package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"jendapp/internal/module"
	"jendapp/internal/store"
)

const (
	maxSourceOutput = 16 << 20
	maxResult       = 8 << 10
)

type Runner struct {
	store *store.Store
	ctx   context.Context // cancelled on shutdown

	mu    sync.Mutex
	locks map[string]*sync.Mutex // module id → serialises its operations
	wg    sync.WaitGroup
}

func NewRunner(ctx context.Context, st *store.Store) *Runner {
	return &Runner{store: st, ctx: ctx, locks: map[string]*sync.Mutex{}}
}

// Wait blocks until running operations have exited.
func (r *Runner) Wait() { r.wg.Wait() }

// Data is what a source prints.
type Data struct {
	Version int              `json:"jendapp"`
	Rows    []map[string]any `json:"rows"`
	Summary map[string]any   `json:"summary"`
	Notes   []string         `json:"notes"`
}

// Snapshot is a source's data as served to the app.
type Snapshot struct {
	Data
	FetchedAt time.Time `json:"fetched_at"`
	// Stale means this is the last good output because the refresh failed (see Error).
	Stale bool   `json:"stale"`
	Error string `json:"error,omitempty"`
}

// Row finds a row by its "id".
func (s *Snapshot) Row(id string) map[string]any {
	for _, r := range s.Rows {
		if rid, _ := r["id"].(string); rid == id {
			return r
		}
	}
	return nil
}

// ParamsKey is the canonical form of the params a source takes.
func ParamsKey(names []string, params map[string]string) string {
	v := url.Values{}
	for _, n := range names {
		v.Set(n, params[n])
	}
	return v.Encode()
}

// Fetch runs the source. If it fails, the last good snapshot is returned marked
// stale; with none, the error.
func (r *Runner) Fetch(ctx context.Context, m *module.Module, src *module.Source, params map[string]string) (*Snapshot, error) {
	key := ParamsKey(src.Params, params)
	args := module.ExpandArgs(src.Run, module.Vars{Params: params})
	ctx, cancel := context.WithTimeout(ctx, src.TimeoutDuration())
	defer cancel()
	out, stderr, err := run(ctx, m.Dir, args, maxSourceOutput)
	var data Data
	if err == nil {
		data, err = parse(out)
	}
	if err == nil {
		now := time.Now().UTC()
		if b, mErr := json.Marshal(data); mErr == nil {
			if sErr := r.store.SaveSnapshot(m.ID, src.ID, key, b, now); sErr != nil {
				slog.Error("save snapshot", "module", m.ID, "source", src.ID, "err", sErr)
			}
		}
		return &Snapshot{Data: data, FetchedAt: now}, nil
	}
	err = describe(err, stderr)
	slog.Warn("source failed", "module", m.ID, "source", src.ID, "params", key, "err", err)
	snap, cErr := r.Cached(m, src, params)
	if cErr != nil {
		return nil, err
	}
	snap.Stale, snap.Error = true, err.Error()
	return snap, nil
}

// Cached returns the last good snapshot without running anything.
func (r *Runner) Cached(m *module.Module, src *module.Source, params map[string]string) (*Snapshot, error) {
	b, at, err := r.store.GetSnapshot(m.ID, src.ID, ParamsKey(src.Params, params))
	if err != nil {
		return nil, err
	}
	var data Data
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return &Snapshot{Data: data, FetchedAt: at}, nil
}

func parse(out []byte) (Data, error) {
	var d Data
	if err := json.Unmarshal(out, &d); err != nil {
		return d, fmt.Errorf("source output is not JSON: %w", err)
	}
	if d.Version != 1 {
		return d, fmt.Errorf(`source output needs "jendapp": 1, got %d`, d.Version)
	}
	for i, row := range d.Rows {
		if id, _ := row["id"].(string); id == "" {
			return d, fmt.Errorf("source row %d has no string id", i)
		}
	}
	if d.Rows == nil {
		d.Rows = []map[string]any{}
	}
	return d, nil
}

// Start queues a command operation as a job; done closes when it has finished.
// Operations of one module run one at a time, since they write the same files.
func (r *Runner) Start(m *module.Module, op *module.Operation, args []string) (*store.Job, <-chan struct{}, error) {
	j := &store.Job{
		ID:        uuid.NewString(),
		ModuleID:  m.ID,
		Kind:      "op",
		Status:    "queued",
		Prompt:    op.Title + "\n" + strings.Join(args, " "),
		CreatedAt: time.Now().UTC(),
	}
	if err := r.store.InsertJob(j); err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(done)
		lock := r.lock(m.ID)
		lock.Lock()
		defer lock.Unlock()
		j.Status = "running"
		_ = r.store.UpdateJob(j)

		ctx, cancel := context.WithTimeout(r.ctx, op.TimeoutDuration())
		defer cancel()
		start := time.Now()
		out, stderr, err := run(ctx, m.Dir, args, maxSourceOutput)
		now := time.Now().UTC()
		j.FinishedAt = &now
		if result := clip(strings.TrimSpace(string(out))); result != "" {
			j.Result = &result
		}
		if err != nil {
			j.Status = "failed"
			msg := describe(err, stderr).Error()
			j.Error = &msg
		} else {
			j.Status = "done"
		}
		if err := r.store.UpdateJob(j); err != nil {
			slog.Error("update job", "job", j.ID, "err", err)
		}
		slog.Info("operation finished", "job", j.ID, "module", m.ID, "op", op.ID, "status", j.Status, "took", time.Since(start).Round(time.Millisecond))
	}()
	return j, done, nil
}

func (r *Runner) lock(moduleID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.locks[moduleID]
	if l == nil {
		l = &sync.Mutex{}
		r.locks[moduleID] = l
	}
	return l
}

// run executes args (no shell) in dir and returns stdout and the stderr tail.
func run(ctx context.Context, dir string, args []string, maxOut int) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd.Stdout = &limitWriter{w: &stdout, n: maxOut}
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil && stdout.Len() >= maxOut {
		err = errors.New("output too large")
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out or cancelled: %w", ctx.Err())
	}
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

// describe prefers the command's own message over "exit status 1".
func describe(err error, stderr string) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && stderr != "" {
		return fmt.Errorf("%s (exit %d)", clip(stderr), exit.ExitCode())
	}
	if stderr != "" {
		return fmt.Errorf("%v: %s", err, clip(stderr))
	}
	return err
}

func clip(s string) string {
	if len(s) > maxResult {
		return "…" + s[len(s)-maxResult:]
	}
	return s
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// tailBuffer keeps the last 16 KiB written, where the error usually is.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - 16<<10; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
