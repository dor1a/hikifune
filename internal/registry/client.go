package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// manifestTypes are asked for in this order of preference. A multi-platform
// image answers with its index, whose digest is what docker pull records.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}

const (
	requestTimeout = 30 * time.Second
	// defaultTokenLife is what the token spec assumes when a token server
	// leaves expires_in out.
	defaultTokenLife = 60 * time.Second
	// tokenMargin renews a token this long before it expires.
	tokenMargin  = 10 * time.Second
	maxErrorBody = 64 << 10
)

// Client reads manifest digests from registries.
type Client struct {
	hc   *http.Client
	keys *Keychain
	now  func() time.Time

	mu    sync.Mutex
	auths map[string]cachedAuth // by registry/repository
}

type cachedAuth struct {
	header  string
	expires time.Time // zero for basic auth, which does not expire
}

// New returns a client that takes credentials from keys. A nil hc gets a
// client with a 30 second timeout per request.
func New(keys *Keychain, hc *http.Client) *Client {
	if keys == nil {
		keys = &Keychain{}
	}
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	// A copy, so the redirect rule never changes the caller's client.
	own := *hc
	own.CheckRedirect = httpsRedirectsOnly
	return &Client{hc: &own, keys: keys, now: time.Now, auths: map[string]cachedAuth{}}
}

// httpsRedirectsOnly refuses a redirect to anything but https, so a request
// that carries credentials is never sent again in plain text, and refuses to
// take credentials to another host or port. Messages name only the scheme
// and host: a redirect URL can carry a ticket in its query.
func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s://%s: only https is followed", req.URL.Scheme, req.URL.Host)
	}
	if via[0].Header.Get("Authorization") != "" && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing a redirect to %s: credentials stay with %s", req.URL.Host, via[0].URL.Host)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// cleanErr keeps only the scheme and host of the URL in a request error,
// which Go fills with the redirect target when a redirect is refused.
func cleanErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	where := "a URL that does not parse"
	if u, perr := url.Parse(ue.URL); perr == nil {
		where = u.Scheme + "://" + u.Host
	}
	// Go quotes a Location it cannot parse in the inner error, query and
	// all, so that error is replaced.
	if strings.Contains(ue.Err.Error(), "failed to parse Location header") {
		return fmt.Errorf("%s %s: the redirect's Location does not parse", ue.Op, where)
	}
	return fmt.Errorf("%s %s: %w", ue.Op, where, ue.Err)
}

// StatusError is a registry or token server answer that is not a success.
type StatusError struct {
	Name    string // registry/repository
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("registry %s: %d %s", e.Name, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Digest asks the registry which manifest digest a tagged reference points
// at, with a HEAD request that downloads nothing.
func (c *Client) Digest(ctx context.Context, ref Reference) (string, error) {
	if ref.Tag == "" {
		return "", fmt.Errorf("%s has no tag to look up", ref)
	}
	resp, err := c.manifest(ctx, ref, ref.Tag)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", c.statusError(ref, resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !validDigest(digest) {
		return "", fmt.Errorf("registry %s: no valid Docker-Content-Digest in the answer (got %q)", ref.Name(), digest)
	}
	return digest, nil
}

// Has reports whether the registry has a manifest with this digest in ref's
// repository, with a HEAD request that downloads nothing. A 404 is false.
func (c *Client) Has(ctx context.Context, ref Reference, digest string) (bool, error) {
	if !validDigest(digest) {
		return false, fmt.Errorf("%q is not a digest", digest)
	}
	resp, err := c.manifest(ctx, ref, digest)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, c.statusError(ref, resp.StatusCode)
}

// manifest sends a HEAD for one tag or digest in ref's repository. A 401 is
// answered once with the credentials the registry's WWW-Authenticate asks
// for: basic auth, or a bearer token from its token server.
func (c *Client) manifest(ctx context.Context, ref Reference, reference string) (*http.Response, error) {
	target := "https://" + apiHost(ref.Registry) + "/v2/" + ref.Repository + "/manifests/" + url.PathEscape(reference)
	key := ref.Name()
	resp, err := c.head(ctx, target, c.cached(key))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	ch, ok := challengeOf(resp.Header)
	if !ok {
		return nil, &StatusError{Name: key, Status: resp.StatusCode,
			Message: "no WWW-Authenticate challenge; " + c.denied(ref)}
	}
	auth, err := c.authorize(ctx, ch, ref)
	if err != nil {
		return nil, err
	}
	if resp, err = c.head(ctx, target, auth.header); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		// The credentials were taken, even if the manifest is missing.
		c.store(key, auth)
	}
	return resp, nil
}

func (c *Client) statusError(ref Reference, status int) error {
	e := &StatusError{Name: ref.Name(), Status: status}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		e.Message = c.denied(ref)
	}
	return e
}

func (c *Client) head(ctx context.Context, target, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, cleanErr(err)
	}
	resp.Body.Close()
	return resp, nil
}

func (c *Client) cached(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.auths[key]
	if !ok || (!a.expires.IsZero() && !c.now().Before(a.expires)) {
		return ""
	}
	return a.header
}

func (c *Client) store(key string, a cachedAuth) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auths[key] = a
}

