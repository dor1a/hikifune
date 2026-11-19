package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := OpenJSON(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.Read(); err != nil || len(st.Agents)+len(st.Runs)+len(st.Failed) != 0 {
		t.Fatalf("empty state: %+v, %v", st, err)
	}
	at := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	err = s.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "atsui", URL: "http://atsui:7341", Token: "t", Added: at})
		for i := range 5 {
			st.Runs = append(st.Runs, Run{ID: string(rune('a' + i)), Kind: "check", Started: at})
		}
		st.Failed = append(st.Failed, Failure{Agent: "atsui", Container: "web", Digest: "sha256:1", At: at, Reason: "exited"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Token != "t" || len(st.Failed) != 1 {
		t.Fatalf("state = %+v", st)
	}
	// The oldest runs go first.
	if len(st.Runs) != 3 || st.Runs[0].ID != "c" || st.Runs[2].ID != "e" {
		t.Fatalf("runs = %+v", st.Runs)
	}
	for _, name := range []string{"agents.json", "state.json"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", name, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", fi.Mode())
	}
}

func TestUpdateWritesNothingOnErrorOrNoChange(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenJSON(dir, 0)
	if err := s.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatal("an unchanged state was written")
	}
	boom := errors.New("boom")
	if err := s.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "x"})
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if st, _ := s.Read(); len(st.Agents) != 0 {
		t.Fatal("a failed update was written")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != ".lock" {
			t.Errorf("left in the directory: %s", e.Name())
		}
	}
}

func TestReadRefusesABrokenFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenJSON(dir, 0)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600)
	if _, err := s.Read(); err == nil {
		t.Fatal("a broken state.json was read")
	}
}

// Changing the runs leaves agents.json alone, so a run never rewrites the
// tokens, and runs and failed digests land in one write.
func TestUpdateWritesOnlyWhatChanged(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenJSON(dir, 0)
	s.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "a"})
		return nil
	})
	before, _ := os.Stat(filepath.Join(dir, "agents.json"))
	time.Sleep(20 * time.Millisecond)
	s.Update(func(st *State) error {
		st.Runs = append(st.Runs, Run{ID: "r"})
		st.Failed = append(st.Failed, Failure{Agent: "a", Container: "web", Digest: "sha256:1"})
		return nil
	})
	after, _ := os.Stat(filepath.Join(dir, "agents.json"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("agents.json was rewritten for a run")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(b), `"id": "r"`) || !strings.Contains(string(b), `"digest": "sha256:1"`) {
		t.Errorf("state.json = %s", b)
	}
}

func TestTrimKeepsEachHostsLastList(t *testing.T) {
	agents := []Agent{{Name: "a"}, {Name: "b"}}
	listed := func(id string, hosts ...HostRun) Run { return Run{ID: id, Kind: "check", Hosts: hosts} }
	byHand := func(id string) Run {
		return Run{ID: id, Kind: "update", Container: "web", Hosts: []HostRun{{Agent: "a", Containers: []Entry{{Container: "web"}}}}}
	}
	runs := []Run{
		listed("old", HostRun{Agent: "a", Containers: []Entry{{Container: "web"}}}, HostRun{Agent: "gone"}),
		listed("b-list", HostRun{Agent: "a", Error: "agent unreachable"}, HostRun{Agent: "b"}),
		listed("failed", HostRun{Agent: "a", Error: "check: x"}, HostRun{Agent: "b", Skipped: "version"}),
		byHand("h1"), byHand("h2"),
	}
	var ids []string
	for _, r := range trim(runs, 2, agents) {
		ids = append(ids, r.ID)
	}
	// a's list is in "old", b's (empty, but listed) in "b-list"; "failed"
	// listed nothing, and the removed agent "gone" keeps nothing.
	if got := strings.Join(ids, " "); got != "old b-list h1 h2" {
		t.Fatalf("kept %s", got)
	}
	if got := trim(runs[:2], 2, agents); len(got) != 2 {
		t.Fatalf("under the limit: %d", len(got))
	}

	// The updates by hand after a host's list are kept with it: without
	// them the page would show web as before its update.
	runs = []Run{
		listed("list", HostRun{Agent: "a", Containers: []Entry{{Container: "web"}, {Container: "other"}}}),
		byHand("web"), byHand("other"),
	}
	if got := trim(runs, 1, agents); len(got) != 3 {
		t.Fatalf("kept %d of the list and its updates by hand", len(got))
	}
}
