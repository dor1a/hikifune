package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dor1a/hikifune/internal/store"
)

var t0 = time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)

func entry(name, status string, available bool) store.Entry {
	return store.Entry{Container: name, Image: name + ":1", Status: status, Available: available, State: "running"}
}

func TestBuildACheckRun(t *testing.T) {
	guard := entry("guard", "update_available", true)
	guard.Protected = true
	run := store.Run{Kind: "check", Started: t0, Hosts: []store.HostRun{
		{Agent: "a", Containers: []store.Entry{entry("web", "update_available", true), guard, entry("db", "up_to_date", false)}},
		{Agent: "quiet", Containers: []store.Entry{entry("db", "up_to_date", false)}},
		{Agent: "old", Skipped: "the agent runs v0 and the server v1"},
	}}
	msgs := Build(run)
	if len(msgs) != 2 || msgs[0].Host != "a" || msgs[1].Host != "old" {
		t.Fatalf("messages %+v", msgs)
	}
	if a := msgs[0]; len(a.Available) != 2 || !a.Available[1].Protected || len(a.Updated) != 0 || a.color() != colorAvailable {
		t.Errorf("a: %+v", a)
	}
	if old := msgs[1]; old.LeftOut != "the agent runs v0 and the server v1" || old.color() != colorBad {
		t.Errorf("old: %+v", old)
	}
}

// A prune that freed nothing still says so; a host that did not prune says
// nothing about it.
func TestAPruneOfNothingIsReported(t *testing.T) {
	for _, tt := range []struct {
		pruned bool
		want   string
	}{{true, "0 B"}, {false, ""}} {
		run := store.Run{Kind: "update", Started: t0, Hosts: []store.HostRun{{Agent: "a", Pruned: tt.pruned,
			Containers: []store.Entry{entry("prom", "updated", true)}}}}
		if m := Build(run); len(m) != 1 || m[0].Reclaimed != tt.want {
			t.Errorf("pruned %v: %+v", tt.pruned, m)
		}
	}
}

func TestBuildAnUpdateRun(t *testing.T) {
	back := entry("web", "rolled_back", true)
	back.Reason = "exited with code 1 within the 2m watch"
	failed := entry("app", "failed", true)
	failed.Reason = "create the new container: port 8181 is already allocated"
	checkFailed := entry("private", "failed", false)
	checkFailed.Reason = "registry: unauthorized"
	run := store.Run{Kind: "update", Started: t0, Hosts: []store.HostRun{{Agent: "a", Reclaimed: 4_800_000_000, Error: "prune: refused",
		Containers: []store.Entry{entry("prom", "updated", true), back, failed, checkFailed, entry("guard", "skipped", true), entry("same", "up_to_date", true)}}}}
	msgs := Build(run)
	if len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
	m := msgs[0]
	// A failed registry check is listed with the failed updates.
	if len(m.Updated) != 1 || len(m.RolledBack) != 1 || len(m.Failed) != 2 || m.Failed[1].Container != "private" ||
		len(m.Available) != 0 || m.Reclaimed != "4.8 GB" || m.Note != "prune: refused" || m.color() != colorBad {
		t.Fatalf("message %+v", m)
	}
	tpl, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	text, err := Render(tpl, m)
	if err != nil {
		t.Fatal(err)
	}
	want := "**Updated**\n• `prom` prom:1\n\n" +
		"**Rolled back**\n• `web` web:1\n  exited with code 1 within the 2m watch\n\n" +
		"**Failed**\n• `app` app:1\n  create the new container: port 8181 is already allocated\n" +
		"• `private` private:1\n  registry: unauthorized\n\n" +
		"Reclaimed 4.8 GB\nprune: refused"
	if text != want {
		t.Errorf("text:\n%s\nwant:\n%s", text, want)
	}
}

