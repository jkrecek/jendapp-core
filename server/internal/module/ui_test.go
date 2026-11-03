package module

import (
	"slices"
	"strings"
	"testing"
)

const uiManifest = `
id = "x"
[[sources]]
id = "entries"
run = ["tool", "list", "{month}"]
[[views]]
id = "review"
title = "Review"
type = "table"
source = "entries"
group_by = "date"
actions = ["push"]
row_actions = ["describe"]
[views.filter]
key = "status"
default = ["todo"]
[[views.columns]]
key = "date"
type = "date"
[[views.columns]]
key = "status"
type = "enum"
role = "badge"
[[operations]]
id = "push"
title = "Push"
run = ["tool", "push", "{month}"]
[[operations]]
id = "describe"
title = "Describe"
scope = "row"
run = ["tool", "edit", "{month}", "{row.id}", "--text={input.text}"]
inputs = [{ key = "text", title = "Text", type = "text", default = "{row.text}" }]
when = { status = ["todo"] }
[[operations]]
id = "ask"
title = "Ask"
kind = "claude"
prompt = "Look at {month} and file into ../{rok}-Q{n}/"
`

func TestUIManifest(t *testing.T) {
	m, err := Load(writeManifest(t, uiManifest))
	if err != nil {
		t.Fatal(err)
	}
	v := m.View("review")
	if !slices.Equal(v.Params, []string{"month"}) {
		t.Fatalf("view params = %v", v.Params)
	}
	if o := m.Operation("describe"); o.Kind != "command" || o.Scope != "row" || !slices.Equal(o.Params, []string{"month"}) {
		t.Fatalf("describe = %+v", o)
	}
	// Claude prompts may contain {words} for Claude; only known placeholders count.
	if o := m.Operation("ask"); !slices.Equal(o.Params, []string{"month"}) {
		t.Fatalf("ask params = %v", o.Params)
	}
}

func TestUIManifestRejects(t *testing.T) {
	cases := map[string][2]string{
		"unknown placeholder in command": {`run = ["tool", "push", "{month}"]`, `run = ["tool", "push", "{year}"]`},
		"row placeholder in module op":   {`run = ["tool", "push", "{month}"]`, `run = ["tool", "push", "{row.id}"]`},
		"undeclared input":               {`--text={input.text}`, `--text={input.other}`},
		"unknown source":                 {`source = "entries"`, `source = "nope"`},
		"unknown view type":              {`type = "table"`, `type = "gallery"`},
		"row op as module action":        {`actions = ["push"]`, `actions = ["describe"]`},
		"unknown operation":              {`row_actions = ["describe"]`, `row_actions = ["nope"]`},
		"group_by not a column":          {`group_by = "date"`, `group_by = "client"`},
		"bad column type":                {`type = "date"`, `type = "datetime"`},
		"claude op with run":             {`kind = "claude"`, "kind = \"claude\"\nrun = [\"x\"]"},
	}
	for name, c := range cases {
		body := strings.Replace(uiManifest, c[0], c[1], 1)
		if body == uiManifest {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Load(writeManifest(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOpParamsMustComeFromView(t *testing.T) {
	body := strings.Replace(uiManifest, `run = ["tool", "list", "{month}"]`, `run = ["tool", "list"]`, 1)
	if _, err := Load(writeManifest(t, body)); err == nil || !strings.Contains(err.Error(), "{month}") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpand(t *testing.T) {
	v := Vars{
		Params: map[string]string{"month": "2026-10"},
		Row:    map[string]any{"id": "a@b", "hours": 1.5, "ok": true},
		Inputs: map[string]string{"text": "two words --apply"},
	}
	got := ExpandArgs([]string{"edit", "{month}", "{row.id}", "--text={input.text}", "{row.hours}", "{row.ok}", "{rok}"}, v)
	want := []string{"edit", "2026-10", "a@b", "--text=two words --apply", "1.5", "true", "{rok}"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestValidateParams(t *testing.T) {
	for _, bad := range []string{"", "2026-13", "2026-1", "--apply", "2026-10 "} {
		if ValidateParams([]string{"month"}, map[string]string{"month": bad}) == nil {
			t.Errorf("month %q accepted", bad)
		}
	}
	if err := ValidateParams([]string{"month"}, map[string]string{"month": "2026-10"}); err != nil {
		t.Fatal(err)
	}
}
