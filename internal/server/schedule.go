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
	Runner   *Runner
	CheckAt  Clock
	UpdateAt Clock
	Location *time.Location

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Loop runs until ctx ends. The next runs are counted from the last
// scheduled time, not from when a run ended, so a long run never makes the
// other one skip a day: an overdue run starts at once. When both fall on the
// same minute, the check goes first.
func (s *Schedule) Loop(ctx context.Context) {
	now, wait := s.now, s.sleep
	if now == nil {
		now = time.Now
	}
	if wait == nil {
		wait = sleep
	}
	ref := now()
	for {
		check, upd := Next(ref, s.CheckAt, s.Location), Next(ref, s.UpdateAt, s.Location)
		kinds, at := []string{KindCheck}, check
		switch {
		case upd.Before(check):
			kinds, at = []string{KindUpdate}, upd
		case upd.Equal(check):
			kinds = []string{KindCheck, KindUpdate}
		}
		slog.Info("next run", "kinds", kinds, "at", at)
		if err := wait(ctx, at.Sub(now())); err != nil {
			return
		}
		for _, kind := range kinds {
			run, err := s.Runner.Run(ctx, kind)
			if err != nil {
				slog.Error("run failed", "kind", kind, "error", err)
				continue
			}
			slog.Info("run finished", "kind", kind, "id", run.ID, "hosts", len(run.Hosts))
		}
		ref = at
	}
}
