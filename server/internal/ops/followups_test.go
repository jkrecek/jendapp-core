package ops

import (
	"slices"
	"strings"
	"testing"
	"time"

	"jendapp/internal/module"
	"jendapp/internal/store"
)

func TestFollowUps(t *testing.T) {
	shown, next := FollowUps("synced\njendapp:next sites\njendapp:next evil\njendapp:next sites\ndone", []string{"sites"})
	if shown != "synced\ndone" || !slices.Equal(next, []string{"sites"}) {
		t.Fatalf("shown=%q next=%v", shown, next)
	}
}

func TestOnDoneGetsFollowUps(t *testing.T) {
	r, m, st := setup(t, "#!/bin/sh\necho changed\necho jendapp:next sites\n[ \"$1\" = fail ] && exit 1\nexit 0\n")
	got := make(chan []string, 2)
	r.OnDone = func(_ *module.Module, _ *module.Operation, _ *store.Job, next []string) { got <- next }
	op := &module.Operation{ID: "sync", Title: "Sync", Run: []string{"./tool.sh"}, Next: []string{"sites"}}

	j, done, err := r.Start(m, op, op.Run)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if next := <-got; !slices.Equal(next, []string{"sites"}) {
		t.Fatalf("next = %v", next)
	}
	j, _ = st.GetJob(j.ID)
	if j.Result == nil || strings.Contains(*j.Result, "jendapp:") || !strings.Contains(*j.Result, "changed") {
		t.Fatalf("result = %v", j.Result)
	}
	if st.LastOpRun(m.ID, "Sync") != time.Now().Format(time.DateOnly) {
		t.Fatal("LastOpRun should report today")
	}

	// A failed run hands over nothing.
	_, done, _ = r.Start(m, op, []string{"./tool.sh", "fail"})
	<-done
	if next := <-got; next != nil {
		t.Fatalf("failed run next = %v", next)
	}
}

func TestDue(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.Local) }
	cases := []struct {
		now  time.Time
		last string
		want bool
	}{
		{at(6, 29), "", false},
		{at(6, 30), "", true},
		{at(9, 0), "2026-11-02", true}, // server was down at 6:30: catch up
		{at(9, 0), "2026-11-03", false},
	}
	for _, c := range cases {
		if got := Due("06:30", c.now, c.last); got != c.want {
			t.Errorf("Due(06:30, %v, %q) = %v", c.now.Format("15:04"), c.last, got)
		}
	}
	if Due("6pm", at(20, 0), "") {
		t.Error("bad time should never be due")
	}
}
