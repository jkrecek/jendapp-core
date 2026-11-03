// Package module loads jendapp.toml manifests from module directories.
package module

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const ManifestName = "jendapp.toml"

type Module struct {
	ID     string `toml:"id" json:"id"`
	Name   string `toml:"name" json:"name"`
	Icon   string `toml:"icon" json:"icon"` // SF Symbol name
	Dir    string `toml:"-" json:"-"`
	Claude Claude `toml:"claude" json:"-"`
	Import []Rule `toml:"import" json:"-"`
	// Run once after a batch of imports settles.
	OnImport *OnImport `toml:"on_import" json:"-"`
	// One-tap prompts shown in the app; each starts a new chat.
	Actions []Action `toml:"actions" json:"actions"`
	// Data commands, the screens built from them, and what the user can do there (see ui.go).
	Sources    []Source    `toml:"sources" json:"-"`
	Views      []View      `toml:"views" json:"views"`
	Operations []Operation `toml:"operations" json:"operations"`
}

type Action struct {
	Title  string `toml:"title" json:"title"`
	Icon   string `toml:"icon" json:"icon,omitempty"` // SF Symbol name
	Prompt string `toml:"prompt" json:"prompt"`
	// Input the app asks for first; "month" substitutes {month} (YYYY-MM) in both prompts.
	Input string `toml:"input" json:"input,omitempty"`
	// Follow-up offered as a button once a run of this action finishes (e.g. "Send").
	Confirm *Confirm `toml:"confirm" json:"confirm,omitempty"`
}

type Confirm struct {
	Title  string `toml:"title" json:"title"`
	Prompt string `toml:"prompt" json:"prompt"`
}

type Claude struct {
	Model        string   `toml:"model"`
	AllowedTools []string `toml:"allowed_tools"`
	MaxBudgetUSD float64  `toml:"max_budget_usd"`
	// Extra directories Claude may access (--add-dir), relative to the module dir.
	// Import rules may also deliver into them. Resolved to absolute paths by Load.
	AddDirs []string `toml:"add_dirs"`
	// Chrome lets Claude drive the user's Chrome on this Mac (claude --chrome), for
	// sites without an API. Its tools still need allowed_tools ("mcp__claude-in-chrome").
	Chrome bool `toml:"chrome"`
}

// Rule routes an imported file into the module when its name matches.
type Rule struct {
	Match     string `toml:"match"`     // filepath.Match glob on the base name
	Dest      string `toml:"dest"`      // directory relative to the module dir (may point into an add_dir)
	Overwrite bool   `toml:"overwrite"` // replace an existing file of the same name
}

type OnImport struct {
	// Prompt sent to Claude; {files} expands to the imported paths, one per line.
	Prompt   string `toml:"prompt"`
	Debounce string `toml:"debounce"` // e.g. "2m"; batches several shares into one run
}

func (o *OnImport) DebounceDuration() time.Duration {
	if o == nil || o.Debounce == "" {
		return 0
	}
	d, err := time.ParseDuration(o.Debounce)
	if err != nil {
		return 0
	}
	return d
}

func Load(dir string) (*Module, error) {
	m := &Module{}
	if _, err := toml.DecodeFile(filepath.Join(dir, ManifestName), m); err != nil {
		return nil, fmt.Errorf("module %s: %w", dir, err)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("module %s: id is required", dir)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	m.Dir = dir
	roots := []string{dir}
	for i, d := range m.Claude.AddDirs {
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		m.Claude.AddDirs[i] = filepath.Clean(d)
		roots = append(roots, m.Claude.AddDirs[i])
	}
	for _, r := range m.Import {
		if _, err := filepath.Match(r.Match, ""); err != nil {
			return nil, fmt.Errorf("module %s: bad match %q: %w", m.ID, r.Match, err)
		}
		if !within(filepath.Join(dir, r.Dest), roots) {
			return nil, fmt.Errorf("module %s: dest %q must stay inside the module dir or an add_dirs entry", m.ID, r.Dest)
		}
	}
	for _, a := range m.Actions {
		if a.Title == "" || a.Prompt == "" {
			return nil, fmt.Errorf("module %s: actions need a title and a prompt", m.ID)
		}
		if a.Input != "" && a.Input != "month" {
			return nil, fmt.Errorf("module %s: action %q: unknown input %q", m.ID, a.Title, a.Input)
		}
		if a.Confirm != nil && (a.Confirm.Title == "" || a.Confirm.Prompt == "") {
			return nil, fmt.Errorf("module %s: action %q: confirm needs a title and a prompt", m.ID, a.Title)
		}
	}
	if err := m.validateUI(); err != nil {
		return nil, err
	}
	return m, nil
}

// MatchRule returns the first rule matching filename.
func (m *Module) MatchRule(filename string) (Rule, bool) {
	for _, r := range m.Import {
		if ok, _ := filepath.Match(r.Match, filename); ok {
			return r, true
		}
	}
	return Rule{}, false
}

// within reports whether path (absolute, cleaned) is one of roots or below one.
func within(path string, roots []string) bool {
	for _, root := range roots {
		if rel, err := filepath.Rel(root, path); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
}

type Registry struct {
	byID    map[string]*Module
	order   []*Module // by name, for display
	routing []*Module // config order, for import routing
}

func LoadAll(dirs []string) (*Registry, error) {
	r := &Registry{byID: map[string]*Module{}}
	for _, d := range dirs {
		m, err := Load(d)
		if err != nil {
			return nil, err
		}
		if _, dup := r.byID[m.ID]; dup {
			return nil, fmt.Errorf("duplicate module id %q", m.ID)
		}
		r.byID[m.ID] = m
		r.order = append(r.order, m)
		r.routing = append(r.routing, m)
	}
	sort.SliceStable(r.order, func(i, j int) bool {
		return strings.ToLower(r.order[i].Name) < strings.ToLower(r.order[j].Name)
	})
	return r, nil
}

func (r *Registry) Get(id string) (*Module, bool) {
	m, ok := r.byID[id]
	return m, ok
}

func (r *Registry) All() []*Module { return r.order }

// Route finds the first module, in config order, that accepts filename.
func (r *Registry) Route(filename string) (*Module, Rule, bool) {
	for _, m := range r.routing {
		if rule, ok := m.MatchRule(filename); ok {
			return m, rule, true
		}
	}
	return nil, Rule{}, false
}

// Exists reports whether the module dir is still present (iCloud folders can vanish).
func (m *Module) Exists() bool {
	st, err := os.Stat(m.Dir)
	return err == nil && st.IsDir()
}
