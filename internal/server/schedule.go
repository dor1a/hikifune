package server

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dor1a/hikifune/internal/store"
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

// Every is how often a run comes (R6). The zero value is every day.
type Every struct {
	// Weekly runs on Weekday each week.
	Weekly  bool
	Weekday time.Weekday
	// Day, from 1 to 30, runs on that day of each month, and on the last day
	// of a shorter month; 31 is the last day of every month.
	Day int
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// LastDay is the Day of an Every that runs on the last day of each month.
const LastDay = 31

// ParseEvery reads day, a weekday (sun to sat, or its full name), a day of
// the month (1 to 30), or last for the last day of the month (31 reads the
// same).
func ParseEvery(s string) (Every, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "day":
		return Every{}, nil
	case "last", "last day":
		return Every{Day: LastDay}, nil
	}
	for name, wd := range weekdayNames {
		if s == name || s == strings.ToLower(wd.String()) {
			return Every{Weekly: true, Weekday: wd}, nil
		}
	}
	// Digits only: Atoi would also take a sign, as in +31, which the page
	// does not.
	if n, err := strconv.Atoi(s); err == nil && strings.Trim(s, "0123456789") == "" && n >= 1 && n <= 31 {
		return Every{Day: n}, nil
	}
	return Every{}, fmt.Errorf("%q is not day, a weekday such as sun, a day of the month from 1 to 30, or last", s)
}

// String is the form ParseEvery reads: day, sun to sat, 1 to 30, or last.
func (e Every) String() string {
	switch {
	case e.Weekly:
		return strings.ToLower(e.Weekday.String()[:3])
	case e.Day == LastDay:
		return "last"
	case e.Day > 0:
		return strconv.Itoa(e.Day)
	}
	return "day"
}

// Words says it for people.
func (e Every) Words() string {
	switch {
	case e.Weekly:
		return "every " + e.Weekday.String()
	case e.Day == LastDay:
		return "on the last day of every month"
	case e.Day >= 29:
		return fmt.Sprintf("on day %d of every month (or its last day)", e.Day)
	case e.Day > 0:
		return fmt.Sprintf("on day %d of every month", e.Day)
	}
	return "every day"
}

// on tells whether a run comes on the date.
func (e Every) on(date time.Time) bool {
	switch {
	case e.Weekly:
		return date.Weekday() == e.Weekday
	case e.Day > 0:
		last := time.Date(date.Year(), date.Month()+1, 0, 12, 0, 0, 0, time.UTC).Day()
		return date.Day() == min(e.Day, last)
	}
	return true
}

// When is a run's schedule: how often and at what time of day.
type When struct {
	Every Every
	At    Clock
}

// Words says it for people, such as "every Sunday at 01:00".
func (w When) Words() string { return w.Every.Words() + " at " + w.At.String() }

// Next is the first time after now that w comes, in loc. On a day when a
// daylight saving change skips that time, it is the moment the clock jumps
// past it.
func (w When) Next(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	// Every schedule comes within a month; the bound only keeps the loop
	// finite.
	for day := 0; day < 400; day++ {
		date := time.Date(n.Year(), n.Month(), n.Day()+day, 12, 0, 0, 0, time.UTC)
		if !w.Every.on(date) {
			continue
		}
		t := time.Date(date.Year(), date.Month(), date.Day(), w.At.Hour, w.At.Minute, 0, 0, loc)
		if l := t.In(loc); l.Hour() != w.At.Hour || l.Minute() != w.At.Minute {
			t = gapEnd(t, date.Year(), date.Month(), date.Day(), w.At, loc)
		}
		if t.After(n) {
			return t
		}
	}
	return n.AddDate(1, 0, 0)
}

// Next is the first time after now that the clock shows at, in loc, on any
// day.
func Next(now time.Time, at Clock, loc *time.Location) time.Time {
	return When{At: at}.Next(now, loc)
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

// Schedule runs the check and update runs, each on its own schedule (R6).
type Schedule struct {
	Runner *Runner
	// CheckAt, UpdateAt, CheckEvery and UpdateEvery come from the
	// environment, and are used until the web UI saves its own.
	CheckAt     Clock
	UpdateAt    Clock
	CheckEvery  Every
	UpdateEvery Every
	Location    *time.Location
	// Wake makes the loop read the times again, after the web UI saved them.
	Wake chan struct{}

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	planned map[string]Planned
}

// Planned is when the loop will next run a kind, and the schedule it
// planned that with. A time already past is a run waiting for the one going
// on, which starts as soon as it ends.
type Planned struct {
	When When
	At   time.Time
}

// Planned is the loop's plan for kind; ok is false until it has made one.
func (s *Schedule) Planned(kind string) (Planned, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.planned[kind]
	return p, ok
}

func (s *Schedule) plan(p map[string]Planned) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planned = p
}

