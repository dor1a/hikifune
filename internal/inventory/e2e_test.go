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
	// The update and server tests' containers carry a hikifune.e2e label
	// and change while this runs.
	byName := map[string]Container{}
	for _, ct := range rep.Containers {
		if ct.Labels["hikifune.e2e"] == "" {
			byName[ct.Name] = ct
		}
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
		{"tag-moved", false, false, "running"},
	}
	if len(byName) != len(want) {
		t.Errorf("%d containers, want %d", len(byName), len(want))
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
		// The containerd store (Docker 29) drops the record of a local image
		// rebuilt under the same name, so tag-moved may have an image error.
		if ct.ImageID == "" || (ct.ImageError != "" && w.name != "tag-moved") {
			t.Errorf("%s: image id %q, image error %q", w.name, ct.ImageID, ct.ImageError)
		}
		// Only tag-moved runs an image its reference no longer names.
		if ct.TagMoved != (w.name == "tag-moved") {
			t.Errorf("%s: tag moved = %v", w.name, ct.TagMoved)
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
	t.Logf("tag-moved image error: %q", byName["tag-moved"].ImageError)
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
