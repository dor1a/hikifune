// Package notify sends run results to a Discord webhook (R17 to R19). Each
// host gets one message built from the run record (R31): an embed titled with
// the host, colored by the worst result, whose description comes from a
// template the web UI can edit. A host with nothing to say gets no message.
package notify

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"

	"github.com/dor1a/hikifune/internal/store"
	"github.com/dor1a/hikifune/internal/update"
)

// Item is one container in a message.
type Item struct {
	Container string
	Image     string
	Reason    string
	Protected bool
	// Self marks hikifune's own containers, which it never recreates (R28).
	Self bool
}

// Message is one host's part of a run, the data the template gets.
type Message struct {
	Host string
	// Kind is check or update.
	Kind string
	At   time.Time
	// LeftOut says why the run left the host out (R16).
	LeftOut string
	// Available is filled after a check run, the other three lists after an
	// update run.
	Available  []Item
	Updated    []Item
	RolledBack []Item
	Failed     []Item
	// Reclaimed is the space prune freed, such as "4.8 GB", or empty.
	Reclaimed string
	// Note is an error that came after the containers were listed, such as
	// a failed prune.
	Note string
}

// Empty tells whether the message has nothing to say.
func (m Message) Empty() bool {
	return m.LeftOut == "" && m.Note == "" && m.Reclaimed == "" &&
		len(m.Available)+len(m.Updated)+len(m.RolledBack)+len(m.Failed) == 0
}

// Embed colors, the web UI's error, done and accent colors.
const (
	colorBad       = 0xC64343
	colorUpdated   = 0x2F7A4F
	colorAvailable = 0x2E7DE9
	colorQuiet     = 0x5B6283
)

func (m Message) color() int {
	switch {
	case m.LeftOut != "" || m.Note != "" || len(m.RolledBack) > 0 || len(m.Failed) > 0:
		return colorBad
	case len(m.Updated) > 0:
		return colorUpdated
	case len(m.Available) > 0:
		return colorAvailable
	}
	return colorQuiet
}

// Build makes the messages for a run: one for each host with something to
// say, in the order of the record. Failed lists failed updates and failed
// registry checks alike, each with its reason.
func Build(run store.Run) []Message {
	var out []Message
	for _, h := range run.Hosts {
		m := Message{Host: h.Agent, Kind: run.Kind, At: run.Started}
		if !h.Listed() {
			m.LeftOut = cmp.Or(h.Skipped, h.Error)
			out = append(out, m)
			continue
		}
		m.Note = h.Error
		for _, e := range h.Containers {
			it := Item{Container: e.Container, Image: e.Image, Reason: e.Reason, Protected: e.Protected, Self: e.Self}
			switch {
			case e.Status == string(update.Failed): // registry.Failed reads the same
				m.Failed = append(m.Failed, it)
			case run.Kind != "update":
				if e.Available {
					it.Reason = ""
					m.Available = append(m.Available, it)
				}
			case e.Status == string(update.Updated):
				it.Reason = ""
				m.Updated = append(m.Updated, it)
			case e.Status == string(update.RolledBack):
				m.RolledBack = append(m.RolledBack, it)
			}
		}
		if h.Reclaimed > 0 {
			m.Reclaimed = humanBytes(h.Reclaimed)
		}
		if !m.Empty() {
			out = append(out, m)
		}
	}
	return out
}

// Default is the template hikifune ships, the layout of the messages it
// replaces.
const Default = `{{define "items"}}{{range .}}• ` + "`{{.Container}}`" + ` {{md .Image}}{{if .Protected}} (protected){{end}}{{if .Self}} (hikifune){{end}}{{with .Reason}}
  {{md .}}{{end}}
{{end}}{{end}}
{{- with .LeftOut}}Left out: {{md .}}

{{end}}
{{- with .Available}}**Available**
{{template "items" .}}
{{end}}
{{- with .Updated}}**Updated**
{{template "items" .}}
{{end}}
{{- with .RolledBack}}**Rolled back**
{{template "items" .}}
{{end}}
{{- with .Failed}}**Failed**
{{template "items" .}}
{{end}}
{{- with .Reclaimed}}Reclaimed {{.}}
{{end}}
{{- with .Note}}{{md .}}
{{end}}`

