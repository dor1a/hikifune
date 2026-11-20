package web

import (
	"html"
	"regexp"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dor1a/hikifune/internal/notify"
)

// templatePreview is the notification preview beside the template editor:
// one sample message, or a run of two sample hosts when hosts share
// messages, laid out in the Discord messages Deliver would post.
type templatePreview struct {
	Samples  []string
	Sample   int
	PerHost  bool
	Messages [][]previewEmbed
	Fallback string // why the default template wrote it, when it did
	Error    string // why the template cannot be saved
	Nothing  bool   // the template writes nothing for this message
}

// previewEmbed is one embed of the preview. Tone picks the color of the
// bar by a class: the page's policy allows no inline style.
type previewEmbed struct {
	Title, Footer, At, Tone string
	Description             template.HTML
}

// preview renders text, the template in the editor, for the sample numbered
// sample (out of range means the first), grouped as perHost says.
func (s *Server) preview(text string, sample int, perHost bool) templatePreview {
	kinds := notify.SampleKinds()
	if sample < 0 || sample >= len(kinds) {
		sample = 0
	}
	p := templatePreview{Sample: sample, PerHost: perHost}
	for _, k := range kinds {
		p.Samples = append(p.Samples, k.Name)
	}
	t, err := notify.Parse(text)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	first := kinds[sample].Message
	embeds, fallback, err := notify.Embeds(t, first)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Nothing = len(embeds) == 0
	if fallback != nil {
		p.Fallback = first.Host + ": " + fallback.Error()
	}
	if perHost {
		for _, e := range embeds {
			p.Messages = append(p.Messages, []previewEmbed{s.previewEmbed(e)})
		}
		return p
	}
	all, fallbacks, err := notify.Shared(t, notify.SampleRun(first))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	if p.Fallback == "" && len(fallbacks) > 0 {
		p.Fallback = fallbacks[0].Error()
	}
	for _, batch := range notify.Pack(all) {
		var msg []previewEmbed
		for _, e := range batch {
			msg = append(msg, s.previewEmbed(e))
		}
		p.Messages = append(p.Messages, msg)
	}
	return p
}

func (s *Server) previewEmbed(e notify.Embed) previewEmbed {
	pe := previewEmbed{Title: e.Title, Footer: e.Footer, Tone: e.Tone, Description: discordHTML(e.Description)}
	if !e.At.IsZero() {
		pe.At = s.local(e.At)
	}
	return pe
}

// previewTemplate answers the editor as it changes (R19). A request from the
// page's script gets the preview alone; a plain form post, without the
// script, gets the whole Settings page with the template kept unsaved.
func (s *Server) previewTemplate(w http.ResponseWriter, r *http.Request) {
	text := strings.ReplaceAll(r.FormValue("template"), "\r\n", "\n")
	sample, _ := strconv.Atoi(r.FormValue("sample"))
	perHost := perHostForm(r)
	p := s.preview(text, sample, perHost)
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, http.StatusOK, "templatepreview", page{Data: p})
		return
	}
	st, ok := s.state(w)
	if !ok {
		return
	}
	d := s.settingsData(st)
	d.Template, d.DefaultTemplate, d.Preview, d.PerHost = text, strings.TrimSpace(text) == "" || strings.TrimSpace(text) == strings.TrimSpace(notify.Default), p, perHost
	s.render(w, http.StatusOK, "settings", s.newPage(r, st, "settings", "Settings", d))
}

// ruleRun is a line drawn with box-drawing characters, such as the rules
// around each host's name. The preview's font draws them wider than
// Discord's, so a rule that fits on one line in Discord would wrap here.
var ruleRun = regexp.MustCompile(`[─━═]{10,}`)

// rules keeps each rule in escaped text on one line, cut at the box's edge.
func rules(escaped string) string {
	return ruleRun.ReplaceAllString(escaped, `<span class="rule">$0</span>`)
}

