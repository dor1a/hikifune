package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/store"
)

// homeData is the Home page: a summary, the map of the server and its
// agents, and the latest runs.
type homeData struct {
	Hosts, CheckOnly, Containers, Available int
	NextCheck, NextUpdate                   string
	Recent                                  []runSummary
	Layout                                  string
	Layouts                                 []struct{ Value, Name string }
	// Map is drawn in the page when it is asked for without the page's
	// script; otherwise the script loads it after the page.
	Map *mapData
}

// home is the first page. The map comes after it loads, from homeMap, since
// it asks every agent how it is.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	s.homePage(w, r, st, nil)
}

func (s *Server) homePage(w http.ResponseWriter, r *http.Request, st store.State, m *mapData) {
	// The layout chosen last is kept in a cookie, so Home opens with it.
	layout := r.URL.Query().Get("map")
	if layout != "" {
		layout = mapLayout(layout)
		rememberLayout(w, layout)
	} else if c, err := r.Cookie(mapCookie); err == nil {
		layout = c.Value
	}
	d := homeData{Hosts: len(st.Agents), Layout: mapLayout(layout), Layouts: mapLayouts, Map: m}
	if m != nil {
		d.Layout = m.Layout
	}
	for _, hv := range view(st) {
		d.Containers += len(hv.Rows)
		d.Available += hv.Available
		if hv.Agent.CheckOnly {
			d.CheckOnly++
		}
	}
	nextCheck, nextUpdate := s.nextRuns(s.cfg.Schedule.Times())
	d.NextCheck, d.NextUpdate = s.whenNext(nextCheck), s.whenNext(nextUpdate)
	runs := st.Runs
	if k := s.cfg.KeepRuns; k > 0 && len(runs) > k {
		runs = runs[len(runs)-k:]
	}
	for i := len(runs) - 1; i >= 0 && len(d.Recent) < 5; i-- {
		d.Recent = append(d.Recent, summarize(runs[i]))
	}
	s.render(w, http.StatusOK, "home", s.newPage(r, st, "home", "Home", d))
}

// whenNext is a next run's time, said to be waiting when it is past.
func (s *Server) whenNext(at time.Time) string {
	if at.Before(s.now()) {
		return s.local(at) + ", after the run going on"
	}
	return s.local(at)
}

// The map is drawn on a fixed canvas the page scales, in one of
// mapLayouts, with a curve from the server to each agent.
const (
	mapW      = 960
	mapCols   = 5
	nodeH     = 96
	serverW   = 240
	serverH   = 72
	serverY   = 24
	firstRowY = 200
	rowGap    = 136
	badgeW    = 70
	badgeH    = 18
	// subChars is as much of the line under a name as a box holds, and
	// serverVersionChars as much of the version as the server's box holds
	// after "Server ".
	subChars           = 20
	serverVersionChars = 16
)

// mapLayouts are the ways to draw the map, the first the default: the
// server in the middle of its agents' orbit, above them, or on the left of a
// column of them.
var mapLayouts = []struct{ Value, Name string }{{"orbit", "Orbit"}, {"tree", "Top down"}, {"flow", "Flow"}}

// mapCookie keeps the map layout chosen last in this browser.
const mapCookie = "hikifune_map"

// mapLayout is the layout asked for, or the first.
func mapLayout(v string) string {
	for _, l := range mapLayouts {
		if l.Value == v {
			return v
		}
	}
	return mapLayouts[0].Value
}

// mapData is what the map template draws.
type mapData struct {
	W, H          int
	Layout        string
	Orbit         mapOrbit
	Server        mapServer
	Nodes         []mapNode
	Links         []mapLink
	Empty         bool
	Answering, Of int
}

type mapServer struct {
	X, Y, W, H          int
	Busy                bool
	BusyKind, Version   string
	LogoX, LogoY, TextX int
	NameY, SubY         int
	ClipX, ClipW        int
}

// mapNode is one agent: its box, its state and what its latest check found.
// State is ok, checkonly, down or busy; Note says why it is down. Sub is
// the line under the name, cut to fit the box; the title holds it whole.
type mapNode struct {
	Name, Label, Href, State, Docker, Note string
	Sub                                    string
	X, Y, W, H                             int
	DotX, DotY, NameX, NameY, TextX        int
	Line2Y, Line3Y, RightX                 int
	BadgeX, BadgeY, BadgeTX, BadgeTY       int
	ClipX, ClipW                           int
	Containers, Available                  int
	CheckOnly                              bool
}

