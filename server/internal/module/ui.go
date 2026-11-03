package module

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// Source produces a module's data: a command printing jendapp JSON
// ({"jendapp":1,"rows":[{"id":…},…],"summary":{…},"notes":[…]}). It runs in the
// module dir without a shell; jendapp never reads module files itself.
type Source struct {
	ID      string   `toml:"id" json:"id"`
	Run     []string `toml:"run" json:"-"`
	Timeout string   `toml:"timeout" json:"-"` // default 60s
	Params  []string `toml:"-" json:"params"`  // derived from {param} placeholders in run
}

// View is a screen built from one source with the app's fixed building blocks.
type View struct {
	ID     string `toml:"id" json:"id"`
	Title  string `toml:"title" json:"title"`
	Icon   string `toml:"icon" json:"icon,omitempty"`
	Type   string `toml:"type" json:"type"` // table
	Source string `toml:"source" json:"-"`
	// Rows are grouped into sections by this column (e.g. a date).
	GroupBy    string   `toml:"group_by" json:"group_by,omitempty"`
	Columns    []Column `toml:"columns" json:"columns"`
	Stats      []Stat   `toml:"stats" json:"stats,omitempty"`
	Filter     *Filter  `toml:"filter" json:"filter,omitempty"`
	RowActions []string `toml:"row_actions" json:"row_actions,omitempty"`
	Actions    []string `toml:"actions" json:"actions,omitempty"`
	Params     []string `toml:"-" json:"params"` // the source's params; the app asks for them
}

type Column struct {
	Key   string `toml:"key" json:"key"`
	Title string `toml:"title" json:"title,omitempty"`
	Type  string `toml:"type" json:"type"` // text | number | hours | date | enum
	// Where the column shows in a row: title | subtitle | caption | warning | trailing | badge.
	// Columns without a role appear only in the row's detail.
	Role string `toml:"role" json:"role,omitempty"`
	// enum: the known values in display order (also the filter menu).
	Options []Option `toml:"options" json:"options,omitempty"`
	// Column shown (dimmed) when this one is empty, e.g. evidence for a missing description.
	Fallback string `toml:"fallback" json:"fallback,omitempty"`
}

type Option struct {
	Value string `toml:"value" json:"value"`
	Label string `toml:"label" json:"label,omitempty"`
	Color string `toml:"color" json:"color,omitempty"` // gray | green | orange | red | blue | purple | yellow
}

// Stat is one summary tile above the rows, read from the source's "summary".
type Stat struct {
	Key   string `toml:"key" json:"key"`
	Title string `toml:"title" json:"title"`
	Type  string `toml:"type" json:"type"` // number | hours | text
}

// Filter lets the user pick which values of an enum column are shown.
type Filter struct {
	Key     string   `toml:"key" json:"key"`
	Default []string `toml:"default" json:"default"`
}

// Operation is something the user can do: run a module command (deterministic,
// preferred) or start Claude with a prompt (for work that needs judgement).
type Operation struct {
	ID      string   `toml:"id" json:"id"`
	Title   string   `toml:"title" json:"title"`
	Icon    string   `toml:"icon" json:"icon,omitempty"`
	Kind    string   `toml:"kind" json:"kind"`   // command | claude
	Scope   string   `toml:"scope" json:"scope"` // module | row
	Run     []string `toml:"run" json:"-"`
	Prompt  string   `toml:"prompt" json:"-"`
	Inputs  []Input  `toml:"inputs" json:"inputs,omitempty"`
	Confirm string   `toml:"confirm" json:"confirm,omitempty"` // question shown before running
	// Row ops only: shown when every listed row field has one of the values.
	When        map[string][]string `toml:"when" json:"-"`
	Conditions  []Condition         `toml:"-" json:"when,omitempty"` // When, as a list for the app
	Destructive bool                `toml:"destructive" json:"destructive,omitempty"`
	Timeout     string              `toml:"timeout" json:"-"` // command ops; default 10m
	Params      []string            `toml:"-" json:"params"`
}

type Condition struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

type Input struct {
	Key   string `toml:"key" json:"key"`
	Title string `toml:"title" json:"title"`
	Type  string `toml:"type" json:"type"` // text | multiline
	// Pre-filled value; may use {row.x}, which the app fills from the row.
	Default string `toml:"default" json:"default,omitempty"`
}

