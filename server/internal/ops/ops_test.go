package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jendapp/internal/module"
	"jendapp/internal/store"
)

func setup(t *testing.T, script string) (*Runner, *module.Module, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &module.Module{ID: "x", Dir: dir}
	return NewRunner(context.Background(), st), m, st
}

const tool = `#!/bin/sh
case "$1" in
  list)
    if [ -f broken ]; then echo "draft missing" >&2; exit 1; fi
    echo '{"jendapp":1,"rows":[{"id":"r1","n":1}],"summary":{"todo":1},"notes":["'"$2"'"]}' ;;
  edit) echo "edited $2 $3"; [ "$3" = "--text=fail" ] && { echo "nope" >&2; exit 3; }; exit 0 ;;
  noid) echo '{"jendapp":1,"rows":[{"n":1}]}' ;;
esac
`

func TestFetchCachesAndFallsBack(t *testing.T) {
	r, m, _ := setup(t, tool)
	src := &module.Source{ID: "s", Run: []string{"./tool.sh", "list", "{month}"}, Params: []string{"month"}}
	params := map[string]string{"month": "2026-10"}
	snap, err := r.Fetch(context.Background(), m, src, params)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Stale || len(snap.Rows) != 1 || snap.Row("r1") == nil || snap.Notes[0] != "2026-10" {
		t.Fatalf("snap = %+v", snap)
	}
	os.WriteFile(filepath.Join(m.Dir, "broken"), nil, 0o644)
	snap, err = r.Fetch(context.Background(), m, src, params)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Stale || !strings.Contains(snap.Error, "draft missing") || snap.Row("r1") == nil {
		t.Fatalf("stale snap = %+v", snap)
	}
	// Other params were never fetched, so there's nothing to fall back to.
	if _, err := r.Fetch(context.Background(), m, src, map[string]string{"month": "2026-09"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchRejectsRowsWithoutID(t *testing.T) {
	r, m, _ := setup(t, tool)
	if _, err := r.Fetch(context.Background(), m, &module.Source{ID: "s", Run: []string{"./tool.sh", "noid"}}, nil); err == nil {
		t.Fatal("rows without id accepted")
	}
}

func TestStartRecordsJob(t *testing.T) {
	r, m, st := setup(t, tool)
	op := &module.Operation{ID: "e", Title: "Edit"}
	j, done, err := r.Start(m, op, []string{"./tool.sh", "edit", "r1", "--text=hi there"})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	got, _ := st.GetJob(j.ID)
	if got.Status != "done" || got.Kind != "op" || *got.Result != "edited r1 --text=hi there" {
		t.Fatalf("job = %+v result=%v", got, got.Result)
	}
	j, done, _ = r.Start(m, op, []string{"./tool.sh", "edit", "r1", "--text=fail"})
	<-done
	got, _ = st.GetJob(j.ID)
	if got.Status != "failed" || got.Error == nil || !strings.Contains(*got.Error, "nope (exit 3)") || got.Result == nil {
		t.Fatalf("failed job = %+v", got)
	}
}