// Limits on a template, which is kept in the state and run for every
// message: its size, what it may write, the numbers it may name, how deep
// its loops nest and how wide printf may pad. Together they keep a template
// from making the server spin or fill its memory.
const (
	MaxTemplate = 10000
	// maxOutput bounds a whole message, which a host with many failures
	// makes long; maxString bounds each string a function makes.
	maxOutput = 1 << 20
	maxString = 64 << 10
	maxNumber = 100
	maxRanges = 2
	maxWidth  = 200
)

var defaultTemplate = sync.OnceValue(func() *template.Template {
	t, err := Parse("")
	if err != nil {
		panic("the default notification template: " + err.Error())
	}
	return t
})

// Parse reads a template; an empty one is Default. It renders a message of
// each kind, to catch errors that only show when the template runs.
func Parse(text string) (*template.Template, error) {
	if strings.TrimSpace(text) == "" {
		text = Default
	}
	if len(text) > MaxTemplate {
		return nil, fmt.Errorf("the template is %d bytes, more than %d", len(text), MaxTemplate)
	}
	t, err := template.New("notification").Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err
	}
	c := checker{t: t, depth: map[string]int{}, active: map[string]bool{}}
	if d, err := c.template(t.Name()); err != nil {
		return nil, err
	} else if d > maxRanges {
		return nil, fmt.Errorf("range is nested %d deep, counting the templates it calls; at most %d", d, maxRanges)
	}
	for _, m := range samples() {
		if _, err := Render(t, m); err != nil {
			return nil, fmt.Errorf("for a %s: %w", m.Host, err)
		}
	}
	if out, _ := Render(t, Sample()); out == "" {
		return nil, errors.New("the template writes nothing for a message with every list filled")
	}
	return t, nil
}

// checker walks a template from its root through the templates it calls,
// refusing large number literals and a template that calls itself, and
// measuring how deep range nests.
type checker struct {
	t      *template.Template
	depth  map[string]int
	active map[string]bool
}

func (c *checker) template(name string) (int, error) {
	if d, ok := c.depth[name]; ok {
		return d, nil
	}
	if c.active[name] {
		return 0, fmt.Errorf("template %q calls itself", name)
	}
	tt := c.t.Lookup(name)
	if tt == nil || tt.Tree == nil {
		return 0, nil
	}
	c.active[name] = true
	d, err := c.node(tt.Tree.Root)
	delete(c.active, name)
	c.depth[name] = d
	return d, err
}

// node is the deepest range nesting under n.
func (c *checker) node(n parse.Node) (int, error) {
	deepest := 0
	visit := func(ns ...parse.Node) error {
		for _, x := range ns {
			d, err := c.node(x)
			if err != nil {
				return err
			}
			deepest = max(deepest, d)
		}
		return nil
	}
	var err error
	switch n := n.(type) {
	case *parse.ListNode:
		if n != nil {
			err = visit(n.Nodes...)
		}
	case *parse.ActionNode:
		err = visit(n.Pipe)
	case *parse.PipeNode:
		if n != nil {
			for _, cmd := range n.Cmds {
				if err = visit(cmd); err != nil {
					break
				}
			}
		}
	case *parse.CommandNode:
		err = visit(n.Args...)
	case *parse.ChainNode:
		err = visit(n.Node)
	case *parse.NumberNode:
		if n.IsComplex || math.Abs(n.Float64) > maxNumber {
			err = fmt.Errorf("the number %s is larger than a template needs (at most %d)", n.Text, maxNumber)
		}
	case *parse.IfNode:
		err = c.branch(&n.BranchNode, &deepest, 0)
	case *parse.WithNode:
		err = c.branch(&n.BranchNode, &deepest, 0)
	case *parse.RangeNode:
		err = c.branch(&n.BranchNode, &deepest, 1)
	case *parse.TemplateNode:
		if err = visit(n.Pipe); err == nil {
			var d int
			d, err = c.template(n.Name)
			deepest = max(deepest, d)
		}
	}
	return deepest, err
}

