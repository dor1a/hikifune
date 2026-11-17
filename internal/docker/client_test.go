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

func newTestClient(t *testing.T, h http.Handler) *Client {
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
		{"minimum raised", "1.60", "1.44", "no longer serves"},
		{"garbage", "abc", "1.12", "not major.minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				if v.APIVersion != tt.api {
					t.Fatalf("APIVersion = %q", v.APIVersion)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
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