func TestACheckRunListsFailedChecks(t *testing.T) {
	bad := entry("private", "failed", false)
	bad.Reason = "registry answered 503"
	msgs := Build(store.Run{Kind: "check", Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{bad}}}})
	if len(msgs) != 1 || len(msgs[0].Failed) != 1 || msgs[0].Failed[0].Reason != "registry answered 503" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestNothingToSay(t *testing.T) {
	run := store.Run{Kind: "update", Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{entry("guard", "skipped", true)}}}}
	if msgs := Build(run); len(msgs) != 0 {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse("{{.Host}} {{range .Updated}}{{.Container}}{{end}}"); err != nil {
		t.Errorf("good template: %v", err)
	}
	for text, want := range map[string]string{
		"{{.Host":                          "unclosed",
		"{{.Nope}}":                        "Nope",
		"{{if false}}x{{end}}":             "writes nothing",
		strings.Repeat("x", 10001):         "more than",
		"{{range .Failed}}{{.Why}}{{end}}": "Why",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.30q: err %v, want %q", text, err, want)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := Escape("my_app *x* `y` @everyone"); got != `my\_app \*x\* \`+"`"+`y\`+"`"+` @everyone` {
		t.Errorf("Escape = %s", got)
	}
	// A value stays on one line, so no line in it starts a heading, quote
	// or list, and a link does not form.
	if got := Escape("# heading\n> quote\r\n- item [label](https://example.invalid) <@1>"); got != `\# heading > quote - item \[label\](https://example.invalid) \<@1>` {
		t.Errorf("Escape = %s", got)
	}
}

func TestTemplateLimits(t *testing.T) {
	for text, want := range map[string]string{
		`{{printf "%8388608s" "x"}}`:                              "widths and precisions",
		`{{printf "%.*s" 5 "x"}}`:                                 "* widths",
		`{{range 1000}}x{{end}}`:                                  "larger than",
		`{{range 3}}{{range 3}}{{range 3}}x{{end}}{{end}}{{end}}`: "nested 3 deep",
		`{{define "a"}}{{range 3}}{{template "b"}}{{end}}{{end}}{{define "b"}}{{range 3}}{{range 3}}x{{end}}{{end}}{{end}}{{template "a"}}`: "nested 3 deep",
		`{{define "a"}}x{{template "a"}}{{end}}{{template "a"}}`:                                                                            "calls itself",
		`{{$x := "ab"}}{{range 100}}{{$x = printf "%s%s" $x $x}}{{end}}{{$x}}`:                                                              "more than",
		`{{$x := "ab"}}{{range 100}}{{$x = print $x $x}}{{end}}{{$x}}`:                                                                      "more than",
		`{{$x := "\\"}}{{range 100}}{{$x = md $x}}{{end}}{{$x}}`:                                                                            "more than",
		"{{if .LeftOut}}{{(index .Updated 0).Container}}{{end}}x":                                                                           "left-out host",
		"{{if .Failed}}{{(index .Available 0).Container}}{{end}}x":                                                                          "check message of failures",
		`{{if and (eq .Kind "update") .Failed}}{{(index .RolledBack 0).Container}}{{end}}x`:                                                 "update message of failures only",
		`{{if and (eq .Kind "update") .RolledBack}}{{(index .Failed 0).Container}}{{end}}x`:                                                 "update message of rollbacks only",
		`{{printf "%201[1]s" "x"}}`: "widths and precisions",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.50s: err %v, want %q", text, err, want)
		}
	}
	if _, err := Parse(`{{range .Available}}{{printf "%-30s" .Container}}{{end}} {{range 3}}.{{end}}{{printf "%20[1]s %-8[2]s" .Host .Kind}}`); err != nil {
		t.Errorf("a template within the limits: %v", err)
	}
}

// Templates that each call the next four times take no loop and pass the
// nesting limit, yet the last one runs 4^7 times.
func TestManyCallsAreRefused(t *testing.T) {
	var b strings.Builder
	for i := range 8 {
		fmt.Fprintf(&b, `{{define "t%d"}}`, i)
		if i < 7 {
			fmt.Fprintf(&b, `{{template "t%d"}}{{template "t%d"}}{{template "t%d"}}{{template "t%d"}}`, i+1, i+1, i+1, i+1)
		}
		b.WriteString(`{{end}}`)
	}
	b.WriteString(`{{template "t0"}}ok`)
	start := time.Now()
	_, err := Parse(b.String())
	if err == nil || !strings.Contains(err.Error(), errTooMuchWork.Error()) {
		t.Fatalf("err %v, want %v", err, errTooMuchWork)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("refusing took %s", d)
	}
	// Six levels stay under the limit.
	ok := strings.Replace(strings.Replace(b.String(), `{{template "t7"}}`, "", -1), `{{define "t7"}}{{end}}`, "", 1)
	if _, err := Parse(ok); err != nil {
		t.Errorf("4^6 calls: %v", err)
	}
}