// branch measures an if, with or range; a range adds one level to its body.
func (c *checker) branch(b *parse.BranchNode, deepest *int, add int) error {
	for _, n := range []parse.Node{b.Pipe, b.List, b.ElseList} {
		if l, ok := n.(*parse.ListNode); ok && l == nil {
			continue
		}
		if p, ok := n.(*parse.PipeNode); ok && p == nil {
			continue
		}
		d, err := c.node(n)
		if err != nil {
			return err
		}
		if n != parse.Node(b.Pipe) {
			d += add
		}
		*deepest = max(*deepest, d)
	}
	return nil
}

// funcs are the functions a template may call. Every one that makes a
// string is bounded, since a variable set again in a loop could double a
// string at each turn; printf also takes small widths only.
var funcs = template.FuncMap{
	"md":       bounded1(Escape),
	"printf":   boundedPrintf,
	"print":    boundedN(fmt.Sprint),
	"println":  boundedN(fmt.Sprintln),
	"html":     boundedN(template.HTMLEscaper),
	"js":       boundedN(template.JSEscaper),
	"urlquery": boundedN(template.URLQueryEscaper),
}

func bound(s string) (string, error) {
	if len(s) > maxString {
		return "", fmt.Errorf("a function made a string of more than %d bytes", maxString)
	}
	return s, nil
}

func bounded1(f func(string) string) func(string) (string, error) {
	return func(s string) (string, error) { return bound(f(s)) }
}

func boundedN(f func(...any) string) func(...any) (string, error) {
	return func(args ...any) (string, error) { return bound(f(args...)) }
}

// boundedPrintf is printf without star widths and with small widths and
// precisions only.
func boundedPrintf(format string, args ...any) (string, error) {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		num := 0
	spec:
		for i++; i < len(format); i++ {
			switch c := format[i]; {
			case c == '*':
				return "", errors.New("printf: * widths are not allowed")
			case c >= '0' && c <= '9':
				num = num*10 + int(c-'0')
				if num > maxWidth {
					return "", fmt.Errorf("printf: widths and precisions go up to %d", maxWidth)
				}
			case c == '.':
				num = 0
			case strings.IndexByte("+-# []", c) >= 0:
			default:
				break spec
			}
		}
	}
	return bound(fmt.Sprintf(format, args...))
}

// errTooLong stops a template that writes too much.
var errTooLong = fmt.Errorf("the template writes more than %d bytes", maxOutput)

type boundedWriter struct{ strings.Builder }

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.Len()+len(p) > maxOutput {
		return 0, errTooLong
	}
	return w.Builder.Write(p)
}

