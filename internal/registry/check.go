package registry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Status is the outcome of checking one image.
type Status string

const (
	// UpToDate means the registry points the tag at the digest the host has.
	UpToDate Status = "up_to_date"
	// UpdateAvailable means the registry points the tag at another digest.
	UpdateAvailable Status = "update_available"
	// Pinned means the reference names a digest, so there is nothing to check.
	Pinned Status = "pinned"
	// NotCheckable means the registry cannot tell: the image was built on
	// the host, or the registry does not show the repository to this host.
	NotCheckable Status = "not_checkable"
	// Failed means the check itself went wrong, such as a timeout or a 5xx.
	Failed Status = "failed"
)

// checkWorkers is how many registry checks run at once.
const checkWorkers = 4

// Result is what a check found for one image.
type Result struct {
	Status       Status `json:"status"`
	LocalDigest  string `json:"local_digest,omitempty"`
	RemoteDigest string `json:"remote_digest,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Target is an image to check: the reference a container was created from
// and the repo digests of the image it runs.
type Target struct {
	Image       string
	RepoDigests []string
	// TagMoved means Image now names another local image than the one the
	// container runs (see inventory.Container).
	TagMoved bool
	// ImageID is the ID of the image the container runs. With the containerd
	// image store it is the digest of what was pulled, so it can match the
	// registry even when the image has lost its repo digests. On the classic
	// store it is the config digest, which never equals a manifest digest.
	ImageID string
	// NamedRepoDigests are the repo digests of the image Image names now,
	// when TagMoved is set. Pulling would replace that image.
	NamedRepoDigests []string
}

// localBuild is the reason given for an image the registry has never had.
const localBuild = "so it was built on this host or deleted from the registry, and an update would replace it with the registry's image"

// Check compares the digest the registry has for an image reference with
// the host's repo digests for the same repository.
func (c *Client) Check(ctx context.Context, t Target) Result {
	ref, err := ParseReference(t.Image)
	if err != nil {
		return Result{Status: NotCheckable, Reason: err.Error()}
	}
	if ref.Digest != "" {
		return Result{Status: Pinned, LocalDigest: ref.Digest}
	}
	if t.TagMoved {
		// Pulling moves the tag back to the registry's image. Make sure what
		// the tag names now came from there.
		named := localDigests(ref, t.NamedRepoDigests)
		if len(named) == 0 {
			return Result{Status: NotCheckable,
				Reason: t.Image + " now names an image with no repo digest for " + ref.Name() + ", " + localBuild}
		}
		if r, ok := c.fromRegistry(ctx, ref, named, t.Image+" now names an image the registry does not have"); !ok {
			return r
		}
	}
	local := localDigests(ref, t.RepoDigests)
	if len(local) == 0 && t.TagMoved {
		// The image the container runs lost its tag to a later pull or build.
		// If the registry has the reference, recreating moves the container
		// to what it has now.
		moved := t.Image + " now names another image on this host; the container still runs the one it was created from"
		remote, err := c.Digest(ctx, ref)
		switch {
		case IsNotCheckable(err):
			return Result{Status: NotCheckable, Reason: moved + ", and " + err.Error()}
		case err != nil:
			return Result{Status: Failed, Reason: moved + ", and " + err.Error()}
		case remote == t.ImageID:
			// The registry points the tag back at what the container runs.
			return Result{Status: UpToDate, LocalDigest: remote, RemoteDigest: remote}
		}
		// With the containerd store the image ID of a pulled image is the
		// digest that was pulled, so the registry still has it. A local
		// build, or any image on the classic store, has an ID the registry
		// does not know.
		if r, ok := c.fromRegistry(ctx, ref, []string{t.ImageID}, moved+", and the registry does not have the image it runs"); !ok {
			r.RemoteDigest = remote
			return r
		}
		return Result{Status: UpdateAvailable, RemoteDigest: remote, Reason: moved}
	}
	if len(local) == 0 {
		return Result{Status: NotCheckable,
			Reason: "the image has no repo digest for " + ref.Name() + ", so it was built on this host or never pulled from there"}
	}
	remote, err := c.Digest(ctx, ref)
	switch {
	case IsNotCheckable(err):
		return Result{Status: NotCheckable, LocalDigest: local[0], Reason: err.Error()}
	case err != nil:
		return Result{Status: Failed, LocalDigest: local[0], Reason: err.Error()}
	case slices.Contains(local, remote) || remote == t.ImageID:
		return Result{Status: UpToDate, LocalDigest: remote, RemoteDigest: remote}
	}
	// With the containerd store a never pushed local build has a repo digest
	// for its own name, so a name the registry also has would look updated.
	if r, ok := c.fromRegistry(ctx, ref, local, "the registry does not have the digest this host runs"); !ok {
		r.LocalDigest, r.RemoteDigest = local[0], remote
		return r
	}
	return Result{Status: UpdateAvailable, LocalDigest: local[0], RemoteDigest: remote}
}

// fromRegistry asks whether the registry has any of digests. When it has
// none, or cannot tell, the result says why and ok is false.
func (c *Client) fromRegistry(ctx context.Context, ref Reference, digests []string, missing string) (Result, bool) {
	for _, d := range digests {
		if !validDigest(d) {
			continue
		}
		has, err := c.Has(ctx, ref, d)
		switch {
		case IsNotCheckable(err):
			return Result{Status: NotCheckable, Reason: err.Error()}, false
		case err != nil:
			return Result{Status: Failed, Reason: err.Error()}, false
		case has:
			return Result{}, true
		}
	}
	return Result{Status: NotCheckable, Reason: missing + ", " + localBuild}, false
}

// localDigests returns the digests of the repo digests that name the same
// registry and repository as ref.
func localDigests(ref Reference, repoDigests []string) []string {
	var out []string
	for _, rd := range repoDigests {
		r, err := ParseReference(rd)
		if err == nil && r.Digest != "" && r.Name() == ref.Name() && !slices.Contains(out, r.Digest) {
			out = append(out, r.Digest)
		}
	}
	return out
}

// CheckAll checks every target, each distinct image and repo digest set
// once, a few at a time. Results are in the order of targets.
func (c *Client) CheckAll(ctx context.Context, targets []Target) []Result {
	type job struct {
		target Target
		result Result
	}
	jobs := map[string]*job{}
	order := make([]*job, len(targets))
	for i, t := range targets {
		digests := slices.Clone(t.RepoDigests)
		slices.Sort(digests)
		named := slices.Clone(t.NamedRepoDigests)
		slices.Sort(named)
		key := fmt.Sprint(t.TagMoved) + "\x00" + t.ImageID + "\x00" + t.Image + "\x00" + strings.Join(digests, "\x00") +
			"\x01" + strings.Join(named, "\x00")
		j, ok := jobs[key]
		if !ok {
			j = &job{target: t}
			jobs[key] = j
		}
		order[i] = j
	}
	sem := make(chan struct{}, checkWorkers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			j.result = c.Check(ctx, j.target)
		}()
	}
	wg.Wait()
	results := make([]Result, len(targets))
	for i, j := range order {
		results[i] = j.result
	}
	return results
}
