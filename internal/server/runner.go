package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/store"
	"github.com/dor1a/hikifune/internal/update"
)

// Run kinds.
const (
	KindCheck  = "check"
	KindUpdate = "update"
)

const (
	// DefaultPoll is how often the server asks an agent for a running job.
	DefaultPoll = 2 * time.Second
	// jobSlack is how long a job may take beyond its watch: pull, stop and
	// rollback.
	jobSlack = 30 * time.Minute
)

// Runner runs check and update runs, one at a time.
type Runner struct {
	Store store.Store
	// Version is the server's version; an agent must run the same (R16).
	Version string
	// Watch is how long each updated container is watched (R24).
	Watch time.Duration
	// Poll is how often a running job is asked for.
	Poll time.Duration

	dial func(store.Agent) (agentAPI, error)
	now  func() time.Time
	mu   sync.Mutex
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) agent(a store.Agent) (agentAPI, error) {
	if r.dial != nil {
		return r.dial(a)
	}
	return NewClient(a.URL, a.Token)
}

// hostResult is one agent's part of a run and what it changes in the failed
// digests. The changes are applied to the state as it is when the run is
// recorded, so a run never undoes what another one recorded meanwhile.
type hostResult struct {
	run  store.HostRun
	drop map[string]bool // failure keys to remove
	add  []store.Failure
}

func failureKey(f store.Failure) string { return f.Agent + "\x00" + f.Container + "\x00" + f.Digest }

// Run runs one check or update run over every agent, the agents at the same
// time, and records it (R31).
func (r *Runner) Run(ctx context.Context, kind string) (store.Run, error) {
	if kind != KindCheck && kind != KindUpdate {
		return store.Run{}, fmt.Errorf("unknown run kind %q", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.Store.Read()
	if err != nil {
		return store.Run{}, err
	}
	run := store.Run{ID: runID(r.clock()), Kind: kind, Started: r.clock()}
	results := make([]hostResult, len(st.Agents))
	var wg sync.WaitGroup
	for i, a := range st.Agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.host(ctx, kind, a, st.Failed)
		}()
	}
	wg.Wait()
	run.Finished = r.clock()
	for _, h := range results {
		run.Hosts = append(run.Hosts, h.run)
	}
	err = r.Store.Update(func(s *store.State) error {
		s.Runs = append(s.Runs, run)
		kept := s.Failed[:0:0]
		have := map[string]bool{}
		for _, f := range s.Failed {
			drop := false
			for _, h := range results {
				drop = drop || h.drop[failureKey(f)]
			}
			if !drop {
				kept = append(kept, f)
				have[failureKey(f)] = true
			}
		}
		for _, h := range results {
			for _, f := range h.add {
				if !have[failureKey(f)] {
					kept = append(kept, f)
					have[failureKey(f)] = true
				}
			}
		}
		s.Failed = kept
		return nil
	})
	return run, err
}

