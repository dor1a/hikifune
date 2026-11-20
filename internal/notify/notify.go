// Package notify sends run results to a Discord webhook (R17 to R19). The
// messages are built from the run record (R31): by default one for the run,
// an embed with every host under its name, or, set per host, one for each
// host, an embed titled with the host. Either is colored by the worst result
// in it, and a template the web UI can edit writes each host's text. A host
// with nothing to say is left out.
package notify

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
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
	// sample marks Sample, the message with every part: it shows in the
	// main color, not in the color of its worst result.
	sample bool
}

// Empty tells whether the message has nothing to say.
func (m Message) Empty() bool {
	return m.LeftOut == "" && m.Note == "" && m.Reclaimed == "" &&
		len(m.Available)+len(m.Updated)+len(m.RolledBack)+len(m.Failed) == 0
}

// Embed colors, the web UI's error, done and accent colors, and the main
// color, the dark theme's accent: Tokyo Night's blue.
const (
	colorBad       = 0xC64343
	colorUpdated   = 0x2F7A4F
	colorAvailable = 0x2E7DE9
	colorQuiet     = 0x5B6283
	colorMain      = 0x7AA2F7
)

// Tones of a message, the worst result in it, which picks its color.
// ToneMain is the sample with every part's, which is no result.
const (
	ToneBad       = "bad"
	ToneUpdated   = "updated"
	ToneAvailable = "available"
	ToneQuiet     = "quiet"
	ToneMain      = "main"
)

// toneRank orders the tones from the mildest, so a shared message takes the
// worst of its hosts. The sample with every part outranks them all.
var toneRank = map[string]int{ToneQuiet: 0, ToneAvailable: 1, ToneUpdated: 2, ToneBad: 3, ToneMain: 4}

func (m Message) tone() string {
	switch {
	case m.sample:
		return ToneMain
	case m.LeftOut != "" || m.Note != "" || len(m.RolledBack) > 0 || len(m.Failed) > 0:
		return ToneBad
	case len(m.Updated) > 0:
		return ToneUpdated
	case len(m.Available) > 0:
		return ToneAvailable
	}
	return ToneQuiet
}

func (m Message) color() int { return toneColor(m.tone()) }

