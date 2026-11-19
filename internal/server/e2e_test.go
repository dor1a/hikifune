//go:build e2e

package server

import (
	"context"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/agent"
	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/store"
)

// TestE2EServer runs the server against one agent per test daemon, each
// served over HTTP by this process, with the containers that
// deploy/compose/e2e/server.sh creates. It checks M4's "done when": both
// hosts checked, one update at a time per host with dependencies first,
// every result recorded, and a failed digest skipped at the next run.
func TestE2EServer(t *testing.T) {
	hosts := strings.Fields(os.Getenv("HIKIFUNE_E2E_HOSTS"))
	if len(hosts) == 0 {
		t.Fatal("HIKIFUNE_E2E_HOSTS is empty; run `make e2e`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	st, err := store.OpenJSON(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(nil, nil).PlainHTTP([]string{"registry:5000", "registry-v2:5000"})
	const token = "e2e-token-e2e-token-e2e-token-e2e-token"
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
		name := strings.TrimPrefix(h, "tcp://")
		st.Update(func(s *store.State) error {
			s.Agents = append(s.Agents, store.Agent{Name: name, URL: srv.URL, Token: token})
			return nil
		})
	}
	r := &Runner{Store: st, Version: buildinfo.Version, Watch: 5 * time.Second, Poll: 500 * time.Millisecond}

	run, err := r.Run(ctx, KindCheck)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Hosts) != len(hosts) {
		t.Fatalf("%d hosts in the check run", len(run.Hosts))
	}
	for _, h := range run.Hosts {
		if h.Error != "" || h.Skipped != "" {
			t.Fatalf("%s: error %q, skipped %q", h.Agent, h.Error, h.Skipped)
		}
		for _, name := range []string{"srv-db", "srv-web", "srv-bad", "srv-guard", "srv-self"} {
			if e := find(t, h, name); e.Status != string(registry.UpdateAvailable) {
				t.Errorf("%s %s: %+v", h.Agent, name, e)
			}
		}
		if !find(t, h, "srv-guard").Protected || !find(t, h, "srv-self").Self {
			t.Errorf("%s: guard and self not marked", h.Agent)
		}
	}

	run, err = r.Run(ctx, KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range run.Hosts {
		t.Logf("%s: Docker %s, reclaimed %d", h.Agent, h.Docker, h.Reclaimed)
		if h.Error != "" {
			t.Errorf("%s: %s", h.Agent, h.Error)
		}
		for name, want := range map[string]string{"srv-db": "updated", "srv-web": "updated",
			"srv-bad": "rolled_back", "srv-guard": "skipped", "srv-self": "skipped"} {
			e := find(t, h, name)
			t.Logf("  %s: %s %s", name, e.Status, e.Reason)
			if e.Status != want {
				t.Errorf("%s %s: %s (%s), want %s", h.Agent, name, e.Status, e.Reason, want)
			}
		}
		if db, web := find(t, h, "srv-db"), find(t, h, "srv-web"); db.Finished == nil || web.Started == nil ||
			db.Finished.After(*web.Started) {
			t.Errorf("%s: srv-db did not finish before srv-web started: %+v %+v", h.Agent, db, web)
		}
		// One update at a time on the host (R13).
		var jobs []store.Entry
		for _, e := range h.Containers {
			if e.Started != nil {
				jobs = append(jobs, e)
			}
			if strings.HasPrefix(e.Container, "upd-") && (e.Status == "updated" || e.Status == "rolled_back") {
				t.Errorf("%s: the protected %s was updated", h.Agent, e.Container)
			}
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Started.Before(*jobs[j].Started) })
		for i := 1; i < len(jobs); i++ {
			if jobs[i-1].Finished == nil || jobs[i-1].Finished.After(*jobs[i].Started) {
				t.Errorf("%s: %s and %s overlap", h.Agent, jobs[i-1].Container, jobs[i].Container)
			}
		}
	}
	state, _ := st.Read()
	if len(state.Failed) != len(hosts) {
		t.Fatalf("failed digests = %+v", state.Failed)
	}

	run, err = r.Run(ctx, KindUpdate)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range run.Hosts {
		if e := find(t, h, "srv-bad"); e.Status != "skipped" || !strings.Contains(e.Reason, "failed on") {
			t.Errorf("%s: srv-bad at the next run: %+v", h.Agent, e)
		}
		for _, name := range []string{"srv-db", "srv-web"} {
			if e := find(t, h, name); e.Status != string(registry.UpToDate) {
				t.Errorf("%s %s at the next run: %+v", h.Agent, name, e)
			}
		}
	}
	if state, _ := st.Read(); len(state.Runs) != 3 {
		t.Errorf("%d runs recorded", len(state.Runs))
	}
}

func find(t *testing.T, h store.HostRun, name string) store.Entry {
	t.Helper()
	for _, e := range h.Containers {
		if e.Container == name {
			return e
		}
	}
	t.Fatalf("%s: no entry for %s", h.Agent, name)
	return store.Entry{}
}
