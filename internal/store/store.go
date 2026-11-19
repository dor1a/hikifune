// Package store keeps the server's state: agents, run records and the
// digests whose update failed (R27). The JSON implementation writes one file
// per part; SQLite can take its place behind the same interface when the
// state grows.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Agent is one agent the server dials (R26). Token is sent as is, so the
// file that holds it is readable by the server's user only.
type Agent struct {
	Name  string    `json:"name"`
	URL   string    `json:"url"`
	Token string    `json:"token"`
	Added time.Time `json:"added"`
}

// Entry is one container in a run record (R31).
type Entry struct {
	Container string `json:"container"`
	Image     string `json:"image"`
	// Status is the check status for a check run, and for an update run the
	// update outcome, the check status of a container that was not updated,
	// or "skipped" with the reason.
	Status       string     `json:"status"`
	Reason       string     `json:"reason,omitempty"`
	RemoteDigest string     `json:"remote_digest,omitempty"`
	Protected    bool       `json:"protected,omitempty"`
	Self         bool       `json:"self,omitempty"`
	Started      *time.Time `json:"started,omitempty"`
	Finished     *time.Time `json:"finished,omitempty"`
}

// HostRun is one agent's part of a run.
type HostRun struct {
	Agent        string  `json:"agent"`
	AgentVersion string  `json:"agent_version,omitempty"`
	Docker       string  `json:"docker,omitempty"`
	Skipped      string  `json:"skipped,omitempty"` // why the host was left out (R16)
	Error        string  `json:"error,omitempty"`
	Containers   []Entry `json:"containers,omitempty"`
	Reclaimed    int64   `json:"reclaimed,omitempty"`
}

// Run is one check or update run.
type Run struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Hosts    []HostRun `json:"hosts"`
}

// Failure is a digest whose update failed; the next run skips it (R15).
type Failure struct {
	Agent     string    `json:"agent"`
	Container string    `json:"container"`
	Digest    string    `json:"digest"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason"`
}

// State is everything the server keeps.
type State struct {
	Agents []Agent
	Runs   []Run // oldest first
	Failed []Failure
}

// Store reads and changes the state.
type Store interface {
	Read() (State, error)
	// Update changes the state under a lock held across processes, so the
	// server and the agent commands never write over each other.
	Update(func(*State) error) error
}

// JSON keeps the state in a directory: agents.json for the agents and
// their tokens, and state.json for the runs and the failed digests, which
// change together. Each is written to a temporary file and renamed, and only
// when it changed.
type JSON struct {
	dir      string
	keepRuns int
}

// DefaultKeepRuns is how many runs are kept when none is set.
const DefaultKeepRuns = 60

const (
	agentsFile = "agents.json"
	stateFile  = "state.json"
)

// runState is what state.json holds.
type runState struct {
	Runs   []Run     `json:"runs"`
	Failed []Failure `json:"failed"`
}

// OpenJSON uses dir, creating it, and keeps the last keepRuns runs.
func OpenJSON(dir string, keepRuns int) (*JSON, error) {
	if keepRuns <= 0 {
		keepRuns = DefaultKeepRuns
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &JSON{dir: dir, keepRuns: keepRuns}, nil
}

// Read loads the state. Missing files are empty parts.
func (j *JSON) Read() (State, error) {
	var s State
	var rs runState
	for _, f := range []struct {
		name string
		v    any
	}{{agentsFile, &s.Agents}, {stateFile, &rs}} {
		b, err := os.ReadFile(filepath.Join(j.dir, f.name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return State{}, err
		}
		if err := json.Unmarshal(b, f.v); err != nil {
			return State{}, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	s.Runs, s.Failed = rs.Runs, rs.Failed
	return s, nil
}

// Update reads the state, lets fn change it and writes back what changed.
func (j *JSON) Update(fn func(*State) error) error {
	lock, err := os.OpenFile(filepath.Join(j.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock the state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	s, err := j.Read()
	if err != nil {
		return err
	}
	agentsBefore, _ := json.Marshal(s.Agents)
	stateBefore, _ := json.Marshal(runState{s.Runs, s.Failed})
	if err := fn(&s); err != nil {
		return err
	}
	if len(s.Runs) > j.keepRuns {
		s.Runs = s.Runs[len(s.Runs)-j.keepRuns:]
	}
	if after, _ := json.Marshal(s.Agents); string(after) != string(agentsBefore) {
		if err := j.write(agentsFile, s.Agents); err != nil {
			return err
		}
	}
	if after, _ := json.Marshal(runState{s.Runs, s.Failed}); string(after) != string(stateBefore) {
		if err := j.write(stateFile, runState{s.Runs, s.Failed}); err != nil {
			return err
		}
	}
	return nil
}

func (j *JSON) write(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(j.dir, name))
}
