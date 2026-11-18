package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistry serves manifest HEADs and a token endpoint like the
// distribution spec and the Docker token spec describe.
type fakeRegistry struct {
	srv       *httptest.Server
	auth      string // "", "basic" or "bearer"
	user      string
	pass      string
	private   map[string]bool // bearer: repositories an anonymous token cannot see
	broken    map[string]int  // repository -> status to answer with
	noDigest  map[string]bool // repositories answered without Docker-Content-Digest
	manifests map[string]fakeManifest
	redirect  map[string]string // repository -> where an authorized HEAD is redirected
	realm     string            // bearer realm, the fake's own /token when empty
	tokenCode int               // status the token server answers with, 200 when 0
	tokenBody string

	mu         sync.Mutex
	heads      int
	tokenCalls int
}

type fakeManifest struct{ mediaType, digest string }

// otherDigest is what the fake answers when the client did not accept the
// manifest's media type, as a registry falls back to another manifest.
const otherDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func newFakeRegistry(t *testing.T, auth string) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{auth: auth, user: "alice", pass: "s3cret",
		private: map[string]bool{}, broken: map[string]int{}, noDigest: map[string]bool{},
		manifests: map[string]fakeManifest{}, redirect: map[string]string{}}
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// host is the registry part of a reference to this fake.
func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeRegistry) ref(t *testing.T, s string) Reference {
	t.Helper()
	r, err := ParseReference(f.host() + "/" + s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fakeRegistry) client(keys *Keychain) *Client { return New(keys, f.srv.Client()) }

func (f *fakeRegistry) keys() *Keychain {
	return &Keychain{auths: map[string]Credentials{f.host(): {f.user, f.pass}}}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.token(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	i := strings.LastIndex(rest, "/manifests/")
	if !ok || i < 0 {
		http.NotFound(w, r)
		return
	}
	repo, tag := rest[:i], rest[i+len("/manifests/"):]
	if r.Method != http.MethodHead {
		http.Error(w, "HEAD only", http.StatusMethodNotAllowed)
		return
	}
	f.heads++
	if !f.authorized(w, r, repo) {
		return
	}
	if loc := f.redirect[repo]; loc != "" {
		http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
		return
	}
	if status := f.broken[repo]; status != 0 {
		w.WriteHeader(status)
		return
	}
	m, ok := f.manifests[repo+":"+tag]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	digest := m.digest
	if !strings.Contains(r.Header.Get("Accept"), m.mediaType) {
		digest = otherDigest
	}
	if !f.noDigest[repo] {
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.Header().Set("Content-Type", m.mediaType)
}

func (f *fakeRegistry) authorized(w http.ResponseWriter, r *http.Request, repo string) bool {
	switch f.auth {
	case "basic":
		if u, p, ok := r.BasicAuth(); ok && u == f.user && p == f.pass {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	case "bearer":
		h := r.Header.Get("Authorization")
		if h == "Bearer user-token" || (h == "Bearer anon-token" && !f.private[repo]) {
			return true
		}
		realm := f.realm
		if realm == "" {
			realm = f.srv.URL + "/token"
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s",service="fake",scope="repository:%s:pull"`, realm, repo))
	default:
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

func (f *fakeRegistry) token(w http.ResponseWriter, r *http.Request) {
	f.tokenCalls++
	if f.tokenCode != 0 {
		w.WriteHeader(f.tokenCode)
		w.Write([]byte(f.tokenBody))
		return
	}
	q := r.URL.Query()
	if q.Get("service") != "fake" || !strings.HasPrefix(q.Get("scope"), "repository:") {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	u, p, ok := r.BasicAuth()
	switch {
	case ok && u == f.user && p == f.pass:
		json.NewEncoder(w).Encode(map[string]any{"access_token": "user-token", "expires_in": 300})
	case ok:
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"bad credentials"}]}`))
	default:
		json.NewEncoder(w).Encode(map[string]any{"token": "anon-token"})
	}
}

func (f *fakeRegistry) counts() (heads, tokens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads, f.tokenCalls
}

func TestDigestMediaTypes(t *testing.T) {
	f := newFakeRegistry(t, "")
	for i, mt := range manifestTypes {
		f.manifests[fmt.Sprintf("team/app:%d", i)] = fakeManifest{mt, digestA}
	}
	c := f.client(nil)
	for i, mt := range manifestTypes {
		got, err := c.Digest(context.Background(), f.ref(t, fmt.Sprintf("team/app:%d", i)))
		if err != nil || got != digestA {
			t.Errorf("%s: Digest = %q, %v, want %s", mt, got, err, digestA)
		}
	}
}

func TestDigestBasic(t *testing.T) {
	f := newFakeRegistry(t, "basic")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[2], digestA}
	ref := f.ref(t, "team/app:1")

	if got, err := f.client(f.keys()).Digest(context.Background(), ref); err != nil || got != digestA {
		t.Fatalf("with credentials: %q, %v", got, err)
	}
	_, err := f.client(nil).Digest(context.Background(), ref)
	if !IsNotCheckable(err) || !strings.Contains(err.Error(), "has none for "+f.host()) {
		t.Errorf("without credentials: %v, want not checkable naming the missing credentials", err)
	}
	wrong := &Keychain{auths: map[string]Credentials{f.host(): {"alice", "nope"}}}
	if _, err := f.client(wrong).Digest(context.Background(), ref); !IsNotCheckable(err) {
		t.Errorf("wrong password: %v, want not checkable", err)
	}
	helper := &Keychain{store: "desktop"}
	if _, err := f.client(helper).Digest(context.Background(), ref); err == nil || !strings.Contains(err.Error(), `"desktop" credential helper`) {
		t.Errorf("credentials in a helper: %v, want the helper named", err)
	}
}

func TestDigestBearerAnonymousAndTokenCache(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.manifests["library/alpine:3"] = fakeManifest{manifestTypes[0], digestA}
	c := f.client(nil)
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	ref := f.ref(t, "library/alpine:3")

	for range 2 {
		if got, err := c.Digest(context.Background(), ref); err != nil || got != digestA {
			t.Fatalf("Digest = %q, %v", got, err)
		}
	}
	if heads, tokens := f.counts(); heads != 3 || tokens != 1 {
		t.Errorf("after two checks: %d HEADs and %d tokens, want 3 and 1 (the token is reused)", heads, tokens)
	}
	now = now.Add(defaultTokenLife) // past the 60 s token, less the margin
	if _, err := c.Digest(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if heads, tokens := f.counts(); heads != 5 || tokens != 2 {
		t.Errorf("after the token expired: %d HEADs and %d tokens, want 5 and 2", heads, tokens)
	}
}

func TestDigestBearerPrivate(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.private["me/secret"] = true
	f.manifests["me/secret:1"] = fakeManifest{manifestTypes[1], digestB}
	ref := f.ref(t, "me/secret:1")

	_, err := f.client(nil).Digest(context.Background(), ref)
	if !IsNotCheckable(err) || !strings.Contains(err.Error(), "anonymous") {
		t.Errorf("anonymous: %v, want not checkable", err)
	}
	if got, err := f.client(f.keys()).Digest(context.Background(), ref); err != nil || got != digestB {
		t.Errorf("with credentials: %q, %v", got, err)
	}
	wrong := &Keychain{auths: map[string]Credentials{f.host(): {"alice", "nope"}}}
	_, err = f.client(wrong).Digest(context.Background(), ref)
	if !IsNotCheckable(err) || !strings.Contains(err.Error(), "refused the credentials for alice") {
		t.Errorf("wrong password: %v, want the refusal named", err)
	}
}

func TestDigestErrors(t *testing.T) {
	f := newFakeRegistry(t, "")
	f.manifests["team/nodigest:1"] = fakeManifest{manifestTypes[2], digestA}
	f.noDigest["team/nodigest"] = true
	f.broken["team/down"] = http.StatusServiceUnavailable
	c := f.client(nil)

	if _, err := c.Digest(context.Background(), f.ref(t, "team/missing:1")); !IsNotCheckable(err) {
		t.Errorf("unknown repository: %v, want not checkable", err)
	}
	if _, err := c.Digest(context.Background(), f.ref(t, "team/down:1")); err == nil || IsNotCheckable(err) {
		t.Errorf("503: %v, want a failure", err)
	}
	if _, err := c.Digest(context.Background(), f.ref(t, "team/nodigest:1")); err == nil || IsNotCheckable(err) {
		t.Errorf("no digest header: %v, want a failure", err)
	}
	pinned := f.ref(t, "team/app@"+digestA)
	if _, err := c.Digest(context.Background(), pinned); err == nil {
		t.Error("a reference without a tag was looked up")
	}
}

func TestCredentialsNeverGoOverHTTP(t *testing.T) {
	var withAuth atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withAuth.Add(1)
		}
	}))
	defer plain.Close()

	// The authorized HEAD is redirected to http.
	f := newFakeRegistry(t, "basic")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[2], digestA}
	f.redirect["team/app"] = plain.URL + "/v2/team/app/manifests/1"
	if _, err := f.client(f.keys()).Digest(context.Background(), f.ref(t, "team/app:1")); err == nil || !strings.Contains(err.Error(), "only https") {
		t.Errorf("redirect to http: %v, want it refused", err)
	}
	// The bearer realm is http.
	g := newFakeRegistry(t, "bearer")
	g.realm = plain.URL + "/token"
	g.manifests["team/app:1"] = fakeManifest{manifestTypes[2], digestA}
	if _, err := g.client(g.keys()).Digest(context.Background(), g.ref(t, "team/app:1")); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Errorf("http realm: %v, want the credentials kept back", err)
	}
	if n := withAuth.Load(); n != 0 {
		t.Errorf("%d requests with credentials reached the http server", n)
	}
}

func TestTokenServerAnswers(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.private["me/secret"] = true
	f.manifests["me/secret:1"] = fakeManifest{manifestTypes[0], digestA}
	ref := f.ref(t, "me/secret:1")

	f.tokenCode, f.tokenBody = http.StatusUnauthorized, `{"errors":[{"message":"wrong password `+f.pass+`"}]}`
	_, err := f.client(f.keys()).Digest(context.Background(), ref)
	if !IsNotCheckable(err) || strings.Contains(err.Error(), f.pass) {
		t.Errorf("refused credentials: %v, want not checkable without the body", err)
	}
	f.tokenCode = http.StatusNotFound
	if _, err := f.client(nil).Digest(context.Background(), ref); err == nil || IsNotCheckable(err) {
		t.Errorf("token server 404: %v, want a failure, not an answer about the repository", err)
	}
}

func TestRefusalsNameTheCredentialHelper(t *testing.T) {
	f := newFakeRegistry(t, "")
	f.broken["team/forbidden"] = http.StatusForbidden
	f.broken["team/nochallenge"] = http.StatusUnauthorized
	helper := &Keychain{helpers: map[string]string{f.host(): "pass"}}
	for _, repo := range []string{"team/forbidden", "team/nochallenge"} {
		_, err := f.client(helper).Digest(context.Background(), f.ref(t, repo+":1"))
		if !IsNotCheckable(err) || !strings.Contains(err.Error(), `"pass" credential helper`) {
			t.Errorf("%s: %v, want not checkable naming the helper", repo, err)
		}
	}
}

func TestRedirectsKeepCredentialsAndTicketsBack(t *testing.T) {
	var withAuth atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withAuth.Add(1)
		}
	}))
	defer other.Close()

	f := newFakeRegistry(t, "basic")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[2], digestA}
	f.redirect["team/app"] = other.URL + "/v2/team/app/manifests/1" // same host, another port
	if _, err := f.client(f.keys()).Digest(context.Background(), f.ref(t, "team/app:1")); err == nil || !strings.Contains(err.Error(), "credentials stay with") {
		t.Errorf("redirect to another port: %v, want it refused", err)
	}
	if n := withAuth.Load(); n != 0 {
		t.Errorf("%d requests with credentials reached the other server", n)
	}

	f.redirect["team/app"] = "http://127.0.0.1:1/v2/team/app/manifests/1?ticket=SECRET"
	_, err := f.client(nil).Digest(context.Background(), f.ref(t, "team/app:1"))
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "ticket") {
		t.Errorf("redirect with a ticket: %v, want it refused without the query", err)
	}
}

func TestAMissingTagKeepsTheToken(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.private["me/secret"] = true
	c := f.client(f.keys())
	for _, tag := range []string{"gone1", "gone2"} {
		if _, err := c.Digest(context.Background(), f.ref(t, "me/secret:"+tag)); !IsNotCheckable(err) {
			t.Fatalf("%s: %v, want not checkable", tag, err)
		}
	}
	if _, tokens := f.counts(); tokens != 1 {
		t.Errorf("%d token requests for two missing tags, want 1", tokens)
	}
}
