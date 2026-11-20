// Package web serves the server's pages (M5): every container on every host,
// the hosts, the run history and the settings, with Check now and Update now.
// Pages are rendered on the server and use HTMX for the actions and for the
// activity strip, which follows a run while it goes and reloads the page when
// it ends.
package web

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dor1a/hikifune/internal/auth"
	"github.com/dor1a/hikifune/internal/notify"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/server"
	"github.com/dor1a/hikifune/internal/store"
	"github.com/dor1a/hikifune/internal/update"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Runner starts runs and tells which one is going on.
type Runner interface {
	Start(ctx context.Context, spec server.Spec, done func(store.Run, error)) error
	Active() *server.Activity
}

// Schedule gives the check and update schedules in force, and those that
// would be with other settings saved.
type Schedule interface {
	Times() (check, update server.When)
	TimesFor(set store.Settings) (check, update server.When)
}

// Config is what the pages need.
type Config struct {
	Store    store.Store
	Runner   Runner
	Schedule Schedule
	// Wake tells the schedule that the settings changed.
	Wake chan<- struct{}
	// Location is the time zone of the schedule and of every time shown.
	Location *time.Location
	// DefaultCheck and DefaultUpdate are the schedules from the environment.
	DefaultCheck, DefaultUpdate server.When
	Watch                       time.Duration
	// Auth puts the pages behind sign-in; nil leaves access to a proxy (R33).
	Auth    *auth.Auth
	Version string
	// Base is the context runs started here run under.
	Base context.Context
	// Status asks one agent for its version, for the hosts page.
	Status func(ctx context.Context, a store.Agent) (protocol.Version, error)
	// Notifier sends Send test messages and shows which webhook is used;
	// nil when no webhook URL is set (R17).
	Notifier *notify.Notifier
	// KeepRuns is how many runs the history shows; the store may keep
	// older ones for the container lists. 0 shows every run.
	KeepRuns int
	Log      *slog.Logger
}

// Server renders the pages.
type Server struct {
	cfg Config
	tpl *template.Template
	now func() time.Time
}

// statusTimeout bounds the live look at one agent on the hosts page.
const statusTimeout = 5 * time.Second

// New parses the templates.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Base == nil {
		cfg.Base = context.Background()
	}
	s := &Server{cfg: cfg, now: time.Now}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"asset":  func(name string) string { return "/static/" + name + "?v=" + cfg.Version },
		"local":  s.local,
		"ago":    s.ago,
		"short":  shortDigest,
		"status": statusClass,
		"words":  func(s string) string { return strings.ReplaceAll(s, "_", " ") },
		"bytes":  humanBytes,
		"rows":   func(rows []row, host bool) rowTable { return rowTable{Rows: rows, Host: host} },
		"when": func(label, name string, f whenForm) whenRow {
			return whenRow{Label: label, Name: name, Form: f, Weekdays: weekdays}
		},
		// A host name as one path segment, for a name from before the
		// naming rule.
		"seg": url.PathEscape,
		"took": func(a, b time.Time) string {
			if b.Before(a) {
				return ""
			}
			return b.Sub(a).Round(time.Second).String()
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl
	if cfg.Auth != nil {
		cfg.Auth.OnErr = s.authError
	}
	return s, nil
}

