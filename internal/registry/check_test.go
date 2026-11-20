package registry

import (
	"context"
	"net/http"
	"slices"
	"testing"
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
