package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/store"
	"github.com/dor1a/hikifune/internal/update"
)

func names(cs []protocol.Checked) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestOrder(t *testing.T) {
	cs := []protocol.Checked{
		{Name: "app-web-1", ComposeProject: "app", ComposeService: "web", DependsOn: []string{"api"}},
		{Name: "app-api-1", ComposeProject: "app", ComposeService: "api", DependsOn: []string{"db", "cache"}},
		{Name: "app-db-1", ComposeProject: "app", ComposeService: "db"},
		{Name: "lone"},
		{Name: "other-db-1", ComposeProject: "other", ComposeService: "db"},
		// Depends on a service that is not being updated.
		{Name: "other-web-1", ComposeProject: "other", ComposeService: "web", DependsOn: []string{"proxy"}},
	}
	got := names(order(cs))
	want := []string{"app-db-1", "app-api-1", "app-web-1", "lone", "other-db-1", "other-web-1"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v\nwant    %v", got, want)
	}
	// A cycle falls back to names.
	cyc := []protocol.Checked{
		{Name: "b", ComposeProject: "p", ComposeService: "b", DependsOn: []string{"a"}},
		{Name: "a", ComposeProject: "p", ComposeService: "a", DependsOn: []string{"b"}},
	}
	if got := names(order(cyc)); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("cycle = %v", got)
	}
}

func TestNextAndParseClock(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at, err := ParseClock("01:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ now, want string }{
		{"2026-11-19T00:30:00+09:00", "2026-11-19T01:00:00+09:00"},
		{"2026-11-19T01:00:00+09:00", "2026-11-20T01:00:00+09:00"},
		{"2026-11-30T23:59:00+09:00", "2026-12-01T01:00:00+09:00"},
		{"2026-11-18T15:30:00Z", "2026-11-19T01:00:00+09:00"},
		{"2026-11-18T16:30:00Z", "2026-11-20T01:00:00+09:00"},
	} {
		now, _ := time.Parse(time.RFC3339, tt.now)
		want, _ := time.Parse(time.RFC3339, tt.want)
		if got := Next(now, at, seoul); !got.Equal(want) {
			t.Errorf("Next(%s) = %s, want %s", tt.now, got, want)
		}
	}
	for _, bad := range []string{"24:00", "01:60", "noon", ""} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) took it", bad)
		}
	}
}

// fakeHost plays one agent.
type fakeHost struct {
	mu       sync.Mutex
	version  protocol.Version
	versErr  error
	report   protocol.CheckReport
	outcomes map[string]update.Result // by container; missing means updated
	started  []string
	busyOnce bool
	lostJobs bool
	pruned   int
	jobs     map[string]protocol.Job
	jobCalls int
	jobErrs  int // asks for a job that fail before it answers
	requests []protocol.UpdateRequest
	onCheck  func()
}