// authorize builds the Authorization header a challenge asks for.
func (c *Client) authorize(ctx context.Context, ch challenge, ref Reference) (cachedAuth, error) {
	cred, haveCred := c.keys.Get(ref.Registry)
	switch ch.scheme {
	case "basic":
		if !haveCred {
			return cachedAuth{}, &StatusError{Name: ref.Name(), Status: http.StatusUnauthorized,
				Message: "the registry asks for a user name and password" + c.missing(ref.Registry)}
		}
		return cachedAuth{header: "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password))}, nil
	case "bearer":
		var credp *Credentials
		if haveCred {
			credp = &cred
		}
		return c.token(ctx, ch, ref, credp)
	}
	return cachedAuth{}, &StatusError{Name: ref.Name(), Status: http.StatusUnauthorized,
		Message: fmt.Sprintf("auth scheme %q is not supported", ch.scheme)}
}

// denied says who the registry refused, and where credentials could be.
func (c *Client) denied(ref Reference) string {
	if cred, ok := c.keys.Get(ref.Registry); ok {
		return "the registry does not show this repository to " + cred.Username
	}
	return "the registry does not show this repository to anonymous requests" + c.missing(ref.Registry)
}

// missing says where the credentials for a registry are, when not in auths.
func (c *Client) missing(registry string) string {
	if h := c.keys.Helper(registry); h != "" {
		return fmt.Sprintf(", and its credentials are in the %q credential helper, which hikifune does not read", h)
	}
	return ", and the docker config has none for " + registry
}

// token gets a bearer token for pulling ref, anonymously or with cred.
func (c *Client) token(ctx context.Context, ch challenge, ref Reference, cred *Credentials) (cachedAuth, error) {
	realm := ch.params["realm"]
	u, err := url.Parse(realm)
	if realm == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return cachedAuth{}, fmt.Errorf("registry %s: bearer challenge with realm %q", ref.Name(), realm)
	}
	if cred != nil && u.Scheme != "https" {
		return cachedAuth{}, fmt.Errorf("registry %s: the token server %s is not https, so the credentials are not sent", ref.Name(), u.Host)
	}
	q := u.Query()
	if s := ch.params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := ch.params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return cachedAuth{}, err
	}
	if cred != nil {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	now := c.now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return cachedAuth{}, fmt.Errorf("registry %s: token: %w", ref.Name(), cleanErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := "token server " + u.Host
		switch {
		case cred != nil:
			// The body may repeat what was sent, so it is left out.
			msg += " refused the credentials for " + cred.Username
		default:
			msg += ": " + readMessage(resp.Body) + c.missing(ref.Registry)
		}
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			// Not an answer about this repository: the token server itself failed.
			return cachedAuth{}, fmt.Errorf("registry %s: token server %s answered %d %s",
				ref.Name(), u.Host, resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return cachedAuth{}, &StatusError{Name: ref.Name(), Status: resp.StatusCode, Message: msg}
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&t); err != nil {
		return cachedAuth{}, fmt.Errorf("registry %s: token server %s: %w", ref.Name(), u.Host, err)
	}
	tok := t.Token
	if tok == "" {
		tok = t.AccessToken
	}
	if tok == "" {
		return cachedAuth{}, fmt.Errorf("registry %s: token server %s gave no token", ref.Name(), u.Host)
	}
	life := time.Duration(t.ExpiresIn) * time.Second
	if life < defaultTokenLife {
		life = defaultTokenLife
	}
	return cachedAuth{header: "Bearer " + tok, expires: now.Add(life - tokenMargin)}, nil
}

// readMessage returns the first message of a registry error body
// ({"errors":[{"message": ...}]}), or the start of the raw body.
func readMessage(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	var e struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Details string `json:"details"`
	}
	if json.Unmarshal(b, &e) == nil {
		if len(e.Errors) > 0 && e.Errors[0].Message != "" {
			return e.Errors[0].Message
		}
		if e.Details != "" {
			return e.Details
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// challenge is one WWW-Authenticate challenge.
type challenge struct {
	scheme string // lower case
	params map[string]string
}

// challengeOf returns the first bearer or basic challenge in h.
func challengeOf(h http.Header) (challenge, bool) {
	for _, v := range h.Values("Www-Authenticate") {
		if ch, ok := parseChallenge(v); ok && (ch.scheme == "bearer" || ch.scheme == "basic") {
			return ch, true
		}
	}
	return challenge{}, false
}

// parseChallenge reads `Scheme key="value", key=value`. Quoted values may
// escape a character with a backslash.
func parseChallenge(v string) (challenge, bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
	ch := challenge{scheme: strings.ToLower(scheme), params: map[string]string{}}
	rest = strings.TrimSpace(rest)
	for rest != "" {
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		after = strings.TrimSpace(after)
		var val string
		if strings.HasPrefix(after, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				b.WriteByte(after[i])
			}
			val, after = b.String(), after[min(i+1, len(after)):]
			_, after, _ = strings.Cut(after, ",")
		} else {
			val, after, _ = strings.Cut(after, ",")
			val = strings.TrimSpace(val)
		}
		ch.params[k] = val
		rest = strings.TrimSpace(after)
	}
	return ch, ch.scheme != ""
}

// IsNotCheckable reports whether err means the registry will not tell this
// host about the repository: it does not exist there, or the credentials
// are missing or not enough. These are not failures of the check.
func IsNotCheckable(err error) bool {
	var e *StatusError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}
