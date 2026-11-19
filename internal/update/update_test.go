package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/registry"
)

const newID = "fedcba987654fedcba987654fedcba987654fedcba987654fedcba987654abcd"

// fakeEngine plays a daemon holding the old container from oldContainer,
// and the new container hikifune creates.
type fakeEngine struct {
	t  *testing.T
	mu sync.Mutex

	calls      []string
	names      map[string]string // container name -> "old" or "new"
	oldRunning bool
	autoRemove bool
	pullTo     string // image ID the reference names after the pull
	pulled     bool
	named      string // image ID the reference names before the pull
	oldGone    bool   // the old image is no longer on the host

	checked registry.Target // what the registry check was asked
	check   registry.Result // what it answers
	// renamedAnyway lists renames that are carried out but answer an error,
	// as when the context is cancelled after the daemon acted.
	renamedAnyway map[string]bool
	// oldStaysDown makes starting the old container answer success without
	// it running.
	oldStaysDown bool
	// onCreate runs when the new container is created.
	onCreate func()

	body       map[string]any // the create body
	connected  map[string]any // network -> endpoint connected after create
	newStarted bool
	inspects   int // inspects of the new container since it started

	// watch gives the new container's state at the nth inspect after start.
	watch func(n int) (docker.ContainerState, int)
	// fail makes the named call return an error.
	fail map[string]error
	// tamper edits the new container's inspect document.
	tamper func(ct map[string]any)
	// onWatch runs at each inspect of the started new container.
	onWatch func(n int)
}

func newFake(t *testing.T) *fakeEngine {
	return &fakeEngine{t: t, names: map[string]string{"apps-web-1": "old"}, oldRunning: true, pullTo: newImage,
		named: oldImage, check: registry.Result{Status: registry.UpdateAvailable, RemoteDigest: newImage},
		connected: map[string]any{}, fail: map[string]error{}, renamedAnyway: map[string]bool{},
		watch: func(int) (docker.ContainerState, int) {
			return docker.ContainerState{Status: "running", Running: true, StartedAt: "t0",
				Health: &docker.Health{Status: "healthy"}}, 0
		}}
}

// opts are fast Options that ask the fake's registry check.
func (f *fakeEngine) opts() Options {
	o := fast
	o.Check = func(ctx context.Context, t registry.Target) registry.Result {
		f.checked = t
		return f.check
	}
	return o
}

func (f *fakeEngine) record(format string, a ...any) error {
	c := fmt.Sprintf(format, a...)
	f.calls = append(f.calls, c)
	return f.fail[c]
}

func notFound(what string) error {
	return &docker.Error{Method: "GET", Path: what, Status: http.StatusNotFound, Message: "No such object: " + what}
}

func toContainer(t *testing.T, m map[string]any) docker.Container {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var c docker.Container
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.Raw = b
	return c
}

func (f *fakeEngine) which(id string) string {
	switch id {
	case oldID:
		return "old"
	case newID:
		if f.body != nil {
			return "new"
		}
	}
	return f.names[id]
}

func (f *fakeEngine) nameOf(which string) string {
	for n, w := range f.names {
		if w == which {
			return n
		}
	}
	return ""
}

func (f *fakeEngine) ContainerInspect(ctx context.Context, id string) (docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.which(id) {
	case "old":
		ct, _ := decode([]byte(oldContainer))
		ct["Name"] = "/" + f.nameOf("old")
		status := "exited"
		if f.oldRunning {
			status = "running"
		}
		ct["State"] = map[string]any{"Status": status, "Running": f.oldRunning}
		ct["HostConfig"].(map[string]any)["AutoRemove"] = f.autoRemove
		return toContainer(f.t, ct), nil
	case "new":
		cfg := map[string]any{}
		for k, v := range f.body {
			if k != "HostConfig" && k != "NetworkingConfig" {
				cfg[k] = v
			}
		}
		nets := map[string]any{}
		for n, ep := range object(object(f.body["NetworkingConfig"])["EndpointsConfig"]) {
			nets[n] = ep
		}
		for n, ep := range f.connected {
			nets[n] = ep
		}
		old, _ := decode([]byte(oldContainer))
		ct := map[string]any{"Id": newID, "Name": "/" + f.nameOf("new"), "Image": f.pullTo, "Config": cfg,
			"HostConfig": f.body["HostConfig"], "NetworkSettings": map[string]any{"Networks": nets},
			"Mounts": old["Mounts"], "State": map[string]any{"Status": "created"}}
		if f.newStarted {
			st, restarts := f.watch(f.inspects)
			if f.onWatch != nil {
				f.onWatch(f.inspects)
			}
			f.inspects++
			ct["State"], ct["RestartCount"] = st, restarts
		}
		if f.tamper != nil {
			f.tamper(ct)
		}
		return toContainer(f.t, ct), nil
	case "":
		return docker.Container{}, notFound(id)
	}
	// Any other container the test put there.
	return toContainer(f.t, map[string]any{"Id": "stray", "Name": "/" + id}), nil
}

