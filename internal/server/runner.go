package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	// Notify, when set, gets each run of Run and RunIf once it is recorded
	// and the next run may start (R17). Runs started by hand are not given.
	Notify func(ctx context.Context, run store.Run)

	dial func(store.Agent) (agentAPI, error)
	now  func() time.Time
	mu   sync.Mutex

	amu    sync.Mutex
	active *Activity
}

// Spec is what one run covers.
type Spec struct {
	Kind string
	// Hosts limits the run to these agents; empty means every agent.
	Hosts []string
	// Container makes an update run update this one container on its one
	// host, by hand: protected, check-only hosts and failed digests do not
	// stop it.
	Container string
	// ContainerID is the ID the person saw. A container of that name with
	// another ID was recreated since, and is not updated.
	ContainerID string
	// Manual names who started the run in the web UI.
	Manual string
}

// Activity is the run going on now.
type Activity struct {
	Spec    Spec
	Started time.Time
}

// ErrBusy means another run is going on.
var ErrBusy = errors.New("another run is going on; try again when it ends")

// Active is the run going on now, or nil.
func (r *Runner) Active() *Activity {
	r.amu.Lock()
	defer r.amu.Unlock()
	if r.active == nil {
		return nil
	}
	a := *r.active
	return &a
}

// Start runs spec in the background, or returns ErrBusy at once when
// another run is going on. done, when not nil, gets the record.
func (r *Runner) Start(ctx context.Context, spec Spec, done func(store.Run, error)) error {
	if err := spec.check(); err != nil {
		return err
	}
	if !r.mu.TryLock() {
		return ErrBusy
	}
	// Shown at once, before the run's goroutine gets to it.
	r.amu.Lock()
	r.active = &Activity{Spec: spec, Started: r.clock()}
	r.amu.Unlock()
	go func() {
		defer r.mu.Unlock()
		run, err := r.run(ctx, spec)
		if done != nil {
			done(run, err)
		}
	}()
	return nil
}

