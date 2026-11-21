package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// digestC is a digest the fake registry never has.
const digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestCheck(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[0], digestA}
	f.old["team/app@"+digestB] = true // what team/app:1 pointed at before
	f.manifests["me/built:dev"] = fakeManifest{manifestTypes[0], digestA}
	f.private["me/secret"] = true
	f.manifests["me/secret:1"] = fakeManifest{manifestTypes[0], digestA}
	f.broken["team/down"] = http.StatusBadGateway
	c := f.client(nil)
	h := f.host()
	app := func(d string) []string { return []string{h + "/team/app@" + d} }

	tests := []struct {
		name   string
		target Target
		want   Result
	}{
		{"same digest", Target{Image: h + "/team/app:1", RepoDigests: app(digestA)},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the tag moved on the registry", Target{Image: h + "/team/app:1", RepoDigests: app(digestB)},
			Result{Status: UpdateAvailable, LocalDigest: digestB, RemoteDigest: digestA}},
		{"one of several digests matches", Target{Image: h + "/team/app:1", RepoDigests: append(app(digestB), app(digestA)...)},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"pinned", Target{Image: h + "/team/app@" + digestB}, Result{Status: Pinned, LocalDigest: digestB}},
		{"no repo digest (Docker 24 local build)", Target{Image: h + "/me/built:dev", RepoDigests: []string{}}, Result{Status: NotCheckable}},
		{"a repo digest of another repository", Target{Image: h + "/team/app:1", RepoDigests: []string{"docker.io/library/app@" + digestA}}, Result{Status: NotCheckable}},
		{"a never-pushed build with its own repo digest (containerd store)", Target{Image: h + "/me/new:dev", RepoDigests: []string{h + "/me/new@" + digestB}},
			Result{Status: NotCheckable, LocalDigest: digestB}},
		{"a never-pushed build named like a registry image (containerd store)", Target{Image: h + "/me/built:dev", RepoDigests: []string{h + "/me/built@" + digestC}},
			Result{Status: NotCheckable, LocalDigest: digestC, RemoteDigest: digestA}},
		{"an old digest the registry deleted", Target{Image: h + "/team/app:1", RepoDigests: app(digestC)},
			Result{Status: NotCheckable, LocalDigest: digestC, RemoteDigest: digestA}},
		{"private without credentials", Target{Image: h + "/me/secret:1", RepoDigests: []string{h + "/me/secret@" + digestA}},
			Result{Status: NotCheckable, LocalDigest: digestA}},
		{"registry error", Target{Image: h + "/team/down:1", RepoDigests: []string{h + "/team/down@" + digestA}},
			Result{Status: Failed, LocalDigest: digestA}},
		{"an image ID", Target{Image: "sha256:" + digestA[7:]}, Result{Status: NotCheckable}},
		{"the tag moved to a pull and the old image kept no digest (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, ImageID: digestB, NamedRepoDigests: app(digestA)},
			Result{Status: UpdateAvailable, RemoteDigest: digestA}},
		{"a local build with no digest, then the tag moved to a pull",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, ImageID: digestC, NamedRepoDigests: app(digestA)},
			Result{Status: NotCheckable, RemoteDigest: digestA}},
		{"no digest and no image ID, then the tag moved to a pull",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, NamedRepoDigests: app(digestA)},
			Result{Status: NotCheckable, RemoteDigest: digestA}},
		{"the tag moved to a local build the registry does not have",
			Target{Image: h + "/me/new:dev", TagMoved: true, NamedRepoDigests: []string{h + "/me/new@" + digestB}},
			Result{Status: NotCheckable}},
		{"the tag moved to a local build with its own repo digest (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, NamedRepoDigests: app(digestC)},
			Result{Status: NotCheckable}},
		{"the tag moved to a local build with no repo digest (classic store)",
			Target{Image: h + "/team/app:1", RepoDigests: app(digestB), TagMoved: true, NamedRepoDigests: []string{}},
			Result{Status: NotCheckable}},
		{"the tag moved but the old image kept its digest (classic store)",
			Target{Image: h + "/team/app:1", RepoDigests: app(digestA), TagMoved: true, NamedRepoDigests: app(digestA)},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the tag moved and the registry went back to what runs (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, ImageID: digestA, NamedRepoDigests: app(digestB)},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the image ID is the pulled digest (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: app(digestB), ImageID: digestA},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"no repo digest, and the image ID is what the tag names (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, ImageID: digestA},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"no repo digest, and the registry had the image ID before (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, ImageID: digestB},
			Result{Status: UpdateAvailable, LocalDigest: digestB, RemoteDigest: digestA}},
		{"no repo digest, and an image ID the registry never had (classic store, local build)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, ImageID: digestC},
			Result{Status: NotCheckable, RemoteDigest: digestA}},
		{"no repo digest, and a registry error",
			Target{Image: h + "/team/down:1", RepoDigests: []string{}, ImageID: digestA},
			Result{Status: Failed}},
	}
	for _, tt := range tests {
		got := c.Check(context.Background(), tt.target)
		if got.Status != tt.want.Status || got.LocalDigest != tt.want.LocalDigest || got.RemoteDigest != tt.want.RemoteDigest {
			t.Errorf("%s: Check = %+v, want %+v", tt.name, got, tt.want)
		}
		if (got.Status == NotCheckable || got.Status == Failed) && got.Reason == "" {
			t.Errorf("%s: no reason given", tt.name)
		}
	}
}

