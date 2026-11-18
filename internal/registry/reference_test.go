package registry

import (
	"strings"
	"testing"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		in   string
		want Reference
	}{
		{"alpine", Reference{"docker.io", "library/alpine", "latest", ""}},
		{"alpine:3.20", Reference{"docker.io", "library/alpine", "3.20", ""}},
		{"grafana/grafana:12.1.0", Reference{"docker.io", "grafana/grafana", "12.1.0", ""}},
		{"docker.io/library/postgres:18", Reference{"docker.io", "library/postgres", "18", ""}},
		{"index.docker.io/traefik:v3", Reference{"docker.io", "library/traefik", "v3", ""}},
		{"ghcr.io/dor1a/kizuna:0.1.4-rc.1", Reference{"ghcr.io", "dor1a/kizuna", "0.1.4-rc.1", ""}},
		{"registry.example.com:5000/team/app", Reference{"registry.example.com:5000", "team/app", "latest", ""}},
		{"localhost/app:dev", Reference{"localhost", "app", "dev", ""}},
		{"localhost:5000/app", Reference{"localhost:5000", "app", "latest", ""}},
		{"alpine@" + digestA, Reference{"docker.io", "library/alpine", "", digestA}},
		{"ghcr.io/a/b:1.2@" + digestB, Reference{"ghcr.io", "a/b", "1.2", digestB}},
		{"quay.io/prometheus/node-exporter:v1.9.1", Reference{"quay.io", "prometheus/node-exporter", "v1.9.1", ""}},
		{"lscr.io/linuxserver/plex:Latest", Reference{"lscr.io", "linuxserver/plex", "Latest", ""}},
		{"REGISTRY/app:Latest", Reference{"registry", "app", "Latest", ""}},
		{"sha256:mytag", Reference{"docker.io", "library/sha256", "mytag", ""}},
	}
	for _, tt := range tests {
		got, err := ParseReference(tt.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseReference(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseReferenceRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"sha256:" + strings.Repeat("a", 64), // an image ID
		"Alpine:3",                          // upper case repository
		"alpine:",                           // empty tag
		"alpine:-x",
		"alpine@sha256:xyz",
		"ghcr.io//app",
		"app_:1",
	} {
		if r, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) = %+v, want an error", in, r)
		}
	}
}

func TestReferenceString(t *testing.T) {
	r, _ := ParseReference("alpine")
	if r.String() != "docker.io/library/alpine:latest" || r.Name() != "docker.io/library/alpine" {
		t.Errorf("String() = %q, Name() = %q", r.String(), r.Name())
	}
	if apiHost(r.Registry) != "registry-1.docker.io" || apiHost("ghcr.io") != "ghcr.io" {
		t.Error("apiHost does not send Docker Hub to registry-1.docker.io")
	}
}

func TestValidDigest(t *testing.T) {
	for d, want := range map[string]bool{
		digestA:                               true,
		"sha512:" + strings.Repeat("0", 128):  true,
		"sha256:" + strings.Repeat("a", 32):   false,
		"sha256:" + strings.Repeat("A", 64):   false,
		"!!!:abc":                             false,
		"multihash+base58:QmRZxt2b1FVZPNqd8h": true,
		"sha256":                              false,
		"sha256:":                             false,
	} {
		if validDigest(d) != want {
			t.Errorf("validDigest(%q) = %v, want %v", d, !want, want)
		}
	}
}
