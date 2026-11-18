// Package registry asks image registries which digest an image reference
// points at now, and compares it with the image a host has. It only reads
// manifests with HEAD requests and never pulls anything.
package registry

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// DockerHub is the registry of a reference without a registry part.
	DockerHub = "docker.io"
	// dockerHubAPI is where Docker Hub serves the registry API.
	dockerHubAPI = "registry-1.docker.io"
	defaultTag   = "latest"
)

// Reference is an image reference, normalized the way Docker does it:
// alpine is docker.io/library/alpine:latest.
type Reference struct {
	Registry   string // host[:port], docker.io for Docker Hub
	Repository string // library/alpine on Docker Hub
	Tag        string // empty only when the reference is pinned by digest
	Digest     string // set when the reference is pinned by digest
}

// Name is registry/repository, the part repo digests are matched by.
func (r Reference) Name() string { return r.Registry + "/" + r.Repository }

func (r Reference) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// apiHost is the host that serves the registry API for a registry name.
func apiHost(registry string) string {
	if registry == DockerHub {
		return dockerHubAPI
	}
	return registry
}

// ParseReference reads an image reference as a container's Config.Image or a
// repo digest gives it. The first path part is the registry when it has a dot
// or a port or is localhost; otherwise the image is on Docker Hub, where a
// name without a slash is under library/. A reference with neither a tag nor
// a digest means the latest tag.
func ParseReference(s string) (Reference, error) {
	if s == "" {
		return Reference{}, errors.New("empty image reference")
	}
	if hex, ok := strings.CutPrefix(s, "sha256:"); ok && len(hex) == 64 && isHex(hex) {
		return Reference{}, fmt.Errorf("%q is an image ID, not a reference", s)
	}
	name, digest, pinned := strings.Cut(s, "@")
	if pinned && !validDigest(digest) {
		return Reference{}, fmt.Errorf("image reference %q: digest %q is not algorithm:hex", s, digest)
	}
	var tag string
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, tag = name[:i], name[i+1:]
		if !validTag(tag) {
			return Reference{}, fmt.Errorf("image reference %q: tag %q is not valid", s, tag)
		}
	}
	registry, repo := DockerHub, name
	// Docker also takes a first part with an upper-case letter as a host,
	// since a repository name cannot have one.
	if first, rest, ok := strings.Cut(name, "/"); ok &&
		(strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first) {
		registry, repo = strings.ToLower(first), rest
	}
	if registry == "index.docker.io" {
		registry = DockerHub
	}
	if registry == DockerHub && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if !validRepository(repo) {
		return Reference{}, fmt.Errorf("image reference %q: repository %q is not valid", s, repo)
	}
	if tag == "" && digest == "" {
		tag = defaultTag
	}
	return Reference{Registry: registry, Repository: repo, Tag: tag, Digest: digest}, nil
}

// validRepository allows lower-case path parts of letters, digits and the
// separators Docker allows between them.
func validRepository(repo string) bool {
	if repo == "" {
		return false
	}
	for part := range strings.SplitSeq(repo, "/") {
		if part == "" || !isAlnum(part[0]) || !isAlnum(part[len(part)-1]) {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !isAlnum(c) && c != '.' && c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
}

func validTag(tag string) bool {
	if tag == "" || len(tag) > 128 || tag[0] == '.' || tag[0] == '-' {
		return false
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if !isAlnum(c) && !(c >= 'A' && c <= 'Z') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// digestLengths are the encoded lengths of the algorithms the OCI image spec
// registers, which are lower-case hex.
var digestLengths = map[string]int{"sha256": 64, "sha512": 128}

// validDigest follows the OCI image spec: algorithm:encoded, with the
// registered algorithms held to their hex length.
func validDigest(d string) bool {
	algo, enc, ok := strings.Cut(d, ":")
	if !ok || !validAlgorithm(algo) || enc == "" {
		return false
	}
	if n, registered := digestLengths[algo]; registered {
		return len(enc) == n && isHex(enc)
	}
	for i := 0; i < len(enc); i++ {
		c := enc[i]
		if !isAlnum(c) && !(c >= 'A' && c <= 'Z') && c != '=' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// validAlgorithm allows lower-case letters and digits, with + . _ - between them.
func validAlgorithm(a string) bool {
	if a == "" || !isAlnum(a[0]) || !isAlnum(a[len(a)-1]) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if !isAlnum(c) && c != '+' && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

func isAlnum(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
