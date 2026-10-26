// Package inbox stores shared files, routes them into modules and triggers
// each module's on_import Claude run.
package inbox

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"jendapp/internal/claude"
	"jendapp/internal/module"
	"jendapp/internal/store"
)

// Manually assigned files without a matching rule land here inside the module.
const manualDest = "inbox"

type Inbox struct {
	dir     string
	store   *store.Store
	modules *module.Registry
	runner  *claude.Runner

	mu      sync.Mutex
	pending map[string]*batch // module id → files awaiting on_import
}

type batch struct {
	files []string
	timer *time.Timer
}

func New(dataDir string, st *store.Store, reg *module.Registry, r *claude.Runner) (*Inbox, error) {
	dir := filepath.Join(dataDir, "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Inbox{dir: dir, store: st, modules: reg, runner: r, pending: map[string]*batch{}}, nil
}

var ErrBadName = errors.New("invalid filename")

// Add saves an upload and routes it. Unmatched files stay pending for manual assignment.
func (in *Inbox) Add(filename string, body io.Reader) (*store.InboxItem, error) {
	filename = filepath.Base(filepath.Clean("/" + filename))
	if filename == "/" || filename == "." || strings.HasPrefix(filename, ".") {
		return nil, ErrBadName
	}
	id := uuid.NewString()
	itemDir := filepath.Join(in.dir, id)
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(itemDir, filename)
	size, err := writeFile(path, body, false)
	if err != nil {
		os.RemoveAll(itemDir)
		return nil, err
	}
	it := &store.InboxItem{ID: id, Filename: filename, Path: path, Size: size, Status: "pending", CreatedAt: time.Now().UTC()}
	if err := in.store.InsertInbox(it); err != nil {
		return nil, err
	}
	if m, rule, ok := in.modules.Route(filename); ok {
		in.deliver(it, m, rule)
	} else {
		slog.Info("inbox: no route", "file", filename)
	}
	return it, nil
}

// Assign delivers a pending item to a module chosen by the user.
func (in *Inbox) Assign(id, moduleID string) (*store.InboxItem, error) {
	it, err := in.store.GetInbox(id)
	if err != nil {
		return nil, err
	}
	m, ok := in.modules.Get(moduleID)
	if !ok {
		return nil, fmt.Errorf("unknown module %q", moduleID)
	}
	rule, ok := m.MatchRule(it.Filename)
	if !ok {
		rule = module.Rule{Dest: manualDest}
	}
	in.deliver(it, m, rule)
	return it, nil
}

func (in *Inbox) deliver(it *store.InboxItem, m *module.Module, rule module.Rule) {
	it.ModuleID = &m.ID
	it.Error = nil
	dest := filepath.Join(m.Dir, rule.Dest, it.Filename)
	err := in.copyInto(it.Path, dest, rule.Overwrite)
	switch {
	case errors.Is(err, os.ErrExist):
		it.Status = "skipped"
		msg := "already exists in module"
		it.Error = &msg
	case err != nil:
		it.Status = "failed"
		msg := err.Error()
		it.Error = &msg
	default:
		it.Status = "routed"
		it.DestPath = &dest
	}
	if err := in.store.UpdateInbox(it); err != nil {
		slog.Error("inbox: update", "id", it.ID, "err", err)
	}
	slog.Info("inbox: delivered", "file", it.Filename, "module", m.ID, "status", it.Status)
	if it.Status == "routed" && m.OnImport != nil && m.OnImport.Prompt != "" {
		in.schedule(m, dest)
	}
}

func (in *Inbox) copyInto(src, dest string, overwrite bool) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = writeFile(dest, f, overwrite)
	return err
}

// writeFile writes via a temp file + rename so a module never sees a partial file.
func writeFile(dest string, r io.Reader, overwrite bool) (int64, error) {
	if !overwrite {
		if _, err := os.Stat(dest); err == nil {
			return 0, os.ErrExist
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".jendapp-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	return n, nil
}

// schedule batches files per module so sharing a run + its Fitness export
// together triggers a single Claude run.
func (in *Inbox) schedule(m *module.Module, path string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	b := in.pending[m.ID]
	if b == nil {
		b = &batch{}
		in.pending[m.ID] = b
	}
	b.files = append(b.files, path)
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(m.OnImport.DebounceDuration(), func() { in.fire(m) })
}

func (in *Inbox) fire(m *module.Module) {
	in.mu.Lock()
	b := in.pending[m.ID]
	delete(in.pending, m.ID)
	in.mu.Unlock()
	if b == nil || len(b.files) == 0 {
		return
	}
	rel := make([]string, len(b.files))
	for i, f := range b.files {
		if r, err := filepath.Rel(m.Dir, f); err == nil {
			f = r
		}
		rel[i] = f
	}
	prompt := strings.ReplaceAll(m.OnImport.Prompt, "{files}", strings.Join(rel, "\n"))
	if _, err := in.runner.Start(m, "import", prompt, ""); err != nil {
		slog.Error("inbox: start on_import", "module", m.ID, "err", err)
	}
}
