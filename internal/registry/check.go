package registry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
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
	// Platform is the platform of the image the container runs.
	Platform Platform
}

// Platform is an image's os, architecture and variant, as image inspect and
// an index's entries name them.
type Platform struct {
	OS, Architecture, Variant string
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// matches tells whether an index entry is for p. A variant left out on
// either side, as arm64 often is, matches any.
func (p Platform) matches(o Platform) bool {
	return p.OS == o.OS && p.Architecture == o.Architecture && (p.Variant == "" || o.Variant == "" || p.Variant == o.Variant)
}

// localBuild is the reason given for an image the registry has never had.
const localBuild = "so it was built on this host or deleted from the registry, and an update would replace it with the registry's image"

// sameImage is a question left for after a check's HEADs: whether the image
// the registry's digest remote holds for the container's platform is the
// one the container runs.
type sameImage struct {
	ref    Reference
	remote string
}

// Check compares the digest the registry has for an image reference with
// the host's repo digests for the same repository.
func (c *Client) Check(ctx context.Context, t Target) Result {
	r, same := c.heads(ctx, t)
	return c.orSameImage(ctx, same, t, r)
}

// heads is the part of a check that only sends HEADs. Its result stands
// unless the answer to same, when there is one, says the container runs the
// registry's image.
func (c *Client) heads(ctx context.Context, t Target) (Result, *sameImage) {
	ref, err := ParseReference(t.Image)
	if err != nil {
		return Result{Status: NotCheckable, Reason: err.Error()}, nil
	}
	if ref.Digest != "" {
		return Result{Status: Pinned, LocalDigest: ref.Digest}, nil
	}
	if t.TagMoved {
		// Pulling moves the tag back to the registry's image. Make sure what
		// the tag names now came from there. An update of a container that
		// already runs the registry's image replaces nothing.
		named := localDigests(ref, t.NamedRepoDigests)
		if len(named) == 0 {
			return c.tagSame(ctx, ref, t, Result{Status: NotCheckable,
				Reason: t.Image + " now names an image with no repo digest for " + ref.Name() + ", " + localBuild})
		}
		if r, ok := c.fromRegistry(ctx, ref, named, t.Image+" now names an image the registry does not have"); !ok {
			return c.tagSame(ctx, ref, t, r)
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
			return Result{Status: NotCheckable, Reason: moved + ", and " + err.Error()}, nil
		case err != nil:
			return Result{Status: Failed, Reason: moved + ", and " + err.Error()}, nil
		case remote == t.ImageID:
			// The registry points the tag back at what the container runs.
			return Result{Status: UpToDate, LocalDigest: remote, RemoteDigest: remote}, nil
		}
		// With the containerd store the image ID of a pulled image is the
		// digest that was pulled, so the registry still has it. A local
		// build, or any image on the classic store, has an ID the registry
		// does not know.
		if r, ok := c.fromRegistry(ctx, ref, []string{t.ImageID}, moved+", and the registry does not have the image it runs"); !ok {
			r.RemoteDigest = remote
			return r, askSame(ref, remote, t)
		}
		return Result{Status: UpdateAvailable, RemoteDigest: remote, Reason: moved}, nil
	}
	if len(local) == 0 {
		lost := "the image has no repo digest for " + ref.Name()
		if !validDigest(t.ImageID) {
			return Result{Status: NotCheckable, Reason: lost + ", so it was built on this host or never pulled from there"}, nil
		}
		// With the containerd store the image ID of a pulled image is the
		// digest that was pulled, so an image that lost its repo digests is
		// still known to the registry by its ID (R25). A local build, or any
		// image on the classic store, has an ID the registry does not know.
		remote, err := c.Digest(ctx, ref)
		switch {
		case IsNotCheckable(err):
			return Result{Status: NotCheckable, Reason: lost + ", and " + err.Error()}, nil
		case err != nil:
			return Result{Status: Failed, Reason: lost + ", and " + err.Error()}, nil
		case remote == t.ImageID:
			return Result{Status: UpToDate, LocalDigest: remote, RemoteDigest: remote}, nil
		}
		if r, ok := c.fromRegistry(ctx, ref, []string{t.ImageID}, lost+" and the registry does not have its image ID"); !ok {
			r.RemoteDigest = remote
			return r, askSame(ref, remote, t)
		}
		return Result{Status: UpdateAvailable, LocalDigest: t.ImageID, RemoteDigest: remote}, nil
	}
	remote, err := c.Digest(ctx, ref)
	switch {
	case IsNotCheckable(err):
		return Result{Status: NotCheckable, LocalDigest: local[0], Reason: err.Error()}, nil
	case err != nil:
		return Result{Status: Failed, LocalDigest: local[0], Reason: err.Error()}, nil
	case slices.Contains(local, remote) || remote == t.ImageID:
		return Result{Status: UpToDate, LocalDigest: remote, RemoteDigest: remote}, nil
	}
	// With the containerd store a never pushed local build has a repo digest
	// for its own name, so a name the registry also has would look updated.
	if r, ok := c.fromRegistry(ctx, ref, local, "the registry does not have the digest this host runs"); !ok {
		r.LocalDigest, r.RemoteDigest = local[0], remote
		return r, askSame(ref, remote, t)
	}
	return Result{Status: UpdateAvailable, LocalDigest: local[0], RemoteDigest: remote}, askSame(ref, remote, t)
}

// askSame returns the question whether the container runs the image remote
// holds for its platform, or nil when the answer is known to be no. A tag
// can move to a new index or manifest with that image unchanged, as when
// the publisher adds a platform or an attestation. On the classic image
// store the image ID is the image's config digest, which is what is
// compared. With the containerd store it is a manifest digest, never a
// config digest. An empty remote asks only whether t can be compared.
func askSame(ref Reference, remote string, t Target) *sameImage {
	p := t.Platform
	if p.OS == "" || p.OS == "unknown" || p.Architecture == "" || p.Architecture == "unknown" || !validDigest(t.ImageID) {
		return nil
	}
	local := localDigests(ref, t.RepoDigests)
	if slices.Contains(local, t.ImageID) {
		return nil
	}
	// The host already has what the tag points at: there is no other
	// manifest to look into.
	if remote == t.ImageID || slices.Contains(local, remote) {
		return nil
	}
	return &sameImage{ref: ref, remote: remote}
}

// tagSame is askSame for a result reached before the registry was asked for
// the tag.
func (c *Client) tagSame(ctx context.Context, ref Reference, t Target, r Result) (Result, *sameImage) {
	if askSame(ref, "", t) == nil {
		return r, nil
	}
	remote, err := c.Digest(ctx, ref)
	if err != nil {
		return r, nil
	}
	return r, askSame(ref, remote, t)
}

// platformTimeout bounds the manifest reads of orSameImage, so a slow
// registry cannot spend the time the checks after it need.
const platformTimeout = 15 * time.Second

// sameImageBudget bounds the manifest reads of one CheckAll, however many
// of them hang, so the results are back well before the caller's deadline.
var sameImageBudget = time.Minute

// orSameImage returns r, unless same is asked and the answer is yes. It
// reads manifests with GETs. They come after every HEAD, and when they
// cannot tell, r stands.
func (c *Client) orSameImage(ctx context.Context, same *sameImage, t Target, r Result) Result {
	if same == nil {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, platformTimeout)
	defer cancel()
	config, index, err := c.PlatformConfig(ctx, same.ref, same.remote, t.Platform)
	if err != nil || config != t.ImageID {
		return r
	}
	reason := "the tag points at another manifest, but it holds the image the container runs"
	if index {
		reason = "the tag points at a new index, but its image for " + t.Platform.String() + " is the one the container runs"
	}
	local := r.LocalDigest
	if local == "" {
		local = t.ImageID
	}
	return Result{Status: UpToDate, LocalDigest: local, RemoteDigest: same.remote, Reason: reason}
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

// CheckAll checks every target, each distinct image, repo digest set and
// platform once, a few at a time. The HEADs of every check come before the
// manifest reads of any, so reads that hang take no time from the HEADs.
// Results are in the order of targets.
func (c *Client) CheckAll(ctx context.Context, targets []Target) []Result {
	type job struct {
		target Target
		result Result
		same   *sameImage
	}
	jobs := map[string]*job{}
	order := make([]*job, len(targets))
	for i, t := range targets {
		digests := slices.Clone(t.RepoDigests)
		slices.Sort(digests)
		named := slices.Clone(t.NamedRepoDigests)
		slices.Sort(named)
		key := fmt.Sprint(t.TagMoved) + "\x00" + t.ImageID + "\x00" + t.Image + "\x00" + strings.Join(digests, "\x00") +
			"\x01" + strings.Join(named, "\x00") +
			"\x01" + t.Platform.OS + "\x00" + t.Platform.Architecture + "\x00" + t.Platform.Variant
		j, ok := jobs[key]
		if !ok {
			j = &job{target: t}
			jobs[key] = j
		}
		order[i] = j
	}
	each := func(do func(j *job)) {
		sem := make(chan struct{}, checkWorkers)
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				do(j)
			}()
		}
		wg.Wait()
	}
	each(func(j *job) { j.result, j.same = c.heads(ctx, j.target) })
	reads, cancel := context.WithTimeout(ctx, sameImageBudget)
	defer cancel()
	each(func(j *job) { j.result = c.orSameImage(reads, j.same, j.target, j.result) })
	results := make([]Result, len(targets))
	for i, j := range order {
		results[i] = j.result
	}
	return results
}