func (f *fakeEngine) ImageInspect(ctx context.Context, ref string) (docker.Image, error) {
	switch ref {
	case "reg.example/team/web:1":
		id := f.named
		if f.pulled {
			id = f.pullTo
		}
		if id == "" {
			return docker.Image{}, notFound(ref)
		}
		return docker.Image{ID: id, RepoDigests: []string{"reg.example/team/web@sha256:" + id[7:]},
			Raw: []byte(`{"Id":"` + id + `","Config":{}}`)}, nil
	case oldImage:
		if f.oldGone {
			return docker.Image{}, notFound(ref)
		}
		return docker.Image{ID: oldImage, RepoDigests: []string{"reg.example/team/web@sha256:old"}, Raw: []byte(oldImageDoc)}, nil
	}
	return docker.Image{}, notFound(ref)
}

func (f *fakeEngine) ImagePull(ctx context.Context, repository, tag, auth string) error {
	if err := f.record("pull %s:%s auth=%s", repository, tag, auth); err != nil {
		return err
	}
	f.pulled = true
	return nil
}

func (f *fakeEngine) ContainerCreate(ctx context.Context, name string, body json.RawMessage) (docker.Created, error) {
	if err := f.record("create %s", name); err != nil {
		return docker.Created{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := decode(body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.body = b
	f.names[name] = "new"
	if f.onCreate != nil {
		f.onCreate()
	}
	return docker.Created{ID: newID}, nil
}

func (f *fakeEngine) NetworkConnect(ctx context.Context, network, container string, endpoint json.RawMessage) error {
	if err := f.record("connect %s", network); err != nil {
		return err
	}
	var ep any
	json.Unmarshal(endpoint, &ep)
	f.connected[network] = ep
	return nil
}

func (f *fakeEngine) ContainerStart(ctx context.Context, id string) error {
	w := f.which(id)
	if err := f.record("start %s", w); err != nil {
		return err
	}
	if w == "new" {
		f.newStarted = true
	} else if !f.oldStaysDown {
		f.oldRunning = true
	}
	return nil
}

func (f *fakeEngine) ContainerStop(ctx context.Context, id string) error {
	w := f.which(id)
	if err := f.record("stop %s", w); err != nil {
		return err
	}
	if w == "old" {
		f.oldRunning = false
	}
	return nil
}

func (f *fakeEngine) ContainerRename(ctx context.Context, id, name string) error {
	w := f.which(id)
	call := fmt.Sprintf("rename %s %s", w, name)
	if err := f.record("%s", call); err != nil && !f.renamedAnyway[call] {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.names[name]; taken {
		return &docker.Error{Status: http.StatusConflict, Message: "name in use"}
	}
	delete(f.names, f.nameOf(w))
	f.names[name] = w
	return f.fail[call]
}

func (f *fakeEngine) ContainerRemove(ctx context.Context, id string, force bool) error {
	w := f.which(id)
	if err := f.record("remove %s force=%v", w, force); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.names, f.nameOf(w))
	if w == "new" {
		f.body = nil
	}
	return nil
}

func (f *fakeEngine) ImageRemove(ctx context.Context, id string) error {
	return f.record("remove image %s", id)
}

var fast = Options{Watch: 40 * time.Millisecond, Poll: 5 * time.Millisecond,
	Auth: func(r string) string { return "auth-for-" + r }}

// ordered reports whether want appears in calls in this order.
func ordered(calls, want []string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestRunUpdates(t *testing.T) {
	f := newFake(t)
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Updated || !r.OldImageRemoved || r.Reason != "" || r.Note != "" {
		t.Fatalf("result = %+v", r)
	}
	if r.Container != "apps-web-1" || r.OldImageID != oldImage || r.NewImageID != newImage {
		t.Errorf("result = %+v", r)
	}
	want := []string{
		"pull reg.example/team/web:1 auth=auth-for-reg.example",
		"create apps-web-1-hikifune-new",
		"connect back", "connect front",
		"stop old",
		"rename old apps-web-1-hikifune-old",
		"rename new apps-web-1",
		"start new",
		"remove old force=false",
		"remove image " + oldImage,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if f.nameOf("new") != "apps-web-1" {
		t.Errorf("the new container is named %q", f.nameOf("new"))
	}
	if f.inspects < 3 {
		t.Errorf("the new container was inspected %d times while watched", f.inspects)
	}
}

func TestRunUpToDate(t *testing.T) {
	f := newFake(t)
	f.pullTo = oldImage
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != UpToDate || len(f.calls) != 1 {
		t.Fatalf("result %+v, calls %v", r, f.calls)
	}
}

func TestRunRefusesBeforeTouchingAnything(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeEngine)
		want  string
	}{
		{"stopped", func(f *fakeEngine) { f.oldRunning = false }, "not running"},
		{"created with --rm", func(f *fakeEngine) { f.autoRemove = true }, "--rm"},
		{"a leftover new container", func(f *fakeEngine) { f.names["apps-web-1-hikifune-new"] = "stray" }, "left from an earlier update"},
		{"a leftover old container", func(f *fakeEngine) { f.names["apps-web-1-hikifune-old"] = "stray" }, "left from an earlier update"},
		{"no such container", func(f *fakeEngine) { delete(f.names, "apps-web-1") }, "inspect"},
	}
	for _, tt := range tests {
		f := newFake(t)
		tt.setup(f)
		r := run(context.Background(), f, "apps-web-1", f.opts())
		if r.Outcome != Failed || !strings.Contains(r.Reason, tt.want) || len(f.calls) != 0 {
			t.Errorf("%s: result %+v, calls %v", tt.name, r, f.calls)
		}
	}
}

func TestRunFailsWithoutStoppingTheOldContainer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeEngine)
		want  string
	}{
		{"pull fails", func(f *fakeEngine) {
			f.fail["pull reg.example/team/web:1 auth=auth-for-reg.example"] = errors.New("manifest unknown")
		}, "pull: manifest unknown"},
		{"the old image is gone", func(f *fakeEngine) { f.oldGone = true }, "gone from this host"},
		{"create refused", func(f *fakeEngine) {
			f.fail["create apps-web-1-hikifune-new"] = &docker.Error{Status: 400, Message: "invalid mount config"}
		}, "create refused: invalid mount config"},
		{"a network refused", func(f *fakeEngine) {
			f.fail["connect front"] = &docker.Error{Status: 404, Message: "network front not found"}
		}, "connect network front: network front not found"},
		{"the daemon dropped a setting", func(f *fakeEngine) {
			f.tamper = func(ct map[string]any) { delete(object(ct["HostConfig"]), "DeviceRequests") }
		}, "config check: HostConfig.DeviceRequests"},
	}
	for _, tt := range tests {
		f := newFake(t)
		tt.setup(f)
		r := run(context.Background(), f, "apps-web-1", f.opts())
		if r.Outcome != Failed || !strings.Contains(r.Reason, tt.want) {
			t.Errorf("%s: result %+v", tt.name, r)
		}
		if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "stop") || strings.HasPrefix(c, "rename") }) {
			t.Errorf("%s: the old container was touched: %v", tt.name, f.calls)
		}
		if f.body != nil {
			t.Errorf("%s: the new container was left: %v", tt.name, f.calls)
		}
		if !f.oldRunning || f.nameOf("old") != "apps-web-1" {
			t.Errorf("%s: the old container is %q, running %v", tt.name, f.nameOf("old"), f.oldRunning)
		}
	}
}