// Times are the check and update schedules in force: the web UI's once
// saved (R36).
func (s *Schedule) Times() (check, update When) {
	check, update, _ = s.times()
	return check, update
}

// TimesFor are the schedules that would be in force with set saved.
func (s *Schedule) TimesFor(set store.Settings) (check, update When) {
	return Saved(set, When{s.CheckEvery, s.CheckAt}, When{s.UpdateEvery, s.UpdateAt})
}

// Saved are the schedules in force with set, given the environment's. A
// saved schedule replaces the environment's whole, so an empty period in it
// is every day.
func Saved(set store.Settings, envCheck, envUpdate When) (check, update When) {
	check, update = envCheck, envUpdate
	if !set.Saved {
		return check, update
	}
	if c, err := ParseClock(set.CheckAt); err == nil {
		check.At = c
	}
	if u, err := ParseClock(set.UpdateAt); err == nil {
		update.At = u
	}
	if e, err := ParseEvery(set.CheckEvery); err == nil {
		check.Every = e
	}
	if e, err := ParseEvery(set.UpdateEvery); err == nil {
		update.Every = e
	}
	return check, update
}

// times also says when each schedule was last changed in the web UI.
func (s *Schedule) times() (check, update When, saved map[string]time.Time) {
	st, err := s.Runner.Store.Read()
	if err != nil {
		check, update = s.TimesFor(store.Settings{})
		return check, update, nil
	}
	check, update = s.TimesFor(st.Settings)
	return check, update, map[string]time.Time{KindCheck: st.Settings.CheckSavedAt, KindUpdate: st.Settings.UpdateSavedAt}
}

// Loop runs until ctx ends. Each kind's next run is counted from its last
// scheduled time, not from when a run ended, so a long run never makes the
// other one skip a day: an overdue run starts at once. When both fall on the
// same minute, the check goes first. A schedule changed in the web UI counts
// from when it was saved, so a time moved to the past waits for its next
// day, a time still ahead then runs even when a run kept the loop busy
// past it, and the other kind keeps its count.
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
	var seen map[string]When
	var seenSaved map[string]time.Time
	for {
		checkWhen, updateWhen, saved := s.times()
		if saved == nil && seen != nil {
			// The state could not be read: keep what was in force.
			checkWhen, updateWhen, saved = seen[KindCheck], seen[KindUpdate], seenSaved
		}
		planned := map[string]When{KindCheck: checkWhen, KindUpdate: updateWhen}
		for kind, c := range planned {
			old, ok := seen[kind]
			// A schedule saved since, even back to what the loop last saw
			// (changed and then reset while a run kept it busy), counts
			// from its save too.
			if !ok || old == c && saved[kind].Equal(seenSaved[kind]) {
				continue
			}
			// Counted from when it was saved, not from when the loop saw it
			// after a run: a time still ahead then still runs.
			ref[kind] = now()
			if at := saved[kind]; !at.IsZero() && at.Before(ref[kind]) && at.After(start) {
				ref[kind] = at
			}
		}
		seen, seenSaved = planned, saved
		check, upd := checkWhen.Next(ref[KindCheck], s.Location), updateWhen.Next(ref[KindUpdate], s.Location)
		s.plan(map[string]Planned{KindCheck: {checkWhen, check}, KindUpdate: {updateWhen, upd}})
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
			// planned for and was not saved since (changed and set back
			// counts as saved), and the next turn of the loop plans it anew.
			run, ran, err := s.Runner.RunIf(ctx, kind, func() bool {
				check, upd, savedNow := s.times()
				switch {
				case savedNow == nil:
					// The state could not be read now: go by the plan.
					return true
				case saved == nil:
					// Nor when the run was planned: go by the times.
					return map[string]When{KindCheck: check, KindUpdate: upd}[kind] == planned[kind]
				}
				return map[string]When{KindCheck: check, KindUpdate: upd}[kind] == planned[kind] &&
					savedNow[kind].Equal(saved[kind])
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
