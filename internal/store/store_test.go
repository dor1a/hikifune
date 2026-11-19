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
		st.Agents = append(st.Agents, Agent{Name: "atsui", URL: "http://atsui:8870", Token: "t", Added: at})
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