func TestRunRollsBack(t *testing.T) {
	running := func(status string) docker.ContainerState {
		return docker.ContainerState{Status: "running", Running: true, StartedAt: "t0", Health: &docker.Health{Status: status,
			Log: []docker.HealthLog{{ExitCode: 1, Output: "connection refused\nmore"}}}}
	}
	tests := []struct {
		name  string
		setup func(f *fakeEngine)
		want  string
	}{
		{"start refused", func(f *fakeEngine) {
			f.fail["start new"] = &docker.Error{Status: 500, Message: "port 8080 is already allocated"}
		}, "start refused: port 8080 is already allocated"},
		{"exits at once", func(f *fakeEngine) {
			f.watch = func(int) (docker.ContainerState, int) {
				return docker.ContainerState{Status: "exited", ExitCode: 1, StartedAt: "t0"}, 0
			}
		}, "exited with code 1"},
		{"restarts", func(f *fakeEngine) {
			f.watch = func(n int) (docker.ContainerState, int) {
				if n < 2 {
					return running("healthy"), 0
				}
				st := running("healthy")
				st.StartedAt, st.ExitCode = "t1", 2
				return st, 1
			}
		}, "restarted within"},
		{"unhealthy", func(f *fakeEngine) {
			f.watch = func(n int) (docker.ContainerState, int) { return running("unhealthy"), 0 }
		}, "unhealthy"},
		{"never healthy", func(f *fakeEngine) {
			f.watch = func(n int) (docker.ContainerState, int) { return running("starting"), 0 }
		}, "not healthy 40ms after start: health check exited with 1: connection refused"},
		{"out of memory", func(f *fakeEngine) {
			f.watch = func(n int) (docker.ContainerState, int) {
				return docker.ContainerState{Status: "exited", OOMKilled: true, StartedAt: "t0"}, 0
			}
		}, "running out of memory"},
	}
	for _, tt := range tests {
		f := newFake(t)
		tt.setup(f)
		r := run(context.Background(), f, "apps-web-1", f.opts())
		if r.Outcome != RolledBack || !strings.Contains(r.Reason, tt.want) {
			t.Errorf("%s: result %+v", tt.name, r)
		}
		tail := []string{"remove new force=true", "rename old apps-web-1", "start old"}
		if !ordered(f.calls, append([]string{"stop old", "rename old apps-web-1-hikifune-old"}, tail...)) {
			t.Errorf("%s: calls %v", tt.name, f.calls)
		}
		if !f.oldRunning || f.nameOf("old") != "apps-web-1" || f.body != nil {
			t.Errorf("%s: old %q running %v, new left %v", tt.name, f.nameOf("old"), f.oldRunning, f.body != nil)
		}
		if slices.Contains(f.calls, "remove image "+oldImage) {
			t.Errorf("%s: the old image was removed", tt.name)
		}
	}
}

