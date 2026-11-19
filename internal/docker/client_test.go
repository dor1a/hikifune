package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewHosts(t *testing.T) {
	tests := []struct {
		host    string
		base    string
		wantErr bool
	}{
		{"", "http://docker", false},
		{"unix:///var/run/docker.sock", "http://docker", false},
		{"tcp://proxy:2375", "http://proxy:2375", false},
		{"tcp://proxy", "http://proxy:2375", false},
		{"http://10.0.0.5:2375", "http://10.0.0.5:2375", false},
		{"tcp://[::1]", "http://[::1]:2375", false},
		{"unix://", "", true},
		{"tcp://", "", true},
		{"tcp://proxy:2375/v1.43", "", true},
		{"https://proxy:2376", "", true},
		{"ssh://host", "", true},
	}
	for _, tt := range tests {
		c, err := New(tt.host)
		if (err != nil) != tt.wantErr {
			t.Errorf("New(%q) err = %v, want error %v", tt.host, err, tt.wantErr)
			continue
		}
		if err == nil && c.base != tt.base {
			t.Errorf("New(%q) base = %q, want %q", tt.host, c.base, tt.base)
		}
	}
}

// newTestClient serves h behind a daemon that reports API 1.43 at /version.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	return newTestClientAt(t, "1.43", h)
}

func newTestClientAt(t *testing.T, api string, h http.Handler) *Client {
	t.Helper()
	return newRawTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			json.NewEncoder(w).Encode(map[string]string{"Version": "test", "ApiVersion": api, "MinAPIVersion": "1.12"})
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// newRawTestClient serves h as it is, /version too.
func newRawTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"Version":"24.0.2","ApiVersion":"1.43"}`)
			return
		}
		if r.URL.Path != "/v1.43/_ping" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "OK")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c, err := New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAPI(t *testing.T) {
	tests := []struct {
		name    string
		api     string
		min     string
		wantErr string
	}{
		{"docker 24", "1.43", "1.12", ""},
		{"docker 29", "1.56", "1.40", ""},
		{"no minimum reported", "1.45", "", ""},
		{"too old", "1.41", "1.12", "older than"},
		{"minimum above 1.43", "1.60", "1.44", ""},
		{"garbage", "abc", "1.12", "not major.minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRawTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/version" {
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{
					"Version": "x", "ApiVersion": tt.api, "MinAPIVersion": tt.min,
				})
			}))
			v, err := c.CheckAPI(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if v.APIVersion != tt.api || c.APIVersion() != tt.api {
					t.Fatalf("reported %q, client asks for %q", v.APIVersion, c.APIVersion())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if c.APIVersion() != "" {
				t.Fatalf("client asks for %q after a refused daemon", c.APIVersion())
			}
		})
	}
}

// Every versioned request asks for the daemon's own version, which is asked
// for once.
func TestRequestsUseTheDaemonsVersion(t *testing.T) {
	var versions, pings int
	c := newRawTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			versions++
			io.WriteString(w, `{"Version":"29.1.0","ApiVersion":"1.56","MinAPIVersion":"1.40"}`)
		case "/v1.56/_ping":
			pings++
			io.WriteString(w, "OK")
		default:
			http.NotFound(w, r)
		}
	}))
	for range 3 {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if versions != 1 || pings != 3 || c.APIVersion() != "1.56" {
		t.Fatalf("version asked %d times, %d pings, client asks for %q", versions, pings, c.APIVersion())
	}
}

func TestRequestsRefuseAnOldDaemon(t *testing.T) {
	c := newRawTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"Version":"20.10.0","ApiVersion":"1.41"}`)
			return
		}
		t.Errorf("request to %s on a refused daemon", r.URL.Path)
	}))
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompareAPIVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.43", "1.43", 0},
		{"1.9", "1.43", -1},
		{"1.56", "1.43", 1},
		{"2.0", "1.99", 1},
	}
	for _, tt := range tests {
		got, err := CompareAPIVersions(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Errorf("CompareAPIVersions(%q, %q) = %d, %v, want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.43/containers/gone/json":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container: gone"}`)
		case "/v1.43/images/odd/json":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "blocked by proxy\n")
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()

	_, err := c.ContainerInspect(ctx, "gone")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
	if !strings.Contains(err.Error(), "No such container: gone") {
		t.Fatalf("message lost: %v", err)
	}

	_, err = c.ImageInspect(ctx, "odd")
	if IsNotFound(err) {
		t.Fatal("403 reported as not found")
	}
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Message != "blocked by proxy" {
		t.Fatalf("err = %#v", err)
	}
}

func TestContainerInspectKeepsRaw(t *testing.T) {
	doc := `{"Id":"abc","Name":"/web","Image":"sha256:1","Config":{"Image":"nginx:1","Labels":{"a":"b"},"Healthcheck":{"Test":["CMD","/app","healthcheck"]}},"State":{"Status":"running","Health":{"Status":"healthy"}},"HostConfig":{"DeviceRequests":[{"Driver":"","Count":-1,"Capabilities":[["gpu"]]}]}}`
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.43/containers/web/json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, doc)
	}))
	ct, err := c.ContainerInspect(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if ct.Name != "/web" || ct.Config.Image != "nginx:1" || ct.State.Health == nil || ct.State.Health.Status != "healthy" {
		t.Fatalf("parsed = %+v", ct)
	}
	if string(ct.Raw) != doc {
		t.Fatalf("Raw changed:\n%s", ct.Raw)
	}
}

