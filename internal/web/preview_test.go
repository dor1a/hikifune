package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDiscordHTML(t *testing.T) {
	for in, want := range map[string]string{
		"**Updated**\n• `web` <b>&":        "<strong>Updated</strong><br>• <code>web</code> &lt;b&gt;&amp;",
		`a \*b\* \_c\_`:                    "a *b* _c_",
		"*one* **two** __three__ ~~four~~": "<em>one</em> <strong>two</strong> <u>three</u> <s>four</s>",
		"a ** b":                           "a ** b",
		"**a** b **":                       "<strong>a</strong> b **",
		"`**x**`":                          "<code>**x**</code>",
		"```\n<x>\n```":                    "<pre><code>&lt;x&gt;\n</code></pre>",
		"un`closed":                        "un`closed",
		`<script>"'`:                       "&lt;script&gt;&#34;&#39;",
		"_one_ and srv_db_1":               "<em>one</em> and srv_db_1",
		"（_one_） 한_글_":                     "（<em>one</em>） 한_글_",
		"***both***":                       "<strong><em>both</em></strong>",
		"**a *b** c*":                      "<strong>a <em>b</em></strong><em> c</em>",
		"__**x__**":                        "<u><strong>x</strong></u>",
	} {
		if got := string(discordHTML(in)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// The preview renders the template in the editor, saved or not, on one
// sample message, as Send would post it (R19).
func TestTemplatePreview(t *testing.T) {
	st := fixture(t)
	s, _ := newServer(t, st, &fakeRunner{}, nil)
	h := s.Handler()
	post := func(form url.Values, script bool) (int, string) {
		req := httptest.NewRequest("POST", "/settings/template/preview", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if script {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// The page's script gets the embed alone: the sample host as the title,
	// the result's color, the run in the footer.
	code, body := post(url.Values{"template": {"**{{.Host}}** <b> {{len .Updated}}"}, "sample": {"3"}}, true)
	if code != http.StatusOK || strings.Contains(body, "<h1") ||
		!strings.Contains(body, `<div class="embed-title">example-host</div>`) ||
		!strings.Contains(body, "<strong>example-host</strong> &lt;b&gt; 1") ||
		!strings.Contains(body, `<div class="embed tone-updated">`) || strings.Contains(body, "style=") || !strings.Contains(body, "update run") {
		t.Errorf("preview: %d\n%s", code, body)
	}
	// A sample out of range is the first.
	if _, body := post(url.Values{"template": {"{{.Kind}}"}, "sample": {"99"}}, true); !strings.Contains(body, `<div class="embed-body">update</div>`) {
		t.Errorf("sample 99:\n%s", body)
	}
	// A template that does not parse says why.
	if _, body := post(url.Values{"template": {"{{.Nope"}}, true); !strings.Contains(body, "The template does not work") {
		t.Errorf("broken template:\n%s", body)
	}
	// A template that writes nothing for a message with something to say
	// gives way to the default, as Send does.
	if _, body := post(url.Values{"template": {"{{with .Updated}}x{{end}}"}, "sample": {"1"}}, true); !strings.Contains(body, "the default template writes it") ||
		!strings.Contains(body, "🆕 <strong>Available:</strong>") {
		t.Errorf("fallback:\n%s", body)
	}

	// Without the script, the whole page comes back with the template kept
	// in the editor and nothing saved.
	if code, body := post(url.Values{"template": {"kept {{.Host}}"}}, false); code != http.StatusOK ||
		!strings.Contains(body, `<h1 class="title">Settings</h1>`) || !strings.Contains(body, ">kept {{.Host}}</textarea>") ||
		!strings.Contains(body, `<div class="embed-body">kept example-host</div>`) {
		t.Errorf("preview without the script: %d\n%s", code, body)
	}
	if got, _ := st.Read(); got.Settings.Template != "" {
		t.Errorf("the preview saved the template: %q", got.Settings.Template)
	}
	// By default the hosts of a run share a message: the preview shows a
	// second sample host in the same one. Per host, each has its own.
	if _, body := post(url.Values{"template": {"{{.Host}}"}}, true); strings.Count(body, `<div class="discord-msg">`) != 1 ||
		!strings.Contains(body, `<div class="embed-title">example-host-2</div>`) {
		t.Errorf("a run's message:\n%s", body)
	}
	if _, body := post(url.Values{"template": {"{{.Host}}"}, "messages": {"host"}}, true); strings.Count(body, `<div class="discord-msg">`) != 1 ||
		strings.Contains(body, "example-host-2") {
		t.Errorf("one host's message:\n%s", body)
	}
	// Settings shows the saved template's preview from the start.
	if _, body := do(t, h, "GET", "/settings", nil); !strings.Contains(body, `<div class="embed tone-bad">`) ||
		!strings.Contains(body, `<option value="0" selected>Message with every part</option>`) {
		t.Errorf("settings page has no preview")
	}
}