func TestLocalDigestsMatchDockerHubShortNames(t *testing.T) {
	ref, _ := ParseReference("alpine:3.20")
	got := localDigests(ref, []string{"alpine@" + digestA, "docker.io/library/alpine@" + digestA, "myalpine@" + digestB, "broken"})
	if !slices.Equal(got, []string{digestA}) {
		t.Errorf("localDigests = %v, want [%s]", got, digestA)
	}
}

func TestCheckAllAsksOncePerImage(t *testing.T) {
	f := newFakeRegistry(t, "")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[0], digestA}
	f.manifests["team/web:2"] = fakeManifest{manifestTypes[0], digestB}
	h := f.host()
	f.old["team/web@"+digestA] = true
	app := Target{Image: h + "/team/app:1", RepoDigests: []string{h + "/team/app@" + digestA}}
	web := Target{Image: h + "/team/web:2", RepoDigests: []string{h + "/team/web@" + digestA}}
	got := f.client(nil).CheckAll(context.Background(), []Target{app, web, app})
	// One HEAD per tag, and one for the digest web runs, which differs.
	if heads, _ := f.counts(); heads != 3 {
		t.Errorf("%d HEADs for two distinct images, want 3", heads)
	}
	want := []Status{UpToDate, UpdateAvailable, UpToDate}
	for i, r := range got {
		if r.Status != want[i] {
			t.Errorf("result %d = %s, want %s", i, r.Status, want[i])
		}
	}
}

