package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadManifest(t *testing.T, toml string) (*Module, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(dir)
}

const nextBase = `
id = "m"
[[operations]]
id = "sync"
title = "Sync"
run = ["./sync"]
next = ["sites"]
[[operations]]
id = "sites"
title = "Sites"
kind = "claude"
prompt = "do it"
`

func TestNextAndSchedules(t *testing.T) {
	m, err := loadManifest(t, nextBase+"[[schedules]]\nop = \"sync\"\nat = \"06:30\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Schedules) != 1 || m.Operation("sync").Next[0] != "sites" {
		t.Fatalf("m = %+v", m)
	}
	bad := map[string]string{
		"unknown next":       strings.Replace(nextBase, `next = ["sites"]`, `next = ["nope"]`, 1),
		"next on claude op":  nextBase + "[[operations]]\nid = \"c\"\ntitle = \"C\"\nkind = \"claude\"\nprompt = \"p\"\nnext = [\"sync\"]\n",
		"next needs inputs":  nextBase + "[[operations.inputs]]\nkey = \"k\"\ntitle = \"K\"\ntype = \"text\"\n",
		"schedule bad time":  nextBase + "[[schedules]]\nop = \"sync\"\nat = \"6pm\"\n",
		"schedule claude op": nextBase + "[[schedules]]\nop = \"sites\"\nat = \"06:30\"\n",
	}
	for name, toml := range bad {
		if _, err := loadManifest(t, toml); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