func TestInspectRejectsEmptyDocuments(t *testing.T) {
	for _, body := range []string{"null", "{}"} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		if _, err := c.ContainerInspect(context.Background(), "x"); err == nil {
			t.Errorf("container inspect of %s: no error", body)
		}
		if _, err := c.ImageInspect(context.Background(), "x"); err == nil {
			t.Errorf("image inspect of %s: no error", body)
		}
	}
}

func TestContainerListAll(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `[{"Id":"1","Names":["/a"],"State":"exited","Labels":null}]`)
	}))
	list, err := c.ContainerList(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "all=1" || len(list) != 1 || list[0].State != "exited" {
		t.Fatalf("query %q, list %+v", gotQuery, list)
	}
}

func TestNetworkConnect(t *testing.T) {
	var got map[string]json.RawMessage
	var path, ctype string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ctype = r.URL.Path, r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	endpoint := json.RawMessage(`{"Aliases":["db"],"IPAMConfig":{"IPv4Address":"172.20.0.5"}}`)
	if err := c.NetworkConnect(context.Background(), "back end", "app", endpoint); err != nil {
		t.Fatal(err)
	}
	if path != "/v1.43/networks/back end/connect" || ctype != "application/json" {
		t.Fatalf("path %q, content type %q", path, ctype)
	}
	if string(got["Container"]) != `"app"` || string(got["EndpointConfig"]) != string(endpoint) {
		t.Fatalf("body = %s / %s", got["Container"], got["EndpointConfig"])
	}

	got = nil
	if err := c.NetworkConnect(context.Background(), "n", "app", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["EndpointConfig"]; ok {
		t.Fatal("nil endpoint was sent")
	}
}

// call is one request a recordingClient received.
type call struct {
	method, path, query, body, auth string
}

func recordingClient(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*Client, *[]call) {
	t.Helper()
	var calls []call
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("X-Registry-Auth")})
		answer(w, r)
	}))
	return c, &calls
}

func TestContainerLifecycle(t *testing.T) {
	c, calls := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/create"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"new1","Warnings":[]}`)
		case strings.HasSuffix(r.URL.Path, "/start") && strings.Contains(r.URL.Path, "/up/"):
			w.WriteHeader(http.StatusNotModified)
		case strings.HasSuffix(r.URL.Path, "/stop") && strings.Contains(r.URL.Path, "/down/"):
			w.WriteHeader(http.StatusNotModified)
		case strings.HasSuffix(r.URL.Path, "/rename") && strings.Contains(r.URL.Path, "/taken/"):
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"name in use"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	body := json.RawMessage(`{"Image":"busybox:1","HostConfig":{"Binds":["d:/d"]}}`)
	created, err := c.ContainerCreate(ctx, "web-hikifune-new", body)
	if err != nil || created.ID != "new1" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if err := c.ContainerStart(ctx, "new1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, "up"); err != nil {
		t.Fatalf("start of a running container: %v", err)
	}
	if err := c.ContainerStop(ctx, "down"); err != nil {
		t.Fatalf("stop of a stopped container: %v", err)
	}
	if err := c.ContainerRename(ctx, "taken", "web"); !IsConflict(err) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if err := c.ContainerRemove(ctx, "old1", false); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerRemove(ctx, "new1", true); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"POST", "/v1.43/containers/create", "name=web-hikifune-new", string(body), ""},
		{"POST", "/v1.43/containers/new1/start", "", "", ""},
		{"POST", "/v1.43/containers/up/start", "", "", ""},
		{"POST", "/v1.43/containers/down/stop", "", "", ""},
		{"POST", "/v1.43/containers/taken/rename", "name=web", "", ""},
		{"DELETE", "/v1.43/containers/old1", "", "", ""},
		{"DELETE", "/v1.43/containers/new1", "force=1", "", ""},
	}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %+v", *calls)
	}
	for i, w := range want {
		if (*calls)[i] != w {
			t.Errorf("call %d = %+v, want %+v", i, (*calls)[i], w)
		}
	}
}

func TestImagePull(t *testing.T) {
	stream := ""
	c, calls := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, stream)
	})
	ctx := context.Background()

	stream = `{"status":"Pulling from library/busybox"}` + "\n" + `{"status":"Digest: sha256:1"}` + "\n"
	if err := c.ImagePull(ctx, "docker.io/library/busybox", "1.37", "e30"); err != nil {
		t.Fatal(err)
	}
	got := (*calls)[0]
	if got.path != "/v1.43/images/create" || got.query != "fromImage=docker.io%2Flibrary%2Fbusybox&tag=1.37" || got.auth != "e30" {
		t.Fatalf("pull call = %+v", got)
	}

	// The daemon answers 200 and reports the failure in the stream.
	stream = `{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}` + "\n"
	if err := c.ImagePull(ctx, "localhost:5000/x", "1", ""); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("err = %v", err)
	}
	if (*calls)[1].auth != "" {
		t.Fatalf("anonymous pull sent X-Registry-Auth %q", (*calls)[1].auth)
	}
}

func TestImageRemoveConflict(t *testing.T) {
	c, calls := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"message":"image is being used by stopped container"}`)
	})
	err := c.ImageRemove(context.Background(), "sha256:old")
	if !IsConflict(err) || (*calls)[0].method != "DELETE" || (*calls)[0].path != "/v1.43/images/sha256:old" {
		t.Fatalf("err = %v, calls %+v", err, *calls)
	}
}

// The Engine API never redirects; a redirect is not followed, so a pull's
// credentials never go to another host.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var elsewhere bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = true
	}))
	defer other.Close()
	c, _ := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	})
	if err := c.ImagePull(context.Background(), "docker.io/library/busybox", "1", "c2VjcmV0"); err == nil {
		t.Fatal("a redirect was taken as success")
	}
	if elsewhere {
		t.Fatal("the redirect was followed")
	}
}