func (h *fakeHost) Version(context.Context) (protocol.Version, error) { return h.version, h.versErr }
func (h *fakeHost) Check(context.Context) (protocol.CheckReport, error) {
	if h.onCheck != nil {
		h.onCheck()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.report, nil
}
func (h *fakeHost) StartUpdate(_ context.Context, req protocol.UpdateRequest) (protocol.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busyOnce {
		h.busyOnce = false
		busy := protocol.Job{ID: "other", Container: "manual", State: protocol.JobDone,
			Result: &update.Result{Outcome: update.Updated}}
		h.jobs["other"] = busy
		return protocol.Job{}, &StatusError{Status: http.StatusConflict, Body: protocol.Error{Error: "busy", Busy: &busy}}
	}
	h.started = append(h.started, req.Container)
	h.requests = append(h.requests, req)
	res, ok := h.outcomes[req.Container]
	if !ok {
		res = update.Result{Container: req.Container, Outcome: update.Updated}
	}
	now := time.Now()
	j := protocol.Job{ID: fmt.Sprint(len(h.started)), Container: req.Container, State: protocol.JobDone,
		Started: now, Finished: &now, Result: &res}
	h.jobs[j.ID] = j
	return protocol.Job{ID: j.ID, State: protocol.JobRunning, Started: now}, nil
}
func (h *fakeHost) Job(_ context.Context, id string) (protocol.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobCalls++
	if h.jobErrs > 0 {
		h.jobErrs--
		return protocol.Job{}, errors.New("unexpected EOF")
	}
	if h.lostJobs {
		return protocol.Job{}, &StatusError{Status: http.StatusNotFound}
	}
	return h.jobs[id], nil
}
func (h *fakeHost) Prune(context.Context) (protocol.Pruned, error) {
	h.pruned++
	return protocol.Pruned{Reclaimed: 1000}, nil
}

func checked(name string, status registry.Status, digest string) protocol.Checked {
	return protocol.Checked{ID: "id-" + name, Name: name, Image: name + ":1", State: "running",
		Check: registry.Result{Status: status, RemoteDigest: digest}}
}

func newHost() *fakeHost {
	return &fakeHost{version: protocol.Version{Agent: "v1", Docker: &inventory.Docker{Version: "29.1.0", APIVersion: "1.52"}},
		outcomes: map[string]update.Result{}, jobs: map[string]protocol.Job{}}
}

func newRunner(t *testing.T, hosts map[string]*fakeHost) *Runner {
	t.Helper()
	st, err := store.OpenJSON(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *store.State) error {
		for _, n := range []string{"a", "b"} {
			if hosts[n] != nil {
				s.Agents = append(s.Agents, store.Agent{Name: n, URL: "http://" + n + ":8870", Token: "t"})
			}
		}
		return nil
	})
	return &Runner{Store: st, Version: "v1", Watch: time.Second, Poll: time.Millisecond,
		dial: func(a store.Agent) (agentAPI, error) { return hosts[a.Name], nil }}
}

func entry(t *testing.T, h store.HostRun, name string) store.Entry {
	t.Helper()
	for _, e := range h.Containers {
		if e.Container == name {
			return e
		}
	}
	t.Fatalf("%s: no entry for %s in %+v", h.Agent, name, h.Containers)
	return store.Entry{}
}

func TestCheckRunRecordsEveryContainer(t *testing.T) {
	a := newHost()
	guard := checked("guard", registry.UpdateAvailable, "sha256:g")
	guard.Protected = true
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w"), guard,
		checked("db", registry.UpToDate, "sha256:d")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	run, err := r.Run(context.Background(), KindCheck)
	if err != nil {
		t.Fatal(err)
	}
	h := run.Hosts[0]
	if len(h.Containers) != 3 || entry(t, h, "guard").Status != "update_available" || !entry(t, h, "guard").Protected ||
		entry(t, h, "db").Status != "up_to_date" || len(a.started) != 0 || h.Docker != "29.1.0" {
		t.Fatalf("host run = %+v, started %v", h, a.started)
	}
	if st, _ := r.Store.Read(); len(st.Runs) != 1 || st.Runs[0].ID != run.ID {
		t.Fatalf("recorded runs = %+v", st.Runs)
	}
}

func TestHostsAreSkippedWithAReason(t *testing.T) {
	old, broken, gone, low := newHost(), newHost(), newHost(), newHost()
	old.version.Agent = "v0"
	broken.version = protocol.Version{Agent: "v1", DockerError: "Docker 20.10 serves Engine API 1.41"}
	gone.versErr = errors.New("connection refused")
	// Only the server's own look at the API version catches this one (R16).
	low.version.Docker = &inventory.Docker{Version: "23.0.0", APIVersion: "1.42"}
	low.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	for _, h := range []*fakeHost{old, broken, gone, low} {
		r := newRunner(t, map[string]*fakeHost{"a": h})
		run, err := r.Run(context.Background(), KindUpdate)
		if err != nil {
			t.Fatal(err)
		}
		hr := run.Hosts[0]
		if hr.Skipped == "" && hr.Error == "" || len(hr.Containers) != 0 {
			t.Errorf("host run = %+v", hr)
		}
	}
}