// Handler returns the routes, guarded against cross-site posts. With sign-in
// on, only /healthz, the static files and the sign-in routes are open.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.containers)
	app.HandleFunc("GET /hosts", s.hosts)
	app.HandleFunc("GET /hosts/{name}", s.host)
	app.HandleFunc("GET /hosts/{name}/status", s.hostStatus)
	app.HandleFunc("POST /hosts/{name}/check-only", s.setCheckOnly)
	app.HandleFunc("GET /runs", s.runs)
	app.HandleFunc("GET /runs/{id}", s.run)
	app.HandleFunc("GET /settings", s.settings)
	app.HandleFunc("POST /settings", s.saveSettings)
	app.HandleFunc("POST /settings/template", s.saveTemplate)
	app.HandleFunc("POST /settings/template/test", s.testTemplate)
	app.HandleFunc("GET /activity", s.activity)
	app.HandleFunc("POST /actions/check", s.checkNow)
	app.HandleFunc("POST /actions/update", s.updateNow)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	if a := s.cfg.Auth; a != nil {
		mux.HandleFunc("GET /login", a.Login)
		mux.HandleFunc("GET /oauth/callback", a.Callback)
		mux.HandleFunc("GET /logout", a.Logout)
		mux.HandleFunc("GET /signed-out", s.signedOut)
		mux.Handle("/", a.Require(app))
	} else {
		mux.Handle("/", app)
	}
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// page is what every page template gets.
type page struct {
	Page     string
	Title    string
	Version  string
	User     *auth.User
	Activity activity
	// Nav lists the hosts in the sidebar; Host is the one whose page this is.
	Nav  []navHost
	Host string
	Data any
}

// navHost is one host in the sidebar.
type navHost struct {
	Name      string
	State     string // ok, bad (its latest run left it out) or never (no list yet)
	Available int
	CheckOnly bool
}

