package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Discord posts to one webhook. The URL holds the webhook's token, so it is
// never written to a log, an error or a page; Redact gives what may be shown.
type Discord struct {
	URL string
	// Avatar is the image URL the messages show; empty keeps the one set on
	// the webhook in Discord.
	Avatar string
	Client *http.Client

	sleep func(context.Context, time.Duration) error
}

type footer struct {
	Text string `json:"text"`
}

type embed struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Color       int     `json:"color"`
	Footer      *footer `json:"footer,omitempty"`
	Timestamp   string  `json:"timestamp,omitempty"`
}

type payload struct {
	Username  string  `json:"username"`
	AvatarURL string  `json:"avatar_url,omitempty"`
	Embeds    []embed `json:"embeds"`
	// An empty parse list keeps a container name or reason from pinging
	// anyone.
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

// Retries for a webhook that is rate limited or briefly down. A rate limit
// longer than maxRetryWait is not waited out.
const (
	maxTries     = 4
	maxRetryWait = 5 * time.Minute
	serverWait   = 2 * time.Second
)

// CheckWebhook tells whether u looks like a Discord webhook URL:
// http(s)://host/api/webhooks/<id>/<token>. The error never repeats it.
func CheckWebhook(u string) error {
	p, err := url.Parse(u)
	if err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != "" {
		if id, token, ok := webhookPath(p.Path); ok && id != "" && token != "" {
			return nil
		}
	}
	return errors.New("not a Discord webhook URL, which looks like https://discord.com/api/webhooks/<id>/<token>")
}

func webhookPath(path string) (id, token string, ok bool) {
	_, rest, ok := strings.Cut(path, "/api/webhooks/")
	if !ok {
		return "", "", false
	}
	id, token, _ = strings.Cut(strings.Trim(rest, "/"), "/")
	return id, token, true
}

// Redact shows a webhook URL without its token.
func Redact(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "…"
	}
	if id, _, ok := webhookPath(p.Path); ok && id != "" {
		return p.Scheme + "://" + p.Host + "/api/webhooks/" + id + "/…"
	}
	return p.Scheme + "://" + p.Host + "/…"
}

// Post sends one embed under the hikifune name. A rate limit is waited out
// as Discord asks, and a server error is tried again shortly.
func (d *Discord) Post(ctx context.Context, e embed) error {
	p := payload{Username: "hikifune", AvatarURL: d.Avatar, Embeds: []embed{e}}
	p.AllowedMentions.Parse = []string{}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// A webhook does not redirect, and following one would hand the token
	// to another server in the Referer header.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	wait := d.sleep
	if wait == nil {
		wait = sleep
	}
	target := d.URL
	if strings.Contains(target, "?") {
		target += "&wait=true"
	} else {
		target += "?wait=true"
	}
	var last error
	for try := 1; try <= maxTries; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return errors.New("the webhook URL does not make a request")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			// A url.Error quotes the URL, token and all.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return errors.New(d.scrub("send to Discord: " + err.Error()))
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// The status code with its standard text: the reason phrase the
		// server sent could carry anything, the URL too.
		status := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			after := retryAfter(resp.Header, msg)
			if after > maxRetryWait {
				return fmt.Errorf("Discord rate limited the webhook for %s; not waiting that long", after.Round(time.Second))
			}
			last = errors.New("Discord rate limited the webhook")
			if err := wait(ctx, after); err != nil {
				return err
			}
		case resp.StatusCode >= 500:
			last = fmt.Errorf("Discord answered %s", status)
			if err := wait(ctx, serverWait); err != nil {
				return err
			}
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			return fmt.Errorf("Discord answered %s; a webhook does not redirect, so the URL is not a Discord webhook", status)
		default:
			if m := discordMessage(msg); m != "" {
				return errors.New(d.scrub(fmt.Sprintf("Discord answered %s: %s", status, m)))
			}
			return fmt.Errorf("Discord answered %s", status)
		}
	}
	return fmt.Errorf("%w; gave up after %d tries", last, maxTries)
}

// retryAfter reads how long Discord asks to wait, from the body's
// retry_after or the Retry-After header, in seconds.
func retryAfter(h http.Header, body []byte) time.Duration {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
	}
	secs := 1.0
	if json.Unmarshal(body, &b) == nil && b.RetryAfter > 0 {
		secs = b.RetryAfter
	} else if v, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && v > 0 {
		secs = v
	}
	return time.Duration(math.Ceil(min(secs, 24*3600)*1000)) * time.Millisecond
}

// discordMessage is the message field of a Discord error body. Anything
// else in the body is left out: a server that is not Discord could echo the
// request back.
func discordMessage(body []byte) string {
	var b struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	m := strings.TrimSpace(b.Message)
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

// scrub takes the webhook URL and its token out of a message.
func (d *Discord) scrub(s string) string {
	if d.URL != "" {
		s = strings.ReplaceAll(s, d.URL, Redact(d.URL))
	}
	if p, err := url.Parse(d.URL); err == nil {
		if _, token, ok := webhookPath(p.Path); ok && token != "" {
			s = strings.ReplaceAll(s, token, "…")
			if esc := url.PathEscape(token); esc != token {
				s = strings.ReplaceAll(s, esc, "…")
			}
		}
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