// host runs one agent's part.
func (r *Runner) host(ctx context.Context, kind string, a store.Agent, failed []store.Failure) hostResult {
	res := hostResult{run: store.HostRun{Agent: a.Name}, drop: map[string]bool{}}
	hr := &res.run
	api, err := r.agent(a)
	if err != nil {
		hr.Error = err.Error()
		return res
	}
	v, err := api.Version(ctx)
	if err != nil {
		hr.Error = "agent unreachable: " + err.Error()
		return res
	}
	hr.AgentVersion = v.Agent
	if v.Docker != nil {
		hr.Docker = v.Docker.Version
	}
	switch {
	case v.Agent != r.Version:
		hr.Skipped = fmt.Sprintf("the agent runs %s and the server %s; run the same version on both", v.Agent, r.Version)
		return res
	case v.DockerError != "":
		hr.Skipped = "Docker: " + v.DockerError
		return res
	case v.Docker == nil:
		hr.Skipped = "the agent did not report its Docker daemon"
		return res
	}
	if cmp, err := docker.CompareAPIVersions(v.Docker.APIVersion, docker.MinAPIVersion); err != nil || cmp < 0 {
		hr.Skipped = fmt.Sprintf("Docker %s serves Engine API %s, older than the %s hikifune needs",
			v.Docker.Version, v.Docker.APIVersion, docker.MinAPIVersion)
		return res
	}
	rep, err := api.Check(ctx)
	if err != nil {
		hr.Error = "check: " + err.Error()
		return res
	}

	prior := map[string]store.Failure{}
	byName := map[string]protocol.Checked{}
	for _, c := range rep.Containers {
		byName[c.Name] = c
	}
	for _, f := range failed {
		if f.Agent != a.Name {
			continue
		}
		prior[f.Container+"@"+f.Digest] = f
		// A failure goes when the check shows the container gone, up to
		// date, or pointing at another digest. A check that could not tell
		// keeps it (R15).
		c, ok := byName[f.Container]
		switch {
		case !ok, c.Check.Status == registry.UpToDate,
			c.Check.Status == registry.UpdateAvailable && c.Check.RemoteDigest != f.Digest:
			res.drop[failureKey(f)] = true
		}
	}
	index := map[string]int{}
	var candidates []protocol.Checked
	for _, c := range rep.Containers {
		e := store.Entry{Container: c.Name, Image: c.Image, Status: string(c.Check.Status), Reason: c.Check.Reason,
			RemoteDigest: c.Check.RemoteDigest, Protected: c.Protected, Self: c.Self}
		if c.LabelError != "" {
			e.Reason = join(e.Reason, c.LabelError)
		}
		f, wasFailed := prior[c.Name+"@"+c.Check.RemoteDigest]
		if kind == KindUpdate && c.Check.Status == registry.UpdateAvailable {
			switch {
			case c.Self:
				e.Status, e.Reason = "skipped", "hikifune does not recreate its own containers"
			case c.Protected:
				e.Status, e.Reason = "skipped", "protected: checked, never updated automatically"
			case c.State != "running":
				e.Status, e.Reason = "skipped", "not running ("+c.State+")"
			case wasFailed:
				e.Status, e.Reason = "skipped", fmt.Sprintf("the update to %s failed on %s: %s",
					short(f.Digest), f.At.Format(time.RFC3339), f.Reason)
			default:
				candidates = append(candidates, c)
			}
		}
		index[c.Name] = len(hr.Containers)
		hr.Containers = append(hr.Containers, e)
	}
	if kind == KindCheck {
		return res
	}

	updated := false
	for _, c := range order(candidates) {
		e := &hr.Containers[index[c.Name]]
		job, err := r.updateOne(ctx, api, c)
		if !job.Started.IsZero() {
			e.Started, e.Finished = &job.Started, job.Finished
		}
		if err != nil {
			e.Status, e.Reason = string(update.Failed), err.Error()
			continue
		}
		e.Status, e.Reason = string(job.Result.Outcome), join(job.Result.Reason, job.Result.Note)
		// The digest the job went for, which may be newer than this check's.
		// A job that failed before its registry check, such as one refused
		// because the container was recreated, tried no digest.
		digest := job.Result.Digest
		switch job.Result.Outcome {
		case update.Updated:
			updated = true
			for key, f := range prior {
				if f.Container == c.Name {
					res.drop[failureKey(prior[key])] = true
				}
			}
		case update.Failed, update.RolledBack:
			if digest != "" {
				res.add = append(res.add, store.Failure{Agent: a.Name, Container: c.Name,
					Digest: digest, At: r.clock(), Reason: job.Result.Reason})
			}
		}
	}
	if updated {
		p, err := api.Prune(ctx)
		if err != nil {
			hr.Error = join(hr.Error, "prune: "+err.Error())
		}
		hr.Reclaimed = p.Reclaimed
	}
	return res
}

// updateOne starts an update and asks for the job until it is done (R32).
// When the agent is busy with another job, it waits for that one and tries
// once more.
func (r *Runner) updateOne(ctx context.Context, api agentAPI, c protocol.Checked) (protocol.Job, error) {
	req := protocol.UpdateRequest{Container: c.Name, ID: c.ID, WatchSeconds: int(r.Watch / time.Second)}
	job, err := api.StartUpdate(ctx, req)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict && se.Body.Busy != nil {
		if _, err := r.wait(ctx, api, se.Body.Busy.ID); err != nil {
			return protocol.Job{}, fmt.Errorf("the agent was busy with %s: %w", se.Body.Busy.Container, err)
		}
		job, err = api.StartUpdate(ctx, req)
	}
	if err != nil {
		return protocol.Job{}, fmt.Errorf("start the update: %w", err)
	}
	return r.wait(ctx, api, job.ID)
}

func (r *Runner) wait(ctx context.Context, api agentAPI, id string) (protocol.Job, error) {
	poll := r.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	watch := r.Watch
	if watch <= 0 {
		watch = update.DefaultWatch
	}
	ctx, cancel := context.WithTimeout(ctx, watch+jobSlack)
	defer cancel()
	var last protocol.Job
	var lastErr error
	for {
		job, err := api.Job(ctx, id)
		var se *StatusError
		switch {
		case errors.As(err, &se) && se.Status == http.StatusNotFound:
			return last, errors.New("the agent no longer knows the job; it may have restarted, and the next check shows where the container is")
		case err != nil:
			// A dropped answer loses nothing: the job runs on and the next
			// ask gets it (R32).
			lastErr = err
		case job.State == protocol.JobDone && job.Result != nil:
			return job, nil
		default:
			last, lastErr = job, nil
		}
		if err := sleep(ctx, poll); err != nil {
			if lastErr != nil {
				return last, fmt.Errorf("the job did not finish in time; the last ask failed: %w", lastErr)
			}
			return last, fmt.Errorf("the job did not finish in time: %w", err)
		}
	}
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func short(digest string) string {
	if d, ok := strings.CutPrefix(digest, "sha256:"); ok && len(d) > 12 {
		return "sha256:" + d[:12]
	}
	return digest
}

func runID(t time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return t.Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}