// activity is the strip under the top bar.
type activity struct {
	Active *server.Activity
	Error  string
	Notice string
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	p.Version = s.cfg.Version
	var b strings.Builder
	if err := s.tpl.ExecuteTemplate(&b, name, p); err != nil {
		s.cfg.Log.Error("render", "template", name, "err", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, b.String())
}

func (s *Server) newPage(r *http.Request, st store.State, name, title string, data any) page {
	p := page{Page: name, Title: title, User: auth.UserFrom(r.Context()),
		Activity: activity{Active: s.cfg.Runner.Active()}, Data: data}
	for _, hv := range view(st) {
		n := navHost{Name: hv.Agent.Name, State: "ok", Available: hv.Available, CheckOnly: hv.Agent.CheckOnly}
		switch {
		case hv.Issue != "":
			n.State = "bad"
		case hv.AsOf.IsZero():
			n.State = "never"
		}
		p.Nav = append(p.Nav, n)
	}
	return p
}

func (s *Server) state(w http.ResponseWriter) (store.State, bool) {
	st, err := s.cfg.Store.Read()
	if err != nil {
		s.cfg.Log.Error("read the state", "err", err)
		http.Error(w, "the state could not be read: "+err.Error(), http.StatusInternalServerError)
		return st, false
	}
	return st, true
}

// --- Containers ---

// row is one container on the containers page.
type row struct {
	Host string
	store.Entry
	// At is when this status was found.
	At time.Time
	// Pending is set while the newer digest a check found is not on the
	// container: the update was not tried, was skipped, rolled back or
	// failed.
	Pending bool
	// CanUpdate offers Update now: a pending, running container that is not
	// hikifune itself.
	CanUpdate bool
}

// rowTable is what the container table template gets.
type rowTable struct {
	Rows []row
	Host bool // show the host column
}

// hostView is what the latest runs say about one host.
type hostView struct {
	Agent                store.Agent
	Rows                 []row
	AsOf                 time.Time // when its container list was made
	Issue                string    // why a run since then left the host out
	IssueAt              time.Time
	AgentVersion, Docker string
	Available            int
}

// view puts together, for every agent, the container list of its latest
// run that listed its containers, with the newer results of updates by
// hand on top. Runs from before the agent was added belong to an earlier
// host of that name and are left out.
func view(st store.State) []hostView {
	out := make([]hostView, 0, len(st.Agents))
	for _, a := range st.Agents {
		hv := hostView{Agent: a}
		newer := map[string]row{}
		var byHand []string // containers with a newer result by hand, newest first
		for i := len(st.Runs) - 1; i >= 0; i-- {
			run := st.Runs[i]
			if run.Started.Before(a.Added) {
				break
			}
			idx := slices.IndexFunc(run.Hosts, func(h store.HostRun) bool { return h.Agent == a.Name })
			if idx < 0 {
				continue
			}
			h := run.Hosts[idx]
			if hv.AgentVersion == "" && h.AgentVersion != "" {
				hv.AgentVersion, hv.Docker = h.AgentVersion, h.Docker
			}
			if !h.Listed() {
				if hv.Issue == "" {
					hv.Issue, hv.IssueAt = cmp.Or(h.Skipped, h.Error), run.Started
				}
				continue
			}
			if run.Container != "" {
				for _, e := range h.Containers {
					if _, ok := newer[e.Container]; !ok {
						newer[e.Container] = row{Host: a.Name, Entry: e, At: run.Started}
						byHand = append(byHand, e.Container)
					}
				}
				continue
			}
			hv.AsOf = run.Started
			listed := map[string]bool{}
			for _, e := range h.Containers {
				r := row{Host: a.Name, Entry: e, At: run.Started}
				if n, ok := newer[e.Container]; ok {
					r = n
				}
				listed[e.Container] = true
				hv.Rows = append(hv.Rows, r)
			}
			// A container updated by hand after the list was made was
			// there then as well.
			for _, name := range byHand {
				if !listed[name] {
					hv.Rows = append(hv.Rows, newer[name])
				}
			}
			break
		}
		for i := range hv.Rows {
			r := &hv.Rows[i]
			r.Pending = pending(r.Entry)
			// Update now sends the ID it shows; a record without one waits
			// for the next check.
			r.CanUpdate = r.Pending && !r.Self && r.State == "running" && r.ID != ""
			if r.Pending {
				hv.Available++
			}
		}
		out = append(out, hv)
	}
	return out
}

// pending tells whether a newer digest the check found is still not on the
// container. A record from before the available field says so by its
// status.
func pending(e store.Entry) bool {
	if !e.Available && e.Status != string(registry.UpdateAvailable) {
		return false
	}
	switch e.Status {
	case string(update.Updated), string(update.UpToDate):
		return false
	}
	return true
}

// filter is what the containers page is narrowed to.
type filter struct {
	Host      string
	Status    string
	Protected bool
	Q         string
}

func (f filter) narrows() bool { return f.Status != "" || f.Protected || f.Q != "" }

// matches tells whether row passes the filter, the host aside. Update
// available matches every pending container, whatever the run did.
func (f filter) matches(r row) bool {
	switch {
	case f.Status == string(registry.UpdateAvailable) && !r.Pending,
		f.Status != "" && f.Status != string(registry.UpdateAvailable) && r.Status != f.Status,
		f.Protected && !r.Protected,
		f.Q != "" && !strings.Contains(strings.ToLower(r.Container+" "+r.Image), strings.ToLower(f.Q)):
		return false
	}
	return true
}

// group is one host's part of the containers page.
type group struct {
	hostView
	Shown []row
}

type containersData struct {
	Hosts              []hostView
	Groups             []group
	Rows               []row // the one-list view
	List               bool
	ByHostURL, ListURL string
	Filter             filter
	Statuses           []string
	Total, Shown       int
	Checked            bool
}

var statuses = []string{
	string(registry.UpdateAvailable), string(registry.UpToDate), string(update.Updated), string(update.RolledBack),
	string(update.Failed), "skipped", string(registry.NotCheckable), string(registry.Pinned),
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := filter{Host: q.Get("host"), Status: q.Get("status"), Protected: q.Get("protected") == "1",
		Q: strings.TrimSpace(q.Get("q"))}
	d := containersData{Hosts: view(st), Filter: f, Statuses: statuses, List: q.Get("view") == "list"}
	q.Del("view")
	d.ByHostURL = "/?" + q.Encode()
	q.Set("view", "list")
	d.ListURL = "/?" + q.Encode()
	for _, h := range d.Hosts {
		d.Total += len(h.Rows)
		if !h.AsOf.IsZero() {
			d.Checked = true
		}
		if f.Host != "" && h.Agent.Name != f.Host {
			continue
		}
		g := group{hostView: h}
		for _, row := range h.Rows {
			if f.matches(row) {
				g.Shown = append(g.Shown, row)
			}
		}
		d.Shown += len(g.Shown)
		d.Rows = append(d.Rows, g.Shown...)
		if len(g.Shown) > 0 || !f.narrows() {
			d.Groups = append(d.Groups, g)
		}
	}
	s.render(w, http.StatusOK, "containers", s.newPage(r, st, "containers", "Containers", d))
}

// --- Hosts ---

type hostsData struct {
	Hosts []hostView
	Saved string
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, "hosts", s.newPage(r, st, "hosts", "Hosts", hostsData{Hosts: view(st), Saved: r.URL.Query().Get("saved")}))
}