// Each message counts its own steps, so a long run of messages is not cut
// short, and messages may render at once.
func TestStepsAreCountedPerMessage(t *testing.T) {
	tmpl, err := Parse(`{{range .Failed}}{{.Container}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	m := Message{Host: "a", Kind: "update"}
	for i := range maxSteps / 2 {
		m.Failed = append(m.Failed, Item{Container: fmt.Sprint(i % 10)})
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Render(tmpl, m); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m.Failed = append(m.Failed, m.Failed...)
	if _, err := Render(tmpl, m); err == nil || !strings.Contains(err.Error(), errTooMuchWork.Error()) {
		t.Errorf("err %v, want %v", err, errTooMuchWork)
	}
}

func TestSplit(t *testing.T) {
	var lines []string
	for range 300 {
		lines = append(lines, "• `container` "+strings.Repeat("é", 20))
	}
	parts := split(strings.Join(lines, "\n"), MaxDescription)
	if len(parts) < 2 {
		t.Fatalf("%d parts", len(parts))
	}
	total := 0
	for _, p := range parts {
		n := utf8.RuneCountInString(p)
		if n > MaxDescription || strings.HasPrefix(p, "\n") {
			t.Errorf("part of %d characters", n)
		}
		total += strings.Count(p, "•")
	}
	if total != 300 {
		t.Errorf("%d lines kept", total)
	}
	if parts := split(strings.Repeat("x", 10000), 4096); len(parts) != 3 || len(parts[0]) != 4096 {
		t.Errorf("long line: %d parts", len(parts))
	}
	// A cut never falls between a backslash and the mark it escapes.
	parts = split(strings.Repeat("x", 4095)+`\*bold`, 4096)
	if len(parts) != 2 || strings.HasSuffix(parts[0], `\`) || !strings.HasPrefix(parts[1], `\*`) {
		t.Errorf("escape cut: %q", parts)
	}
}

func TestWebhookURLs(t *testing.T) {
	good := "https://discord.com/api/webhooks/123456/secret-token"
	if err := CheckWebhook(good); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "discord.com/api/webhooks/1/t", "https://discord.com/api/webhooks/123456", "ftp://x/api/webhooks/1/t"} {
		if err := CheckWebhook(bad); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if got := Redact(good); got != "https://discord.com/api/webhooks/123456/…" {
		t.Errorf("Redact = %s", got)
	}
}

// fakeDiscord plays a webhook: it answers with the statuses given, in turn,
// then 204, and keeps what it got.
type fakeDiscord struct {
	mu       sync.Mutex
	statuses []int
	bodies   []payload
	queries  []string
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var p payload
	json.Unmarshal(b, &p)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, p)
	f.queries = append(f.queries, r.URL.RawQuery)
	status := http.StatusNoContent
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	switch status {
	case http.StatusTooManyRequests:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"message": "You are being rate limited.", "retry_after": 0.25, "global": false}`)
	case http.StatusBadRequest:
		w.WriteHeader(status)
		io.WriteString(w, `{"message": "Invalid Form Body", "code": 50035}`)
	default:
		w.WriteHeader(status)
	}
}

func newDiscord(t *testing.T, f *fakeDiscord) (*Discord, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var waits []time.Duration
	d := &Discord{URL: srv.URL + "/api/webhooks/1/secret-token", Avatar: "https://example.com/logo.png",
		sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	return d, &waits
}

func TestPost(t *testing.T) {
	f := &fakeDiscord{statuses: []int{http.StatusTooManyRequests, http.StatusInternalServerError}}
	d, waits := newDiscord(t, f)
	n := &Notifier{Discord: d}
	run := store.Run{Kind: "check", Started: t0, Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{entry("web", "update_available", true)}}}}
	if err := n.Run(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 3 || len(*waits) != 2 || (*waits)[0] != 250*time.Millisecond || (*waits)[1] != serverWait {
		t.Fatalf("%d posts, waits %v", len(f.bodies), *waits)
	}
	p := f.bodies[2]
	if p.Username != "hikifune" || p.AvatarURL != "https://example.com/logo.png" || p.AllowedMentions.Parse == nil || len(p.Embeds) != 1 || f.queries[2] != "wait=true" {
		t.Fatalf("payload %+v, query %q", p, f.queries[2])
	}
	e := p.Embeds[0]
	if e.Title != "a" || e.Color != colorAvailable || e.Footer == nil || e.Footer.Text != "check run" ||
		e.Timestamp != "2026-11-20T01:00:00Z" || e.Description != "**Available**\n• `web` web:1" {
		t.Errorf("embed %+v", e)
	}
}