func TestUpdateRun(t *testing.T) {
	a, b := newHost(), newHost()
	db := checked("app-db-1", registry.UpdateAvailable, "sha256:db")
	db.ComposeProject, db.ComposeService = "app", "db"
	web := checked("app-web-1", registry.UpdateAvailable, "sha256:web")
	web.ComposeProject, web.ComposeService, web.DependsOn = "app", "web", []string{"db"}
	guard := checked("guard", registry.UpdateAvailable, "sha256:g")
	guard.Protected = true
	self := checked("hikifune-agent", registry.UpdateAvailable, "sha256:s")
	self.Self = true
	stopped := checked("job", registry.UpdateAvailable, "sha256:j")
	stopped.State = "exited"
	bad := checked("bad", registry.UpdateAvailable, "sha256:bad1")
	a.report.Containers = []protocol.Checked{web, db, guard, self, stopped, bad, checked("same", registry.UpToDate, "sha256:x")}
	a.outcomes["bad"] = update.Result{Container: "bad", Outcome: update.RolledBack, Reason: "exited with code 1", Digest: "sha256:bad1"}
	b.report.Containers = []protocol.Checked{checked("lone", registry.UpdateAvailable, "sha256:l")}
	b.busyOnce = true
	r := newRunner(t, map[string]*fakeHost{"a": a, "b": b})

	run, err := r.Run(context.Background(), KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	// Dependencies first, one container at a time, the rest skipped with a reason.
	if !slices.Equal(a.started, []string{"app-db-1", "app-web-1", "bad"}) {
		t.Fatalf("started on a: %v", a.started)
	}
	// Each request names the container the check saw.
	if a.requests[0].ID != "id-app-db-1" || a.requests[0].WatchSeconds != 1 {
		t.Errorf("request = %+v", a.requests[0])
	}
	ha := run.Hosts[0]
	for name, want := range map[string]string{"app-db-1": "updated", "app-web-1": "updated", "bad": "rolled_back",
		"guard": "skipped", "hikifune-agent": "skipped", "job": "skipped", "same": "up_to_date"} {
		if e := entry(t, ha, name); e.Status != want || (want == "skipped" && e.Reason == "") {
			t.Errorf("%s: %+v, want %s", name, e, want)
		}
	}
	if e := entry(t, ha, "app-db-1"); e.Started == nil || e.Finished == nil {
		t.Errorf("app-db-1 has no times: %+v", e)
	}
	if a.pruned != 1 || ha.Reclaimed != 1000 {
		t.Errorf("pruned %d, reclaimed %d", a.pruned, ha.Reclaimed)
	}
	// The busy agent finished its own job first, then took this one.
	if !slices.Equal(b.started, []string{"lone"}) || entry(t, run.Hosts[1], "lone").Status != "updated" {
		t.Errorf("b: started %v, %+v", b.started, run.Hosts[1])
	}
	st, _ := r.Store.Read()
	if len(st.Failed) != 1 || st.Failed[0].Container != "bad" || st.Failed[0].Digest != "sha256:bad1" {
		t.Fatalf("failed = %+v", st.Failed)
	}

	// The next run skips the failed digest (R15).
	a.started = nil
	db.Check.Status, web.Check.Status = registry.UpToDate, registry.UpToDate
	a.report.Containers = []protocol.Checked{db, web, bad}
	run, _ = r.Run(context.Background(), KindUpdate)
	if len(a.started) != 0 || !strings.Contains(entry(t, run.Hosts[0], "bad").Reason, "failed on") || a.pruned != 1 {
		t.Fatalf("second run: started %v, %+v", a.started, run.Hosts[0])
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 1 {
		t.Fatalf("the failure was not kept: %+v", st.Failed)
	}

	// A newer digest is tried again, and the old failure goes.
	bad.Check.RemoteDigest = "sha256:bad2"
	a.report.Containers = []protocol.Checked{bad}
	delete(a.outcomes, "bad")
	r.Run(context.Background(), KindUpdate)
	if !slices.Equal(a.started, []string{"bad"}) {
		t.Fatalf("third run: started %v", a.started)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("failed = %+v", st.Failed)
	}
}

// A job the agent no longer knows is recorded as failed without blocking the
// digest: the container's state is unknown.
func TestLostJob(t *testing.T) {
	a := newHost()
	a.lostJobs = true
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	run, _ := r.Run(context.Background(), KindUpdate)
	e := entry(t, run.Hosts[0], "web")
	if e.Status != "failed" || !strings.Contains(e.Reason, "no longer knows") || a.pruned != 0 {
		t.Fatalf("entry %+v", e)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("failed = %+v", st.Failed)
	}
}

// The schedule runs a check and then an update when both fall on the same
// minute, every day, counted from the scheduled times.
func TestScheduleLoop(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 11, 19, 23, 50, 0, 0, seoul)
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	r.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Schedule{Runner: r, Location: seoul, CheckAt: Clock{0, 0}, UpdateAt: Clock{0, 0},
		now: func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			if st, _ := r.Store.Read(); len(st.Runs) >= 4 {
				return context.Canceled
			}
			now = now.Add(d)
			return nil
		}}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	var kinds []string
	for _, run := range st.Runs {
		kinds = append(kinds, run.Kind+"@"+run.Started.In(seoul).Format("01-02 15:04"))
	}
	want := []string{"check@11-20 00:00", "update@11-20 00:00", "check@11-21 00:00", "update@11-21 00:00"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("runs = %v\nwant   %v", kinds, want)
	}
}