type hostData struct {
	View  hostView
	Saved bool
}

// host is one host's page: what it is, its switch, and its containers.
func (s *Server) host(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	views := view(st)
	i := slices.IndexFunc(views, func(hv hostView) bool { return hv.Agent.Name == name })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	p := s.newPage(r, st, "host", name, hostData{View: views[i], Saved: r.URL.Query().Get("saved") == "1"})
	p.Host = name
	s.render(w, http.StatusOK, "host", p)
}

type hostStatus struct {
	Version protocol.Version
	Error   string
	Took    time.Duration
}

func (s *Server) hostStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	i := slices.IndexFunc(st.Agents, func(a store.Agent) bool { return a.Name == r.PathValue("name") })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), statusTimeout)
	defer cancel()
	start := s.now()
	v, err := s.cfg.Status(ctx, st.Agents[i])
	hs := hostStatus{Version: v, Took: s.now().Sub(start).Round(time.Millisecond)}
	switch {
	case err != nil:
		hs.Error = err.Error()
	case v.Agent != s.cfg.Version:
		hs.Error = fmt.Sprintf("the agent runs %s and the server %s", v.Agent, s.cfg.Version)
	case v.DockerError != "":
		hs.Error = "Docker: " + v.DockerError
	}
	s.render(w, http.StatusOK, "hoststatus", page{Data: hs})
}