// Render runs the template for one message.
func Render(t *template.Template, m Message) (string, error) {
	var b boundedWriter
	if err := t.Execute(&b, m); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// Escape keeps Discord from reading a name or reason as markdown: a value
// stays on one line, marks are escaped, and a mark that counts only at the
// start of a line is escaped there too. Mentions are kept from pinging by
// the payload, not here.
func Escape(s string) string {
	s = strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
	var b strings.Builder
	for i, r := range s {
		if strings.ContainsRune(`\*_~|[]<`+"`", r) || i == 0 && strings.ContainsRune("#->+", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Sample is a message with every part filled, for checking a template and
// for Send test.
func Sample() Message {
	return Message{
		Host: "example-host", Kind: "update", At: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC),
		Available: []Item{{Container: "monitoring-grafana-1", Image: "grafana/grafana:latest", Protected: true}},
		Updated:   []Item{{Container: "monitoring-prometheus-1", Image: "docker.io/prom/prometheus:main"}},
		RolledBack: []Item{{Container: "apps-web-1", Image: "ghcr.io/example/web:latest",
			Reason: "exited with code 1 within the 2m watch"}},
		Failed: []Item{{Container: "apps-tautulli-1", Image: "ghcr.io/tautulli/tautulli:latest",
			Reason: "create the new container: port 8181 is already allocated"}},
		Reclaimed: "4.8 GB",
	}
}

// samples are the kinds of message a run makes, each with only its own
// parts, for checking that a template works on every one.
func samples() []Message {
	all := Sample()
	return []Message{
		all,
		{Host: "check message", Kind: "check", At: all.At, Available: all.Available},
		{Host: "update message", Kind: "update", At: all.At, Updated: all.Updated},
		{Host: "message of failures", Kind: "update", At: all.At, RolledBack: all.RolledBack, Failed: all.Failed},
		{Host: "left-out host", Kind: "check", At: all.At, LeftOut: "the agent runs v0.1.0 and the server v0.2.0"},
		{Host: "message with a note", Kind: "update", At: all.At, Reclaimed: all.Reclaimed, Note: "prune: the daemon refused"},
	}
}

// MaxDescription is Discord's limit for an embed description, in characters.
const MaxDescription = 4096

// split cuts text into parts of at most limit characters, at line ends when
// it can. A line cut in the middle is never cut between a backslash and the
// mark it escapes.
func split(text string, limit int) []string {
	var parts []string
	var cur strings.Builder
	n := 0
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
		n = 0
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		l := utf8.RuneCountInString(line)
		if n+l > limit {
			flush()
		}
		for l > limit {
			r := []rune(line)
			cut := limit
			slashes := 0
			for k := cut - 1; k >= 0 && r[k] == '\\'; k-- {
				slashes++
			}
			if slashes%2 == 1 {
				cut--
			}
			parts = append(parts, string(r[:cut]))
			line, l = string(r[cut:]), l-cut
		}
		cur.WriteString(line)
		n += l
	}
	flush()
	return parts
}

// Notifier sends the messages of the runs it is given.
type Notifier struct {
	Discord *Discord
	// Template gives the template in force; empty means Default.
	Template func() string
	Log      *slog.Logger
}

// Run sends one message for each host of run with something to say. A
// template that no longer parses gives way to Default, so a run is never
// left unreported.
func (n *Notifier) Run(ctx context.Context, run store.Run) error {
	text := ""
	if n.Template != nil {
		text = n.Template()
	}
	t, err := Parse(text)
	if err != nil {
		n.logError("the notification template does not work; using the default", err)
		t = defaultTemplate()
	}
	var errs []error
	for _, m := range Build(run) {
		if err := n.Send(ctx, t, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Host, err))
		}
	}
	return errors.Join(errs...)
}

// Send renders one message and posts it, in parts when it is too long for
// one embed. When the template fails on the message, or writes nothing for
// a message with something to say, the default template writes it.
func (n *Notifier) Send(ctx context.Context, t *template.Template, m Message) error {
	text, err := Render(t, m)
	if (err != nil || text == "") && !m.Empty() && t != defaultTemplate() {
		n.logError("the notification template failed on a message; using the default", cmp.Or(err, errors.New("it wrote nothing")))
		text, err = Render(defaultTemplate(), m)
	}
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	parts := split(text, MaxDescription)
	for i, part := range parts {
		title := m.Host
		if len(parts) > 1 {
			title = fmt.Sprintf("%s (%d/%d)", m.Host, i+1, len(parts))
		}
		e := embed{Title: title, Description: part, Color: m.color(), Footer: &footer{Text: m.Kind + " run"}}
		if !m.At.IsZero() {
			e.Timestamp = m.At.UTC().Format(time.RFC3339)
		}
		if err := n.Discord.Post(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) logError(msg string, err error) {
	if n.Log != nil {
		n.Log.Error(msg, "err", err)
	}
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