func TestRunRollbackFailureNamesTheOldContainer(t *testing.T) {
	f := newFake(t)
	f.fail["start new"] = errors.New("boom")
	f.fail["rename old apps-web-1"] = errors.New("daemon gone")
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "rollback failed") ||
		!strings.Contains(r.Reason, "the old container is apps-web-1-hikifune-old") {
		t.Fatalf("result %+v", r)
	}
	// The old container is started even though its name could not come back.
	if !f.oldRunning {
		t.Fatalf("the old container was not started: %v", f.calls)
	}
}

// A rename the daemon carried out but answered with an error, as when the
// context is cancelled right after it acted.
func TestRunRollbackReadsTheOldNameAgain(t *testing.T) {
	f := newFake(t)
	f.fail["rename old apps-web-1-hikifune-old"] = context.Canceled
	f.renamedAnyway["rename old apps-web-1-hikifune-old"] = true
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != RolledBack || f.nameOf("old") != "apps-web-1" || !f.oldRunning {
		t.Fatalf("result %+v, old %q running %v, calls %v", r, f.nameOf("old"), f.oldRunning, f.calls)
	}
}

func TestRunRollbackGoesOnAfterAFailedStep(t *testing.T) {
	f := newFake(t)
	f.fail["start new"] = errors.New("boom")
	f.fail["remove new force=true"] = errors.New("busy")
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "remove the new container: busy") {
		t.Fatalf("result %+v", r)
	}
	if !slices.Contains(f.calls, "start old") {
		t.Fatalf("the old container was not started: %v", f.calls)
	}
}

func TestRunReportsANewContainerItCouldNotRemove(t *testing.T) {
	f := newFake(t)
	f.tamper = func(ct map[string]any) { delete(object(ct["HostConfig"]), "DeviceRequests") }
	f.fail["remove new force=true"] = errors.New("busy")
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "the new container apps-web-1-hikifune-new was left: busy") {
		t.Fatalf("result %+v", r)
	}
}

// A restart before the first look after start still counts.
func TestRunCatchesARestartBeforeTheFirstLook(t *testing.T) {
	f := newFake(t)
	f.watch = func(int) (docker.ContainerState, int) {
		return docker.ContainerState{Status: "running", Running: true, StartedAt: "t1"}, 1
	}
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != RolledBack || !strings.Contains(r.Reason, "restarted") {
		t.Fatalf("result %+v", r)
	}
}