// discordHTML renders the part of Discord's markdown a notification is
// likely to use, for the preview: backslash escapes, `code`, ```code
// blocks```, **bold**, *italic* or _italic_, __underline__, ~~strike~~ and
// line breaks. The rest stays text. Every piece of text is escaped and the
// tags always nest, so the result is safe whatever the template writes. It
// is close to Discord, not the same.
func discordHTML(s string) template.HTML {
	type token struct {
		kind string // text, code, block, mark, br
		val  string
	}
	var toks []token
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			toks = append(toks, token{"text", text.String()})
			text.Reset()
		}
	}
	// A single underscore inside a word, as in a_b_c, is not a mark. The
	// characters on either side are read whole, so punctuation outside ASCII,
	// such as a full-width bracket, is not a letter.
	word := func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	before := func(i int) rune {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		return r
	}
	after := func(i int) rune {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return r
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\*_~`|[]<>#-+.!()", s[i+1]) >= 0:
			text.WriteByte(s[i+1])
			i += 2
		case strings.HasPrefix(s[i:], "```"):
			if end := strings.Index(s[i+3:], "```"); end >= 0 {
				flush()
				toks = append(toks, token{"block", strings.TrimPrefix(s[i+3:i+3+end], "\n")})
				i += 3 + end + 3
			} else {
				text.WriteString("```")
				i += 3
			}
		case s[i] == '`':
			if end := strings.IndexByte(s[i+1:], '`'); end > 0 {
				flush()
				toks = append(toks, token{"code", s[i+1 : i+1+end]})
				i += 1 + end + 1
			} else {
				text.WriteByte('`')
				i++
			}
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "__"), strings.HasPrefix(s[i:], "~~"):
			flush()
			toks = append(toks, token{"mark", s[i : i+2]})
			i += 2
		case s[i] == '*', s[i] == '_' && !(word(before(i)) && word(after(i+1))):
			flush()
			toks = append(toks, token{"mark", s[i : i+1]})
			i++
		case s[i] == '\n':
			flush()
			toks = append(toks, token{"br", ""})
			i++
		default:
			text.WriteByte(s[i])
			i++
		}
	}
	flush()
	// A mark counts only when it has a partner; the last of an odd number
	// is plain text.
	count := map[string]int{}
	for _, t := range toks {
		if t.kind == "mark" {
			count[t.val]++
		}
	}
	for i, seen := 0, map[string]int{}; i < len(toks); i++ {
		if t := toks[i]; t.kind == "mark" {
			if seen[t.val]++; seen[t.val] == count[t.val] && count[t.val]%2 == 1 {
				toks[i].kind = "text"
			}
		}
	}
	tags := map[string]string{"**": "strong", "*": "em", "_": "em", "__": "u", "~~": "s"}
	var b strings.Builder
	var open []string // the marks open now, innermost last
	push := func(m string) {
		b.WriteString("<" + tags[m] + ">")
		open = append(open, m)
	}
	// shut closes m. The marks opened after it close first and open again,
	// so the tags nest even when the marks cross.
	shut := func(m string) {
		at := slices.Index(open, m)
		for j := len(open) - 1; j >= at; j-- {
			b.WriteString("</" + tags[open[j]] + ">")
		}
		again := slices.Clone(open[at+1:])
		open = open[:at]
		for _, a := range again {
			push(a)
		}
	}
	for i := 0; i < len(toks); {
		t := toks[i]
		switch t.kind {
		case "text":
			b.WriteString(rules(html.EscapeString(t.val)))
		case "code":
			b.WriteString("<code>" + html.EscapeString(t.val) + "</code>")
		case "block":
			b.WriteString("<pre><code>" + html.EscapeString(t.val) + "</code></pre>")
		case "br":
			b.WriteString("<br>")
		case "mark":
			// Marks side by side, as in ***, close innermost first, then
			// the rest open in the order written.
			j := i
			for j < len(toks) && toks[j].kind == "mark" {
				j++
			}
			group := toks[i:j]
			var closing, opening []string
			for _, g := range group {
				if slices.Contains(open, g.val) && !slices.Contains(closing, g.val) {
					closing = append(closing, g.val)
				} else {
					opening = append(opening, g.val)
				}
			}
			slices.SortFunc(closing, func(x, y string) int { return slices.Index(open, y) - slices.Index(open, x) })
			for _, m := range closing {
				shut(m)
			}
			for _, m := range opening {
				if slices.Contains(open, m) {
					shut(m)
				} else {
					push(m)
				}
			}
			i = j
			continue
		}
		i++
	}
	for j := len(open) - 1; j >= 0; j-- {
		b.WriteString("</" + tags[open[j]] + ">")
	}
	return template.HTML(b.String())
}
