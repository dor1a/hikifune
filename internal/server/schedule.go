package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Clock is a time of day, HH:MM.
type Clock struct{ Hour, Minute int }

// ParseClock reads HH:MM.
func ParseClock(s string) (Clock, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return Clock{}, fmt.Errorf("time of day %q is not HH:MM", s)
	}
	return Clock{t.Hour(), t.Minute()}, nil
}

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// Next is the first time after now that the clock shows at, in loc. On a
// day when a daylight saving change skips that time, it is the moment the
// clock jumps past it.
func Next(now time.Time, at Clock, loc *time.Location) time.Time {
	n := now.In(loc)
	for day := 0; ; day++ {
		t := time.Date(n.Year(), n.Month(), n.Day()+day, at.Hour, at.Minute, 0, 0, loc)
		if l := t.In(loc); l.Hour() != at.Hour || l.Minute() != at.Minute {
			t = gapEnd(t, n.Year(), n.Month(), n.Day()+day, at, loc)
		}
		if t.After(n) {
			return t
		}
	}
}

// gapEnd is the first moment the clock in loc shows a later time than at on
// that day, or the next day: the end of the daylight saving gap that skips
// at. Go places a time in a gap by one of the two offsets, so the search
// starts an hour or two before.
func gapEnd(t time.Time, year int, month time.Month, day int, at Clock, loc *time.Location) time.Time {
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	want := date.Year()*10000 + int(date.Month())*100 + date.Day()
	for c := t.Add(-3 * time.Hour).Truncate(time.Minute); c.Before(t.Add(3 * time.Hour)); c = c.Add(time.Minute) {
		l := c.In(loc)
		d := l.Year()*10000 + int(l.Month())*100 + l.Day()
		if d > want || d == want && l.Hour()*60+l.Minute() > at.Hour*60+at.Minute {
			return c
		}
	}
	return t
}

// Schedule runs a check run and an update run every day (R6).
type Schedule struct {
	Runner *Runner
	// CheckAt and UpdateAt are the times from the environment, used until
	// the web UI saves its own.
	CheckAt  Clock
	UpdateAt Clock
	Location *time.Location
	// Wake makes the loop read the times again, after the web UI saved them.
	Wake chan struct{}

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Clocks are the check and update times in force: the web UI's once saved.
func (s *Schedule) Clocks() (check, update Clock) {
	check, update = s.CheckAt, s.UpdateAt
	st, err := s.Runner.Store.Read()
	if err != nil || !st.Settings.Saved {
		return check, update
	}
	if c, err := ParseClock(st.Settings.CheckAt); err == nil {
		check = c
	}
	if u, err := ParseClock(st.Settings.UpdateAt); err == nil {
		update = u
	}
	return check, update
}

// Loop runs until ctx ends. Each kind's next run is counted from its last
// scheduled time, not from when a run ended, so a long run never makes the
// other one skip a day: an overdue run starts at once. When both fall on the
// same minute, the check goes first. A time changed in the web UI counts
// from the moment the change is seen, so a time moved to the past waits for
// tomorrow, and the other kind keeps its count.
func (s *Schedule) Loop(ctx context.Context) {
	now, wait := s.now, s.sleep
	if now == nil {
		now = time.Now
	}
	if wait == nil {
		wait = sleep
	}
	start := now()
	ref := map[string]time.Time{KindCheck: start, KindUpdate: start}
	var seen map[string]Clock
	for {
		checkAt, updateAt := s.Clocks()
		planned := map[string]Clock{KindCheck: checkAt, KindUpdate: updateAt}
		for kind, c := range planned {
			if old, ok := seen[kind]; ok && old != c {
				ref[kind] = now()
			}
		}
		seen = planned
		check, upd := Next(ref[KindCheck], checkAt, s.Location), Next(ref[KindUpdate], updateAt, s.Location)
		kinds, at := []string{KindCheck}, check
		switch {
		case upd.Before(check):
			kinds, at = []string{KindUpdate}, upd
		case upd.Equal(check):
			kinds = []string{KindCheck, KindUpdate}
		}
		slog.Info("next run", "kinds", kinds, "at", at)
		woke, err := s.waitOrWake(ctx, wait, at.Sub(now()))
		if woke {
			// The times were saved: the top of the loop sees what changed.
			continue
		}
		if err != nil {
			return
		}
		for _, kind := range kinds {
			// A run waits for one going on, and the times may change
			// meanwhile: it starts only if its time is still the one it was
			// planned for, and the next turn of the loop plans it anew.
			run, ran, err := s.Runner.RunIf(ctx, kind, func() bool {
				check, upd := s.Clocks()
				return map[string]Clock{KindCheck: check, KindUpdate: upd}[kind] == planned[kind]
			})
			switch {
			case err != nil:
				slog.Error("run failed", "kind", kind, "error", err)
			case !ran:
				slog.Info("run left for its new time", "kind", kind)
			default:
				slog.Info("run finished", "kind", kind, "id", run.ID, "hosts", len(run.Hosts))
			}
			ref[kind] = at
		}
	}
}

// waitOrWake waits d, or less when Wake fires; woke tells which. It
// returns only once its watcher has stopped, so no watcher left behind takes
// a wake meant for the next wait.
func (s *Schedule) waitOrWake(ctx context.Context, wait func(context.Context, time.Duration) error, d time.Duration) (bool, error) {
	if s.Wake == nil {
		return false, wait(ctx, d)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woke := make(chan bool, 1)
	go func() {
		select {
		case <-s.Wake:
			cancel()
			woke <- true
		case <-wctx.Done():
			woke <- false
		}
	}()
	err := wait(wctx, d)
	cancel()
	return <-woke, err
}