// A run that ends after the other one's time does not push it to the next
// day: the overdue run starts at once.
func TestScheduleRunsAnOverdueRun(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 11, 19, 23, 59, 0, 0, seoul)
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	// Each run takes 90 minutes.
	r.now = func() time.Time { return now }
	var waits []time.Duration
	s := &Schedule{Runner: r, Location: seoul, CheckAt: Clock{0, 0}, UpdateAt: Clock{1, 0},
		now: func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			if st, _ := r.Store.Read(); len(st.Runs) >= 2 {
				return context.Canceled
			}
			waits = append(waits, d)
			if d > 0 {
				now = now.Add(d)
			}
			now = now.Add(90 * time.Minute)
			return nil
		}}
	s.Loop(context.Background())
	st, _ := r.Store.Read()
	if len(st.Runs) != 2 || st.Runs[1].Kind != KindUpdate || waits[1] > 0 {
		t.Fatalf("runs %d, waits %v", len(st.Runs), waits)
	}
}

// A dropped answer to an ask for the job loses nothing (R32).
func TestJobAskRetries(t *testing.T) {
	a := newHost()
	a.jobErrs = 2
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	run, _ := r.Run(context.Background(), KindUpdate)
	if e := entry(t, run.Hosts[0], "web"); e.Status != "updated" || a.pruned != 1 {
		t.Fatalf("entry %+v, pruned %d", e, a.pruned)
	}
}