func (s Spec) check() error {
	switch {
	case s.Kind != KindCheck && s.Kind != KindUpdate:
		return fmt.Errorf("unknown run kind %q", s.Kind)
	case s.Container != "" && (s.Kind != KindUpdate || len(s.Hosts) != 1):
		return errors.New("a container is updated by hand on exactly one host")
	case s.Container != "" && s.ContainerID == "":
		return errors.New("an update by hand needs the ID of the container that was shown")
	}
	return nil
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
// time, and records it (R31). It waits for a run going on to end first.
func (r *Runner) Run(ctx context.Context, kind string) (store.Run, error) {
	run, _, err := r.RunIf(ctx, kind, nil)
	return run, err
}

// RunIf is Run that, once the run going on has ended, starts only when
// still, if set, says the run is still wanted. ran tells whether it started.
func (r *Runner) RunIf(ctx context.Context, kind string, still func() bool) (run store.Run, ran bool, err error) {
	spec := Spec{Kind: kind}
	if err := spec.check(); err != nil {
		return store.Run{}, false, err
	}
	r.mu.Lock()
	if still != nil && !still() {
		r.mu.Unlock()
		return store.Run{}, false, nil
	}
	run, err = r.run(ctx, spec)
	r.mu.Unlock()
	// Sent with the lock released, so a slow webhook holds back no run.
	if err == nil && r.Notify != nil {
		r.Notify(ctx, run)
	}
	return run, true, err
}

// run runs spec; the caller holds r.mu.
func (r *Runner) run(ctx context.Context, spec Spec) (store.Run, error) {
	r.amu.Lock()
	r.active = &Activity{Spec: spec, Started: r.clock()}
	r.amu.Unlock()
	defer func() {
		r.amu.Lock()
		r.active = nil
		r.amu.Unlock()
	}()
	st, err := r.Store.Read()
	if err != nil {
		return store.Run{}, err
	}
	var agents []store.Agent
	for _, a := range st.Agents {
		if len(spec.Hosts) == 0 || slices.Contains(spec.Hosts, a.Name) {
			agents = append(agents, a)
		}
	}
	if len(spec.Hosts) > 0 && len(agents) != len(spec.Hosts) {
		return store.Run{}, fmt.Errorf("no agent named %s", strings.Join(spec.Hosts, ", "))
	}
	run := store.Run{ID: runID(r.clock()), Kind: spec.Kind, Manual: spec.Manual, Container: spec.Container, Started: r.clock()}
	results := make([]hostResult, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.host(ctx, spec, a, st.Failed)
		}()
	}
	wg.Wait()
	run.Finished = r.clock()
	for _, h := range results {
		run.Hosts = append(run.Hosts, h.run)
	}
	err = r.Store.Update(func(s *store.State) error {
		s.Runs = append(s.Runs, run)
		// An agent removed during the run, or added again under the same
		// name, is another host now: its part of the run changes no
		// failure.
		same := func(name string) bool {
			i := slices.IndexFunc(s.Agents, func(a store.Agent) bool { return a.Name == name })
			return i >= 0 && !s.Agents[i].Added.After(run.Started)
		}
		kept := s.Failed[:0:0]
		have := map[string]bool{}
		for _, f := range s.Failed {
			drop := false
			for _, h := range results {
				drop = drop || h.drop[failureKey(f)] && same(h.run.Agent)
			}
			if !drop {
				kept = append(kept, f)
				have[failureKey(f)] = true
			}
		}
		for _, h := range results {
			if !same(h.run.Agent) {
				continue
			}
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
func (r *Runner) host(ctx context.Context, spec Spec, a store.Agent, failed []store.Failure) hostResult {
	kind := spec.Kind
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
	found := false
	for _, c := range rep.Containers {
		if spec.Container != "" && c.Name != spec.Container {
			continue
		}
		found = true
		e := store.Entry{Container: c.Name, ID: c.ID, Image: c.Image, State: c.State, Status: string(c.Check.Status),
			Reason: c.Check.Reason, RemoteDigest: c.Check.RemoteDigest, Available: c.Check.Status == registry.UpdateAvailable,
			Protected: c.Protected, Self: c.Self}
		if c.LabelError != "" {
			e.Reason = join(e.Reason, c.LabelError)
		}
		f, wasFailed := prior[c.Name+"@"+c.Check.RemoteDigest]
		if kind == KindUpdate && c.Check.Status == registry.UpdateAvailable {
			switch {
			case spec.Container != "" && c.ID != spec.ContainerID:
				e.Reason = join(e.Reason, "not updated: "+c.Name+" was recreated since the page showed it; look at it again and press Update now")
			case spec.Container != "":
				// By hand: the person asked for this one container.
				candidates = append(candidates, c)
			case c.Self:
				e.Status, e.Reason = "skipped", "hikifune does not recreate its own containers"
			case c.Protected:
				e.Status, e.Reason = "skipped", "protected: checked, never updated automatically"
			case c.State != "running":
				e.Status, e.Reason = "skipped", "not running ("+c.State+")"
			case wasFailed:
				e.Status, e.Reason = "skipped", fmt.Sprintf("the update to %s failed on %s: %s",
					short(f.Digest), f.At.Format(time.RFC3339), f.Reason)
			case a.CheckOnly:
				e.Status, e.Reason = "skipped", "check-only host: checked, not updated on schedule"
			default:
				candidates = append(candidates, c)
			}
		}
		index[c.Name] = len(hr.Containers)
		hr.Containers = append(hr.Containers, e)
	}
	if spec.Container != "" && !found {
		hr.Error = "no container named " + spec.Container + " on this host"
		return res
	}
	if kind == KindCheck {
		return res
	}

	updated := false
	for _, c := range order(candidates) {
		e := &hr.Containers[index[c.Name]]
		var still func() bool
		if spec.Container == "" {
			still = func() bool { return !r.checkOnlyNow(a.Name) }
		}
		job, err := r.updateOne(ctx, api, c, still)
		if !job.Started.IsZero() {
			e.Started, e.Finished = &job.Started, job.Finished
		}
		if errors.Is(err, errCheckOnly) {
			e.Status, e.Reason = "skipped", "check-only host: the switch was turned on during the run"
			continue
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

// checkOnlyNow reads the host's check-only switch again, so turning it on
// stops the scheduled updates still to come in a run going on (R35).
func (r *Runner) checkOnlyNow(name string) bool {
	st, err := r.Store.Read()
	if err != nil {
		return false
	}
	i := slices.IndexFunc(st.Agents, func(a store.Agent) bool { return a.Name == name })
	return i >= 0 && st.Agents[i].CheckOnly
}

// errCheckOnly means the host's check-only switch was turned on before the
// update was sent.
var errCheckOnly = errors.New("the check-only switch was turned on")

// updateOne starts an update and asks for the job until it is done (R32).
// still, when set, is asked before each time the update is sent, so a
// check-only switch turned on meanwhile keeps it from going. When the agent
// is busy with another job, it waits for that one and tries once more.
func (r *Runner) updateOne(ctx context.Context, api agentAPI, c protocol.Checked, still func() bool) (protocol.Job, error) {
	if still != nil && !still() {
		return protocol.Job{}, errCheckOnly
	}
	req := protocol.UpdateRequest{Container: c.Name, ID: c.ID, WatchSeconds: int(r.Watch / time.Second)}
	job, err := api.StartUpdate(ctx, req)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict && se.Body.Busy != nil {
		if _, err := r.wait(ctx, api, se.Body.Busy.ID); err != nil {
			return protocol.Job{}, fmt.Errorf("the agent was busy with %s: %w", se.Body.Busy.Container, err)
		}
		if still != nil && !still() {
			return protocol.Job{}, errCheckOnly
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
