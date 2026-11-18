package inventory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dor1a/hikifune/internal/docker"
)

// fakeEngine serves the few Engine API calls Collect makes.
type fakeEngine struct {
	containers map[string]string // id -> inspect document
	images     map[string]string // id -> inspect document
	listed     []string          // ids returned by the list, may include removed ones
	imageCalls atomic.Int32
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/version":
		json.NewEncoder(w).Encode(map[string]string{
			"Version": "24.0.2", "ApiVersion": "1.43", "MinAPIVersion": "1.12", "Os": "linux", "Arch": "amd64",
		})
	case p == "/v1.43/containers/json":
		var out []map[string]string
		for _, id := range f.listed {
			out = append(out, map[string]string{"Id": id})
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/v1.43/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.43/containers/"), "/json")
		doc, ok := f.containers[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such container"}`))
			return
		}
		w.Write([]byte(doc))
	case strings.HasPrefix(p, "/v1.43/images/") && strings.HasSuffix(p, "/json"):
		f.imageCalls.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1.43/images/"), "/json")
		doc, ok := f.images[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such image"}`))
			return
		}
		w.Write([]byte(doc))
	default:
		http.NotFound(w, r)
	}
}

func TestCollect(t *testing.T) {
	f := &fakeEngine{
		listed: []string{"c-web", "c-guard", "c-typo", "c-orphan", "c-gone", "c-local", "c-moved"},
		containers: map[string]string{
			"c-web":    `{"Id":"c-web","Name":"/demo-web-1","Image":"sha256:nginx","Config":{"Image":"nginx:1.27","Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":"web"}},"State":{"Status":"running","Health":{"Status":"healthy"}}}`,
			"c-guard":  `{"Id":"c-guard","Name":"/proxy","Image":"sha256:nginx","Config":{"Image":"nginx:1.27","Labels":{"hikifune.protected":"true"}},"State":{"Status":"running"}}`,
			"c-typo":   `{"Id":"c-typo","Name":"/typo","Image":"sha256:nginx","Config":{"Image":"nginx:1.27","Labels":{"hikifune.protected":"ture"}},"State":{"Status":"exited"}}`,
			"c-orphan": `{"Id":"c-orphan","Name":"/orphan","Image":"sha256:deleted","Config":{"Image":"old:1","Labels":null},"State":{"Status":"created"}}`,
			"c-local":  `{"Id":"c-local","Name":"/local","Image":"sha256:built","Config":{"Image":"local/built:1","Labels":{}},"State":{"Status":"running"}}`,
			"c-moved":  `{"Id":"c-moved","Name":"/moved","Image":"sha256:caddy-old","Config":{"Image":"caddy:2-alpine","Labels":{}},"State":{"Status":"running"}}`,
		},
		images: map[string]string{
			"sha256:nginx": `{"Id":"sha256:nginx","RepoTags":["nginx:1.27"],"RepoDigests":["nginx@sha256:aaa"]}`,
			"sha256:built": `{"Id":"sha256:built","RepoTags":["local/built:1"],"RepoDigests":[]}`,
			// A later pull moved caddy:2-alpine; the old image kept nothing.
			"sha256:caddy-old": `{"Id":"sha256:caddy-old","RepoTags":[],"RepoDigests":[]}`,
			"nginx:1.27":       `{"Id":"sha256:nginx","RepoTags":["nginx:1.27"],"RepoDigests":["nginx@sha256:aaa"]}`,
			"local/built:1":    `{"Id":"sha256:built","RepoTags":["local/built:1"],"RepoDigests":[]}`,
			"caddy:2-alpine":   `{"Id":"sha256:caddy-new","RepoTags":["caddy:2-alpine"],"RepoDigests":["caddy@sha256:bbb"]}`,
		},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := docker.New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	rep, err := Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Docker.Version != "24.0.2" || rep.Docker.RequestedAPI != "1.43" {
		t.Fatalf("docker = %+v", rep.Docker)
	}

	var names []string
	byName := map[string]Container{}
	for _, ct := range rep.Containers {
		names = append(names, ct.Name)
		byName[ct.Name] = ct
	}
	if got := strings.Join(names, ","); got != "demo-web-1,local,moved,orphan,proxy,typo" {
		t.Fatalf("names = %s (the removed container must be left out, the rest sorted)", got)
	}

	web := byName["demo-web-1"]
	if web.ComposeProject != "demo" || web.ComposeService != "web" || web.Protected || web.Health != "healthy" {
		t.Errorf("web = %+v", web)
	}
	if len(web.RepoDigests) != 1 || web.RepoDigests[0] != "nginx@sha256:aaa" || web.ImageID != "sha256:nginx" || web.Image != "nginx:1.27" {
		t.Errorf("web image = %+v", web)
	}
	if p := byName["proxy"]; !p.Protected || p.LabelError != "" {
		t.Errorf("proxy = %+v", p)
	}
	if ty := byName["typo"]; !ty.Protected || ty.LabelError == "" || ty.State != "exited" {
		t.Errorf("typo = %+v", ty)
	}
	o := byName["orphan"]
	if o.ImageError == "" || o.RepoDigests == nil || len(o.RepoDigests) != 0 || o.Labels == nil {
		t.Errorf("orphan = %+v", o)
	}
	if l := byName["local"]; l.RepoDigests == nil || len(l.RepoDigests) != 0 || l.ImageError != "" || l.TagMoved {
		t.Errorf("local = %+v", l)
	}
	if m := byName["moved"]; !m.TagMoved || len(m.RepoDigests) != 0 {
		t.Errorf("moved = %+v, want the tag marked as moved", m)
	}
	if web.TagMoved || o.TagMoved {
		t.Errorf("tag moved on web %v or orphan %v", web.TagMoved, o.TagMoved)
	}
	// By ID: nginx once for three containers, built, deleted and caddy-old.
	// By reference: nginx:1.27 once, local/built:1, caddy:2-alpine and the
	// orphan's old:1, which names nothing any more.
	if n := f.imageCalls.Load(); n != 8 {
		t.Errorf("image inspects = %d, want 8", n)
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"repo_digests":[]`) || strings.Contains(string(b), `"repo_digests":null`) {
		t.Errorf("repo_digests must be an empty list, not null: %s", b)
	}
}

func TestCollectRefusesOldDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Version": "20.10.0", "ApiVersion": "1.41"})
	}))
	defer srv.Close()
	c, _ := docker.New("tcp://" + srv.Listener.Addr().String())
	rep, err := Collect(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("err = %v", err)
	}
	if rep.Docker.Version != "20.10.0" {
		t.Fatalf("the report should still name the daemon: %+v", rep.Docker)
	}
}
