//go:build e2e

package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/agent"
	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/server"
	"github.com/dor1a/hikifune/internal/store"
)

// TestE2EWeb drives the pages against one agent per test daemon. It checks
// M5's "done when": every container on every host appears with protected
// ones marked, and Check now and Update now run and show their results.
func TestE2EWeb(t *testing.T) {
	hosts := strings.Fields(os.Getenv("HIKIFUNE_E2E_HOSTS"))
	if len(hosts) == 0 {
		t.Fatal("HIKIFUNE_E2E_HOSTS is empty; run `make e2e`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	st, err := store.OpenJSON(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(nil, nil).PlainHTTP([]string{"registry:5000", "registry-v2:5000"})
	const token = "e2e-token-e2e-token-e2e-token-e2e-token"
	var names []string
	for _, h := range hosts {
		d, err := docker.New(h)
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(ctx, agent.Config{Docker: d, Registry: reg, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(a.Handler())
		defer srv.Close()
		defer a.Wait()
		name := strings.TrimSuffix(strings.TrimPrefix(h, "tcp://"), ":2375")
		names = append(names, name)
		st.Update(func(s *store.State) error {
			s.Agents = append(s.Agents, store.Agent{Name: name, URL: srv.URL, Token: token})
			return nil
		})
	}
	r := &server.Runner{Store: st, Version: buildinfo.Version, Watch: 5 * time.Second, Poll: 500 * time.Millisecond}
	sched := &server.Schedule{Runner: r, CheckAt: server.Clock{Hour: 0}, UpdateAt: server.Clock{Hour: 1}, Location: time.UTC}
	s, err := New(Config{Store: st, Runner: r, Schedule: sched, Location: time.UTC, Version: buildinfo.Version, Base: ctx,
		Status: func(ctx context.Context, a store.Agent) (protocol.Version, error) {
			c, err := server.NewClient(a.URL, a.Token)
			if err != nil {
				return protocol.Version{}, err
			}
			return c.Version(ctx)
		}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	wait := func() {
		t.Helper()
		for r.Active() != nil {
			if ctx.Err() != nil {
				t.Fatal("the run did not end")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	if _, body := do(t, h, "POST", "/actions/check", url.Values{}); !strings.Contains(body, "Checking every host") {
		t.Fatalf("Check now: %s", body)
	}
	wait()
	_, body := do(t, h, "GET", "/", nil)
	for _, name := range []string{"ui-guard", "srv-db", "upd-noshell", "plain", "local-build"} {
		if !strings.Contains(body, ">"+name) {
			t.Errorf("the containers page has no %s", name)
		}
	}
	for _, n := range names {
		if !strings.Contains(body, `<h2><a href="/hosts/`+n+`">`+n+`</a></h2>`) {
			t.Errorf("the containers page has nothing on %s", n)
		}
	}
	if !strings.Contains(body, `hx-confirm="ui-guard is protected`) {
		t.Error("ui-guard is not offered as a protected update")
	}
	_, hostsPage := do(t, h, "GET", "/hosts/"+names[0]+"/status", nil)
	if !strings.Contains(hostsPage, "agent "+buildinfo.Version) {
		t.Errorf("host status: %s", hostsPage)
	}

	// The page sends the ID it showed; an ID that is not the container's
	// now is taken for a container recreated since, and nothing is updated.
	state, _ := st.Read()
	id := ""
	for _, h := range state.Runs[len(state.Runs)-1].Hosts {
		for _, e := range h.Containers {
			if h.Agent == names[0] && e.Container == "ui-guard" {
				id = e.ID
			}
		}
	}
	if id == "" {
		t.Fatal("the check recorded no ID for ui-guard")
	}
	do(t, h, "POST", "/actions/update", url.Values{"host": {names[0]}, "container": {"ui-guard"}, "id": {"0123456789ab"}})
	wait()
	state, _ = st.Read()
	if e := state.Runs[len(state.Runs)-1].Hosts[0].Containers; len(e) != 1 || e[0].Status == "updated" || e[0].ID != id ||
		!strings.Contains(e[0].Reason, "recreated since the page showed it") {
		t.Fatalf("update with an old ID: %+v", e)
	}

	if _, body := do(t, h, "POST", "/actions/update", url.Values{"host": {names[0]}, "container": {"ui-guard"}, "id": {id}}); !strings.Contains(body, "Updating ui-guard") {
		t.Fatalf("Update now: %s", body)
	}
	wait()
	state, _ = st.Read()
	last := state.Runs[len(state.Runs)-1]
	if last.Container != "ui-guard" || last.Manual != "web" || len(last.Hosts) != 1 || len(last.Hosts[0].Containers) != 1 ||
		last.Hosts[0].Containers[0].Status != "updated" {
		t.Fatalf("the update run: %+v", last)
	}
	_, body = do(t, h, "GET", "/runs/"+last.ID, nil)
	if !strings.Contains(body, `class="badge good">updated`) {
		t.Errorf("run page: %s", body)
	}
	_, body = do(t, h, "GET", "/?q=ui-guard&host="+names[0], nil)
	if !strings.Contains(body, `class="badge good">updated`) || !strings.Contains(body, "1 of") {
		t.Errorf("the containers page does not show the update: %s", body)
	}
}
