//go:build e2e

package update

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/registry"
)

// TestE2EUpdate runs against the daemons that `make e2e` starts, with the
// containers deploy/compose/e2e/update.sh creates on each of them.
func TestE2EUpdate(t *testing.T) {
	hosts := strings.Fields(os.Getenv("HIKIFUNE_E2E_HOSTS"))
	if len(hosts) == 0 {
		t.Fatal("HIKIFUNE_E2E_HOSTS is empty; run `make e2e`")
	}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			c, err := docker.New(host)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			v, err := c.CheckAPI(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Docker %s, API %s", v.Version, c.APIVersion())
			e2eCases(ctx, t, c, optFor(host))
		})
	}
}

// testRegistries are the names of the shared test registry in
// deploy/compose/compose.e2e.yaml, which speaks plain HTTP.
var testRegistries = []string{"registry:5000", "registry-v2:5000"}

// optFor watches long enough for a health check every second to settle and
// asks the test registry through the real registry check.
func optFor(string) Options {
	return Options{Watch: 8 * time.Second, Poll: 500 * time.Millisecond,
		Check: registry.New(nil, nil).PlainHTTP(testRegistries).Check}
}

type inspected struct {
	docker.Container
	doc map[string]any
}

func inspect(ctx context.Context, t *testing.T, c *docker.Client, name string) inspected {
	t.Helper()
	ct, err := c.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	doc, err := decode(ct.Raw)
	if err != nil {
		t.Fatal(err)
	}
	return inspected{ct, doc}
}