var (
	columnTypes = []string{"text", "number", "hours", "date", "enum"}
	columnRoles = []string{"", "title", "subtitle", "caption", "warning", "trailing", "badge"}
	statTypes   = []string{"number", "hours", "text"}
	colors      = []string{"", "gray", "green", "orange", "red", "blue", "purple", "yellow"}
	inputTypes  = []string{"text", "multiline"}
	// Values the app can supply for {param}; each is checked against its pattern.
	paramPatterns = map[string]*regexp.Regexp{
		"month": regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`),
	}
	placeholder = regexp.MustCompile(`\{([a-z_]+)(?:\.([A-Za-z0-9_]+))?\}`)
)

func (m *Module) Source(id string) *Source {
	for i := range m.Sources {
		if m.Sources[i].ID == id {
			return &m.Sources[i]
		}
	}
	return nil
}

func (m *Module) View(id string) *View {
	for i := range m.Views {
		if m.Views[i].ID == id {
			return &m.Views[i]
		}
	}
	return nil
}

func (m *Module) Operation(id string) *Operation {
	for i := range m.Operations {
		if m.Operations[i].ID == id {
			return &m.Operations[i]
		}
	}
	return nil
}

func (s *Source) TimeoutDuration() time.Duration { return parseTimeout(s.Timeout, time.Minute) }
func (o *Operation) TimeoutDuration() time.Duration {
	return parseTimeout(o.Timeout, 10*time.Minute)
}

func parseTimeout(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// validateUI checks sources, views and operations and fills in derived fields.
func (m *Module) validateUI() error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("module %s: %s", m.ID, fmt.Sprintf(format, a...))
	}
	seen := map[string]bool{}
	for i := range m.Sources {
		s := &m.Sources[i]
		if s.ID == "" || len(s.Run) == 0 {
			return fail("sources need an id and run")
		}
		if seen["source:"+s.ID] {
			return fail("duplicate source %q", s.ID)
		}
		seen["source:"+s.ID] = true
		params, err := templateParams(s.Run, "", nil)
		if err != nil {
			return fail("source %q: %v", s.ID, err)
		}
		s.Params = params
	}
	for i := range m.Operations {
		o := &m.Operations[i]
		if o.ID == "" || o.Title == "" {
			return fail("operations need an id and a title")
		}
		if seen["op:"+o.ID] {
			return fail("duplicate operation %q", o.ID)
		}
		seen["op:"+o.ID] = true
		if o.Kind == "" {
			o.Kind = "command"
		}
		if o.Scope == "" {
			o.Scope = "module"
		}
		if o.Scope != "module" && o.Scope != "row" {
			return fail("operation %q: scope must be module or row", o.ID)
		}
		inputs := map[string]bool{}
		for _, in := range o.Inputs {
			if in.Key == "" || !slices.Contains(inputTypes, in.Type) || inputs[in.Key] {
				return fail("operation %q: inputs need a unique key and type text or multiline", o.ID)
			}
			inputs[in.Key] = true
		}
		var parts []string
		switch o.Kind {
		case "command":
			if len(o.Run) == 0 || o.Prompt != "" {
				return fail("operation %q: command operations need run (and no prompt)", o.ID)
			}
			parts = o.Run
		case "claude":
			if o.Prompt == "" || len(o.Run) > 0 {
				return fail("operation %q: claude operations need a prompt (and no run)", o.ID)
			}
			parts = []string{o.Prompt}
		default:
			return fail("operation %q: kind must be command or claude", o.ID)
		}
		for _, in := range o.Inputs {
			parts = append(parts, in.Default)
		}
		// Prompts may contain other {words} meant for Claude; commands may not.
		params, err := templateParams(parts, o.Scope, inputs)
		if err != nil && o.Kind == "command" {
			return fail("operation %q: %v", o.ID, err)
		}
		o.Params = params
		if len(o.When) > 0 && o.Scope != "row" {
			return fail("operation %q: when is only for row operations", o.ID)
		}
		o.Conditions = nil
		for _, f := range slices.Sorted(maps.Keys(o.When)) {
			o.Conditions = append(o.Conditions, Condition{Field: f, Values: o.When[f]})
		}
	}
	for i := range m.Views {
		v := &m.Views[i]
		if v.ID == "" || v.Title == "" {
			return fail("views need an id and a title")
		}
		if seen["view:"+v.ID] {
			return fail("duplicate view %q", v.ID)
		}
		seen["view:"+v.ID] = true
		if v.Type != "table" {
			return fail("view %q: type must be table", v.ID)
		}
		src := m.Source(v.Source)
		if src == nil {
			return fail("view %q: unknown source %q", v.ID, v.Source)
		}
		v.Params = src.Params
		keys := map[string]bool{}
		for _, c := range v.Columns {
			if c.Key == "" || !slices.Contains(columnTypes, c.Type) || !slices.Contains(columnRoles, c.Role) {
				return fail("view %q: column %q needs a key, a type (%v) and an optional role (%v)", v.ID, c.Key, columnTypes, columnRoles[1:])
			}
			for _, opt := range c.Options {
				if opt.Value == "" || !slices.Contains(colors, opt.Color) {
					return fail("view %q: column %q: options need a value and a color from %v", v.ID, c.Key, colors[1:])
				}
			}
			keys[c.Key] = true
		}
		if len(v.Columns) == 0 {
			return fail("view %q: needs columns", v.ID)
		}
		for _, s := range v.Stats {
			if s.Key == "" || !slices.Contains(statTypes, s.Type) {
				return fail("view %q: stats need a key and a type (%v)", v.ID, statTypes)
			}
		}
		if v.GroupBy != "" && !keys[v.GroupBy] {
			return fail("view %q: group_by %q is not a column", v.ID, v.GroupBy)
		}
		if v.Filter != nil && !keys[v.Filter.Key] {
			return fail("view %q: filter key %q is not a column", v.ID, v.Filter.Key)
		}
		check := func(ids []string, scope string) error {
			for _, id := range ids {
				o := m.Operation(id)
				if o == nil {
					return fail("view %q: unknown operation %q", v.ID, id)
				}
				if o.Scope != scope {
					return fail("view %q: operation %q has scope %s, expected %s", v.ID, id, o.Scope, scope)
				}
				for _, p := range o.Params {
					if !slices.Contains(v.Params, p) {
						return fail("view %q: operation %q needs {%s}, which the view's source doesn't take", v.ID, id, p)
					}
				}
			}
			return nil
		}
		if err := check(v.RowActions, "row"); err != nil {
			return err
		}
		if err := check(v.Actions, "module"); err != nil {
			return err
		}
	}
	return nil
}

// templateParams lists the {param} names used in parts. It reports an unknown
// placeholder, {row.x} outside row scope, or an undeclared {input.x}.
func templateParams(parts []string, scope string, inputs map[string]bool) ([]string, error) {
	params := []string{}
	var firstErr error
	for _, s := range parts {
		for _, mt := range placeholder.FindAllStringSubmatch(s, -1) {
			name, field := mt[1], mt[2]
			var err error
			switch {
			case name == "row" && field != "":
				if scope != "row" {
					err = fmt.Errorf("%s is only available to row operations", mt[0])
				}
			case name == "input" && field != "":
				if !inputs[field] {
					err = fmt.Errorf("%s is not a declared input", mt[0])
				}
			case field == "" && paramPatterns[name] != nil:
				if !slices.Contains(params, name) {
					params = append(params, name)
				}
			default:
				err = fmt.Errorf("unknown placeholder %s", mt[0])
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return params, firstErr
}

// Vars are the values substituted into run arguments and prompts.
type Vars struct {
	Params map[string]string
	Row    map[string]any
	Inputs map[string]string
}

// ValidateParams checks each needed param is present and well-formed.
func ValidateParams(needed []string, params map[string]string) error {
	for _, p := range needed {
		v, ok := params[p]
		if !ok || v == "" {
			return fmt.Errorf("missing %s", p)
		}
		if !paramPatterns[p].MatchString(v) {
			return fmt.Errorf("invalid %s %q", p, v)
		}
	}
	return nil
}

// Expand substitutes {param}, {row.x} and {input.x}. Placeholders it has no value
// for are left as they are (prompts may contain other {words}); load-time
// validation already rejected unknown ones in commands.
func Expand(s string, v Vars) string {
	return placeholder.ReplaceAllStringFunc(s, func(match string) string {
		mt := placeholder.FindStringSubmatch(match)
		name, field := mt[1], mt[2]
		switch {
		case name == "row" && field != "" && v.Row != nil:
			return Stringify(v.Row[field])
		case name == "input" && field != "" && v.Inputs != nil:
			if val, ok := v.Inputs[field]; ok {
				return val
			}
		case field == "":
			if val, ok := v.Params[name]; ok {
				return val
			}
		}
		return match
	})
}

// ExpandArgs expands every argument separately, so a value never splits into
// several arguments.
func ExpandArgs(run []string, v Vars) []string {
	out := make([]string, len(run))
	for i, a := range run {
		out[i] = Expand(a, v)
	}
	return out
}

// Stringify renders a JSON value as a command argument.
func Stringify(x any) string {
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