func toneColor(tone string) int {
	switch tone {
	case ToneMain:
		return colorMain
	case ToneBad:
		return colorBad
	case ToneUpdated:
		return colorUpdated
	case ToneAvailable:
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
		if h.Pruned || h.Reclaimed > 0 {
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
//
// The image goes in a code span as it is: an image reference holds no
// backtick, and an escape would show inside the span.
const Default = `{{define "items"}}{{range .}}• **{{md .Container}}**{{with .Image}} ` + "`{{.}}`" + `{{end}}{{if .Protected}} (protected){{end}}{{if .Self}} (hikifune){{end}}{{with .Reason}}
  {{md .}}{{end}}
{{end}}{{end}}
{{- with .LeftOut}}⚠️ **Left out:** {{md .}}

{{end}}
{{- with .Available}}🆕 **Available:**
{{template "items" .}}
{{end}}
{{- with .Updated}}✅ **Updated:**
{{template "items" .}}
{{end}}
{{- with .RolledBack}}🔁 **Rolled back:**
{{template "items" .}}
{{end}}
{{- with .Failed}}❌ **Failed:**
{{template "items" .}}
{{end}}
{{- with .Note}}{{md .}}
{{end}}
{{- with .Reclaimed}}Total reclaimed space: {{.}}
{{end}}`

// Limits on a template, which is kept in the state and run for every
// message: its size, what it may write, the numbers it may name, how deep
// its loops nest, how much work one message may take and how wide printf
// may pad. Together they keep a template from making the server spin or
// fill its memory.
const (
	MaxTemplate = 10000
	// maxOutput bounds a whole message, which a host with many failures
	// makes long; maxString bounds each string a function makes.
	maxOutput = 1 << 20
	maxString = 64 << 10
	maxNumber = 100
	maxRanges = 2
	// maxSteps bounds the templates entered and the range turns of one
	// message. A few templates that each call the next several times take
	// no loop, yet multiply the calls.
	maxSteps = 20000
	maxWidth = 200
)

// stepFunc is the function counted at each step. Parse puts a call to it at
// the top of every template and range body; Render gives each message its
// own count.
const stepFunc = "hikifune_step"

var errTooMuchWork = fmt.Errorf("the template enters templates and turns ranges more than %d times for one message", maxSteps)

var defaultTemplate = sync.OnceValue(func() *template.Template {
	t, err := Parse("")
	if err != nil {
		panic("the default notification template: " + err.Error())
	}
	return t
})

// Parse reads a template to save; an empty one is Default. On top of Load,
// it renders a message of each kind, to catch errors that only show when the
// template runs.
func Parse(text string) (*template.Template, error) {
	t, err := Load(text)
	if err != nil {
		return nil, err
	}
	// The messages checked are the ones the preview shows, the second host
	// of a run included, so a template that fails in the preview is never
	// saved.
	for _, k := range sampleKinds() {
		for _, m := range SampleRun(k.Message) {
			if _, err := Render(t, m); err != nil {
				return nil, fmt.Errorf("for a %s on %s: %w", k.Name, m.Host, err)
			}
		}
	}
	if out, _ := Render(t, Sample()); out == "" {
		return nil, errors.New("the template writes nothing for a message with every list filled")
	}
	return t, nil
}

// Load reads a saved template to send with; an empty one is Default. It
// keeps the limits but does not render the samples again: a template saved
// under older checks still sends, and a message it fails on gives way to
// Default alone.
func Load(text string) (*template.Template, error) {
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
	for _, tt := range t.Templates() {
		if tt.Tree != nil {
			countSteps(tt.Tree, tt.Tree.Root)
		}
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

// countSteps puts a call to stepFunc at the top of list, a template's root
// or a range body, and of every range body under it.
func countSteps(tr *parse.Tree, list *parse.ListNode) {
	if list == nil {
		return
	}
	rangeBodies(tr, list)
	pos := list.Pos
	call := &parse.ActionNode{NodeType: parse.NodeAction, Pos: pos, Pipe: &parse.PipeNode{NodeType: parse.NodePipe, Pos: pos,
		Cmds: []*parse.CommandNode{{NodeType: parse.NodeCommand, Pos: pos,
			Args: []parse.Node{parse.NewIdentifier(stepFunc).SetTree(tr).SetPos(pos)}}}}}
	list.Nodes = append([]parse.Node{call}, list.Nodes...)
}

// rangeBodies finds the range bodies under list, inside if and with too.
func rangeBodies(tr *parse.Tree, list *parse.ListNode) {
	if list == nil {
		return
	}
	for _, n := range list.Nodes {
		switch n := n.(type) {
		case *parse.IfNode:
			rangeBodies(tr, n.List)
			rangeBodies(tr, n.ElseList)
		case *parse.WithNode:
			rangeBodies(tr, n.List)
			rangeBodies(tr, n.ElseList)
		case *parse.RangeNode:
			countSteps(tr, n.List)
			rangeBodies(tr, n.ElseList)
		}
	}
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
			case c == '[':
				// An argument index, such as %20[1]s, is not a width.
				for i < len(format) && format[i] != ']' {
					i++
				}
				num = 0
			case strings.IndexByte("+-# ", c) >= 0:
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

// Render runs a template from Parse for one message. It runs a copy that
// counts the steps of this message alone, so messages may render at once.
func Render(t *template.Template, m Message) (string, error) {
	c, err := t.Clone()
	if err != nil {
		return "", err
	}
	steps := 0
	c.Funcs(template.FuncMap{stepFunc: func() (string, error) {
		if steps++; steps > maxSteps {
			return "", errTooMuchWork
		}
		return "", nil
	}})
	var b boundedWriter
	if err := c.Execute(&b, m); err != nil {
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
		sample: true,
		Host:   "example-host", Kind: "update", At: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC),
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
		{Host: "check message of failures", Kind: "check", At: all.At, Failed: all.Failed},
		{Host: "update message", Kind: "update", At: all.At, Updated: all.Updated},
		{Host: "message of failures", Kind: "update", At: all.At, RolledBack: all.RolledBack, Failed: all.Failed},
		{Host: "update message of failures only", Kind: "update", At: all.At, Failed: all.Failed},
		{Host: "update message of rollbacks only", Kind: "update", At: all.At, RolledBack: all.RolledBack},
		{Host: "left-out host", Kind: "check", At: all.At, LeftOut: "the agent runs v0.1.0 and the server v0.2.0"},
		{Host: "message with a note", Kind: "update", At: all.At, Reclaimed: all.Reclaimed, Note: "prune: the daemon refused"},
	}
}

// SampleRun is the messages of a run with two hosts, first and a second
// host of the same kind, for Send test and the preview when a run's hosts
// share messages.
func SampleRun(first Message) []Message {
	all := Sample()
	second := Message{Host: "example-host-2", Kind: first.Kind, At: first.At}
	if first.Kind == "check" {
		second.Available = all.Available
	} else {
		second.Updated, second.Reclaimed = all.Updated, "1.2 GB"
	}
	return []Message{first, second}
}

// SampleKind is one kind of message a run makes, named, for a preview.
type SampleKind struct {
	Name    string
	Message Message
}

// SampleKinds are the messages a template is checked on, each under the
// sample host name, named for the preview's options.
func SampleKinds() []SampleKind {
	out := sampleKinds()
	for i, k := range out {
		out[i].Name = strings.ToUpper(k.Name[:1]) + k.Name[1:]
	}
	return out
}

// sampleKinds are SampleKinds named as an error names them.
func sampleKinds() []SampleKind {
	var out []SampleKind
	for i, m := range samples() {
		name := m.Host
		if i == 0 {
			name = "message with every part"
		}
		m.Host = Sample().Host
		out = append(out, SampleKind{Name: name, Message: m})
	}
	return out
}

// MaxDescription is Discord's limit for an embed description, in characters.
const MaxDescription = 4096

// split cuts text into parts of at most limit characters, at line ends when
// it can. A line cut in the middle is never cut between a backslash and the
// mark it escapes.
func split(text string, limit int) []string {
	return splitEach(text, func(int) int { return limit })
}

// splitEach is split with a limit for each part, by its number from 0.
func splitEach(text string, limitOf func(part int) int) []string {
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
		if n+l > limitOf(len(parts)) {
			flush()
		}
		for l > limitOf(len(parts)) {
			r := []rune(line)
			cut := limitOf(len(parts))
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
	// PerHost, when set and true, gives each host its own message. Otherwise
	// the hosts of a run share one, as Shared lays it out.
	PerHost func() bool
	Log     *slog.Logger
}

// Run sends the messages of run, one for each host with something to say,
// as Deliver groups them. A template that no longer parses gives way to
// Default, so a run is never left unreported.
func (n *Notifier) Run(ctx context.Context, run store.Run) error {
	text := ""
	if n.Template != nil {
		text = n.Template()
	}
	t, err := Load(text)
	if err != nil {
		n.logError("the notification template does not work; using the default", err)
		t = defaultTemplate()
	}
	return n.Deliver(ctx, t, Build(run), n.PerHost != nil && n.PerHost())
}

// Embed is one embed of a message as Send posts it. Tone names its Color.
type Embed struct {
	Host, Title, Description, Footer string
	Color                            int
	Tone                             string
	At                               time.Time
}

func (e Embed) wire() embed {
	w := embed{Title: e.Title, Description: e.Description, Color: e.Color, Footer: &footer{Text: e.Footer}}
	if !e.At.IsZero() {
		w.Timestamp = e.At.UTC().Format(time.RFC3339)
	}
	return w
}

// Embeds renders m as Send posts it, in parts when it is too long for one
// embed. When t fails on m, or writes nothing for a message with something
// to say, the default template writes it, and fallback says why. No embeds
// and no error mean nothing is sent.
func Embeds(t *template.Template, m Message) (out []Embed, fallback, err error) {
	text, fallback, err := write(t, m)
	if err != nil || text == "" {
		return nil, fallback, err
	}
	parts := split(text, MaxDescription)
	for i, part := range parts {
		title := m.Host
		if len(parts) > 1 {
			title = fmt.Sprintf("%s (%d/%d)", m.Host, i+1, len(parts))
		}
		out = append(out, Embed{Host: m.Host, Title: title, Description: part, Color: m.color(), Tone: m.tone(),
			Footer: m.Kind + " run", At: m.At})
	}
	return out, fallback, nil
}

// write renders m with t, or with the default template when t fails on m or
// writes nothing for a message with something to say; fallback says why.
func write(t *template.Template, m Message) (text string, fallback, err error) {
	text, err = Render(t, m)
	if (err != nil || text == "") && !m.Empty() && t != defaultTemplate() {
		fallback = cmp.Or(err, errors.New("it wrote nothing"))
		text, err = Render(defaultTemplate(), m)
	}
	return text, fallback, err
}

// rule is the line above and below each host's name in a shared message.
var rule = strings.Repeat("─", 30)

// maxHeadName bounds a host's name where it heads its part of a shared
// message, so a name of any length leaves room for the text under it. Names
// added by the agent command are far shorter.
const maxHeadName = 100

// blankTop puts an empty line between a shared embed's title and its first
// host. The line holds a zero-width space, which Discord shows as empty.
const blankTop = "\u200b\n"

// Shared lays out the hosts of one run as one embed, titled with the run and
// colored by the worst host. Each host's part is its name between two rules, then
// what the template writes for it, with an empty line above and below the
// rules. Text longer than an embed takes goes on in more embeds with numbered
// titles, cut between hosts, and inside a host's text only when that host
// alone is too long; each part of it then goes under the host's name, marked
// continued after the first. A host the template writes nothing for is left out. A
// host whose part cannot be written is left out too and named in err;
// fallbacks name the hosts the default template wrote, and why.
func Shared(t *template.Template, msgs []Message) (out []Embed, fallbacks []error, err error) {
	var errs []error
	type chunk struct {
		text  string
		hosts []string
	}
	var chunks []chunk
	var cur chunk
	limit := MaxDescription - utf8.RuneCountInString(blankTop)
	tone, kind, at := ToneQuiet, "", time.Time{}
	for _, m := range msgs {
		text, fallback, err := write(t, m)
		if fallback != nil {
			fallbacks = append(fallbacks, fmt.Errorf("%s: %w", m.Host, fallback))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Host, err))
			continue
		}
		if text == "" {
			continue
		}
		if kind == "" {
			kind, at = m.Kind, m.At
		}
		if toneRank[m.tone()] > toneRank[tone] {
			tone = m.tone()
		}
		name := m.Host
		if r := []rune(name); len(r) > maxHeadName {
			name = string(r[:maxHeadName]) + "…"
		}
		head := func(more string) string {
			return rule + "\n🖥 **" + Escape(name) + "**" + more + "\n" + rule + "\n\n"
		}
		pieces := []string{head("") + text}
		if utf8.RuneCountInString(pieces[0]) > limit {
			// The text goes on in more parts, each under the host's name, so
			// a part read alone still says whose it is, and the name never
			// ends a part without the text it heads.
			first, cont := head(""), head(" (continued)")
			pieces = nil
			for i, b := range splitEach(text, func(part int) int {
				if part == 0 {
					return limit - utf8.RuneCountInString(first)
				}
				return limit - utf8.RuneCountInString(cont)
			}) {
				if i == 0 {
					pieces = append(pieces, first+b)
				} else {
					pieces = append(pieces, cont+b)
				}
			}
		}
		for _, p := range pieces {
			if cur.text != "" && utf8.RuneCountInString(cur.text)+2+utf8.RuneCountInString(p) > limit {
				chunks = append(chunks, cur)
				cur = chunk{}
			}
			if cur.text != "" {
				cur.text += "\n\n"
			}
			cur.text += p
			if !slices.Contains(cur.hosts, m.Host) {
				cur.hosts = append(cur.hosts, m.Host)
			}
		}
	}
	if cur.text != "" {
		chunks = append(chunks, cur)
	}
	title := ""
	if kind != "" {
		title = strings.ToUpper(kind[:1]) + kind[1:] + " run"
	}
	for i, c := range chunks {
		e := Embed{Host: strings.Join(c.hosts, ", "), Title: title, Description: blankTop + c.text,
			Color: toneColor(tone), Tone: tone, Footer: kind + " run", At: at}
		if len(chunks) > 1 {
			e.Title = fmt.Sprintf("%s (%d/%d)", title, i+1, len(chunks))
		}
		out = append(out, e)
	}
	return out, fallbacks, errors.Join(errs...)
}

// Discord's limits on one message: how many embeds, and how many characters
// all of them hold together.
const (
	maxEmbeds     = 10
	maxEmbedChars = 6000
)

// Pack puts embeds into messages, in order, each message within Discord's
// limits. One embed is always within them: its text is at most
// MaxDescription characters.
func Pack(es []Embed) [][]Embed {
	var out [][]Embed
	var cur []Embed
	chars := 0
	for _, e := range es {
		n := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer)
		if len(cur) > 0 && (len(cur) == maxEmbeds || chars+n > maxEmbedChars) {
			out = append(out, cur)
			cur, chars = nil, 0
		}
		cur = append(cur, e)
		chars += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Deliver posts msgs: each host its own message, in parts when long, when
// perHost; otherwise one message with every host, as Shared lays it out, and
// more only past what Discord takes in one (see Pack). A host whose message
// fails to post is named in the error.
func (n *Notifier) Deliver(ctx context.Context, t *template.Template, msgs []Message, perHost bool) error {
	var errs []error
	if !perHost {
		shared, fallbacks, err := Shared(t, msgs)
		for _, f := range fallbacks {
			n.logError("the notification template failed on a message; using the default", f)
		}
		if err != nil {
			errs = append(errs, err)
		}
		return errors.Join(append(errs, n.postShared(ctx, shared))...)
	}
	for _, m := range msgs {
		es, fallback, err := Embeds(t, m)
		if fallback != nil {
			n.logError("the notification template failed on a message; using the default", fmt.Errorf("%s: %w", m.Host, fallback))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Host, err))
			continue
		}
		for _, e := range es {
			if err := n.Discord.Post(ctx, e.wire()); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", m.Host, err))
				break
			}
		}
	}
	return errors.Join(errs...)
}

// postShared posts the embeds of a shared message, as few to a post as
// Discord takes.
func (n *Notifier) postShared(ctx context.Context, shared []Embed) error {
	var errs []error
	for _, batch := range Pack(shared) {
		var wire []embed
		var hosts []string
		for _, e := range batch {
			wire = append(wire, e.wire())
			for h := range strings.SplitSeq(e.Host, ", ") {
				if !slices.Contains(hosts, h) {
					hosts = append(hosts, h)
				}
			}
		}
		if err := n.Discord.PostAll(ctx, wire); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", strings.Join(hosts, ", "), err))
		}
	}
	return errors.Join(errs...)
}

// Send posts one message as its own, as Embeds lays it out.
func (n *Notifier) Send(ctx context.Context, t *template.Template, m Message) error {
	return n.Deliver(ctx, t, []Message{m}, true)
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
