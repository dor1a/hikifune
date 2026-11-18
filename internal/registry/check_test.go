package registry

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

func TestCheck(t *testing.T) {
	f := newFakeRegistry(t, "bearer")
	f.manifests["team/app:1"] = fakeManifest{manifestTypes[0], digestA}
	f.manifests["me/built:dev"] = fakeManifest{manifestTypes[0], digestA}
	f.private["me/secret"] = true
	f.manifests["me/secret:1"] = fakeManifest{manifestTypes[0], digestA}
	f.broken["team/down"] = http.StatusBadGateway
	c := f.client(nil)
	h := f.host()

	tests := []struct {
		name   string
		target Target
		want   Result
	}{
		{"same digest", Target{h + "/team/app:1", []string{h + "/team/app@" + digestA}, false, ""},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the tag moved", Target{h + "/team/app:1", []string{h + "/team/app@" + digestB}, false, ""},
			Result{Status: UpdateAvailable, LocalDigest: digestB, RemoteDigest: digestA}},
		{"one of several digests matches", Target{h + "/team/app:1", []string{h + "/team/app@" + digestB, h + "/team/app@" + digestA}, false, ""},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"pinned", Target{h + "/team/app@" + digestB, nil, false, ""}, Result{Status: Pinned, LocalDigest: digestB}},
		{"no repo digest (Docker 24 local build)", Target{h + "/me/built:dev", []string{}, false, ""}, Result{Status: NotCheckable}},
		{"a repo digest of another repository", Target{h + "/team/app:1", []string{"docker.io/library/app@" + digestA}, false, ""}, Result{Status: NotCheckable}},
		{"a never-pushed build with its own repo digest (containerd store)", Target{h + "/me/new:dev", []string{h + "/me/new@" + digestB}, false, ""},
			Result{Status: NotCheckable, LocalDigest: digestB}},
		{"private without credentials", Target{h + "/me/secret:1", []string{h + "/me/secret@" + digestA}, false, ""},
			Result{Status: NotCheckable, LocalDigest: digestA}},
		{"registry error", Target{h + "/team/down:1", []string{h + "/team/down@" + digestA}, false, ""},
			Result{Status: Failed, LocalDigest: digestA}},
		{"an image ID", Target{"sha256:" + digestA[7:], nil, false, ""}, Result{Status: NotCheckable}},
		{"the tag moved and the old image kept no digest (containerd store)", Target{h + "/team/app:1", []string{}, true, ""},
			Result{Status: UpdateAvailable, RemoteDigest: digestA}},
		{"the tag moved to a local build the registry does not have", Target{h + "/me/new:dev", nil, true, ""},
			Result{Status: NotCheckable}},
		{"the tag moved but the old image kept its digest (classic store)", Target{h + "/team/app:1", []string{h + "/team/app@" + digestA}, true, ""},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the tag moved and the registry went back to what runs (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{}, TagMoved: true, ImageID: digestA},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
		{"the image ID is the pulled digest (containerd store)",
			Target{Image: h + "/team/app:1", RepoDigests: []string{h + "/team/app@" + digestB}, ImageID: digestA},
			Result{Status: UpToDate, LocalDigest: digestA, RemoteDigest: digestA}},
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
	app := Target{h + "/team/app:1", []string{h + "/team/app@" + digestA}, false, ""}
	web := Target{h + "/team/web:2", []string{h + "/team/web@" + digestA}, false, ""}
	got := f.client(nil).CheckAll(context.Background(), []Target{app, web, app})
	if heads, _ := f.counts(); heads != 2 {
		t.Errorf("%d HEADs for two distinct images, want 2", heads)
	}
	want := []Status{UpToDate, UpdateAvailable, UpToDate}
	for i, r := range got {
		if r.Status != want[i] {
			t.Errorf("result %d = %s, want %s", i, r.Status, want[i])
		}
	}
}
