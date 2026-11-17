//go:build e2e

package inventory

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
)

// TestE2EInventory runs against the daemons that `make e2e` starts and fills
// from deploy/compose/e2e/containers.sh. HIKIFUNE_E2E_HOSTS lists them,
// separated by spaces.
func TestE2EInventory(t *testing.T) {
	hosts := strings.Fields(os.Getenv("HIKIFUNE_E2E_HOSTS"))
	if len(hosts) == 0 {
		t.Fatal("HIKIFUNE_E2E_HOSTS is empty; run `make e2e`")
	}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			c, err := docker.New(host)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Collect(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Docker %s, API %s (min %s), %d containers",
				rep.Docker.Version, rep.Docker.APIVersion, rep.Docker.MinAPIVersion, len(rep.Containers))
			checkE2E(t, rep)
		})
	}
}

func checkE2E(t *testing.T, rep Report) {
	t.Helper()
	byName := map[string]Container{}
	for _, ct := range rep.Containers {
		byName[ct.Name] = ct
	}
	want := []struct {
		name       string
		protected  bool
		labelError bool
		state      string
	}{
		{"plain", false, false, "running"},
		{"protected", true, false, "running"},
		{"unprotected", false, false, "running"},
		{"typo", true, true, "running"},
		{"bare-label", true, false, "running"},
		{"compose-web", false, false, "running"},
		{"healthy", false, false, "running"},
		{"created", false, false, "created"},
		{"exited", false, false, "exited"},
		{"local-build", false, false, "running"},
	}
	if len(rep.Containers) != len(want) {
		t.Errorf("%d containers, want %d", len(rep.Containers), len(want))
	}
	for _, w := range want {
		ct, ok := byName[w.name]
		if !ok {
			t.Errorf("%s: missing", w.name)
			continue
		}
		if ct.Protected != w.protected {
			t.Errorf("%s: protected = %v, want %v", w.name, ct.Protected, w.protected)
		}
		if (ct.LabelError != "") != w.labelError {
			t.Errorf("%s: label error = %q", w.name, ct.LabelError)
		}
		if ct.State != w.state {
			t.Errorf("%s: state = %q, want %q", w.name, ct.State, w.state)
		}
		if ct.ImageID == "" || ct.ImageError != "" {
			t.Errorf("%s: image id %q, image error %q", w.name, ct.ImageID, ct.ImageError)
		}
	}

	if p := byName["plain"]; p.Image != "busybox:1.37" || len(p.RepoDigests) == 0 || !strings.HasPrefix(p.RepoDigests[0], "busybox@sha256:") {
		t.Errorf("plain: image %q, repo digests %v", p.Image, p.RepoDigests)
	}
	// A local build has no repo digest on the classic image store (Docker 24),
	// but the containerd store (the default on new Docker 29 installs) gives it
	// one for its own name, though it was never pushed.
	l := byName["local-build"]
	if l.Image != "local/built:1" {
		t.Errorf("local-build: image %q", l.Image)
	}
	for _, d := range l.RepoDigests {
		if !strings.HasPrefix(d, "local/built@sha256:") {
			t.Errorf("local-build: unexpected repo digest %q", d)
		}
	}
	t.Logf("local-build repo digests: %v", l.RepoDigests)
	if w := byName["compose-web"]; w.ComposeProject != "demo" || w.ComposeService != "web" {
		t.Errorf("compose-web: project %q, service %q", w.ComposeProject, w.ComposeService)
	}
	if h := byName["healthy"]; h.Health != "healthy" {
		t.Errorf("healthy: health = %q", h.Health)
	}
	if p := byName["plain"]; p.Health != "" {
		t.Errorf("plain: health = %q, want none", p.Health)
	}
}
