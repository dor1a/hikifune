package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/update"
)

const token = "0123456789abcdef0123456789abcdef"

// fakeAgent answers with fixed results; release lets a running update end.
func fakeAgent(t *testing.T) (*Agent, chan struct{}, *httptest.Server) {
	t.Helper()
	release := make(chan struct{})
	a := &Agent{token: token, watch: time.Minute, limit: JobLimit, base: context.Background(), now: time.Now,
		version: func(context.Context) protocol.Version {
			return protocol.Version{Agent: "v1", Docker: &inventory.Docker{Version: "29.1.0", APIVersion: "1.52"}}
		},
		inventory: func(context.Context) (inventory.Report, error) { return inventory.Report{}, nil },
		check: func(context.Context) (protocol.CheckReport, error) {
			return protocol.CheckReport{Containers: []protocol.Checked{{Name: "web", Check: registry.Result{Status: registry.UpToDate}}}}, nil
		},
		inspect: func(_ context.Context, name string) (docker.Container, error) {
			return docker.Container{ID: "id-" + name, Name: "/" + name}, nil
		},
		update: func(_ context.Context, name string, watch time.Duration) update.Result {
			<-release
			return update.Result{Container: name, Outcome: update.Updated, Reason: watch.String()}
		},
		prune: func(context.Context) (int64, error) { return 42, nil },
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return a, release, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, auth, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestTokenIsRequired(t *testing.T) {
	_, _, srv := fakeAgent(t)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x", token} {
		if code, _ := call(t, srv, "GET", protocol.PathVersion, auth, ""); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: %d", auth, code)
		}
	}
	if code, _ := call(t, srv, "GET", protocol.PathVersion, "Bearer "+token, ""); code != http.StatusOK {
		t.Errorf("right token: %d", code)
	}
	if code, _ := call(t, srv, "GET", protocol.PathHealth, "", ""); code != http.StatusOK {
		t.Errorf("healthz without a token: %d", code)
	}
	if _, err := New(context.Background(), Config{Token: "short", Docker: &docker.Client{}, Registry: registry.New(nil, nil)}); err == nil {
		t.Error("a short token was taken")
	}
}