func TestRunAsksTheRegistryFirst(t *testing.T) {
	f := newFake(t)
	f.check = registry.Result{Status: registry.NotCheckable, Reason: "the registry does not have the digest this host runs"}
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "not_checkable") || len(f.calls) != 0 {
		t.Fatalf("result %+v, calls %v", r, f.calls)
	}
	want := registry.Target{Image: "reg.example/team/web:1", RepoDigests: []string{"reg.example/team/web@sha256:old"}, ImageID: oldImage}
	if !reflect.DeepEqual(f.checked, want) {
		t.Errorf("checked %+v, want %+v", f.checked, want)
	}

	f = newFake(t)
	f.check = registry.Result{Status: registry.UpToDate}
	if r := run(context.Background(), f, "apps-web-1", f.opts()); r.Outcome != UpToDate || len(f.calls) != 0 {
		t.Fatalf("result %+v, calls %v", r, f.calls)
	}

	// The tag names another image now: the check is told what it names.
	f = newFake(t)
	f.named = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	f.check = registry.Result{Status: registry.NotCheckable}
	run(context.Background(), f, "apps-web-1", f.opts())
	if !f.checked.TagMoved || len(f.checked.NamedRepoDigests) != 1 || !strings.Contains(f.checked.NamedRepoDigests[0], "3333") {
		t.Errorf("checked %+v", f.checked)
	}

	f = newFake(t)
	r = run(context.Background(), f, "apps-web-1", fast)
	if r.Outcome != Failed || len(f.calls) != 0 {
		t.Fatalf("without a check: result %+v, calls %v", r, f.calls)
	}
}

func TestRunRollsBackAfterCancel(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onWatch = func(n int) {
		if n == 1 {
			cancel()
		}
	}
	o := f.opts()
	o.Watch, o.Poll = time.Minute, time.Millisecond
	r := run(ctx, f, "apps-web-1", o)
	if r.Outcome != RolledBack || !strings.Contains(r.Reason, "watch interrupted") {
		t.Fatalf("result %+v", r)
	}
	if !f.oldRunning || f.nameOf("old") != "apps-web-1" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestRunKeepsAnImageInUse(t *testing.T) {
	f := newFake(t)
	f.fail["remove image "+oldImage] = &docker.Error{Status: http.StatusConflict, Message: "image is being used"}
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Updated || r.OldImageRemoved || r.Note != "" {
		t.Fatalf("result %+v", r)
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Minute: "2m", 45 * time.Second: "45s", 90 * time.Second: "1m30s", 1500 * time.Millisecond: "2s",
		40 * time.Millisecond: "40ms", 71801625 * time.Nanosecond: "72ms",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%v) = %q, want %q", d, got, want)
		}
	}
}

// The tag moved between the check and the pull: the pulled image is not the
// one the check saw, so nothing is created.
func TestRunRefusesAnImageTheCheckDidNotSee(t *testing.T) {
	f := newFake(t)
	f.check.RemoteDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "another pull or build") || f.body != nil ||
		slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Fatalf("result %+v, calls %v", r, f.calls)
	}
}

// A cancel while the new container is set up stops before the old one is
// touched; a cancel after the comparison rolls back before the next step.
// Calls already sent run to their end.
func TestRunTakesACancelBetweenSteps(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onCreate = cancel
	r := run(ctx, f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "interrupted") || f.body != nil {
		t.Fatalf("during set up: result %+v, calls %v", r, f.calls)
	}

	f = newFake(t)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.tamper = func(map[string]any) {
		if !f.newStarted {
			cancel()
		}
	}
	r = run(ctx, f, "apps-web-1", f.opts())
	if r.Outcome != RolledBack || !strings.Contains(r.Reason, "interrupted before the step to stop") {
		t.Fatalf("after the comparison: result %+v, calls %v", r, f.calls)
	}
	if slices.Contains(f.calls, "stop old") || !f.oldRunning || f.nameOf("old") != "apps-web-1" || f.body != nil {
		t.Fatalf("calls %v", f.calls)
	}
}

// Rolled back only when the old container runs under its own name.
func TestRunRollbackChecksTheOldContainerRuns(t *testing.T) {
	f := newFake(t)
	f.fail["start new"] = errors.New("boom")
	f.oldStaysDown = true
	r := run(context.Background(), f, "apps-web-1", f.opts())
	if r.Outcome != Failed || !strings.Contains(r.Reason, "see the old container running as apps-web-1") {
		t.Fatalf("result %+v", r)
	}
}