// A tag that points at a new index whose image for the container's platform
// is the one it runs is up to date: the publisher added a platform or an
// attestation. On the classic store the image ID is the config digest.
// sha256Of is the digest a registry gives a manifest with this body.
func sha256Of(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestCheckSeesThroughANewIndex(t *testing.T) {
	const (
		config   = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		armConf  = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
		oldIndex = digestB
	)
	amdBody := `{"config":{"digest":"` + config + `"}}`
	armBody := `{"config":{"digest":"` + armConf + `"}}`
	// An attestation whose config is the one the amd64 image has: it must
	// never be taken for an image.
	attBody := `{"config":{"digest":"` + config + `"}, "attestation":true}`
	amd64, arm64, att := sha256Of(amdBody), sha256Of(armBody), sha256Of(attBody)
	indexBody := `{"mediaType":"` + manifestTypes[0] + `","manifests":[` +
		`{"digest":"` + att + `","platform":{"os":"unknown","architecture":"unknown"}},` +
		`{"digest":"` + amd64 + `","platform":{"os":"linux","architecture":"amd64"}},` +
		`{"digest":"` + arm64 + `","platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`
	newIndex := sha256Of(indexBody)
	f := newFakeRegistry(t, "bearer")
	f.manifests["library/traefik:latest"] = fakeManifest{manifestTypes[0], newIndex}
	f.old["library/traefik@"+oldIndex] = true // what the tag pointed at before
	for digest, body := range map[string]string{newIndex: indexBody, amd64: amdBody, arm64: armBody, att: attBody} {
		f.old["library/traefik@"+digest] = true
		f.bodies["library/traefik@"+digest] = body
	}
	c := f.client(nil)
	img := f.host() + "/library/traefik:latest"
	old := []string{f.host() + "/library/traefik@" + oldIndex}
	linux := func(arch, variant string) Platform {
		return Platform{OS: "linux", Architecture: arch, Variant: variant}
	}

	tests := []struct {
		name   string
		target Target
		want   Status
	}{
		{"same image for amd64", Target{Image: img, RepoDigests: old, ImageID: config, Platform: linux("amd64", "")}, UpToDate},
		{"same image for arm64, inspect without a variant", Target{Image: img, RepoDigests: old, ImageID: armConf, Platform: linux("arm64", "")}, UpToDate},
		{"same image, no repo digest left", Target{Image: img, ImageID: config, Platform: linux("amd64", "")}, UpToDate},
		{"same image, the tag moved on the host", Target{Image: img, ImageID: config, TagMoved: true,
			NamedRepoDigests: old, Platform: linux("amd64", "")}, UpToDate},
		{"same image, the tag names a local build", Target{Image: img, ImageID: config, TagMoved: true, Platform: linux("amd64", "")}, UpToDate},
		{"same image, the tag names an image the registry lost", Target{Image: img, ImageID: config, TagMoved: true,
			NamedRepoDigests: []string{f.host() + "/library/traefik@" + digestC}, Platform: linux("amd64", "")}, UpToDate},
		{"a new image for amd64", Target{Image: img, RepoDigests: old, ImageID: armConf, Platform: linux("amd64", "")}, UpdateAvailable},
		{"no image for this platform", Target{Image: img, RepoDigests: old, ImageID: config, Platform: linux("riscv64", "")}, UpdateAvailable},
		{"platform unknown", Target{Image: img, RepoDigests: old, ImageID: config}, UpdateAvailable},
		{"platform without an os", Target{Image: img, RepoDigests: old, ImageID: config, Platform: Platform{Architecture: "amd64"}}, UpdateAvailable},
		{"an attestation is not an image", Target{Image: img, RepoDigests: old, ImageID: config,
			Platform: Platform{OS: "unknown", Architecture: "unknown"}}, UpdateAvailable},
		{"no repo digest and another image", Target{Image: img, ImageID: armConf, Platform: linux("amd64", "")}, NotCheckable},
	}
	for _, tt := range tests {
		got := c.Check(context.Background(), tt.target)
		if got.Status != tt.want || got.RemoteDigest != newIndex {
			t.Errorf("%s: Check = %+v, want %s", tt.name, got, tt.want)
		}
		if tt.want == UpToDate && !strings.Contains(got.Reason, "new index") {
			t.Errorf("%s: reason %q does not name the index", tt.name, got.Reason)
		}
	}

	// A host that already has what the tag points at has no other manifest
	// to look into: what the HEADs found stands, and no GET is sent.
	f.gets = 0
	has := Target{Image: img, RepoDigests: []string{f.host() + "/library/traefik@" + newIndex}, ImageID: config,
		TagMoved: true, Platform: linux("amd64", "")}
	if got := c.Check(context.Background(), has); got.Status != NotCheckable || f.gets != 0 {
		t.Errorf("the host has the tag's digest: %+v after %d GETs, want not_checkable and none", got, f.gets)
	}

	// Targets that differ only in platform are checked apart.
	all := c.CheckAll(context.Background(), []Target{
		{Image: img, RepoDigests: old, ImageID: config, Platform: linux("amd64", "")},
		{Image: img, RepoDigests: old, ImageID: config, Platform: linux("arm64", "")},
	})
	if all[0].Status != UpToDate || all[1].Status != UpdateAvailable {
		t.Errorf("CheckAll by platform = %s, %s, want up_to_date, update_available", all[0].Status, all[1].Status)
	}
	// Even when the platforms would read the same written out.
	all = c.CheckAll(context.Background(), []Target{
		{Image: img, RepoDigests: old, ImageID: armConf, Platform: linux("arm64", "v8")},
		{Image: img, RepoDigests: old, ImageID: armConf, Platform: linux("arm64/v8", "")},
	})
	if all[0].Status != UpToDate || all[1].Status != UpdateAvailable {
		t.Errorf("CheckAll by platform fields = %s, %s, want up_to_date, update_available", all[0].Status, all[1].Status)
	}

	// A tag that points at a single manifest is read directly, and the
	// reason does not speak of an index.
	plain := sha256Of(amdBody)
	f.manifests["library/plain:1"] = fakeManifest{manifestTypes[2], plain}
	f.bodies["library/plain@"+plain] = amdBody
	f.old["library/plain@"+oldIndex] = true
	single := Target{Image: f.host() + "/library/plain:1",
		RepoDigests: []string{f.host() + "/library/plain@" + oldIndex}, ImageID: config, Platform: linux("amd64", "")}
	if got := c.Check(context.Background(), single); got.Status != UpToDate || strings.Contains(got.Reason, "index") || got.Reason == "" {
		t.Errorf("single manifest: %+v", got)
	}

	// An index with no entries is not an image manifest, whatever else it
	// carries.
	emptyBody := `{"mediaType":"` + manifestTypes[0] + `","manifests":[],"config":{"digest":"` + config + `"}}`
	empty := sha256Of(emptyBody)
	f.manifests["library/empty:1"] = fakeManifest{manifestTypes[0], empty}
	f.bodies["library/empty@"+empty] = emptyBody
	f.old["library/empty@"+oldIndex] = true
	if got := c.Check(context.Background(), Target{Image: f.host() + "/library/empty:1",
		RepoDigests: []string{f.host() + "/library/empty@" + oldIndex}, ImageID: config, Platform: linux("amd64", "")}); got.Status != UpdateAvailable {
		t.Errorf("empty index: %+v, want update_available", got)
	}

	// A body that does not hash to the digest asked for is not trusted: cut
	// short, or another manifest than the HEAD named.
	for name, body := range map[string]string{"cut short": amdBody[:len(amdBody)-1], "another manifest": amdBody + " "} {
		f.bodies["library/plain@"+plain] = body
		if got := c.Check(context.Background(), single); got.Status != UpdateAvailable {
			t.Errorf("%s: %+v, want update_available", name, got)
		}
	}
}

// GETs that hang do not take the time of the HEADs, which all come first,
// and each check falls back to what its HEADs found.
func TestCheckSameImageLookupIsBounded(t *testing.T) {
	const config = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	f := newFakeRegistry(t, "bearer")
	f.hangGet = true
	var targets []Target
	for i := range checkWorkers + 2 {
		repo := fmt.Sprintf("team/app%d", i)
		f.manifests[repo+":1"] = fakeManifest{manifestTypes[0], digestA}
		f.old[repo+"@"+digestB] = true
		targets = append(targets, Target{Image: f.host() + "/" + repo + ":1", RepoDigests: []string{f.host() + "/" + repo + "@" + digestB},
			ImageID: config, Platform: Platform{OS: "linux", Architecture: "amd64"}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c := f.client(nil)
	for i, got := range c.CheckAll(ctx, targets) {
		if got.Status != UpdateAvailable {
			t.Errorf("target %d: %+v, want update_available", i, got)
		}
	}
	if got := c.Check(ctx, targets[0]); got.Status != Failed {
		t.Errorf("after the deadline: %+v, want failed (the deadline was spent by the GETs, not before)", got)
	}
}

// However many reads hang, CheckAll gives them one budget together.
func TestCheckAllSameImageBudget(t *testing.T) {
	const config = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	f := newFakeRegistry(t, "bearer")
	f.hangGet = true
	var targets []Target
	for i := range 3 * checkWorkers {
		repo := fmt.Sprintf("team/app%d", i)
		f.manifests[repo+":1"] = fakeManifest{manifestTypes[0], digestA}
		f.old[repo+"@"+digestB] = true
		targets = append(targets, Target{Image: f.host() + "/" + repo + ":1", RepoDigests: []string{f.host() + "/" + repo + "@" + digestB},
			ImageID: config, Platform: Platform{OS: "linux", Architecture: "amd64"}})
	}
	budget := sameImageBudget
	sameImageBudget = 200 * time.Millisecond
	t.Cleanup(func() { sameImageBudget = budget })
	start := time.Now()
	for i, got := range f.client(nil).CheckAll(context.Background(), targets) {
		if got.Status != UpdateAvailable {
			t.Errorf("target %d: %+v, want update_available", i, got)
		}
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("CheckAll took %s with a budget of %s", took, sameImageBudget)
	}
}
