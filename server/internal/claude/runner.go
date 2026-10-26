// Package claude runs the Claude Code CLI headless (`claude -p`) inside module directories.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"jendapp/internal/module"
	"jendapp/internal/store"
)

const (
	jobTimeout = 30 * time.Minute
	// How long a finished job's event log stays replayable in memory.
	liveRetention = 10 * time.Minute
)

type Runner struct {
	bin   string
	store *store.Store
	sem   chan struct{}
	ctx   context.Context // cancelled on shutdown; kills running processes

	mu   sync.Mutex
	live map[string]*liveJob
	wg   sync.WaitGroup
}

func NewRunner(ctx context.Context, bin string, maxJobs int, st *store.Store) *Runner {
	return &Runner{bin: bin, store: st, sem: make(chan struct{}, maxJobs), ctx: ctx, live: map[string]*liveJob{}}
}

// Wait blocks until all running jobs have exited.
func (r *Runner) Wait() { r.wg.Wait() }

// Start queues a Claude run. An empty sessionID starts a new session.
func (r *Runner) Start(m *module.Module, kind, prompt, sessionID string) (*store.Job, error) {
	resume := sessionID != ""
	if !resume {
		sessionID = uuid.NewString()
	}
	j := &store.Job{
		ID:        uuid.NewString(),
		ModuleID:  m.ID,
		Kind:      kind,
		SessionID: sessionID,
		Status:    "queued",
		Prompt:    prompt,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.store.InsertJob(j); err != nil {
		return nil, err
	}
	lj := newLiveJob()
	r.mu.Lock()
	r.live[j.ID] = lj
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.run(m, j, resume, lj)
		time.AfterFunc(liveRetention, func() {
			r.mu.Lock()
			delete(r.live, j.ID)
			r.mu.Unlock()
		})
	}()
	return j, nil
}

func (r *Runner) run(m *module.Module, j *store.Job, resume bool, lj *liveJob) {
	defer lj.finish()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-r.ctx.Done():
		r.fail(j, "server shutting down")
		return
	}
	j.Status = "running"
	_ = r.store.UpdateJob(j)

	ctx, cancel := context.WithTimeout(r.ctx, jobTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin, buildArgs(m, j.SessionID, resume)...)
	cmd.Dir = m.Dir
	cmd.Stdin = strings.NewReader(j.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.fail(j, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		r.fail(j, err.Error())
		return
	}
	slog.Info("claude started", "job", j.ID, "module", m.ID, "kind", j.Kind, "session", j.SessionID, "resume", resume)

	var res resultEvent
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lj.append(line)
			parseEvent(line, j, &res)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn("claude stdout", "job", j.ID, "err", err)
			}
			break
		}
	}
	waitErr := cmd.Wait()

	now := time.Now().UTC()
	j.FinishedAt = &now
	if res.seen {
		j.Result = &res.Result
		j.CostUSD = &res.TotalCostUSD
		if len(res.PermissionDenials) > 0 && string(res.PermissionDenials) != "[]" {
			d := string(res.PermissionDenials)
			j.Denials = &d
		}
	}
	switch {
	case waitErr == nil && res.seen && !res.IsError:
		j.Status = "done"
	default:
		j.Status = "failed"
		msg := strings.TrimSpace(stderr.String())
		if res.IsError && res.Result != "" {
			msg = res.Result
		}
		if waitErr != nil {
			msg = strings.TrimSpace(fmt.Sprintf("%v\n%s", waitErr, msg))
		}
		j.Error = &msg
	}
	if err := r.store.UpdateJob(j); err != nil {
		slog.Error("update job", "job", j.ID, "err", err)
	}
	slog.Info("claude finished", "job", j.ID, "status", j.Status, "cost_usd", res.TotalCostUSD)
}

func (r *Runner) fail(j *store.Job, msg string) {
	now := time.Now().UTC()
	j.Status, j.Error, j.FinishedAt = "failed", &msg, &now
	_ = r.store.UpdateJob(j)
}

func buildArgs(m *module.Module, sessionID string, resume bool) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		// Headless: nothing can answer a permission prompt, so unlisted tools are denied.
		"--permission-mode", "dontAsk",
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if m.Claude.Model != "" {
		args = append(args, "--model", m.Claude.Model)
	}
	args = append(args, "--append-system-prompt", headlessNote(m))
	if m.Claude.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(m.Claude.MaxBudgetUSD, 'f', 2, 64))
	}
	// Variadic flags: each is followed by another flag; the prompt goes in via stdin.
	if len(m.Claude.AddDirs) > 0 {
		args = append(args, "--add-dir")
		args = append(args, m.Claude.AddDirs...)
	}
	if len(m.Claude.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, m.Claude.AllowedTools...)
	}
	return args
}

// headlessNote tells Claude it can't ask for permissions, so it works within
// allowed_tools instead of giving up after the first denied call.
func headlessNote(m *module.Module) string {
	var b strings.Builder
	b.WriteString("You are running headless via jendapp, the user's phone/desktop app: nobody can approve permission prompts, and tool calls outside the allow-list are denied.")
	if len(m.Claude.AllowedTools) > 0 {
		b.WriteString(" Allowed tools: " + strings.Join(m.Claude.AllowedTools, ", ") + ".")
	}
	b.WriteString(" Run each Bash command on its own so it matches an allowed pattern exactly: no cd, &&, ;, pipes or subshells (you already run in the module directory). " +
		"Read PDFs and images with the Read tool. If a call is denied, try an allowed alternative and carry on with the rest of the task; say at the end what you could not do.")
	return b.String()
}

type resultEvent struct {
	seen              bool
	Result            string          `json:"result"`
	IsError           bool            `json:"is_error"`
	TotalCostUSD      float64         `json:"total_cost_usd"`
	PermissionDenials json.RawMessage `json:"permission_denials"`
}

func parseEvent(line []byte, j *store.Job, res *resultEvent) {
	var head struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	switch {
	case head.Type == "system" && head.Subtype == "init" && head.SessionID != "":
		j.SessionID = head.SessionID
	case head.Type == "result":
		if json.Unmarshal(line, res) == nil {
			res.seen = true
		}
	}
}

// Stream calls fn for every event line of a job, replaying from the start and
// following until the job ends or ctx is done. ok is false when the job is no
// longer held in memory.
func (r *Runner) Stream(ctx context.Context, jobID string, fn func(line []byte) error) (ok bool, err error) {
	r.mu.Lock()
	lj := r.live[jobID]
	r.mu.Unlock()
	if lj == nil {
		return false, nil
	}
	for i := 0; ; {
		lines, done, wait := lj.since(i)
		for _, l := range lines {
			if err := fn(l); err != nil {
				return true, err
			}
		}
		i += len(lines)
		if done && len(lines) == 0 {
			return true, nil
		}
		if len(lines) > 0 {
			continue
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}

type liveJob struct {
	mu     sync.Mutex
	lines  [][]byte
	done   bool
	notify chan struct{}
}

func newLiveJob() *liveJob { return &liveJob{notify: make(chan struct{})} }

func (l *liveJob) append(line []byte) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
}

func (l *liveJob) finish() {
	l.mu.Lock()
	l.done = true
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
}

func (l *liveJob) since(i int) ([][]byte, bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lines[i:], l.done, l.notify
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.n > 0 {
		k := min(len(p), lw.n)
		lw.w.Write(p[:k])
		lw.n -= k
	}
	return len(p), nil
}
