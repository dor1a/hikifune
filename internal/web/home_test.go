package web

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/server"
	"github.com/dor1a/hikifune/internal/store"
)

// Home is the first page: the summary, the map that loads after it, and the
// latest runs; Containers moved to its own path.
func TestHome(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	h := s.Handler()
	_, body := do(t, h, "GET", "/", nil)
	for _, want := range []string{
		`<h1 class="title">Home</h1>`, `<span class="tile-label">Hosts</span><span class="tile-value">2</span>`,
		`hx-get="/home/map?layout=orbit"`, `href="/runs/r2"`, `<a href="/" aria-current="page">`,
		`href="/containers?status=update_available"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page has no %q", want)
		}
	}
	if _, body := do(t, h, "GET", "/containers", nil); !strings.Contains(body, `<a href="/containers" aria-current="page">`) {
		t.Error("containers page not at /containers")
	}
}

// The map asks every agent: one that answers is ok and its link flows, one
// that does not is down with a dashed link and no dot running to it.
func TestHomeMap(t *testing.T) {
	r := &fakeRunner{}
	s, _ := newServer(t, fixture(t), r, nil)
	h := s.Handler()
	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	body := get("/home/map")
	for _, want := range []string{
		"1 of 2 agents answering", `<a href="/hosts/a" class="node ok">`, `<a href="/hosts/b" class="node down">`,
		`<title>b: No answer (check only)</title>`, `class="link ok"`, `class="link down"`, `<mpath href="#link-0"/>`,
		"Docker 29.1.0", "Server v1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("map has no %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<mpath href="#link-1"/>`) || strings.Contains(body, "style=") {
		t.Errorf("a dot runs to the agent that is down, or an inline style:\n%s", body)
	}
	// While a run goes on, its hosts are busy and the server says so.
	r.active = &server.Activity{Spec: server.Spec{Kind: "update", Hosts: []string{"a"}}}
	body = get("/home/map")
	if !strings.Contains(body, `<a href="/hosts/a" class="node busy">`) || !strings.Contains(body, "Update run going on") {
		t.Errorf("busy map:\n%s", body)
	}
	r.active = nil

	// Without the page's script, Home loads with no map and a link to it,
	// and the link gives Home with the map drawn in and its choice kept.
	if _, body := do(t, h, "GET", "/", nil); !strings.Contains(body, `<noscript><p><a href="/home/map?layout=orbit">Show the map</a>`) || strings.Contains(body, "<svg class=\"map") {
		t.Errorf("home without the script:\n%s", body)
	}
	res, body := do(t, h, "GET", "/home/map?layout=flow", nil)
	if !strings.Contains(body, `<h1 class="title">Home</h1>`) || !strings.Contains(body, `<svg class="map flow"`) ||
		!strings.Contains(body, `hx-trigger="every 15s"`) || !strings.Contains(body, `<a href="/home/map?layout=orbit">Orbit</a>`) ||
		strings.Contains(body, "Asking the agents") {
		t.Errorf("the map without the script:\n%s", body)
	}
	if c := res.Cookies(); len(c) != 1 || c[0].Value != "flow" {
		t.Errorf("cookies %+v", c)
	}
	if v := res.Header.Get("Vary"); v != "HX-Request" {
		t.Errorf("Vary %q: a cache could hand the map alone to a plain visit", v)
	}
}

// A run that waits for the one going on shows at its own time, past, not
// the next day's (the plan comes from the schedule loop).
func TestHomeShowsAWaitingRun(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	now := time.Date(2026, 11, 21, 1, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	f := s.cfg.Schedule.(fakeSchedule)
	f.planned = map[string]server.Planned{server.KindUpdate: {When: f.update, At: time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)}}
	s.cfg.Schedule = f
	_, body := do(t, s.Handler(), "GET", "/", nil)
	if !strings.Contains(body, "Update 2026-11-21 01:00, after the run going on") || !strings.Contains(body, "Check 2026-11-22 00:00<") {
		t.Errorf("home:\n%s", body)
	}
	// While the planned update itself runs, the next is tomorrow's.
	r := s.cfg.Runner.(*fakeRunner)
	r.active = &server.Activity{Spec: server.Spec{Kind: server.KindUpdate}, Started: now}
	if _, body := do(t, s.Handler(), "GET", "/", nil); !strings.Contains(body, "Update 2026-11-22 01:00<") {
		t.Errorf("home while the update runs:\n%s", body)
	}
	r.active = nil
	// A plan made with other times, as just after they were saved, is not
	// used.
	f.planned[server.KindUpdate] = server.Planned{When: server.When{At: server.Clock{Hour: 5}}, At: time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)}
	s.cfg.Schedule = f
	if _, body := do(t, s.Handler(), "GET", "/", nil); !strings.Contains(body, "Update 2026-11-22 01:00<") {
		t.Errorf("home with an old plan:\n%s", body)
	}
}

