package ops

import (
	"log/slog"
	"time"

	"jendapp/internal/module"
)

// Due reports whether a daily schedule at "HH:MM" should run now, given the date (YYYY-MM-DD)
// it last ran. It runs once the time has passed on a day it hasn't run yet, so a server that
// was down at that minute catches up when it starts, and never runs twice a day.
func Due(at string, now time.Time, lastRun string) bool {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return false
	}
	today := now.Format(time.DateOnly)
	if lastRun == today {
		return false
	}
	return now.Hour()*60+now.Minute() >= t.Hour()*60+t.Minute()
}

// Schedule starts the modules' scheduled operations until the runner's context ends.
// lastRun returns when an operation last ran (from the jobs table), so a restart doesn't
// run it again the same day.
func (r *Runner) Schedule(modules func() []*module.Module, lastRun func(moduleID, title string) string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		started := map[string]string{} // module/op → date started in this process
		for {
			now := time.Now()
			for _, m := range modules() {
				for _, sc := range m.Schedules {
					op := m.Operation(sc.Op)
					if op == nil {
						continue
					}
					key := m.ID + "/" + sc.Op
					last := started[key]
					if last == "" {
						last = lastRun(m.ID, op.Title)
					}
					if !Due(sc.At, now, last) {
						continue
					}
					started[key] = now.Format(time.DateOnly)
					if _, _, err := r.Start(m, op, op.Run); err != nil {
						slog.Error("scheduled operation", "module", m.ID, "op", op.ID, "err", err)
					} else {
						slog.Info("scheduled operation started", "module", m.ID, "op", op.ID)
					}
				}
			}
			select {
			case <-r.ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