type mapLink struct {
	ID, D, State string
	Delay        string
}

// mapOrbit is the ellipse the agents sit on in the orbit layout.
type mapOrbit struct{ CX, CY, RX, RY int }

// homeMap asks every agent how it is, at once, and draws the map. The
// page's script gets the map alone; without the script, Home comes back
// with the map in it.
func (s *Server) homeMap(w http.ResponseWriter, r *http.Request) {
	st, ok := s.state(w)
	if !ok {
		return
	}
	d := s.mapOf(r.Context(), st, mapLayout(r.URL.Query().Get("layout")))
	// The same address answers the script and a plain visit differently, so
	// a cache has to tell the two apart.
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, http.StatusOK, "homemap", page{Data: d})
		return
	}
	if r.URL.Query().Get("layout") != "" {
		rememberLayout(w, d.Layout)
	}
	s.homePage(w, r, st, &d)
}

// rememberLayout keeps the layout chosen last in a cookie for a year.
func rememberLayout(w http.ResponseWriter, layout string) {
	http.SetCookie(w, &http.Cookie{Name: mapCookie, Value: layout, Path: "/", MaxAge: 365 * 24 * 60 * 60,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// mapOf asks every agent of st how it is, at once, each for statusTimeout
// at most, and lays out the map as layout says.
func (s *Server) mapOf(ctx context.Context, st store.State, layout string) mapData {
	views := view(st)
	type answer struct {
		v   protocol.Version
		err error
	}
	answers := make([]answer, len(st.Agents))
	var wg sync.WaitGroup
	for i, a := range st.Agents {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, statusTimeout)
			defer cancel()
			v, err := s.cfg.Status(ctx, a)
			answers[i] = answer{v, err}
		})
	}
	wg.Wait()
	busy := map[string]bool{}
	d := mapData{Of: len(st.Agents), Empty: len(st.Agents) == 0}
	d.Server.Version = shorten(s.cfg.Version, serverVersionChars)
	if act := s.cfg.Runner.Active(); act != nil {
		d.Server.Busy, d.Server.BusyKind = true, act.Spec.Kind
		for _, a := range st.Agents {
			if len(act.Spec.Hosts) == 0 || slices.Contains(act.Spec.Hosts, a.Name) {
				busy[a.Name] = true
			}
		}
	}
	d.layout(len(st.Agents), layout)
	for i, a := range st.Agents {
		n := &d.Nodes[i]
		n.Name, n.Label, n.Href, n.CheckOnly = a.Name, shorten(a.Name, 14), "/hosts/"+url.PathEscape(a.Name), a.CheckOnly
		if i < len(views) {
			n.Containers, n.Available, n.Docker = len(views[i].Rows), views[i].Available, views[i].Docker
		}
		v, err := answers[i].v, answers[i].err
		switch {
		case err != nil:
			n.State, n.Note = "down", "No answer"
		case v.Agent != s.cfg.Version:
			n.State, n.Note = "down", "Agent "+v.Agent
		case v.DockerError != "":
			n.State, n.Note = "down", "Docker down"
		case busy[a.Name]:
			n.State = "busy"
		case a.CheckOnly:
			n.State = "checkonly"
		default:
			n.State = "ok"
		}
		if err == nil && v.Docker != nil {
			n.Docker = v.Docker.Version
		}
		switch {
		case n.Note != "":
			n.Sub = n.Note
		case n.Docker != "":
			n.Sub = "Docker " + n.Docker
		default:
			n.Sub = "Not checked yet"
		}
		n.Sub = shorten(n.Sub, subChars)
		if n.State != "down" {
			d.Answering++
		}
		d.Links[i].State = n.State
	}
	return d
}

// layout places the server and n agents, and the curves between them, as
// kind says.
func (d *mapData) layout(n int, kind string) {
	d.W, d.Layout = mapW, kind
	switch kind {
	case "orbit":
		d.orbit(n)
	case "flow":
		d.flow(n)
	default:
		d.tree(n)
	}
	for i := range d.Links {
		// The dots set off a little apart, all within one trip (2.8s), so
		// each one runs well before the map is drawn again.
		delay := i * 35 % 280 // hundredths of a second
		d.Links[i].ID, d.Links[i].Delay = fmt.Sprintf("link-%d", i), fmt.Sprintf("%d.%02ds", delay/100, delay%100)
	}
}

