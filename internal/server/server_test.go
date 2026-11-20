package server

import (
	"cmp"
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
	onStart  func(container string)
	// lostAnswers and lostRequests are update requests whose answer, or
	// which themselves, are lost on the way; gatewayAnswers are taken and
	// then answered with a 502 by something between.
	lostAnswers    int
	lostRequests   int
	gatewayAnswers int
}

func (h *fakeHost) Version(context.Context) (protocol.Version, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version, h.versErr
}
func (h *fakeHost) Check(context.Context) (protocol.CheckReport, error) {
	if h.onCheck != nil {
		h.onCheck()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.report, nil
}
func (h *fakeHost) StartUpdate(_ context.Context, req protocol.UpdateRequest) (protocol.Job, error) {
	if h.onStart != nil {
		h.onStart(req.Container)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lostRequests > 0 {
		h.lostRequests--
		return protocol.Job{}, errors.New("POST /api/v1/updates: connection reset by peer")
	}
	if j, ok := h.jobs[req.Job]; ok && req.Job != "" {
		return j, nil
	}
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
	j := protocol.Job{ID: cmp.Or(req.Job, fmt.Sprint(len(h.started))), Container: req.Container, State: protocol.JobDone,
		Started: now, Finished: &now, Result: &res}
	h.jobs[j.ID] = j
	if h.lostAnswers > 0 {
		h.lostAnswers--
		return protocol.Job{}, errors.New("POST /api/v1/updates: decode the answer: unexpected EOF")
	}
	if h.gatewayAnswers > 0 {
		h.gatewayAnswers--
		return protocol.Job{}, &StatusError{Status: http.StatusBadGateway}
	}
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
	j, ok := h.jobs[id]
	if h.lostJobs || !ok {
		return protocol.Job{}, &StatusError{Status: http.StatusNotFound}
	}
	return j, nil
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
				s.Agents = append(s.Agents, store.Agent{Name: n, URL: "http://" + n + ":7778", Token: "t"})
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

// An update by hand goes to the one container asked for, whatever stops a
// scheduled one, and records that container only.
func TestUpdateByHand(t *testing.T) {
	a := newHost()
	guard := checked("guard", registry.UpdateAvailable, "sha256:g")
	guard.Protected = true
	a.report.Containers = []protocol.Checked{guard, checked("web", registry.UpdateAvailable, "sha256:w")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Store.Update(func(s *store.State) error {
		s.Agents[0].CheckOnly = true
		s.Failed = append(s.Failed, store.Failure{Agent: "a", Container: "guard", Digest: "sha256:g", Reason: "x"})
		return nil
	})
	done := make(chan store.Run, 1)
	err := r.Start(context.Background(), Spec{Kind: KindUpdate, Hosts: []string{"a"}, Container: "guard", ContainerID: "id-guard", Manual: "alice"},
		func(run store.Run, err error) { done <- run })
	if err != nil {
		t.Fatal(err)
	}
	run := <-done
	if !slices.Equal(a.started, []string{"guard"}) || run.Manual != "alice" || run.Container != "guard" ||
		len(run.Hosts[0].Containers) != 1 || run.Hosts[0].Containers[0].Status != "updated" {
		t.Fatalf("started %v, run %+v", a.started, run)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Errorf("the update did not clear the failure: %+v", st.Failed)
	}
	if err := r.Start(context.Background(), Spec{Kind: KindUpdate, Container: "web", ContainerID: "id-web"}, nil); err == nil {
		t.Error("an update by hand without its host was taken")
	}
	if err := r.Start(context.Background(), Spec{Kind: KindUpdate, Hosts: []string{"a"}, Container: "web"}, nil); err == nil {
		t.Error("an update by hand without the shown ID was taken")
	}
}

// A container recreated since the page showed it is not updated by hand;
// the record shows the new one, so the page can offer it again.
func TestUpdateByHandOfARecreatedContainer(t *testing.T) {
	a := newHost()
	web := checked("web", registry.UpdateAvailable, "sha256:w")
	web.ID = "id-replacement"
	a.report.Containers = []protocol.Checked{web}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	done := make(chan store.Run, 1)
	if err := r.Start(context.Background(), Spec{Kind: KindUpdate, Hosts: []string{"a"}, Container: "web", ContainerID: "id-web"},
		func(run store.Run, err error) { done <- run }); err != nil {
		t.Fatal(err)
	}
	run := <-done
	e := entry(t, run.Hosts[0], "web")
	if len(a.started) != 0 || e.ID != "id-replacement" || e.Status != "update_available" || !strings.Contains(e.Reason, "recreated since the page showed it") {
		t.Fatalf("started %v, entry %+v", a.started, e)
	}
}

// Turning check-only on during a scheduled run stops the updates still to
// come on that host.
func TestCheckOnlyTurnedOnDuringARun(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("one", registry.UpdateAvailable, "sha256:1"), checked("two", registry.UpdateAvailable, "sha256:2")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onStart = func(string) {
		r.Store.Update(func(s *store.State) error {
			s.Agents[0].CheckOnly = true
			return nil
		})
	}
	run, err := r.Run(context.Background(), KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.started) != 1 {
		t.Fatalf("started %v", a.started)
	}
	if e := entry(t, run.Hosts[0], a.started[0]); e.Status != "updated" {
		t.Errorf("first: %+v", e)
	}
	other := map[string]string{"one": "two", "two": "one"}[a.started[0]]
	if e := entry(t, run.Hosts[0], other); e.Status != "skipped" || !strings.Contains(e.Reason, "turned on during the run") || !e.Available {
		t.Errorf("second: %+v", e)
	}
}

// A check-only switch turned on while the agent is busy with another job
// keeps the waiting update from being sent.
func TestCheckOnlyTurnedOnWhileTheAgentIsBusy(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.busyOnce = true
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onStart = func(string) {
		r.Store.Update(func(s *store.State) error {
			s.Agents[0].CheckOnly = true
			return nil
		})
	}
	run, err := r.Run(context.Background(), KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if e := entry(t, run.Hosts[0], "web"); len(a.started) != 0 || e.Status != "skipped" || !strings.Contains(e.Reason, "turned on during the run") {
		t.Fatalf("started %v, entry %+v", a.started, e)
	}
}

// A host removed and added again under its name during a run is another
// host: the run's failures do not reach it.
func TestARunChangesNoFailureOfAHostAddedSince(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.outcomes["web"] = update.Result{Container: "web", Outcome: update.RolledBack, Reason: "exited", Digest: "sha256:w"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onStart = func(string) {
		r.Store.Update(func(s *store.State) error {
			s.Agents[0] = store.Agent{Name: "a", URL: "http://other:7778", Token: "t2", Added: time.Now().Add(time.Second)}
			return nil
		})
	}
	if _, err := r.Run(context.Background(), KindUpdate); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("failures %+v", st.Failed)
	}
}

// The answer to an update request is lost after the agent took the job:
// the server asks for the job by its own number until the agent answers,
// records the result and prunes. A request lost on its way is sent again,
// and started once.
func TestALostAnswerLosesNoJob(t *testing.T) {
	for _, tt := range []struct {
		name                                string
		requests, answers, lookups, gateway int
	}{
		{"the answer", 0, 1, 0, 0},
		{"the request", 1, 0, 0, 0},
		{"the request, then the answer to the second", 1, 1, 0, 0},
		{"the answer and the first ask for the job", 0, 1, 1, 0},
		{"the answer, a 502 from something between", 0, 0, 0, 1},
	} {
		a := newHost()
		a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
		a.lostRequests, a.lostAnswers, a.jobErrs, a.gatewayAnswers = tt.requests, tt.answers, tt.lookups, tt.gateway
		r := newRunner(t, map[string]*fakeHost{"a": a})
		run, err := r.Run(context.Background(), KindUpdate)
		if err != nil {
			t.Fatal(err)
		}
		e := entry(t, run.Hosts[0], "web")
		if e.Status != string(update.Updated) || len(a.started) != 1 || a.pruned != 1 || !protocol.ValidJob(a.requests[0].Job) {
			t.Errorf("lost %s: entry %+v, started %v, pruned %d, requests %+v", tt.name, e, a.started, a.pruned, a.requests)
		}
	}
}

// An agent that restarted after a request was lost is not sent it again:
// the agent that may have taken it is gone (R32).
func TestARestartedAgentIsNotSentALostRequestAgain(t *testing.T) {
	a := newHost()
	a.version.Boot = "first"
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.lostRequests = 1
	a.onStart = func(string) {
		a.mu.Lock()
		a.version.Boot = "second"
		a.mu.Unlock()
	}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	run, err := r.Run(context.Background(), KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if e := entry(t, run.Hosts[0], "web"); e.Status != string(update.Failed) || !strings.Contains(e.Reason, "restarted") || len(a.started) != 0 {
		t.Fatalf("entry %+v, started %v", e, a.started)
	}
}

// The check-only switch turned on before a lost request is sent again keeps
// it from going, as before the first send (R35).
func TestCheckOnlyStopsASendAgain(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.lostRequests = 1
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onStart = func(string) {
		r.Store.Update(func(s *store.State) error {
			s.Agents[0].CheckOnly = true
			return nil
		})
	}
	run, err := r.Run(context.Background(), KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if e := entry(t, run.Hosts[0], "web"); e.Status != "skipped" || len(a.started) != 0 {
		t.Fatalf("entry %+v, started %v", e, a.started)
	}
}

// A host removed and added again under its name right after the run read
// the agents, with an added time before the run's start, is another host
// all the same: the run's failures do not reach it.
func TestARunChangesNoFailureOfAHostAddedAgain(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.outcomes["web"] = update.Result{Container: "web", Outcome: update.RolledBack, Reason: "exited", Digest: "sha256:w"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	a.onCheck = func() {
		r.Store.Update(func(s *store.State) error {
			s.Agents[0] = store.Agent{Name: "a", URL: "http://other:7778", Token: "t2", Added: time.Now().Add(-time.Hour)}
			return nil
		})
	}
	if _, err := r.Run(context.Background(), KindUpdate); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 0 {
		t.Fatalf("failures %+v", st.Failed)
	}
}

// agent remove that wrote agents.json but failed on state.json leaves the
// old host's failures behind. A host added later under the name does not
// skip on them, and the next run drops them, with those of hosts gone.
func TestFailuresFromBeforeAHostWasAddedAreNotItsOwn(t *testing.T) {
	added := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		id   string
		old  store.Failure
	}{
		// The clock went back between the old failure and the new agent:
		// the IDs still tell them apart.
		{"by ID", "new", store.Failure{Agent: "a", AgentID: "old", Container: "web", Digest: "sha256:w", At: added.Add(time.Minute)}},
		// An agent added before agents had IDs goes by the added time.
		{"by the added time", "", store.Failure{Agent: "a", Container: "web", Digest: "sha256:w", At: added.Add(-time.Hour)}},
	} {
		a := newHost()
		a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
		a.outcomes["web"] = update.Result{Container: "web", Outcome: update.Updated, Digest: "sha256:w"}
		r := newRunner(t, map[string]*fakeHost{"a": a})
		r.Store.Update(func(s *store.State) error {
			s.Agents[0].ID, s.Agents[0].Added = tt.id, added
			s.Failed = []store.Failure{tt.old, {Agent: "gone", Container: "web", Digest: "sha256:w", At: added}}
			return nil
		})
		run, err := r.Run(context.Background(), KindUpdate)
		if err != nil {
			t.Fatal(err)
		}
		if e := entry(t, run.Hosts[0], "web"); e.Status != string(update.Updated) || len(a.started) != 1 {
			t.Fatalf("%s: entry %+v, started %v", tt.name, e, a.started)
		}
		if st, _ := r.Store.Read(); len(st.Failed) != 0 {
			t.Fatalf("%s: failures %+v", tt.name, st.Failed)
		}
	}
}

// A failure is recorded with the ID of the agent it happened on.
func TestAFailureCarriesTheAgentsID(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	a.outcomes["web"] = update.Result{Container: "web", Outcome: update.RolledBack, Reason: "exited", Digest: "sha256:w"}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Store.Update(func(s *store.State) error {
		s.Agents[0].ID = "this"
		return nil
	})
	if _, err := r.Run(context.Background(), KindUpdate); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Store.Read(); len(st.Failed) != 1 || st.Failed[0].AgentID != "this" {
		t.Fatalf("failures %+v", st.Failed)
	}
}

// Changing one time while a run goes keeps the other kind's count: the
// update due during a long check still starts after it.
func TestScheduleKeepsAnOverdueRunWhenTheOtherTimeChanges(t *testing.T) {
	a := newHost()
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Store.Update(func(st *store.State) error {
		st.Settings = store.Settings{Saved: true, CheckAt: "01:00", UpdateAt: "02:00"}
		return nil
	})
	s := &Schedule{Runner: r, Location: time.UTC, Wake: make(chan struct{}, 1)}
	now := time.Date(2026, 11, 19, 0, 59, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	// The 01:00 check lasts until 03:00, and meanwhile the check moves to
	// 04:00. The update run checks too, so this happens once.
	var once sync.Once
	a.onCheck = func() {
		once.Do(func() {
			mu.Lock()
			now = time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
			mu.Unlock()
			r.Store.Update(func(st *store.State) error {
				st.Settings.CheckAt = "04:00"
				return nil
			})
			s.Wake <- struct{}{}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := true
	s.now = clock
	s.sleep = func(ctx context.Context, d time.Duration) error {
		switch {
		case d <= 0:
			return nil
		case first:
			first = false
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return nil
		}
		cancel()
		return context.Canceled
	}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	var kinds []string
	for _, run := range st.Runs {
		kinds = append(kinds, run.Kind)
	}
	if strings.Join(kinds, " ") != "check update" {
		t.Fatalf("runs %v", kinds)
	}
}

// A run that waits while its time is changed does not start; the wake that
// came with the change plans it at the new time.
func TestScheduleLeavesARunWhoseTimeChanged(t *testing.T) {
	a := newHost()
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Store.Update(func(st *store.State) error {
		st.Settings = store.Settings{Saved: true, CheckAt: "01:00", UpdateAt: "01:00"}
		return nil
	})
	s := &Schedule{Runner: r, CheckAt: Clock{0, 0}, UpdateAt: Clock{1, 0}, Location: time.UTC, Wake: make(chan struct{}, 1)}
	// The check on the same minute runs first; meanwhile the update is moved
	// to 02:00, as the settings page does it.
	a.onCheck = func() {
		r.Store.Update(func(st *store.State) error {
			st.Settings.UpdateAt = "02:00"
			return nil
		})
		s.Wake <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 11, 19, 0, 59, 0, 0, time.UTC)
	var waits []time.Duration
	s.now = func() time.Time { return now }
	s.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		switch len(waits) {
		case 1:
			now = now.Add(d)
			return nil
		case 2:
			<-ctx.Done()
			return ctx.Err()
		}
		cancel()
		return context.Canceled
	}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	if len(st.Runs) != 1 || st.Runs[0].Kind != KindCheck {
		t.Fatalf("runs %+v", st.Runs)
	}
	if len(waits) != 3 || waits[2] != time.Hour {
		t.Fatalf("waits %v", waits)
	}
}

// A check-only host is checked but not updated on schedule.
func TestCheckOnlyHost(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	r.Store.Update(func(s *store.State) error {
		s.Agents[0].CheckOnly = true
		return nil
	})
	run, _ := r.Run(context.Background(), KindUpdate)
	if e := entry(t, run.Hosts[0], "web"); e.Status != "skipped" || !strings.Contains(e.Reason, "check-only") || len(a.started) != 0 {
		t.Fatalf("entry %+v", e)
	}
}

// Start refuses at once while another run goes on.
func TestStartWhileBusy(t *testing.T) {
	a := newHost()
	release := make(chan struct{})
	a.onCheck = func() { <-release }
	r := newRunner(t, map[string]*fakeHost{"a": a})
	done := make(chan struct{})
	if err := r.Start(context.Background(), Spec{Kind: KindCheck}, func(store.Run, error) { close(done) }); err != nil {
		t.Fatal(err)
	}
	if act := r.Active(); act == nil || act.Spec.Kind != KindCheck {
		t.Fatalf("active %+v", act)
	}
	if err := r.Start(context.Background(), Spec{Kind: KindCheck}, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}
	close(release)
	<-done
	if r.Active() != nil {
		t.Error("still active after the run")
	}
}

// Saved settings win over the environment, and saving wakes the loop.
func TestScheduleFollowsTheSettings(t *testing.T) {
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	s := &Schedule{Runner: r, CheckAt: Clock{0, 0}, UpdateAt: Clock{1, 0}, Location: time.UTC, Wake: make(chan struct{}, 1)}
	if c, u := s.Times(); c != (When{At: Clock{0, 0}}) || u != (When{At: Clock{1, 0}}) {
		t.Fatalf("env times %v %v", c, u)
	}
	r.Store.Update(func(st *store.State) error {
		st.Settings = store.Settings{Saved: true, CheckAt: "02:30", UpdateAt: "03:15", UpdateEvery: "sun"}
		return nil
	})
	if c, u := s.Times(); c != (When{At: Clock{2, 30}}) || u != (When{Every: Every{Weekly: true, Weekday: time.Sunday}, At: Clock{3, 15}}) {
		t.Fatalf("saved times %v %v", c, u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 1 {
			s.Wake <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		cancel()
		return context.Canceled
	}
	s.Loop(ctx)
	// Woken while waiting for 02:30 tomorrow, it read the times again.
	if len(waits) != 2 || waits[0] != 14*time.Hour+30*time.Minute {
		t.Fatalf("waits %v", waits)
	}
	if st, _ := r.Store.Read(); len(st.Runs) != 0 {
		t.Fatalf("a wake started a run: %+v", st.Runs)
	}
}

func TestEvery(t *testing.T) {
	for in, want := range map[string]string{"": "day", "day": "day", "SUN": "sun", "Wednesday": "wed", "1": "1", "30": "30", "31": "last", "last": "last", "Last Day": "last"} {
		e, err := ParseEvery(in)
		if err != nil || e.String() != want {
			t.Errorf("ParseEvery(%q) = %v, %v; want %s", in, e, err, want)
		}
	}
	for _, bad := range []string{"0", "32", "weekly", "-1"} {
		if _, err := ParseEvery(bad); err == nil {
			t.Errorf("ParseEvery(%q) took it", bad)
		}
	}
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, seoul)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tt := range []struct {
		every, now, want string
	}{
		// 2026-11-19 is a Thursday.
		{"day", "2026-11-19 12:00", "2026-11-20 01:00"},
		{"sun", "2026-11-19 12:00", "2026-11-22 01:00"},
		{"thu", "2026-11-19 00:30", "2026-11-19 01:00"},
		{"thu", "2026-11-19 01:00", "2026-11-26 01:00"},
		{"15", "2026-11-19 12:00", "2026-12-15 01:00"},
		// A shorter month runs on its last day.
		{"last", "2026-11-19 12:00", "2026-11-30 01:00"},
		{"last", "2026-11-30 02:00", "2026-12-31 01:00"},
		{"last", "2027-02-01 00:00", "2027-02-28 01:00"},
		{"30", "2027-02-01 00:00", "2027-02-28 01:00"},
		{"29", "2028-02-01 00:00", "2028-02-29 01:00"},
	} {
		e, _ := ParseEvery(tt.every)
		if got := (When{Every: e, At: Clock{1, 0}}).Next(at(tt.now), seoul); !got.Equal(at(tt.want)) {
			t.Errorf("%s from %s: %s, want %s", tt.every, tt.now, got.In(seoul).Format("2006-01-02 15:04"), tt.want)
		}
	}
	if w := (When{Every: Every{Day: LastDay}, At: Clock{1, 0}}).Words(); w != "on the last day of every month at 01:00" {
		t.Errorf("words %q", w)
	}
}

// Scheduled runs and runs from the CLI are notified; runs by hand are not.
func TestNotify(t *testing.T) {
	a := newHost()
	a.report.Containers = []protocol.Checked{checked("web", registry.UpdateAvailable, "sha256:w")}
	r := newRunner(t, map[string]*fakeHost{"a": a})
	var got []store.Run
	r.Notify = func(_ context.Context, run store.Run) {
		got = append(got, run)
		// The lock is released by now: a slow webhook holds back no run.
		if err := r.Start(context.Background(), Spec{Kind: KindCheck, Manual: "bob"}, nil); err != nil {
			t.Errorf("start during the notification: %v", err)
		}
		for r.Active() != nil {
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := r.Run(context.Background(), KindCheck); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := r.Start(context.Background(), Spec{Kind: KindCheck, Manual: "alice"}, func(store.Run, error) { close(done) }); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(got) != 1 || got[0].Manual != "" || got[0].Kind != KindCheck {
		t.Fatalf("notified %+v", got)
	}
}

// A schedule saved during a run counts from when it was saved: an update
// time still ahead then runs, though the loop saw the change only after a
// slow notification, and though the check time was saved again after it.
func TestScheduleCountsAChangeFromWhenItWasSaved(t *testing.T) {
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	r.Store.Update(func(st *store.State) error {
		st.Settings = store.Settings{Saved: true, CheckAt: "01:00", UpdateAt: "02:00"}
		return nil
	})
	s := &Schedule{Runner: r, Location: time.UTC, Wake: make(chan struct{}, 1)}
	var mu sync.Mutex
	now := time.Date(2026, 11, 19, 0, 59, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	var once sync.Once
	r.Notify = func(_ context.Context, run store.Run) {
		once.Do(func() {
			r.Store.Update(func(st *store.State) error {
				st.Settings.UpdateAt = "01:01"
				st.Settings.UpdateSavedAt = time.Date(2026, 11, 19, 1, 0, 5, 0, time.UTC)
				return nil
			})
			r.Store.Update(func(st *store.State) error {
				st.Settings.CheckAt = "05:00"
				st.Settings.CheckSavedAt = time.Date(2026, 11, 19, 1, 1, 15, 0, time.UTC)
				return nil
			})
			s.Wake <- struct{}{}
			mu.Lock()
			now = time.Date(2026, 11, 19, 1, 1, 35, 0, time.UTC)
			mu.Unlock()
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := true
	s.now = clock
	s.sleep = func(ctx context.Context, d time.Duration) error {
		switch {
		case d <= 0:
			return nil
		case first:
			first = false
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return nil
		}
		cancel()
		return context.Canceled
	}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	var kinds []string
	for _, run := range st.Runs {
		kinds = append(kinds, run.Kind)
	}
	if strings.Join(kinds, " ") != "check update" {
		t.Fatalf("runs %v", kinds)
	}
}

// The update time changed and then reset while a check's notification kept
// the loop busy is back to what the loop last saw, yet it was saved: it
// counts from the reset, so the update time it passed waits for its next
// week (R36).
func TestAScheduleResetDuringARunCountsFromTheReset(t *testing.T) {
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	ck, _ := ParseClock("01:00")
	up, _ := ParseClock("01:01")
	now := time.Date(2026, 11, 19, 0, 59, 0, 0, time.UTC)
	s := &Schedule{Runner: r, Location: time.UTC, CheckAt: ck, UpdateAt: up, UpdateEvery: Every{Weekly: true, Weekday: time.Thursday}}
	s.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sleep = func(_ context.Context, d time.Duration) error {
		if d <= 0 {
			return nil
		}
		if now.Minute() == 59 {
			now = now.Add(d)
			return nil
		}
		cancel()
		return context.Canceled
	}
	r.Notify = func(_ context.Context, run store.Run) {
		if run.Kind != KindCheck {
			return
		}
		now = now.Add(5 * time.Second)
		r.Store.Update(func(st *store.State) error {
			st.Settings = store.Settings{Saved: true, CheckAt: "01:00", UpdateAt: "02:00", UpdateEvery: "thu", UpdateSavedAt: now}
			return nil
		})
		now = now.Add(70 * time.Second)
		r.Store.Update(func(st *store.State) error {
			st.Settings = store.Settings{UpdateSavedAt: now}
			return nil
		})
	}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	if len(st.Runs) != 1 || st.Runs[0].Kind != KindCheck {
		t.Fatalf("%d runs, the last %s", len(st.Runs), st.Runs[len(st.Runs)-1].Kind)
	}
}

// The same with the check and the update on the same minute: the update
// planned with the check sees the reset before it starts, and waits for
// its next week too.
func TestAScheduleResetBeforeAPlannedRunStopsIt(t *testing.T) {
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	at, _ := ParseClock("01:00")
	now := time.Date(2026, 11, 19, 0, 59, 0, 0, time.UTC)
	s := &Schedule{Runner: r, Location: time.UTC, CheckAt: at, UpdateAt: at, UpdateEvery: Every{Weekly: true, Weekday: time.Thursday}}
	s.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sleep = func(_ context.Context, d time.Duration) error {
		if d <= 0 {
			return nil
		}
		if now.Minute() == 59 {
			now = now.Add(d)
			return nil
		}
		cancel()
		return context.Canceled
	}
	r.Notify = func(_ context.Context, run store.Run) {
		if run.Kind != KindCheck {
			return
		}
		now = now.Add(5 * time.Second)
		r.Store.Update(func(st *store.State) error {
			st.Settings = store.Settings{Saved: true, CheckAt: "01:00", UpdateAt: "02:00", UpdateEvery: "thu", UpdateSavedAt: now}
			return nil
		})
		now = now.Add(70 * time.Second)
		r.Store.Update(func(st *store.State) error {
			st.Settings = store.Settings{UpdateSavedAt: now}
			return nil
		})
	}
	s.Loop(ctx)
	st, _ := r.Store.Read()
	if len(st.Runs) != 1 || st.Runs[0].Kind != KindCheck {
		t.Fatalf("%d runs, the last %s", len(st.Runs), st.Runs[len(st.Runs)-1].Kind)
	}
}

// A schedule saved in the web UI replaces the environment's whole: an
// empty period in it is every day.
func TestASavedScheduleWithoutAPeriodIsDaily(t *testing.T) {
	r := newRunner(t, map[string]*fakeHost{"a": newHost()})
	s := &Schedule{Runner: r, UpdateEvery: Every{Weekly: true, Weekday: time.Sunday}, Location: time.UTC}
	r.Store.Update(func(st *store.State) error {
		st.Settings = store.Settings{Saved: true, CheckAt: "00:00", UpdateAt: "01:00"}
		return nil
	})
	if _, u := s.Times(); u.Every != (Every{}) {
		t.Fatalf("update every %v", u.Every)
	}
}