func (s *Server) setCheckOnly(w http.ResponseWriter, r *http.Request) {
	name, on := r.PathValue("name"), r.FormValue("on") == "1"
	err := s.cfg.Store.Update(func(st *store.State) error {
		for i := range st.Agents {
			if st.Agents[i].Name == name {
				st.Agents[i].CheckOnly = on
				return nil
			}
		}
		return fmt.Errorf("no agent named %s", name)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if r.FormValue("back") == "host" {
		http.Redirect(w, r, "/hosts/"+url.PathEscape(name)+"?saved=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/hosts?saved="+url.QueryEscape(name), http.StatusSeeOther)
}

// --- Runs ---

type runSummary struct {
	store.Run
	Counts map[string]int
	Issues int
}

func summarize(run store.Run) runSummary {
	s := runSummary{Run: run, Counts: map[string]int{}}
	for _, h := range run.Hosts {
		if h.Skipped != "" || h.Error != "" {
			s.Issues++
		}
		for _, e := range h.Containers {
			s.Counts[e.Status]++
		}
	}
	return s
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	runs := st.Runs
	if k := s.cfg.KeepRuns; k > 0 && len(runs) > k {
		runs = runs[len(runs)-k:]
	}
	list := make([]runSummary, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		list = append(list, summarize(runs[i]))
	}
	s.render(w, http.StatusOK, "runs", s.newPage(r, st, "runs", "Runs", list))
}

// order puts what changed first in a run's detail.
var order = map[string]int{
	string(update.RolledBack): 0, string(update.Failed): 1, string(update.Updated): 2,
	string(registry.UpdateAvailable): 3, "skipped": 4,
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	i := slices.IndexFunc(st.Runs, func(run store.Run) bool { return run.ID == r.PathValue("id") })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	run := st.Runs[i]
	for h := range run.Hosts {
		es := slices.Clone(run.Hosts[h].Containers)
		slices.SortStableFunc(es, func(a, b store.Entry) int {
			oa, ok := order[a.Status]
			if !ok {
				oa = 9
			}
			ob, ok := order[b.Status]
			if !ok {
				ob = 9
			}
			return cmp.Or(cmp.Compare(oa, ob), cmp.Compare(a.Container, b.Container))
		})
		run.Hosts[h].Containers = es
	}
	s.render(w, http.StatusOK, "run", s.newPage(r, st, "runs", "Run "+run.ID, summarize(run)))
}

// --- Settings ---

type settingsData struct {
	Check, Update               whenForm
	DefaultCheck, DefaultUpdate string
	Saved                       bool
	Location                    string
	Watch                       time.Duration
	NextCheck, NextUpdate       time.Time
	Error                       string
	Done                        bool
	// Notifications: the webhook without its token, the template in the
	// editor and whether it is the default.
	Webhook         string
	Avatar          bool
	Template        string
	DefaultTemplate bool
	TemplateError   string
	TemplateDone    bool
}

func (s *Server) settingsData(st store.State) settingsData {
	check, upd := s.cfg.Schedule.Times()
	now := s.now()
	d := settingsData{
		Check: formOf(check), Update: formOf(upd),
		DefaultCheck: s.cfg.DefaultCheck.Words(), DefaultUpdate: s.cfg.DefaultUpdate.Words(),
		Saved: st.Settings.Saved, Location: s.cfg.Location.String(), Watch: s.cfg.Watch,
		NextCheck: check.Next(now, s.cfg.Location), NextUpdate: upd.Next(now, s.cfg.Location),
		Template: cmp.Or(st.Settings.Template, notify.Default), DefaultTemplate: st.Settings.Template == "",
	}
	if n := s.cfg.Notifier; n != nil && n.Discord != nil {
		d.Webhook, d.Avatar = notify.Redact(n.Discord.URL), n.Discord.Avatar != ""
	}
	return d
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	d := s.settingsData(st)
	d.Done = r.URL.Query().Get("saved") == "1"
	d.TemplateDone = r.URL.Query().Get("saved") == "template"
	s.render(w, http.StatusOK, "settings", s.newPage(r, st, "settings", "Settings", d))
}

// whenForm is one schedule as the settings form shows it: a period, the
// field that period needs, and the time of day.
type whenForm struct {
	Period  string // day, week or month
	Weekday string // sun to sat, for every week
	Day     string // 1 to 30 or "last day", for every month
	At      string
}

type weekday struct{ Value, Name string }

var weekdays = func() []weekday {
	var out []weekday
	for d := time.Sunday; d <= time.Saturday; d++ {
		out = append(out, weekday{server.Every{Weekly: true, Weekday: d}.String(), d.String()})
	}
	return out
}()

// whenRow is what the schedule row template gets.
type whenRow struct {
	Label, Name string
	Form        whenForm
	Weekdays    []weekday
}

func formOf(w server.When) whenForm {
	f := whenForm{Period: "day", Weekday: "sun", Day: "1", At: w.At.String()}
	switch {
	case w.Every.Weekly:
		f.Period, f.Weekday = "week", w.Every.String()
	case w.Every.Day == server.LastDay:
		f.Period, f.Day = "month", "last day"
	case w.Every.Day > 0:
		f.Period, f.Day = "month", w.Every.String()
	}
	return f
}

// formFrom keeps what was sent, to show it again with an error.
func formFrom(r *http.Request, prefix string) whenForm {
	return whenForm{Period: r.FormValue(prefix + "_period"), Weekday: r.FormValue(prefix + "_weekday"),
		Day: r.FormValue(prefix + "_day"), At: r.FormValue(prefix + "_at")}
}

// whenFrom reads one schedule from the form fields prefix_period,
// prefix_weekday, prefix_day and prefix_at.
func whenFrom(r *http.Request, prefix string) (server.When, error) {
	at, err := server.ParseClock(r.FormValue(prefix + "_at"))
	if err != nil {
		return server.When{}, err
	}
	var every server.Every
	switch p := r.FormValue(prefix + "_period"); p {
	case "", "day":
	case "week":
		every, err = server.ParseEvery(r.FormValue(prefix + "_weekday"))
		if err == nil && !every.Weekly {
			err = fmt.Errorf("%q is not a weekday", r.FormValue(prefix+"_weekday"))
		}
	case "month":
		every, err = server.ParseEvery(r.FormValue(prefix + "_day"))
		if err == nil && every.Day == 0 {
			err = fmt.Errorf("%q is not a day of the month from 1 to 30, or last day", r.FormValue(prefix+"_day"))
		}
	default:
		err = fmt.Errorf("%q is not day, week or month", p)
	}
	return server.When{Every: every, At: at}, err
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	reset := r.FormValue("reset") == "1"
	check, errC := whenFrom(r, "check")
	upd, errU := whenFrom(r, "update")
	if !reset && (errC != nil || errU != nil) {
		st, _ := s.cfg.Store.Read()
		d := s.settingsData(st)
		d.Check, d.Update = formFrom(r, "check"), formFrom(r, "update")
		d.Error = cmp.Or(errC, errU).Error()
		s.render(w, http.StatusUnprocessableEntity, "settings", s.newPage(r, st, "settings", "Settings", d))
		return
	}
	err := s.cfg.Store.Update(func(st *store.State) error {
		set := &st.Settings
		oldCheck, oldUpdate := s.cfg.Schedule.TimesFor(*set)
		if reset {
			set.Saved, set.CheckAt, set.UpdateAt, set.CheckEvery, set.UpdateEvery = false, "", "", "", ""
		} else {
			set.Saved, set.CheckAt, set.UpdateAt = true, check.At.String(), upd.At.String()
			set.CheckEvery, set.UpdateEvery = check.Every.String(), upd.Every.String()
		}
		// Only a schedule that changed counts from now, so saving one
		// leaves the other's count alone.
		newCheck, newUpdate := s.cfg.Schedule.TimesFor(*set)
		now := s.now().UTC()
		if newCheck != oldCheck {
			set.CheckSavedAt = now
		}
		if newUpdate != oldUpdate {
			set.UpdateSavedAt = now
		}
		return nil
	})
	if err != nil {
		http.Error(w, "the settings could not be saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cfg.Wake != nil {
		select {
		case s.cfg.Wake <- struct{}{}:
		default:
		}
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// saveTemplate keeps the notification template once it parses and renders
// a message with every list filled (R19). The default is kept as empty, so
// a newer default reaches those who never changed it.
func (s *Server) saveTemplate(w http.ResponseWriter, r *http.Request) {
	text := strings.ReplaceAll(r.FormValue("template"), "\r\n", "\n")
	if r.FormValue("reset") == "1" || strings.TrimSpace(text) == strings.TrimSpace(notify.Default) {
		text = ""
	}
	if _, err := notify.Parse(text); err != nil {
		st, _ := s.cfg.Store.Read()
		d := s.settingsData(st)
		d.Template, d.DefaultTemplate, d.TemplateError = text, false, err.Error()
		s.render(w, http.StatusUnprocessableEntity, "settings", s.newPage(r, st, "settings", "Settings", d))
		return
	}
	err := s.cfg.Store.Update(func(st *store.State) error {
		st.Settings.Template = text
		return nil
	})
	if err != nil {
		http.Error(w, "the template could not be saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?saved=template", http.StatusSeeOther)
}

// testTemplate sends the sample message with the template in the editor,
// saved or not (P10).
func (s *Server) testTemplate(w http.ResponseWriter, r *http.Request) {
	res := testResult{}
	switch t, err := notify.Parse(strings.ReplaceAll(r.FormValue("template"), "\r\n", "\n")); {
	case s.cfg.Notifier == nil || s.cfg.Notifier.Discord == nil:
		res.Error = "No webhook is set. Set HIKIFUNE_DISCORD_WEBHOOK_URL and restart the server."
	case err != nil:
		res.Error = "The template does not work: " + err.Error()
	default:
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := s.cfg.Notifier.Send(ctx, t, notify.Sample()); err != nil {
			res.Error = err.Error()
		} else {
			res.Notice = "Sent a sample message for example-host."
		}
	}
	s.render(w, http.StatusOK, "testresult", page{Data: res})
}

type testResult struct {
	Notice, Error string
}

// --- Activity and actions ---

// activity answers the strip's poll. When a run it followed has ended, the
// page reloads to show the results.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	a := activity{Active: s.cfg.Runner.Active()}
	if a.Active == nil && r.URL.Query().Get("following") == "1" {
		w.Header().Set("HX-Refresh", "true")
	}
	s.render(w, http.StatusOK, "activity", page{Activity: a})
}

func (s *Server) who(r *http.Request) string {
	if u := auth.UserFrom(r.Context()); u != nil {
		return cmp.Or(u.Name, u.Email, u.Subject)
	}
	return "web"
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, spec server.Spec, what string) {
	spec.Manual = s.who(r)
	err := s.cfg.Runner.Start(s.cfg.Base, spec, func(run store.Run, err error) {
		if err != nil {
			s.cfg.Log.Error("run by hand failed", "what", what, "err", err)
			return
		}
		s.cfg.Log.Info("run by hand finished", "what", what, "id", run.ID, "by", spec.Manual)
	})
	a := activity{Active: s.cfg.Runner.Active()}
	switch {
	case errors.Is(err, server.ErrBusy):
		a.Error = "Another run is going on. " + what + " was not started."
	case err != nil:
		a.Error = err.Error()
	default:
		s.cfg.Log.Info("run by hand started", "what", what, "by", spec.Manual)
		if a.Active == nil {
			a.Notice = what + " started."
		}
	}
	s.render(w, http.StatusOK, "activity", page{Activity: a})
}

func (s *Server) checkNow(w http.ResponseWriter, r *http.Request) {
	spec := server.Spec{Kind: server.KindCheck}
	what := "Check of every host"
	if h := r.FormValue("host"); h != "" {
		spec.Hosts = []string{h}
		what = "Check of " + h
	}
	s.start(w, r, spec, what)
}

func (s *Server) updateNow(w http.ResponseWriter, r *http.Request) {
	host, name, id := r.FormValue("host"), r.FormValue("container"), r.FormValue("id")
	if host == "" || name == "" || id == "" {
		http.Error(w, "host, container and id are required", http.StatusBadRequest)
		return
	}
	s.start(w, r, server.Spec{Kind: server.KindUpdate, Hosts: []string{host}, Container: name, ContainerID: id},
		"Update of "+name+" on "+host)
}

// --- Sign-in pages ---

type notice struct {
	Title, Text string
	SignIn      bool
}

func (s *Server) signedOut(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "notice", page{Page: "notice", Title: "Signed out",
		Data: notice{Title: "Signed out", Text: "You are signed out of hikifune.", SignIn: true}})
}

func (s *Server) authError(w http.ResponseWriter, r *http.Request, status int, why auth.Failure) {
	n := notice{Title: "Sign-in failed", Text: "The sign-in did not go through. Try again.", SignIn: true}
	switch why {
	case auth.ProviderDown:
		n.Text = "The sign-in provider did not answer. Try again in a moment."
	case auth.NotInGroup:
		n = notice{Title: "No access", Text: "You signed in, but you are not in the group that may use hikifune (" + s.cfg.Auth.Group() + ")."}
	}
	s.render(w, status, "notice", page{Page: "notice", Title: n.Title, Data: n})
}

// --- Helpers ---

func (s *Server) local(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.cfg.Location).Format("2006-01-02 15:04")
}

func (s *Server) ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := s.now().Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

func shortDigest(d string) string {
	if h, ok := strings.CutPrefix(d, "sha256:"); ok && len(h) > 12 {
		return h[:12]
	}
	return d
}

// statusClass is the badge class for a status.
func statusClass(status string) string {
	switch status {
	case string(registry.UpdateAvailable):
		return "available"
	case string(registry.UpToDate), string(update.Updated):
		return "good"
	case string(update.RolledBack), string(update.Failed): // registry.Failed reads the same
		return "bad"
	}
	return "quiet"
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