// A long line under a name is cut to fit the box; the title keeps it whole.
func TestMapCutsLongLines(t *testing.T) {
	st := fixture(t)
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	s.cfg.Status = func(context.Context, store.Agent) (protocol.Version, error) {
		return protocol.Version{Agent: "v1.0.0-" + strings.Repeat("w", 80)}, nil
	}
	req := httptest.NewRequest("GET", "/home/map", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	want := ">" + shorten("Agent v1.0.0-"+strings.Repeat("w", 80), subChars) + "</text>"
	if !strings.Contains(body, want) || !strings.Contains(body, "<title>a: Agent v1.0.0-"+strings.Repeat("w", 80)) {
		t.Errorf("no %q:\n%s", want, body)
	}
	// The server's own version too, in its box.
	s.cfg.Version = "v1.0.0-" + strings.Repeat("w", 80)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if want := "Server " + shorten(s.cfg.Version, serverVersionChars) + "</text>"; !strings.Contains(rec.Body.String(), want) ||
		!strings.Contains(rec.Body.String(), `<g clip-path="url(#server-clip)">`) {
		t.Errorf("no %q:\n%s", want, rec.Body.String())
	}
}

func TestShorten(t *testing.T) {
	if got := shorten("a-very-long-host-name", 10); got != "a-very-lo…" {
		t.Errorf("got %q", got)
	}
	if got := shorten("sol", 10); got != "sol" {
		t.Errorf("got %q", got)
	}
}

// Every layout keeps the boxes on the canvas and apart, the server's
// included, for one to 64 agents, and every dot sets off within one trip.
func TestMapLayouts(t *testing.T) {
	type box struct{ x, y, w, h int }
	overlap := func(a, b box) bool {
		return a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h
	}
	for _, l := range mapLayouts {
		for n := 1; n <= 64; n++ {
			var d mapData
			d.layout(n, l.Value)
			if len(d.Nodes) != n || len(d.Links) != n || d.Layout != l.Value {
				t.Fatalf("%s %d: %d nodes, %d links", l.Value, n, len(d.Nodes), len(d.Links))
			}
			for _, ln := range d.Links {
				if delay, err := strconv.ParseFloat(strings.TrimSuffix(ln.Delay, "s"), 64); err != nil || delay >= 2.8 {
					t.Errorf("%s %d: link %s sets off after %s, past one trip", l.Value, n, ln.ID, ln.Delay)
				}
			}
			boxes := []box{{d.Server.X, d.Server.Y, d.Server.W, d.Server.H}}
			for _, nd := range d.Nodes {
				boxes = append(boxes, box{nd.X, nd.Y, nd.W, nd.H})
				if nd.BadgeY < 0 || nd.BadgeX < nd.X {
					t.Errorf("%s %d: badge off its box or the canvas: %+v", l.Value, n, nd)
				}
			}
			for i, a := range boxes {
				if a.x < 0 || a.y < 0 || a.x+a.w > d.W || a.y+a.h > d.H {
					t.Errorf("%s %d: box %d %+v off the %dx%d canvas", l.Value, n, i, a, d.W, d.H)
				}
				for j := i + 1; j < len(boxes); j++ {
					if overlap(a, boxes[j]) {
						t.Errorf("%s %d: boxes %d and %d overlap: %+v %+v", l.Value, n, i, j, a, boxes[j])
					}
				}
			}
		}
	}
}

// The layout is chosen on Home and asked of the map; an unknown one is the
// first.
func TestMapLayoutChoice(t *testing.T) {
	s, _ := newServer(t, fixture(t), &fakeRunner{}, nil)
	h := s.Handler()
	if _, body := do(t, h, "GET", "/?map=orbit", nil); !strings.Contains(body, `hx-get="/home/map?layout=orbit"`) ||
		!strings.Contains(body, `<a href="/?map=orbit" aria-current="true">Orbit</a>`) {
		t.Error("orbit not chosen on Home")
	}
	// The choice is remembered: Home opens with it next time.
	req := httptest.NewRequest("GET", "/?map=flow", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "hikifune_map" || cookies[0].Value != "flow" || !cookies[0].HttpOnly {
		t.Fatalf("cookies %+v", cookies)
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `hx-get="/home/map?layout=flow"`) {
		t.Error("the layout chosen last was not kept")
	}
	if _, body := do(t, h, "GET", "/home/map?layout=orbit", nil); !strings.Contains(body, `class="map orbit"`) || !strings.Contains(body, `<ellipse class="orbit"`) {
		t.Error("orbit map")
	}
	if _, body := do(t, h, "GET", "/home/map?layout=flow", nil); !strings.Contains(body, `class="map flow"`) {
		t.Error("flow map")
	}
	if _, body := do(t, h, "GET", "/home/map?layout=nope", nil); !strings.Contains(body, `class="map orbit"`) {
		t.Error("an unknown layout is not the first")
	}
}
