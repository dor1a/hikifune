package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/auth"
	"github.com/dor1a/hikifune/internal/auth/authtest"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/notify"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/server"
	"github.com/dor1a/hikifune/internal/store"
)

type fakeRunner struct {
	mu     sync.Mutex
	specs  []server.Spec
	busy   bool
	active *server.Activity
}

func (f *fakeRunner) Start(_ context.Context, spec server.Spec, _ func(store.Run, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return server.ErrBusy
	}
	f.specs = append(f.specs, spec)
	f.active = &server.Activity{Spec: spec, Started: time.Now()}
	return nil
}

func (f *fakeRunner) Active() *server.Activity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

type fakeSchedule struct {
	check, update server.When
	planned       map[string]server.Planned
}

func (f fakeSchedule) Times() (server.When, server.When) { return f.check, f.update }

func (f fakeSchedule) Planned(kind string) (server.Planned, bool) {
	p, ok := f.planned[kind]
	return p, ok
}

func (f fakeSchedule) TimesFor(set store.Settings) (server.When, server.When) {
	return server.Saved(set, f.check, f.update)
}

var t0 = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)

// fixture is a state with two agents: a full check of each, then an update
// by hand of one container on a.
func fixture(t *testing.T) *store.JSON {
	t.Helper()
	st, err := store.OpenJSON(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *store.State) error {
		s.Agents = []store.Agent{{Name: "a", URL: "http://a:7778", Token: "t"}, {Name: "b", URL: "http://b:7778", Token: "t", CheckOnly: true}}
		s.Runs = []store.Run{
			{ID: "r1", Kind: "check", Started: t0, Finished: t0.Add(time.Minute), Hosts: []store.HostRun{
				{Agent: "a", AgentVersion: "v1", Docker: "29.1.0", Containers: []store.Entry{
					{Container: "web", ID: "id-web", Image: "nginx:1", State: "running", Status: "update_available", RemoteDigest: "sha256:w", Available: true},
					{Container: "guard", ID: "id-guard", Image: "cloudflared:1", State: "running", Status: "update_available", RemoteDigest: "sha256:g", Available: true, Protected: true},
					{Container: "hikifune-agent", ID: "id-agent", Image: "ghcr.io/dor1a/hikifune-agent:1", State: "running", Status: "update_available", RemoteDigest: "sha256:s", Available: true, Self: true},
					{Container: "db", ID: "id-db", Image: "postgres:18", State: "running", Status: "up_to_date"},
					{Container: "job", ID: "id-job", Image: "busybox:1", State: "exited", Status: "update_available", RemoteDigest: "sha256:j", Available: true},
				}},
				{Agent: "b", Skipped: "the agent runs v0 and the server v1; run the same version on both"},
			}},
			{ID: "r2", Kind: "update", Manual: "alice", Container: "web", Started: t0.Add(time.Hour), Finished: t0.Add(time.Hour + time.Minute),
				Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{
					{Container: "web", ID: "id-web", Image: "nginx:1", State: "running", Status: "updated", RemoteDigest: "sha256:w", Available: true},
				}}}},
		}
		return nil
	})
	return st
}