// The failure records the digest the job went for, which may be newer than
// the run's own check.
func TestFailureKeepsTheJobsDigest(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:A")}
	a.outcomes["web"] = update.Result{Outcome: update.Failed, Reason: "pull: boom", Digest: "sha256:B"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Run(context.Background(), KindUpdate)
	st, _ := r.Store.Read()
	if len(st.Failed) != 1 || st.Failed[0].Digest != "sha256:B" {
		t.Fatalf("failed = %+v", st.Failed)
	}
}

// A check that cannot tell keeps a failure; one that shows another digest or
// the container gone drops it.
func TestFailuresSurviveAnUnsureCheck(t *testing.T) {
	a := newHost()
	bad := checked("bad", registry.UpdateAvailable, "sha256:A")
	a.report.Containers = []protocol.Checked{bad}
	a.outcomes["bad"] = update.Result{Outcome: update.RolledBack, Reason: "exited", Digest: "sha256:A"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Run(context.Background(), KindUpdate)

	unsure := checked("bad", registry.Failed, "")
	a.report.Containers = []protocol.Checked{unsure}
	r.Run(context.Background(), KindCheck)
	if st, _ := r.Store.Read(); len(st.Failed) != 1 {
		t.Fatalf("an unsure check dropped the failure: %+v", st.Failed)
	}
	a.report.Containers = []protocol.Checked{bad}
	a.started = nil
	run, _ := r.Run(context.Background(), KindUpdate)
	if len(a.started) != 0 || entry(t, run.Hosts[0], "bad").Status != "skipped" {
		t.Fatalf("A was tried again: %v", a.started)
	}
	a.report.Containers = nil
	r.Run(context.Background(), KindCheck)
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("a gone container kept its failure: %+v", st.Failed)
	}
}

// A run records its changes against the state as it is then, so a failure
// another run recorded meanwhile stays.
func TestRunsDoNotUndoEachOther(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("other", registry.UpdateAvailable, "sha256:D")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onCheck = func() {
		r.Store.Update(func(s *store.State) error {
			s.Failed = append(s.Failed, store.Failure{Agent: "a", Container: "other", Digest: "sha256:D", Reason: "x"})
			return nil
		})
	}
	r.Run(context.Background(), KindCheck)
	if st, _ := r.Store.Read(); len(st.Failed) != 1 {
		t.Fatalf("failed = %+v", st.Failed)
	}
}

// An agent that takes a request and never answers fails its host within the
// call's limit, so the run ends.
func TestAnAgentThatHangsFailsItsHost(t *testing.T) {
	old := shortCall
	shortCall = 50 * time.Millisecond
	defer func() { shortCall = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	st, _ := store.OpenJSON(t.TempDir(), 0)
	st.Update(func(s *store.State) error {
		s.Agents = append(s.Agents, store.Agent{Name: "slow", URL: srv.URL, Token: "t"})
		return nil
	})
	r := &Runner{Store: st, Version: "v1"}
	start := time.Now()
	run, err := r.Run(context.Background(), KindCheck)
	if err != nil || !strings.Contains(run.Hosts[0].Error, "unreachable") || time.Since(start) > 5*time.Second {
		t.Fatalf("run %+v, %v, took %s", run, err, time.Since(start))
	}
}

func TestOrderWaitsForEveryReplica(t *testing.T) {
	cs := []protocol.Checked{
		{Name: "p-app-1", ComposeProject: "p", ComposeService: "app", DependsOn: []string{"db"}},
		{Name: "p-db-1", ComposeProject: "p", ComposeService: "db"},
		{Name: "p-db-2", ComposeProject: "p", ComposeService: "db"},
	}
	if got := names(order(cs)); !slices.Equal(got, []string{"p-db-1", "p-db-2", "p-app-1"}) {
		t.Errorf("order = %v", got)
	}
}

// A daylight saving change that skips the time runs at the end of the gap.
func TestNextOverADaylightSavingGap(t *testing.T) {
	havana, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 3, 7, 23, 59, 0, 0, havana)
	got := Next(now, Clock{0, 0}, havana)
	if l := got.In(havana); !got.After(now) || l.Day() != 8 || l.Hour() != 1 || l.Minute() != 0 {
		t.Fatalf("Next = %s", got.In(havana))
	}
	if again := Next(got, Clock{0, 0}, havana); again.In(havana).Day() != 9 || again.In(havana).Hour() != 0 {
		t.Fatalf("the day after = %s", again.In(havana))
	}
	newYork, _ := time.LoadLocation("America/New_York")
	got = Next(time.Date(2026, 3, 8, 0, 0, 0, 0, newYork), Clock{2, 30}, newYork)
	if l := got.In(newYork); l.Day() != 8 || l.Hour() != 3 || l.Minute() != 0 {
		t.Fatalf("New York 02:30 = %s", l)
	}
}

// A job refused before its registry check tried no digest, so nothing is
// skipped at the next run.
func TestARefusedJobBlocksNoDigest(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:A")}
	a.outcomes["web"] = update.Result{Outcome: update.Failed, Reason: "the container was recreated since the check"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	run, _ := r.Run(context.Background(), KindUpdate)
	if e := entry(t, run.Hosts[0], "web"); e.Status != "failed" {
		t.Fatalf("entry %+v", e)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("failed = %+v", st.Failed)
	}
}