func TestUpdateJobs(t *testing.T) {
	a, release, srv := fakeAgent(t)
	auth := "Bearer " + token
	code, body := call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"web","watch_seconds":5}`)
	var job protocol.Job
	json.Unmarshal(body, &job)
	if code != http.StatusAccepted || job.ID == "" || job.State != protocol.JobRunning {
		t.Fatalf("start: %d %s", code, body)
	}

	// One job at a time on a host (R13); the answer names the running one.
	code, body = call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"db"}`)
	var busy protocol.Error
	json.Unmarshal(body, &busy)
	if code != http.StatusConflict || busy.Busy == nil || busy.Busy.ID != job.ID {
		t.Fatalf("second start: %d %s", code, body)
	}
	if code, _ := call(t, srv, "POST", protocol.PathPrune, auth, ""); code != http.StatusConflict {
		t.Fatalf("prune during an update: %d", code)
	}

	close(release)
	a.Wait()
	code, body = call(t, srv, "GET", protocol.PathUpdates+"/"+job.ID, auth, "")
	json.Unmarshal(body, &job)
	if code != http.StatusOK || job.State != protocol.JobDone || job.Result == nil ||
		job.Result.Outcome != update.Updated || job.Result.Reason != "5s" || job.Finished == nil {
		t.Fatalf("finished job: %d %s", code, body)
	}
	if code, _ := call(t, srv, "GET", protocol.PathUpdates+"/nope", auth, ""); code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", code)
	}
	code, body = call(t, srv, "POST", protocol.PathPrune, auth, "")
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"reclaimed":42`)) {
		t.Fatalf("prune: %d %s", code, body)
	}
}

func TestUpdateRefusesBadRequestsAndRecreatedContainers(t *testing.T) {
	a, release, srv := fakeAgent(t)
	close(release)
	auth := "Bearer " + token
	for _, body := range []string{``, `{}`, `{"container":" "}`, `{"container":"web","watch_seconds":-1}`, `{"container":"web","x":1}`} {
		if code, _ := call(t, srv, "POST", protocol.PathUpdates, auth, body); code != http.StatusBadRequest {
			t.Errorf("body %q: %d", body, code)
		}
	}
	// The check saw another container under this name.
	_, body := call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"web","id":"id-old"}`)
	var job protocol.Job
	json.Unmarshal(body, &job)
	a.Wait()
	_, body = call(t, srv, "GET", protocol.PathUpdates+"/"+job.ID, auth, "")
	json.Unmarshal(body, &job)
	if job.Result == nil || job.Result.Outcome != update.Failed || !strings.Contains(job.Result.Reason, "recreated") {
		t.Fatalf("recreated container: %s", body)
	}
	// The same container goes on, by its ID.
	var target string
	a.update = func(_ context.Context, name string, _ time.Duration) update.Result {
		target = name
		return update.Result{Container: name, Outcome: update.Updated}
	}
	_, body = call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"web","id":"id-web"}`)
	json.Unmarshal(body, &job)
	a.Wait()
	_, body = call(t, srv, "GET", protocol.PathUpdates+"/"+job.ID, auth, "")
	json.Unmarshal(body, &job)
	if job.Result == nil || job.Result.Outcome != update.Updated || target != "id-web" || job.Result.Container != "web" {
		t.Fatalf("same container: %s, updated %q", body, target)
	}
}

// A job that hangs, as on a pull that never ends, ends at its limit and
// frees the host.
func TestAJobHasALimit(t *testing.T) {
	a, _, srv := fakeAgent(t)
	a.watch, a.limit = time.Millisecond, 50*time.Millisecond
	a.update = func(ctx context.Context, name string, _ time.Duration) update.Result {
		<-ctx.Done()
		return update.Result{Container: name, Outcome: update.Failed, Reason: "pull: " + ctx.Err().Error()}
	}
	auth := "Bearer " + token
	call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"web","watch_seconds":0}`)
	done := make(chan struct{})
	go func() {
		a.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not end at its limit")
	}
	if code, _ := call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"db"}`); code != http.StatusAccepted {
		t.Fatalf("the host stayed busy: %d", code)
	}
	a.Wait()
}

// A prune and an update never run at the same time on a host.
func TestPruneHoldsTheHost(t *testing.T) {
	a, release, srv := fakeAgent(t)
	close(release)
	pruning, finish := make(chan struct{}), make(chan struct{})
	a.prune = func(context.Context) (int64, error) {
		close(pruning)
		<-finish
		return 0, nil
	}
	auth := "Bearer " + token
	done := make(chan int)
	go func() {
		code, _ := call(t, srv, "POST", protocol.PathPrune, auth, "")
		done <- code
	}()
	<-pruning
	if code, _ := call(t, srv, "POST", protocol.PathUpdates, auth, `{"container":"web"}`); code != http.StatusConflict {
		t.Errorf("update during a prune: %d", code)
	}
	if code, _ := call(t, srv, "POST", protocol.PathPrune, auth, ""); code != http.StatusConflict {
		t.Errorf("second prune: %d", code)
	}
	close(finish)
	if code := <-done; code != http.StatusOK {
		t.Errorf("prune: %d", code)
	}
}

func TestDependsOnAndSelf(t *testing.T) {
	if got := DependsOn("db:service_started:false, cache:service_healthy:true,"); !slices.Equal(got, []string{"db", "cache"}) {
		t.Errorf("DependsOn = %v", got)
	}
	if got := DependsOn(""); got != nil {
		t.Errorf("DependsOn of nothing = %v", got)
	}
	for img, want := range map[string]bool{
		"ghcr.io/dor1a/hikifune:0.1": true, "hikifune-agent": true, "registry:5000/e2e/hikifune-agent:1": true,
		"ghcr.io/dor1a/hikifune-ui:1": false, "nginx:1": false, "sha256:" + strings.Repeat("a", 64): false,
	} {
		if update.IsSelf(img) != want {
			t.Errorf("IsSelf(%q) = %v", img, !want)
		}
	}
}