func (d *mapData) placeServer(x, y int) {
	s := &d.Server
	s.X, s.Y, s.W, s.H = x, y, serverW, serverH
	s.LogoX, s.LogoY, s.TextX = x+18, y+(serverH-40)/2, x+70
	s.ClipX, s.ClipW = x+64, serverW-72
	s.NameY, s.SubY = y+32, y+53
}

func (d *mapData) addNode(x, y, w int) {
	// The check-only badge sits on the top edge, at the right.
	bx := x + w - 12 - badgeW
	d.Nodes = append(d.Nodes, mapNode{X: x, Y: y, W: w, H: nodeH, DotX: x + 20, DotY: y + 24,
		NameX: x + 34, NameY: y + 29, TextX: x + 16, Line2Y: y + 56, Line3Y: y + 78, RightX: x + w - 14,
		BadgeX: bx, BadgeY: y - badgeH/2, BadgeTX: bx + badgeW/2, BadgeTY: y + 4,
		// The texts are clipped a little inside the box, whatever their width.
		ClipX: x + 8, ClipW: w - 16})
}

func (d *mapData) addLink(path string, args ...any) {
	d.Links = append(d.Links, mapLink{D: fmt.Sprintf(path, args...)})
}

// tree puts the server at the top and its agents in rows of five below.
func (d *mapData) tree(n int) {
	d.placeServer((mapW-serverW)/2, serverY)
	rows := max(1, (n+mapCols-1)/mapCols)
	d.H = firstRowY + (rows-1)*rowGap + nodeH + 24
	sx, sy := mapW/2, serverY+serverH
	for i := range n {
		row, col := i/mapCols, i%mapCols
		inRow := min(mapCols, n-row*mapCols)
		slot := mapW / inRow
		w := min(176, slot-24)
		cx, y := slot*col+slot/2, firstRowY+row*rowGap
		d.addNode(cx-w/2, y, w)
		mid := (sy + y) / 2
		d.addLink("M %d %d C %d %d, %d %d, %d %d", sx, sy, sx, mid, cx, mid, cx, y)
	}
}

// orbit puts the server in the middle and its agents on an ellipse around
// it, each curve bent a little off the straight line.
func (d *mapData) orbit(n int) {
	const w = 168
	rx, ry := 330, 200
	if n > 8 {
		// Past eight the ellipse grows with the agents, about 230 a box
		// around it, and the canvas with it; the page scales it to fit.
		rx = max(370, int(math.Ceil(230*float64(n)/(2*math.Pi))))
		ry = rx * 250 / 370
	}
	d.W = max(mapW, 2*rx+w+48)
	d.H = 2*ry + nodeH + 48
	cx, cy := d.W/2, d.H/2
	d.placeServer(cx-serverW/2, cy-serverH/2)
	d.Orbit = mapOrbit{CX: cx, CY: cy, RX: rx, RY: ry}
	for i := range n {
		a := -math.Pi/2 + math.Pi/float64(n) + 2*math.Pi*float64(i)/float64(n)
		nx, ny := cx+int(float64(rx)*math.Cos(a)), cy+int(float64(ry)*math.Sin(a))
		d.addNode(nx-w/2, ny-nodeH/2, w)
		mx, my := (cx+nx)/2, (cy+ny)/2
		qx, qy := mx-(ny-cy)/5, my+(nx-cx)/5
		d.addLink("M %d %d Q %d %d, %d %d", cx, cy, qx, qy, nx, ny)
	}
}

// flow puts the server on the left and its agents in columns of five on the
// right, the curves running across. Past three columns the columns grow
// longer instead, so they stay on the canvas.
func (d *mapData) flow(n int) {
	const gap, colGap, w = 18, 40, 176
	perCol := max(5, (n+2)/3)
	cols := max(1, (n+perCol-1)/perCol)
	rows := max(1, min(n, perCol))
	d.H = max(rows*(nodeH+gap)-gap+48, serverH+48)
	d.placeServer(24, (d.H-serverH)/2)
	sx, sy := 24+serverW, d.H/2
	left := mapW - 24 - cols*w - (cols-1)*colGap
	for i := range n {
		col, row := i/perCol, i%perCol
		inCol := min(perCol, n-col*perCol)
		x := left + col*(w+colGap)
		y := (d.H-(inCol*(nodeH+gap)-gap))/2 + row*(nodeH+gap)
		d.addNode(x, y, w)
		ty, mid := y+nodeH/2, (sx+x)/2
		d.addLink("M %d %d C %d %d, %d %d, %d %d", sx, sy, mid, sy, mid, ty, x, ty)
	}
}

// shorten keeps a name to n characters for a box of fixed width.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