func TestPostErrorsHideTheToken(t *testing.T) {
	f := &fakeDiscord{statuses: []int{http.StatusBadRequest}}
	d, _ := newDiscord(t, f)
	err := d.Post(context.Background(), embed{Title: "a"})
	if err == nil || !strings.Contains(err.Error(), "Invalid Form Body") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("400: %v", err)
	}

	f = &fakeDiscord{statuses: []int{500, 500, 500, 500}}
	d, _ = newDiscord(t, f)
	if err := d.Post(context.Background(), embed{Title: "a"}); err == nil || !strings.Contains(err.Error(), "gave up after 4 tries") {
		t.Errorf("500s: %v", err)
	}

	// A server that is not Discord and echoes the request.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message": "no such path `+r.URL.String()+`"}`)
	}))
	defer echo.Close()
	err = (&Discord{URL: echo.URL + "/api/webhooks/1/secret-token"}).Post(context.Background(), embed{Title: "a"})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("echoing 404: %v", err)
	}

	// A message cut short through the token keeps none of it.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message": "`+strings.Repeat("x", 190)+`secret-token"}`) // the token crosses byte 200
	}))
	defer cut.Close()
	err = (&Discord{URL: cut.URL + "/api/webhooks/1/secret-token"}).Post(context.Background(), embed{Title: "a"})
	if err == nil || strings.Contains(err.Error(), "secr") {
		t.Errorf("cut message: %v", err)
	}

	// A redirect is not followed, so no other server gets the URL.
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Referer()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer redirect.Close()
	err = (&Discord{URL: redirect.URL + "/api/webhooks/1/secret-token"}).Post(context.Background(), embed{Title: "a"})
	if err == nil || !strings.Contains(err.Error(), "302") || leaked != "" {
		t.Errorf("redirect: %v, the other server got %q", err, leaked)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	gone := srv.URL + "/api/webhooks/1/secret-token"
	srv.Close()
	err = (&Discord{URL: gone}).Post(context.Background(), embed{Title: "a"})
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "webhooks") {
		t.Errorf("unreachable: %v", err)
	}
}

func TestALongRateLimitIsWaitedOutOrRefused(t *testing.T) {
	hits := 0
	retry := "90"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"retry_after": `+retry+`}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var waits []time.Duration
	d := &Discord{URL: srv.URL + "/api/webhooks/1/t", sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	if err := d.Post(context.Background(), embed{Title: "a"}); err != nil || len(waits) != 1 || waits[0] != 90*time.Second || hits != 2 {
		t.Fatalf("90s: err %v, waits %v, hits %d", err, waits, hits)
	}
	hits, retry, waits = 0, "600", nil
	if err := d.Post(context.Background(), embed{Title: "a"}); err == nil || !strings.Contains(err.Error(), "10m0s") || hits != 1 || len(waits) != 0 {
		t.Fatalf("600s: err %v, waits %v, hits %d", err, waits, hits)
	}
	// Send test gives a minute: 90 seconds are told, not waited for.
	hits, retry, waits = 0, "90", nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := d.Post(ctx, embed{Title: "a"}); err == nil || !strings.Contains(err.Error(), "1m30s") || hits != 1 || len(waits) != 0 {
		t.Fatalf("90s within a minute: err %v, waits %v, hits %d", err, waits, hits)
	}
}