func newServer(t *testing.T, st store.Store, r Runner, a *auth.Auth) (*Server, chan struct{}) {
	t.Helper()
	wake := make(chan struct{}, 1)
	check, upd := server.When{At: server.Clock{Hour: 0, Minute: 0}}, server.When{At: server.Clock{Hour: 1, Minute: 0}}
	s, err := New(Config{Store: st, Runner: r, Schedule: fakeSchedule{check: check, update: upd}, Wake: wake,
		Location: time.UTC, DefaultCheck: check, DefaultUpdate: upd, Watch: 2 * time.Minute,
		Auth: a, Version: "v1",
		Status: func(_ context.Context, a store.Agent) (protocol.Version, error) {
			if a.Name == "b" {
				return protocol.Version{}, errors.New("connection refused")
			}
			return protocol.Version{Agent: "v1", Docker: &inventory.Docker{Version: "29.1.0", APIVersion: "1.52"}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return t0.Add(2 * time.Hour) }
	return s, wake
}

func do(t *testing.T, h http.Handler, method, target string, form url.Values) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func TestContainersShowsEveryContainer(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	res, body := do(t, s.Handler(), "GET", "/containers", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for _, want := range []string{
		">web<", ">guard ", ">db<", ">hikifune-agent ", ">job<",
		// The update by hand is newer than the check.
		`class="badge good">Updated`,
		// Protected and hikifune itself are marked.
		`title="hikifune.protected`, `<span class="tag">hikifune</span>`,
		// Update now asks first for a protected container.
		`hx-confirm="guard is protected`,
		// The left-out host says why, in its section.
		`<p class="msg bad">the agent runs v0`,
		"5 of 5 containers",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	// Update now is offered for guard only: web is updated, db is up to
	// date, the agent is hikifune itself and job does not run.
	if n := strings.Count(body, `name="container" value=`); n != 1 || !strings.Contains(body, `name="container" value="guard"><input type="hidden" name="id" value="id-guard">`) {
		t.Errorf("Update now offered %d times", n)
	}
	// By host: a section for each host, the left-out one too; the sidebar
	// lists both, with a's count of pending updates.
	for _, want := range []string{`<h2><a href="/hosts/a">a</a></h2>`, `<h2><a href="/hosts/b">b</a></h2>`,
		`<a href="/hosts/a" title="3 with an update">`, `<span class="dot bad"`, `id="mode-toggle"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %q", want)
		}
	}
	// One list: a single table with the host column.
	if _, body := do(t, s.Handler(), "GET", "/containers?view=list", nil); strings.Count(body, "<table") != 1 || !strings.Contains(body, `<th class="c-host">Host</th>`) ||
		!strings.Contains(body, "b: the agent runs v0") {
		t.Errorf("one list: %d tables", strings.Count(body, "<table"))
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("no security headers")
	}
}

func TestContainersFilters(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	h := s.Handler()
	for q, want := range map[string]string{
		"/containers?status=up_to_date": "1 of 5", "/containers?protected=1": "1 of 5", "/containers?q=POSTGRES": "1 of 5", "/containers?host=b": "0 of 5",
		// Every container whose newer digest is not on it yet.
		"/containers?status=update_available": "3 of 5", "/containers?status=update_available&view=list": "3 of 5",
	} {
		if _, body := do(t, h, "GET", q, nil); !strings.Contains(body, want+" containers") {
			t.Errorf("%s: no %q", q, want)
		}
	}
}

func TestEmptyStates(t *testing.T) {
	st, _ := store.OpenJSON(t.TempDir(), 0)
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	if _, body := do(t, s.Handler(), "GET", "/containers", nil); !strings.Contains(body, "hikifune agent add") {
		t.Error("no agents: no hint")
	}
	st.Update(func(s *store.State) error {
		s.Agents = []store.Agent{{Name: "a", URL: "http://a:7778"}}
		return nil
	})
	if _, body := do(t, s.Handler(), "GET", "/containers", nil); !strings.Contains(body, "No check has run yet") {
		t.Error("no runs: no hint")
	}
}

func TestActions(t *testing.T) {
	r := &fakeRunner{}
	s, _ := newServer(t, fixture(t), r, nil)
	h := s.Handler()
	_, body := do(t, h, "POST", "/actions/update", url.Values{"host": {"a"}, "container": {"guard"}, "id": {"id-guard"}})
	if len(r.specs) != 1 || r.specs[0].Kind != "update" || r.specs[0].Container != "guard" || r.specs[0].ContainerID != "id-guard" ||
		r.specs[0].Hosts[0] != "a" || r.specs[0].Manual != "web" || !strings.Contains(body, "Updating guard on a") {
		t.Fatalf("specs %+v, body %s", r.specs, body)
	}
	// While it runs the strip asks again; when it ends the page reloads.
	if res, body := do(t, h, "GET", "/activity?following=1", nil); res.Header.Get("HX-Refresh") != "" || !strings.Contains(body, "every 2s") {
		t.Errorf("busy strip: %s", body)
	}
	r.active = nil
	if res, _ := do(t, h, "GET", "/activity?following=1", nil); res.Header.Get("HX-Refresh") != "true" {
		t.Error("the page was not reloaded after the run")
	}

	do(t, h, "POST", "/actions/check", url.Values{"host": {"b"}})
	if len(r.specs) != 2 || r.specs[1].Kind != "check" || len(r.specs[1].Hosts) != 1 || r.specs[1].Hosts[0] != "b" {
		t.Fatalf("specs %+v", r.specs)
	}
	r.busy, r.active = true, nil
	if _, body := do(t, h, "POST", "/actions/check", url.Values{}); !strings.Contains(body, "Another run is going on") {
		t.Errorf("busy: %s", body)
	}
	// Refused while a run shows in the menu, it says so under the run.
	r.active = &server.Activity{Spec: server.Spec{Kind: "check", Manual: "alice"}, Started: t0}
	if _, body := do(t, h, "POST", "/actions/check", url.Values{}); !strings.Contains(body, "Another run is going on") ||
		!strings.Contains(body, "started by alice") || !strings.Contains(body, "every 2s") {
		t.Errorf("busy with a run shown: %s", body)
	}
	r.active = nil
	// Idle, the menu asks now and then with the latest run it has shown, so
	// a run started elsewhere shows, even one that ended between two asks.
	if _, body := do(t, h, "GET", "/runs", nil); !strings.Contains(body, `hx-get="/activity?seen=r2&amp;page=runs" hx-trigger="every 10s"`) {
		t.Errorf("idle menu: %s", body)
	}
	if res, _ := do(t, h, "GET", "/activity?seen=r2&page=runs", nil); res.Header.Get("HX-Refresh") != "" {
		t.Error("reloaded with no new run")
	}
	if res, _ := do(t, h, "GET", "/activity?seen=r1&page=runs", nil); res.Header.Get("HX-Refresh") != "true" {
		t.Error("a run that ended between two asks did not reload the page")
	}
	// Settings does not reload, which would drop the edits: it says so and
	// keeps asking with what it saw, so the note stays.
	if res, body := do(t, h, "GET", "/activity?seen=r1&page=settings", nil); res.Header.Get("HX-Refresh") != "" ||
		!strings.Contains(body, "A run ended.") || !strings.Contains(body, "seen=r1&amp;page=settings") {
		t.Errorf("settings: %v\n%s", res.Header, body)
	}
	// A host removed since the page was drawn is not run.
	if _, body := do(t, h, "POST", "/actions/check", url.Values{"host": {"gone"}}); !strings.Contains(body, "There is no host named gone now") {
		t.Errorf("a removed host: %s", body)
	}
	// A scheduled run says so where a run by hand names who started it.
	r.active = &server.Activity{Spec: server.Spec{Kind: "update"}, Started: t0}
	if _, body := do(t, h, "GET", "/activity", nil); !strings.Contains(body, "on schedule, since") {
		t.Errorf("scheduled run: %s", body)
	}
	r.active = nil
	if res, _ := do(t, h, "POST", "/actions/update", url.Values{"host": {"a"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("update without a container: %d", res.StatusCode)
	}
	if res, _ := do(t, h, "POST", "/actions/update", url.Values{"host": {"a"}, "container": {"guard"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("update without the shown ID: %d", res.StatusCode)
	}
}

func TestSettings(t *testing.T) {
	st := fixture(t)
	s, wake := newServer(t, st, &fakeRunner{}, nil)
	// The real schedule, which reads what the page saves.
	s.cfg.Schedule = &server.Schedule{Runner: &server.Runner{Store: st}, UpdateAt: server.Clock{Hour: 1}, Location: time.UTC}
	h := s.Handler()
	_, body := do(t, h, "GET", "/settings", nil)
	if !strings.Contains(body, "From the environment: check every day at 00:00, update every day at 01:00") ||
		!strings.Contains(body, `<option value="day" selected>Every day</option>`) ||
		!strings.Contains(body, `<span class="when-field off" data-for="week">`) || !strings.Contains(body, `<option value="sat">Saturday</option>`) {
		t.Errorf("settings page: %s", body)
	}
	st.Update(func(s *store.State) error {
		s.Settings.Template = "{{.Host}}"
		return nil
	})
	res, _ := do(t, h, "POST", "/settings", url.Values{"check_at": {"02:30"}, "update_at": {"03:00"},
		"check_period": {"week"}, "check_weekday": {"sun"}, "check_day": {"1"}, "update_period": {"month"}, "update_weekday": {"sun"}, "update_day": {"last day"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", res.StatusCode)
	}
	if got, _ := st.Read(); !got.Settings.Saved || got.Settings.CheckAt != "02:30" || got.Settings.UpdateAt != "03:00" ||
		got.Settings.CheckEvery != "sun" || got.Settings.UpdateEvery != "last" || got.Settings.Template != "{{.Host}}" ||
		!got.Settings.CheckSavedAt.Equal(t0.Add(2*time.Hour)) || !got.Settings.UpdateSavedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("settings %+v", got.Settings)
	}
	// Saving the check time alone leaves the update's count alone, so an
	// update time saved just before still runs (R6).
	s.now = func() time.Time { return t0.Add(3 * time.Hour) }
	do(t, h, "POST", "/settings", url.Values{"check_at": {"04:00"}, "update_at": {"03:00"},
		"check_period": {"week"}, "check_weekday": {"sun"}, "update_period": {"month"}, "update_day": {"last day"}})
	if got, _ := st.Read(); got.Settings.CheckAt != "04:00" || !got.Settings.CheckSavedAt.Equal(t0.Add(3*time.Hour)) ||
		!got.Settings.UpdateSavedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("check saved alone: %+v", got.Settings)
	}
	<-wake
	do(t, h, "POST", "/settings", url.Values{"check_at": {"02:30"}, "update_at": {"03:00"},
		"check_period": {"week"}, "check_weekday": {"sun"}, "update_period": {"month"}, "update_day": {"last day"}})
	// The saved period's field shows; the other stays hidden.
	if _, body := do(t, h, "GET", "/settings", nil); !strings.Contains(body, `<span class="when-field" data-for="week"><span class="muted">on</span>`) ||
		!strings.Contains(body, `<option value="sun" selected>Sunday</option>`) || !strings.Contains(body, `name="update_day" value="Last day"`) {
		t.Errorf("saved schedule not shown")
	}
	// A day past 30 is the last day, as the page's script shows it.
	if res, _ := do(t, h, "POST", "/settings", url.Values{"check_at": {"02:30"}, "update_at": {"03:00"},
		"check_period": {"week"}, "check_weekday": {"sun"}, "update_period": {"month"}, "update_day": {"45"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("day 45: %d", res.StatusCode)
	}
	if got, _ := st.Read(); got.Settings.UpdateEvery != "last" {
		t.Errorf("day 45 saved as %q", got.Settings.UpdateEvery)
	}
	for _, day := range []string{"0", "abc", "12x", "-3", "+1", "+31", "Lasting"} {
		if res, body := do(t, h, "POST", "/settings", url.Values{"check_at": {"02:30"}, "update_at": {"03:00"}, "update_period": {"month"}, "update_day": {day}}); res.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(body, `value="`+strings.ReplaceAll(day, "+", "&#43;")+`"`) {
			t.Errorf("day %q: %d", day, res.StatusCode)
		}
	}
	select {
	case <-wake:
	default:
		t.Error("the schedule was not woken")
	}
	if res, body := do(t, h, "POST", "/settings", url.Values{"check_at": {"25:00"}, "update_at": {"03:00"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not HH:MM") {
		t.Errorf("bad time: %d", res.StatusCode)
	}
	do(t, h, "POST", "/settings", url.Values{"reset": {"1"}})
	if got, _ := st.Read(); got.Settings.Saved || got.Settings.CheckEvery != "" || got.Settings.Template != "{{.Host}}" {
		t.Errorf("reset: %+v", got.Settings)
	}
}

// The notification template is edited, checked and tried in Settings; the
// webhook shows without its token.
func TestNotificationSettings(t *testing.T) {
	st := fixture(t)
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	h := s.Handler()
	if _, body := do(t, h, "GET", "/settings", nil); !strings.Contains(body, "Not set, so no notifications are sent") {
		t.Error("no webhook: no hint")
	}
	if _, body := do(t, h, "POST", "/settings/template/test", url.Values{"template": {""}}); !strings.Contains(body, "No webhook is set") {
		t.Errorf("test without a webhook: %s", body)
	}

	s.cfg.Notifier = &notify.Notifier{Discord: &notify.Discord{URL: hook.URL + "/api/webhooks/42/secret-token"}}
	_, body := do(t, h, "GET", "/settings", nil)
	if !strings.Contains(body, "/api/webhooks/42/…") || strings.Contains(body, "secret-token") || !strings.Contains(body, `<span class="tag">Default</span>`) {
		t.Errorf("settings page: webhook or default not shown right")
	}
	if res, body := do(t, h, "POST", "/settings/template", url.Values{"template": {"{{.Nope}}"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Nope") {
		t.Errorf("bad template: %d", res.StatusCode)
	}
	do(t, h, "POST", "/settings/template", url.Values{"template": {"{{.Kind}} on {{.Host}}\r\n"}})
	if got, _ := st.Read(); got.Settings.Template != "{{.Kind}} on {{.Host}}\n" {
		t.Errorf("saved template %q", got.Settings.Template)
	}
	if _, body := do(t, h, "POST", "/settings/template/test", url.Values{"template": {"test {{.Host}}"}}); !strings.Contains(body, "Sent a sample message") {
		t.Errorf("send test: %s", body)
	}
	mu.Lock()
	// By default the hosts of a run share a message, so the test sends two
	// sample hosts in one embed, each under its name.
	if len(posts) != 1 || strings.Count(posts[0], `"title"`) != 1 || !strings.Contains(posts[0], `"title":"Update run"`) ||
		!strings.Contains(posts[0], `\n🖥 **example-host**\n`) || !strings.Contains(posts[0], `\n\ntest example-host\n\n──────────────────────────────\n🖥 **example-host-2**\n`) ||
		!strings.Contains(posts[0], `\n\ntest example-host-2"`) {
		t.Errorf("posts %v", posts)
	}
	posts = nil
	mu.Unlock()
	if _, body := do(t, h, "POST", "/settings/template/test", url.Values{"template": {"test {{.Host}}"}, "messages": {"host"}}); !strings.Contains(body, "Sent a sample message for example-host.") {
		t.Errorf("send test per host: %s", body)
	}
	mu.Lock()
	if len(posts) != 1 || strings.Contains(posts[0], "example-host-2") {
		t.Errorf("posts per host %v", posts)
	}
	posts = nil
	mu.Unlock()
	// Send test sends the sample the preview shows.
	do(t, h, "POST", "/settings/template/test", url.Values{"template": {"{{.Kind}}"}, "sample": {"1"}})
	mu.Lock()
	if len(posts) != 1 || !strings.Contains(posts[0], `"title":"Check run"`) || !strings.Contains(posts[0], `\n\ncheck\n\n`) {
		t.Errorf("send test of the check sample %v", posts)
	}
	mu.Unlock()
	// The choice is saved with the template, and shown again.
	do(t, h, "POST", "/settings/template", url.Values{"template": {"{{.Kind}} on {{.Host}}"}, "messages": {"host"}})
	if got, _ := st.Read(); !got.Settings.MessagePerHost {
		t.Error("one message per host was not saved")
	}
	if _, body := do(t, h, "GET", "/settings", nil); !strings.Contains(body, `<option value="host" selected>A message for each host</option>`) {
		t.Error("one message per host not shown")
	}
	do(t, h, "POST", "/settings/template", url.Values{"template": {"{{.Kind}} on {{.Host}}"}, "messages": {"run"}})
	// Saving the default, or asking for it, keeps the template empty.
	do(t, h, "POST", "/settings/template", url.Values{"template": {notify.Default}})
	if got, _ := st.Read(); got.Settings.Template != "" {
		t.Errorf("default saved as %q", got.Settings.Template)
	}
}

func TestHosts(t *testing.T) {
	st := fixture(t)
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	h := s.Handler()
	_, body := do(t, h, "GET", "/hosts", nil)
	for _, want := range []string{`hx-get="/hosts/a/status"`, "5 containers, 3 with an update", `<span class="tag warn">Check only</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("hosts page has no %q", want)
		}
	}
	if _, body := do(t, h, "GET", "/hosts/a/status", nil); !strings.Contains(body, "Docker 29.1.0, API 1.52") {
		t.Errorf("status a: %s", body)
	}
	if _, body := do(t, h, "GET", "/hosts/b/status", nil); !strings.Contains(body, "connection refused") {
		t.Errorf("status b: %s", body)
	}
	if res, _ := do(t, h, "GET", "/hosts/c/status", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host: %d", res.StatusCode)
	}
	do(t, h, "POST", "/hosts/a/check-only", url.Values{"on": {"1"}})
	if got, _ := st.Read(); !got.Agents[0].CheckOnly {
		t.Error("check-only not saved")
	}

	// A host's own page.
	_, body = do(t, h, "GET", "/hosts/a", nil)
	for _, want := range []string{"Check this host", `name="back" value="host"`, `<a href="/hosts/a" aria-current="page"`,
		`name="container" value="guard"`, "Turn updates on"} {
		if !strings.Contains(body, want) {
			t.Errorf("host page has no %q", want)
		}
	}
	if strings.Contains(body, `<th class="c-host">Host</th>`) {
		t.Error("host page has a host column")
	}
	if res, _ := do(t, h, "GET", "/hosts/c", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host page: %d", res.StatusCode)
	}
	if res, _ := do(t, h, "POST", "/hosts/a/check-only", url.Values{"on": {"0"}, "back": {"host"}}); res.Header.Get("Location") != "/hosts/a?saved=1" {
		t.Errorf("back to the host page: %q", res.Header.Get("Location"))
	}
}

// The container list follows what the runs say.
func TestViewFollowsTheRuns(t *testing.T) {
	list := func(id string, at time.Time, byHand string, hosts ...store.HostRun) store.Run {
		return store.Run{ID: id, Kind: "check", Container: byHand, Started: at, Hosts: hosts}
	}
	e := func(name, status string, available bool) store.Entry {
		return store.Entry{Container: name, ID: "id-" + name, State: "running", Status: status, Available: available, RemoteDigest: "sha256:" + name}
	}
	names := func(hv hostView) string {
		var n []string
		for _, r := range hv.Rows {
			n = append(n, r.Container)
		}
		return strings.Join(n, " ")
	}
	t1 := t0.Add(time.Hour)

	// A check that found no containers is a list too.
	st := store.State{Agents: []store.Agent{{Name: "a"}}, Runs: []store.Run{
		list("1", t0, "", store.HostRun{Agent: "a", Containers: []store.Entry{e("old-web", "up_to_date", false)}}),
		list("2", t1, "", store.HostRun{Agent: "a"}),
	}}
	if hv := view(st)[0]; len(hv.Rows) != 0 || !hv.AsOf.Equal(t1) {
		t.Errorf("empty list: %q as of %v", names(hv), hv.AsOf)
	}
	// A failed check keeps the last list and says why.
	st.Runs[1].Hosts[0].Error = "check: x"
	if hv := view(st)[0]; names(hv) != "old-web" || hv.Issue != "check: x" {
		t.Errorf("failed check: %q, %q", names(hv), hv.Issue)
	}
	// A newer update by hand adds its container to the list.
	st.Runs[1] = list("2", t1, "new-web", store.HostRun{Agent: "a", Containers: []store.Entry{e("new-web", "updated", true)}})
	if hv := view(st)[0]; names(hv) != "old-web new-web" {
		t.Errorf("by hand: %q", names(hv))
	}
	// Runs from before the agent was added belong to another host.
	st.Agents[0].Added = t1.Add(time.Minute)
	if hv := view(st)[0]; len(hv.Rows) != 0 || !hv.AsOf.IsZero() {
		t.Errorf("added later: %q", names(hv))
	}

	// A newer digest stays pending whatever the run did with it, until it
	// is on the container.
	st = store.State{Agents: []store.Agent{{Name: "a"}}, Runs: []store.Run{
		list("1", t0, "", store.HostRun{Agent: "a", Containers: []store.Entry{
			e("skipped", "skipped", true), e("back", "rolled_back", true), e("done", "updated", true), e("same", "up_to_date", false)}}),
	}}
	hv := view(st)[0]
	for _, r := range hv.Rows {
		want := r.Container == "skipped" || r.Container == "back"
		if r.Pending != want || r.CanUpdate != want {
			t.Errorf("%s: pending %v, can update %v", r.Container, r.Pending, r.CanUpdate)
		}
	}
	if hv.Available != 2 {
		t.Errorf("available %d", hv.Available)
	}

	// A record from before the available and id fields is pending by its
	// status; with no ID to send, Update now waits for the next check.
	st.Runs[0].Hosts[0].Containers = []store.Entry{{Container: "old", State: "running", Status: "update_available", RemoteDigest: "sha256:o"}}
	if r := view(st)[0].Rows[0]; !r.Pending || r.CanUpdate {
		t.Errorf("old record: pending %v, can update %v", r.Pending, r.CanUpdate)
	}
}

// The history shows the last runs only, though the store keeps an older
// one for a host's container list.
func TestRunsShowTheHistoryOnly(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	s.cfg.KeepRuns = 1
	_, body := do(t, s.Handler(), "GET", "/runs", nil)
	if strings.Contains(body, `href="/runs/r1"`) || !strings.Contains(body, `href="/runs/r2"`) {
		t.Errorf("runs page: %s", body)
	}
}

// A name from before the naming rule still makes working links.
func TestHostNameWithASlash(t *testing.T) {
	st := fixture(t)
	st.Update(func(s *store.State) error {
		s.Agents[0].Name = "rack/a"
		for i := range s.Runs {
			for j := range s.Runs[i].Hosts {
				if s.Runs[i].Hosts[j].Agent == "a" {
					s.Runs[i].Hosts[j].Agent = "rack/a"
				}
			}
		}
		return nil
	})
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	h := s.Handler()
	_, body := do(t, h, "GET", "/hosts", nil)
	if !strings.Contains(body, `href="/hosts/rack%2fa"`) && !strings.Contains(body, `href="/hosts/rack%2Fa"`) {
		t.Fatalf("hosts page links: %s", body)
	}
	if res, body := do(t, h, "GET", "/hosts/rack%2Fa", nil); res.StatusCode != http.StatusOK || !strings.Contains(body, "Check this host") {
		t.Errorf("host page: %d", res.StatusCode)
	}
	if res, _ := do(t, h, "POST", "/hosts/rack%2Fa/check-only", url.Values{"on": {"1"}, "back": {"host"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("check-only: %d", res.StatusCode)
	}
	if got, _ := st.Read(); !got.Agents[0].CheckOnly {
		t.Error("check-only not saved")
	}
}

func TestRuns(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	h := s.Handler()
	_, body := do(t, h, "GET", "/runs", nil)
	if !strings.Contains(body, `href="/runs/r2"`) || strings.Index(body, "/runs/r2") > strings.Index(body, "/runs/r1") ||
		!strings.Contains(body, "Update of web") || !strings.Contains(body, "By alice") || !strings.Contains(body, "1 host left out") {
		t.Errorf("runs page: %s", body)
	}
	_, body = do(t, h, "GET", "/runs/r1", nil)
	if !strings.Contains(body, "Left out: the agent runs v0") || !strings.Contains(body, "Docker 29.1.0") {
		t.Errorf("run page: %s", body)
	}
	if res, _ := do(t, h, "GET", "/runs/nope", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run: %d", res.StatusCode)
	}
}

// With sign-in on, every page but /healthz and the static files needs a
// session, and a signed-in user's name goes on runs started by hand.
func TestSignIn(t *testing.T) {
	p := authtest.New(t)
	const base = "https://hikifune.example.com"
	a := auth.New(auth.Settings{PublicURL: base, Issuer: p.URL, ClientID: authtest.ClientID, ClientSecret: "s",
		Group: "hikifune-users", GroupsClaim: "groups", Scopes: []string{"openid", "profile", "email"},
		SessionKey: []byte(strings.Repeat("k", 32))}, nil)
	r := &fakeRunner{}
	s, _ := newServer(t, fixture(t), r, a)
	h := s.Handler()
	for _, path := range []string{"/", "/hosts", "/settings"} {
		if res, _ := do(t, h, "GET", path, nil); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
			t.Errorf("%s without a session: %d %s", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if res, _ := do(t, h, "POST", "/actions/check", url.Values{}); res.StatusCode != http.StatusSeeOther || len(r.specs) != 0 {
		t.Errorf("action without a session: %d", res.StatusCode)
	}
	for _, path := range []string{"/healthz", "/static/style.css"} {
		if res, _ := do(t, h, "GET", path, nil); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	cookies, res := p.SignIn(t, h, base, "/hosts")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/hosts" || len(cookies) == 0 {
		t.Fatalf("sign-in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	req := httptest.NewRequest("POST", "/actions/check", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(r.specs) != 1 || r.specs[0].Manual != "Test User" {
		t.Fatalf("specs %+v", r.specs)
	}
}

// The sidebar shows the signed-in user's initial, and their Gravatar only with
// HIKIFUNE_GRAVATAR on, which also lets the page load it.
func TestGravatar(t *testing.T) {
	const base = "https://hikifune.example.com"
	const img = `<img src="https://gravatar.com/avatar/b4c9a289323b21a01c3e940f150eb9b8c542587f1abfd8f0e1cc1ffc5e475514?s=64&amp;d=blank" alt="" width="28" height="28">`
	for _, on := range []bool{false, true} {
		p := authtest.New(t)
		a := auth.New(auth.Settings{PublicURL: base, Issuer: p.URL, ClientID: authtest.ClientID, ClientSecret: "s",
			Group: "hikifune-users", GroupsClaim: "groups", Scopes: []string{"openid", "profile", "email"},
			SessionKey: []byte(strings.Repeat("k", 32))}, nil)
		s, _ := newServer(t, fixture(t), &fakeRunner{}, a)
		s.cfg.Gravatar = on
		h := s.Handler()
		cookies, _ := p.SignIn(t, h, base, "/hosts")
		req := httptest.NewRequest("GET", "/hosts", nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, csp := rec.Body.String(), rec.Header().Get("Content-Security-Policy")
		if rec.Code != http.StatusOK || !strings.Contains(body, `<span class="face" aria-hidden="true">T`) || !strings.Contains(body, `<span class="name">Test User</span>`) {
			t.Fatalf("gravatar %v: %d, no initial and name:\n%s", on, rec.Code, body)
		}
		if strings.Contains(body, img) != on || strings.Contains(body, "gravatar.com") != on {
			t.Errorf("gravatar %v: the image is wrong:\n%s", on, body)
		}
		if strings.Contains(csp, "img-src 'self' data: https://gravatar.com;") != on || !strings.Contains(csp, "img-src 'self' data:") {
			t.Errorf("gravatar %v: CSP %q", on, csp)
		}
	}
}

// An address of blanks alone is no address; one with blanks around it is
// trimmed and lowered first.
func TestAvatar(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	if got := s.avatar("user@example.com"); got != "" {
		t.Errorf("Gravatar off: %q", got)
	}
	s.cfg.Gravatar = true
	for _, blank := range []string{"", "  \t "} {
		if got := s.avatar(blank); got != "" {
			t.Errorf("%q: %q", blank, got)
		}
	}
	if got := s.avatar("  USER@Example.COM  "); got != "https://gravatar.com/avatar/b4c9a289323b21a01c3e940f150eb9b8c542587f1abfd8f0e1cc1ffc5e475514?s=64&d=blank" {
		t.Errorf("trimmed: %q", got)
	}
}

func TestInitial(t *testing.T) {
	for in, want := range map[string]string{"dor1": "D", " 9lives": "9", "_x": "X", "ärger": "Ä", "": "?", "--": "?"} {
		if got := initial(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// A save shows its success message beside the button it came from, marked to
// fade out; without saved= in the address there is none.
func TestSavedMessages(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	h := s.Handler()
	for _, target := range []string{"/settings?saved=1", "/settings?saved=template", "/hosts?saved=a", "/hosts/a?saved=1"} {
		res, body := do(t, h, "GET", target, nil)
		if res.StatusCode != http.StatusOK || strings.Count(body, `class="msg flash"`) != 1 {
			t.Errorf("%s: %d, %d flash messages", target, res.StatusCode, strings.Count(body, `class="msg flash"`))
		}
	}
	for _, target := range []string{"/settings", "/hosts", "/hosts/a"} {
		if _, body := do(t, h, "GET", target, nil); strings.Contains(body, `class="msg flash"`) {
			t.Errorf("%s shows a saved message", target)
		}
	}
	// The schedule's and the template's messages sit in their forms' buttons.
	_, body := do(t, h, "GET", "/settings?saved=1", nil)
	if !regexp.MustCompile(`<button type="submit" class="primary">Save</button>\s*(<button[^>]*>[^<]*</button>\s*)?<span class="msg flash" role="status">Saved\. The schedule`).MatchString(body) {
		t.Errorf("the schedule's message is not beside Save:\n%s", body)
	}
	_, body = do(t, h, "GET", "/settings?saved=template", nil)
	if !regexp.MustCompile(`<span class="msg flash" role="status">Saved\. The next notification uses this template\.</span>\s*<span id="test-result"`).MatchString(body) {
		t.Errorf("the template's message is not beside Save:\n%s", body)
	}
}

// A cross-site form post is refused.
func TestCrossSitePost(t *testing.T) {
	r := &fakeRunner{}
	s, _ := newServer(t, fixture(t), r, nil)
	req := httptest.NewRequest("POST", "/actions/check", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(r.specs) != 0 {
		t.Fatalf("cross-site post: %d, specs %+v", rec.Code, r.specs)
	}
}

// Any number past 30, however long, is the last day; the rest is left for
// ParseEvery to accept or refuse.
func TestLastDay(t *testing.T) {
	for in, want := range map[string]string{
		"45": "last", "100": "last", "0301": "last", "99999999999999999999": "last", " 31 ": "last",
		"30": "30", "030": "030", "7": "7", "0": "0", "00": "00", "abc": "abc", "12x": "12x", "": "", "last day": "last day",
	} {
		if got := lastDay(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
