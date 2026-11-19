// Package agent serves the agent's HTTP API: the server dials it (R26) to
// read the inventory, check images, start updates and prune.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/update"
)

const (
	// MinTokenLength keeps a guessable token out.
	MinTokenLength = 32
	// keepJobs is how many finished jobs the agent still answers for.
	keepJobs = 32
	maxBody  = 1 << 20
)

// JobLimit is how long a job may take beyond its watch: pull, stop and
// rollback. The server waits five minutes more, so it sees the job end.
const JobLimit = 25 * time.Minute

// Config is what an agent needs.
type Config struct {
	Docker   *docker.Client
	Registry *registry.Client
	// Auth gives the X-Registry-Auth for a pull from a registry.
	Auth func(registry string) string
	// Token is what the server sends in Authorization: Bearer.
	Token string
	// Watch is used when an update request gives none.
	Watch time.Duration
}

// Agent answers the server. Jobs run under the context New was given, so
// cancelling it rolls a running update back.
type Agent struct {
	token string
	watch time.Duration
	limit time.Duration // JobLimit, shorter in tests
	base  context.Context
	now   func() time.Time

	// What the agent does, replaced in tests.
	version   func(ctx context.Context) protocol.Version
	inventory func(ctx context.Context) (inventory.Report, error)
	check     func(ctx context.Context) (protocol.CheckReport, error)
	inspect   func(ctx context.Context, container string) (docker.Container, error)
	update    func(ctx context.Context, container string, watch time.Duration) update.Result
	prune     func(ctx context.Context) (int64, error)

	mu       sync.Mutex
	current  *protocol.Job
	pruning  bool
	finished []protocol.Job
	jobs     sync.WaitGroup
}

// New returns an agent for cfg. Jobs run under ctx.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	if len(cfg.Token) < MinTokenLength {
		return nil, fmt.Errorf("the agent token must be at least %d characters; hikifune agent add makes one", MinTokenLength)
	}
	if cfg.Docker == nil || cfg.Registry == nil {
		return nil, errors.New("the agent needs a Docker client and a registry client")
	}
	if cfg.Watch <= 0 {
		cfg.Watch = update.DefaultWatch
	}
	d, reg, auth := cfg.Docker, cfg.Registry, cfg.Auth
	a := &Agent{token: cfg.Token, watch: cfg.Watch, limit: JobLimit, base: ctx, now: time.Now}
	a.version = func(ctx context.Context) protocol.Version {
		v := protocol.Version{Agent: buildinfo.Version}
		dv, err := d.CheckAPI(ctx)
		if err != nil {
			v.DockerError = err.Error()
			return v
		}
		v.Docker = &inventory.Docker{Version: dv.Version, APIVersion: dv.APIVersion, MinAPIVersion: dv.MinAPIVersion,
			RequestedAPI: d.APIVersion(), OS: dv.Os, Arch: dv.Arch}
		return v
	}
	a.inventory = func(ctx context.Context) (inventory.Report, error) { return inventory.Collect(ctx, d) }
	a.check = func(ctx context.Context) (protocol.CheckReport, error) { return Check(ctx, d, reg) }
	a.inspect = d.ContainerInspect
	a.update = func(ctx context.Context, container string, watch time.Duration) update.Result {
		return update.Container(ctx, d, container, update.Options{Watch: watch, Auth: auth, Check: reg.Check})
	}
	a.prune = d.ImagesPrune
	return a, nil
}

// Wait returns when no job runs.
func (a *Agent) Wait() { a.jobs.Wait() }

// Handler serves the API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.PathHealth, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	api := http.NewServeMux()
	api.HandleFunc("GET "+protocol.PathVersion, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.version(r.Context()))
	})
	api.HandleFunc("GET "+protocol.PathInventory, func(w http.ResponseWriter, r *http.Request) {
		rep, err := a.inventory(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	api.HandleFunc("POST "+protocol.PathCheck, func(w http.ResponseWriter, r *http.Request) {
		rep, err := a.check(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	api.HandleFunc("POST "+protocol.PathUpdates, a.startUpdate)
	api.HandleFunc("GET "+protocol.PathUpdates+"/{id}", a.getJob)
	api.HandleFunc("POST "+protocol.PathPrune, a.runPrune)
	mux.Handle("/api/", a.authorized(api))
	return mux
}

// authorized lets a request through only with the agent's token.
func (a *Agent) authorized(next http.Handler) http.Handler {
	want := []byte("Bearer " + a.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or wrong agent token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Agent) startUpdate(w http.ResponseWriter, r *http.Request) {
	var req protocol.UpdateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.Container) == "" || req.WatchSeconds < 0 {
		writeError(w, http.StatusBadRequest, errors.New(`the body must be {"container": "<name>", "watch_seconds": <n>}`))
		return
	}
	watch := a.watch
	if req.WatchSeconds > 0 {
		watch = time.Duration(req.WatchSeconds) * time.Second
	}
	a.mu.Lock()
	if a.current != nil {
		busy := *a.current
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, protocol.Error{Error: "an update is running on this host", Busy: &busy})
		return
	}
	if a.pruning {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("a prune is running on this host"))
		return
	}
	job := &protocol.Job{ID: newID(), Container: req.Container, State: protocol.JobRunning, Started: a.now().UTC()}
	a.current = job
	accepted := *job
	a.jobs.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.jobs.Done()
		res := a.run(job.Container, req.ID, watch)
		a.mu.Lock()
		defer a.mu.Unlock()
		done := a.now().UTC()
		job.State, job.Finished, job.Result = protocol.JobDone, &done, &res
		a.finished = append(a.finished, *job)
		if len(a.finished) > keepJobs {
			a.finished = a.finished[len(a.finished)-keepJobs:]
		}
		a.current = nil
		slog.Info("update finished", "container", job.Container, "outcome", res.Outcome, "reason", res.Reason)
	}()
	slog.Info("update started", "container", accepted.Container, "job", accepted.ID)
	writeJSON(w, http.StatusAccepted, accepted)
}

// run is one job, bounded so a pull that hangs cannot hold the host. A
// container recreated under the same name since the check is left alone:
// its labels may say something else now. The update then goes by ID, so it
// can only ever touch the container the check saw.
func (a *Agent) run(container, id string, watch time.Duration) update.Result {
	ctx, cancel := context.WithTimeout(a.base, watch+a.limit)
	defer cancel()
	target := container
	if id != "" {
		ct, err := a.inspect(ctx, container)
		if err != nil {
			return update.Result{Container: container, Outcome: update.Failed, Reason: "inspect: " + err.Error()}
		}
		if ct.ID != id {
			return update.Result{Container: container, Image: ct.Config.Image, Outcome: update.Failed,
				Reason: "the container was recreated since the check; the next run checks it again"}
		}
		target = id
	}
	res := a.update(ctx, target, watch)
	res.Container = container
	return res
}

func (a *Agent) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current != nil && a.current.ID == id {
		writeJSON(w, http.StatusOK, *a.current)
		return
	}
	for _, j := range a.finished {
		if j.ID == id {
			writeJSON(w, http.StatusOK, j)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("no job %s on this agent", id))
}

func (a *Agent) runPrune(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	switch {
	case a.current != nil:
		busy := *a.current
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, protocol.Error{Error: "an update is running on this host", Busy: &busy})
		return
	case a.pruning:
		a.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("a prune is running on this host"))
		return
	}
	a.pruning = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.pruning = false
		a.mu.Unlock()
	}()
	n, err := a.prune(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.Pruned{Reclaimed: n})
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, protocol.Error{Error: err.Error()})
}