func TestALongMessageGoesInParts(t *testing.T) {
	f := &fakeDiscord{}
	d, _ := newDiscord(t, f)
	var es []store.Entry
	for i := range 200 {
		e := entry("container-with-a-long-name-"+strings.Repeat("x", 10), "rolled_back", true)
		e.Reason = strings.Repeat("the new container exited with code 1 ", 2) + string(rune('a'+i%26))
		es = append(es, e)
	}
	run := store.Run{Kind: "update", Started: t0, Hosts: []store.HostRun{{Agent: "a", Containers: es}}}
	if err := (&Notifier{Discord: d}).Run(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) < 3 {
		t.Fatalf("%d posts", len(f.bodies))
	}
	for i, p := range f.bodies {
		e := p.Embeds[0]
		if utf8.RuneCountInString(e.Description) > MaxDescription || !strings.HasPrefix(e.Title, "a (") || !strings.HasSuffix(e.Title, fmt.Sprintf("/%d)", len(f.bodies))) {
			t.Errorf("part %d: title %q, %d characters", i, e.Title, utf8.RuneCountInString(e.Description))
		}
	}
}

func TestABrokenTemplateFallsBackToTheDefault(t *testing.T) {
	f := &fakeDiscord{}
	d, _ := newDiscord(t, f)
	n := &Notifier{Discord: d, Template: func() string { return "{{.Nope}}" }}
	run := store.Run{Kind: "check", Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{entry("web", "update_available", true)}}}}
	if err := n.Run(context.Background(), run); err != nil || len(f.bodies) != 1 || !strings.Contains(f.bodies[0].Embeds[0].Description, "**Available**") {
		t.Fatalf("err %v, posts %+v", err, f.bodies)
	}
}

// A template that fails on a message, or writes nothing for one with
// something to say, gives way to the default for that message.
func TestATemplateThatFailsOnAMessage(t *testing.T) {
	f := &fakeDiscord{}
	d, _ := newDiscord(t, f)
	for _, tpl := range []string{"{{range .Available}}{{.Container}}{{end}}", "{{with .Updated}}{{(index . 0).Container}}{{end}}"} {
		f.bodies = nil
		n := &Notifier{Discord: d, Template: func() string { return tpl }}
		run := store.Run{Kind: "check", Hosts: []store.HostRun{{Agent: "old", Skipped: "the agent runs v0"}}}
		if err := n.Run(context.Background(), run); err != nil || len(f.bodies) != 1 || !strings.Contains(f.bodies[0].Embeds[0].Description, "Left out: the agent runs v0") {
			t.Errorf("%s: err %v, posts %+v", tpl, err, f.bodies)
		}
	}
}

func TestAnEditedTemplateIsUsed(t *testing.T) {
	f := &fakeDiscord{}
	d, _ := newDiscord(t, f)
	n := &Notifier{Discord: d, Template: func() string { return "{{.Kind}} on {{.Host}}: {{len .Available}} new" }}
	run := store.Run{Kind: "check", Hosts: []store.HostRun{{Agent: "a", Containers: []store.Entry{entry("web", "update_available", true)}}}}
	if err := n.Run(context.Background(), run); err != nil || f.bodies[0].Embeds[0].Description != "check on a: 1 new" {
		t.Fatalf("err %v, posts %+v", err, f.bodies)
	}
}

// A status line is never repeated: a server that is not Discord could put
// the URL in its reason phrase.
func TestTheReasonPhraseIsNotRepeated(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			n, _ := c.Read(buf)
			path := strings.Fields(string(buf[:n]))[1]
			io.WriteString(c, "HTTP/1.1 404 "+path+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			c.Close()
		}
	}()
	err = (&Discord{URL: "http://" + ln.Addr().String() + "/api/webhooks/1/secret-token"}).Post(context.Background(), embed{Title: "a"})
	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("err %v", err)
	}
}

// A host with many long failures still gets its message, in parts.
func TestManyLongFailuresGoInParts(t *testing.T) {
	f := &fakeDiscord{}
	d, _ := newDiscord(t, f)
	var es []store.Entry
	for i := range 200 {
		e := entry(fmt.Sprintf("app-%03d", i), "failed", false)
		e.Reason = "registry: " + strings.Repeat("connection reset by peer; ", 17)
		es = append(es, e)
	}
	run := store.Run{Kind: "check", Hosts: []store.HostRun{{Agent: "a", Containers: es}}}
	if err := (&Notifier{Discord: d}).Run(context.Background(), run); err != nil || len(f.bodies) < 20 {
		t.Fatalf("err %v, %d posts", err, len(f.bodies))
	}
}
