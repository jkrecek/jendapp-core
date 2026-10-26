package module

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDestMayPointIntoAddDir(t *testing.T) {
	dir := writeManifest(t, `
id = "taxes"
[claude]
add_dirs = ["../faktury"]
[[import]]
match = "*.pdf"
dest = "../faktury/inbox"
[[actions]]
title = "Pending"
prompt = "what is pending?"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(dir), "faktury")
	if len(m.Claude.AddDirs) != 1 || m.Claude.AddDirs[0] != want {
		t.Fatalf("add_dirs = %v, want [%s]", m.Claude.AddDirs, want)
	}
	if len(m.Actions) != 1 || m.Actions[0].Title != "Pending" {
		t.Fatalf("actions = %+v", m.Actions)
	}
}

func TestDestOutsideRootsRejected(t *testing.T) {
	for _, dest := range []string{"..", "../other", "../faktury-evil"} {
		dir := writeManifest(t, `
id = "taxes"
[claude]
add_dirs = ["../faktury"]
[[import]]
match = "*.pdf"
dest = "`+dest+`"
`)
		if _, err := Load(dir); err == nil {
			t.Errorf("dest %q accepted", dest)
		}
	}
}

func TestActionNeedsPrompt(t *testing.T) {
	dir := writeManifest(t, `
id = "x"
[[actions]]
title = "Empty"
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("action without prompt accepted")
	}
}

func TestActionInputAndConfirm(t *testing.T) {
	dir := writeManifest(t, `
id = "x"
[[actions]]
title = "Fill"
prompt = "fill {month}"
input = "month"
[actions.confirm]
title = "Send"
prompt = "send {month}"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := m.Actions[0]; a.Input != "month" || a.Confirm == nil || a.Confirm.Title != "Send" {
		t.Fatalf("action = %+v", a)
	}
	bad := writeManifest(t, `
id = "x"
[[actions]]
title = "Fill"
prompt = "fill"
input = "date"
`)
	if _, err := Load(bad); err == nil {
		t.Fatal("unknown input accepted")
	}
}