func (i inspected) at(path ...string) any {
	var v any = i.doc
	for _, p := range path {
		v = object(v)[p]
	}
	return v
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// noLeftovers checks that no temporary container is left for name.
func noLeftovers(ctx context.Context, t *testing.T, c *docker.Client, name string) {
	t.Helper()
	for _, n := range []string{name + newSuffix, name + oldSuffix} {
		if _, err := c.ContainerInspect(ctx, n); !docker.IsNotFound(err) {
			t.Errorf("%s is left (%v)", n, err)
		}
	}
}

func mustUpdate(ctx context.Context, t *testing.T, c *docker.Client, opt Options, name string) (before, after inspected) {
	t.Helper()
	before = inspect(ctx, t, c, name)
	r := Container(ctx, c, name, opt)
	t.Logf("%s: %+v", name, r)
	if r.Outcome != Updated {
		t.Fatalf("%s: %s: %s", name, r.Outcome, r.Reason)
	}
	after = inspect(ctx, t, c, name)
	if after.ID == before.ID || after.Image == before.Image || after.Image != r.NewImageID {
		t.Errorf("%s: container %s on %s, was %s on %s", name, after.ID, after.Image, before.ID, before.Image)
	}
	if !after.State.Running {
		t.Errorf("%s: not running after the update: %+v", name, after.State)
	}
	// The old image waits, nameless, for the prune after the run (R20):
	// nothing in this package prunes, so it must still be there.
	if img, err := c.ImageInspect(ctx, before.Image); docker.IsNotFound(err) {
		t.Errorf("%s: the old image %s is gone before any prune: %v", name, before.Image, err)
	} else if err != nil {
		t.Errorf("%s: inspect the old image %s: %v", name, before.Image, err)
	} else if len(img.RepoTags) > 0 {
		t.Errorf("%s: the old image is still named %v (%s)", name, img.RepoTags, r.Note)
	}
	noLeftovers(ctx, t, c, name)
	return before, after
}

func e2eCases(ctx context.Context, t *testing.T, c *docker.Client, opt Options) {
	t.Run("exec-form health check on an image without a shell", func(t *testing.T) {
		_, after := mustUpdate(ctx, t, c, opt, "upd-noshell")
		if got := asJSON(after.at("Config", "Healthcheck", "Test")); got != `["CMD","/busybox","true"]` {
			t.Errorf("health check = %s", got)
		}
		if after.State.Health == nil || after.State.Health.Status != "healthy" {
			t.Errorf("health = %+v", after.State.Health)
		}
	})

	t.Run("three networks attached before start", func(t *testing.T) {
		before, after := mustUpdate(ctx, t, c, opt, "upd-nets")
		nets := object(after.at("NetworkSettings", "Networks"))
		for _, n := range []string{"upd-n1", "upd-n2", "upd-n3"} {
			if nets[n] == nil {
				t.Errorf("%s not attached", n)
			}
		}
		aliases := func(n string) []string {
			var out []string
			for _, a := range list(object(nets[n])["Aliases"]) {
				out = append(out, a.(string))
			}
			return out
		}
		if !slices.Contains(aliases("upd-n1"), "a1") || !slices.Contains(aliases("upd-n2"), "a2") {
			t.Errorf("aliases n1 %v, n2 %v", aliases("upd-n1"), aliases("upd-n2"))
		}
		for _, n := range []string{"upd-n1", "upd-n2", "upd-n3"} {
			for _, a := range aliases(n) {
				if strings.HasPrefix(before.ID, a) {
					t.Errorf("%s: the old container's ID %s is an alias", n, a)
				}
			}
		}
	})

	t.Run("published ports", func(t *testing.T) {
		_, after := mustUpdate(ctx, t, c, opt, "upd-web")
		if got := asJSON(after.at("HostConfig", "PortBindings", "80/tcp")); !strings.Contains(got, `"HostPort":"18080"`) {
			t.Errorf("port bindings = %s", got)
		}
	})

	t.Run("volumes, environment, entrypoint and the compose label", func(t *testing.T) {
		before, after := mustUpdate(ctx, t, c, opt, "upd-app")
		env := list(after.at("Config", "Env"))
		for _, want := range []string{"IMG=v2", "KEEP=mine", "EXTRA=x"} {
			if !slices.Contains(env, any(want)) {
				t.Errorf("Env %v has no %s", env, want)
			}
		}
		if got := asJSON(after.at("Config", "Entrypoint")) + asJSON(after.at("Config", "Cmd")); got != `["/bin/sleep"]["2d"]` {
			t.Errorf("entrypoint and command = %s", got)
		}
		// The label holds what compose would write: the platform image's ID
		// where the daemon can tell it, the image ID otherwise.
		want := after.Image
		if id, ok, err := c.ImagePlatformID(ctx, after.Image); err != nil {
			t.Errorf("platform image of %s: %v", after.Image, err)
		} else if ok {
			want = id
		}
		if got := after.Config.Labels[composeImageLabel]; got != want {
			t.Errorf("compose label = %s, want %s (image %s)", got, want, after.Image)
		}
		t.Logf("compose label %s, image %s", after.Config.Labels[composeImageLabel], after.Image)
		mounts := func(i inspected) map[string]string {
			m := map[string]string{}
			for _, raw := range list(i.doc["Mounts"]) {
				mt := object(raw)
				src := mt["Source"]
				if mt["Type"] == "volume" {
					src = mt["Name"]
				}
				m[mt["Destination"].(string)] = asJSON(mt["Type"]) + " " + asJSON(src) + " rw=" + asJSON(mt["RW"])
			}
			return m
		}
		b, a := mounts(before), mounts(after)
		for dst, want := range b {
			if a[dst] != want {
				t.Errorf("mount %s: %s, was %s", dst, a[dst], want)
			}
		}
		if len(b) != 3 {
			t.Errorf("fixture mounts = %v, want /data, /bind and the anonymous /anon", b)
		}
	})

	t.Run("new image that exits at once is rolled back", func(t *testing.T) {
		before := inspect(ctx, t, c, "upd-crash")
		r := Container(ctx, c, "upd-crash", opt)
		t.Logf("%+v", r)
		if r.Outcome != RolledBack || !(strings.Contains(r.Reason, "restarted") || strings.Contains(r.Reason, "exited")) {
			t.Fatalf("%s: %s", r.Outcome, r.Reason)
		}
		after := inspect(ctx, t, c, "upd-crash")
		if after.ID != before.ID || after.Image != before.Image || !after.State.Running {
			t.Errorf("after rollback: %s on %s, %+v", after.ID, after.Image, after.State)
		}
		if object(after.at("NetworkSettings", "Networks"))["upd-n1"] == nil {
			t.Error("upd-n1 is no longer attached")
		}
		noLeftovers(ctx, t, c, "upd-crash")
	})

	t.Run("new image whose health check fails is rolled back", func(t *testing.T) {
		before := inspect(ctx, t, c, "upd-unhealthy")
		r := Container(ctx, c, "upd-unhealthy", opt)
		t.Logf("%+v", r)
		if r.Outcome != RolledBack || !strings.Contains(r.Reason, "unhealthy") {
			t.Fatalf("%s: %s", r.Outcome, r.Reason)
		}
		after := inspect(ctx, t, c, "upd-unhealthy")
		if after.ID != before.ID || !after.State.Running {
			t.Errorf("after rollback: %s, %+v", after.ID, after.State)
		}
		noLeftovers(ctx, t, c, "upd-unhealthy")
	})

	t.Run("a container created with --rm is refused", func(t *testing.T) {
		before := inspect(ctx, t, c, "upd-rm")
		r := Container(ctx, c, "upd-rm", opt)
		if r.Outcome != Failed || !strings.Contains(r.Reason, "--rm") {
			t.Fatalf("%s: %s", r.Outcome, r.Reason)
		}
		if after := inspect(ctx, t, c, "upd-rm"); after.ID != before.ID || !after.State.Running {
			t.Errorf("the container was touched: %+v", after.State)
		}
	})

	t.Run("device requests are copied", func(t *testing.T) {
		old := inspect(ctx, t, c, "upd-gpu")
		var r Result
		newID, ok := prepare(ctx, c, old.Container, opt, &r)
		if !ok {
			t.Fatalf("%s: %s", r.Outcome, r.Reason)
		}
		defer c.ContainerRemove(ctx, newID, true)
		got := inspect(ctx, t, c, newID)
		want := asJSON(old.at("HostConfig", "DeviceRequests"))
		if g := asJSON(got.at("HostConfig", "DeviceRequests")); g != want || !strings.Contains(want, `"gpu"`) {
			t.Errorf("device requests = %s, want %s", g, want)
		}
	})
}
