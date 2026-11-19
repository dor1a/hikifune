package update

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/registry"
)

const (
	// DefaultWatch is how long a new container is watched before it passes (R24).
	DefaultWatch = 2 * time.Minute
	// DefaultPoll is how often a watched container is inspected.
	DefaultPoll = 2 * time.Second

	newSuffix = "-hikifune-new"
	oldSuffix = "-hikifune-old"

	// rollbackTimeout bounds a rollback, which runs even when the update's
	// context was cancelled.
	rollbackTimeout = 3 * time.Minute
	// stepTimeout bounds one call that changes a container. Stopping waits
	// for the container's own stop timeout.
	stepTimeout = 5 * time.Minute
)

// Outcome is how an update ended.
type Outcome string

const (
	// Updated means the new container passed the watch and replaced the old one.
	Updated Outcome = "updated"
	// RolledBack means the new container was removed and the old one runs again.
	RolledBack Outcome = "rolled_back"
	// Failed means the update stopped. The reason says whether the old
	// container was touched and where it is.
	Failed Outcome = "failed"
	// UpToDate means the pull brought the image the container already runs.
	UpToDate Outcome = "up_to_date"
)

// Result is what one update did.
type Result struct {
	Container  string  `json:"container"`
	Image      string  `json:"image"`
	Outcome    Outcome `json:"outcome"`
	Reason     string  `json:"reason,omitempty"`
	OldImageID string  `json:"old_image_id,omitempty"`
	NewImageID string  `json:"new_image_id,omitempty"`
	// Digest is the registry digest the update went for, as its registry
	// check saw it. A failed update skips this digest next time (R15).
	Digest string `json:"digest,omitempty"`
	// OldImageRemoved is set when nothing else used the old image and it was removed.
	OldImageRemoved bool `json:"old_image_removed,omitempty"`
	// Note reports something left behind by an update that otherwise passed.
	Note string `json:"note,omitempty"`

	// labelNote waits for the update to pass: a new container that is rolled
	// back takes its compose label with it.
	labelNote string
}

// Options tune one update.
type Options struct {
	// Watch is how long the new container is watched. Zero means DefaultWatch.
	Watch time.Duration
	// Poll is how often the new container is inspected while it is watched.
	// Zero means DefaultPoll.
	Poll time.Duration
	// Auth returns the X-Registry-Auth value for pulling from a registry, or
	// "" to pull anonymously. Nil pulls anonymously.
	Auth func(registry string) string
	// Check asks the registry about the image before anything is pulled
	// (R25). Only an update_available result goes on, so a local build is
	// never replaced. The agent passes the registry client's Check.
	Check func(ctx context.Context, t registry.Target) registry.Result
}

// engine is the part of the Docker client an update uses.
type engine interface {
	ContainerInspect(ctx context.Context, id string) (docker.Container, error)
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
	ImagePull(ctx context.Context, repository, tag, auth string) error
	ContainerCreate(ctx context.Context, name string, body json.RawMessage) (docker.Created, error)
	NetworkConnect(ctx context.Context, network, container string, endpoint json.RawMessage) error
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string) error
	ContainerRename(ctx context.Context, id, name string) error
	ContainerRemove(ctx context.Context, id string, force bool) error
	ImageRemove(ctx context.Context, id string) error
	ImagePlatformID(ctx context.Context, ref string) (string, bool, error)
}

// Container updates one running container to the image its reference names
// on the registry now, following the seven steps of the plan, and rolls it
// back when the new container fails the watch.
func Container(ctx context.Context, c *docker.Client, name string, opt Options) Result {
	return run(ctx, c, name, opt)
}

func run(ctx context.Context, e engine, name string, opt Options) Result {
	if opt.Watch <= 0 {
		opt.Watch = DefaultWatch
	}
	if opt.Poll <= 0 {
		opt.Poll = DefaultPoll
	}
	old, err := e.ContainerInspect(ctx, name)
	if err != nil {
		return Result{Container: name, Outcome: Failed, Reason: "inspect: " + err.Error()}
	}
	name = strings.TrimPrefix(old.Name, "/")
	r := Result{Container: name, Image: old.Config.Image, OldImageID: old.Image}
	fail := func(format string, a ...any) Result {
		r.Outcome = Failed
		r.Reason = fmt.Sprintf(format, a...)
		return r
	}

	if !old.State.Running {
		return fail("not running (%s); only running containers are updated", old.State.Status)
	}
	if old.HostConfig.AutoRemove {
		return fail("created with --rm: stopping it would remove it and leave nothing to roll back to")
	}
	if IsSelf(old.Config.Image) {
		return fail("hikifune does not recreate its own containers")
	}
	newID, ok := prepare(ctx, e, old, opt, &r)
	if !ok {
		return r
	}

	// 4. to 6. Swap and start. Each call is sent whole even when ctx is
	// cancelled, so no change is left half known; a cancel is taken between
	// steps and rolls back.
	steps := []struct {
		why string
		do  func(context.Context) error
	}{
		{"stop the old container", func(c context.Context) error { return e.ContainerStop(c, old.ID) }},
		{"rename the old container", func(c context.Context) error { return e.ContainerRename(c, old.ID, name+oldSuffix) }},
		{"rename the new container", func(c context.Context) error { return e.ContainerRename(c, newID, name) }},
		{"start refused", func(c context.Context) error { return e.ContainerStart(c, newID) }},
	}
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return rollback(ctx, e, r, name, old.ID, newID, "interrupted before the step to "+st.why+": "+err.Error())
		}
		if err := whole(ctx, st.do); err != nil {
			return rollback(ctx, e, r, name, old.ID, newID, st.why+": "+message(err))
		}
	}
	if why := watch(ctx, e, newID, opt); why != "" {
		return rollback(ctx, e, r, name, old.ID, newID, why)
	}

	// 7. Remove the old container, and its image when no name is left on it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	r.Outcome = Updated
	if r.labelNote != "" {
		r.addNote(r.labelNote)
	}
	if err := e.ContainerRemove(ctx, old.ID, false); err != nil {
		r.addNote("the old container " + name + oldSuffix + " was left: " + err.Error())
		return r
	}
	// Removing an image by ID takes its last name with it, so an image
	// another name still points at stays. The API has no remove-if-unnamed,
	// so a name added between this inspect and the removal is lost.
	switch img, err := e.ImageInspect(ctx, old.Image); {
	case docker.IsNotFound(err):
		return r
	case err != nil:
		r.addNote("the old image was left: " + err.Error())
		return r
	case len(img.RepoTags) > 0:
		r.addNote("the old image was kept: it is still named " + strings.Join(img.RepoTags, ", "))
		return r
	}
	switch err := e.ImageRemove(ctx, old.Image); {
	case err == nil:
		r.OldImageRemoved = true
	case docker.IsConflict(err) || docker.IsNotFound(err):
		// Another container still uses it.
	default:
		r.addNote("the old image was left: " + err.Error())
	}
	return r
}

// addNote adds to the result's note.
func (r *Result) addNote(s string) {
	if r.Note != "" {
		r.Note += "; "
	}
	r.Note += s
}

// composeLabel returns the value of the compose image label to replace and
// its replacement (R22). Compose writes the image ID there on the classic
// image store and the platform image's ID on the containerd store. A label
// that matches neither is left as it is, with a note: compose will recreate
// the container at its next up.
func composeLabel(ctx context.Context, e engine, old docker.Container, newImageID string) (from, to, note string) {
	label := old.Config.Labels[composeImageLabel]
	if label == "" || label == old.Image {
		return old.Image, newImageID, ""
	}
	left := "the compose label " + composeImageLabel + " was left at " + label +
		", so the next docker compose up may recreate the container"
	oldID, ok, err := e.ImagePlatformID(ctx, old.Image)
	if err != nil || !ok || oldID != label {
		return old.Image, newImageID, left
	}
	newID, ok, err := e.ImagePlatformID(ctx, newImageID)
	if err != nil || !ok {
		return old.Image, newImageID, left
	}
	return label, newID, ""
}

// prepare runs steps 1 to 3: it pulls, builds the new configuration, and
// creates the new container under its temporary name, connected and
// compared. The old container is not touched. When ok is false, r holds the
// outcome: the image was up to date, or the update failed.
func prepare(ctx context.Context, e engine, old docker.Container, opt Options, r *Result) (newID string, ok bool) {
	fail := func(format string, a ...any) (string, bool) {
		r.Outcome = Failed
		r.Reason = fmt.Sprintf(format, a...)
		return "", false
	}
	name := strings.TrimPrefix(old.Name, "/")
	ref, err := registry.ParseReference(old.Config.Image)
	if err != nil {
		return fail("%v", err)
	}
	if ref.Digest != "" {
		return fail("%s is pinned by digest", old.Config.Image)
	}
	for _, n := range []string{name + newSuffix, name + oldSuffix} {
		_, err := e.ContainerInspect(ctx, n)
		if err == nil {
			return fail("a container named %s is left from an earlier update; remove or rename it by hand", n)
		}
		if !docker.IsNotFound(err) {
			return fail("inspect %s: %v", n, err)
		}
	}

	// What the reference names now, and what the container runs, for the
	// registry check and for telling the image's defaults apart.
	oldImg, err := e.ImageInspect(ctx, old.Image)
	if docker.IsNotFound(err) {
		return fail("the image the container runs is gone from this host, so what the container set cannot be told from the image's defaults")
	}
	if err != nil {
		return fail("inspect the old image: %v", err)
	}
	t := registry.Target{Image: old.Config.Image, RepoDigests: oldImg.RepoDigests, ImageID: old.Image}
	switch named, err := e.ImageInspect(ctx, old.Config.Image); {
	case docker.IsNotFound(err):
	case err != nil:
		return fail("inspect %s: %v", old.Config.Image, err)
	case named.ID != old.Image:
		t.TagMoved, t.NamedRepoDigests = true, named.RepoDigests
	}
	if opt.Check == nil {
		return fail("no registry check was given")
	}
	c := opt.Check(ctx, t)
	r.Digest = c.RemoteDigest
	switch c.Status {
	case registry.UpdateAvailable:
	case registry.UpToDate:
		r.Outcome = UpToDate
		return "", false
	default:
		return fail("not updated, the registry check says %s: %s", c.Status, c.Reason)
	}

	// 1. Pull.
	auth := ""
	if opt.Auth != nil {
		auth = opt.Auth(ref.Registry)
	}
	if err := e.ImagePull(ctx, ref.Registry+"/"+ref.Repository, ref.Tag, auth); err != nil {
		return fail("pull: %v", err)
	}
	newImg, err := e.ImageInspect(ctx, old.Config.Image)
	if err != nil {
		return fail("inspect the pulled image: %v", err)
	}
	r.NewImageID = newImg.ID
	if newImg.ID == old.Image {
		r.Outcome = UpToDate
		return "", false
	}
	// Another pull or build may have moved the tag since the check. Only
	// the image the check saw on the registry goes on.
	if !pulledFrom(newImg, ref, c.RemoteDigest) {
		return fail("%s now names %s, not the %s the registry check saw; another pull or build may have moved it", old.Config.Image, newImg.ID, c.RemoteDigest)
	}

	// 2. Build the new configuration.
	from, to, note := composeLabel(ctx, e, old, newImg.ID)
	r.labelNote = note
	s, err := build(old.Raw, oldImg.Raw, from, to)
	if err != nil {
		return fail("build the new configuration: %v", err)
	}

	// 3. Create, connect and compare.
	newID, err = create(ctx, e, name+newSuffix, s)
	if err != nil {
		return fail("%v", err)
	}
	created, err := e.ContainerInspect(ctx, newID)
	if err == nil {
		err = compare(s, old.Raw, created.Raw, newImg.ID)
	}
	if err != nil {
		return fail("config check: %v%s", err, discard(ctx, e, newID, name+newSuffix))
	}
	return newID, true
}

// create creates the new container with its primary network and connects
// the others while it is still stopped (R12). On failure nothing is left.
func create(ctx context.Context, e engine, name string, s spec) (string, error) {
	body, err := s.body()
	if err != nil {
		return "", fmt.Errorf("encode the new configuration: %w", err)
	}
	var created docker.Created
	err = whole(ctx, func(c context.Context) (err error) {
		created, err = e.ContainerCreate(c, name, body)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("create refused: %s", message(err))
	}
	for _, n := range s.connectLast {
		ep, err := s.endpoint(n)
		if err == nil && ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", ctx.Err())
		}
		if err == nil {
			err = whole(ctx, func(c context.Context) error { return e.NetworkConnect(c, n, created.ID, ep) })
		}
		if err != nil {
			return "", fmt.Errorf("connect network %s: %s%s", n, message(err), discard(ctx, e, created.ID, name))
		}
	}
	return created.ID, nil
}

// whole runs one call that changes a container to its end even when ctx is
// cancelled, so its outcome is always known.
func whole(ctx context.Context, do func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stepTimeout)
	defer cancel()
	return do(ctx)
}

// pulledFrom reports whether img is the image with the manifest digest the
// registry check saw: one of its repo digests for ref's repository, or its
// ID with the containerd image store.
func pulledFrom(img docker.Image, ref registry.Reference, digest string) bool {
	if digest == "" {
		return false
	}
	if img.ID == digest {
		return true
	}
	for _, rd := range img.RepoDigests {
		r, err := registry.ParseReference(rd)
		if err == nil && r.Name() == ref.Name() && r.Digest == digest {
			return true
		}
	}
	return false
}

// discard removes a new container that never ran. It returns what to add to
// the reason when the container could not be removed: it is left under its
// temporary name and stops the next update until it is removed.
func discard(ctx context.Context, e engine, id, name string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := e.ContainerRemove(ctx, id, true); err != nil && !docker.IsNotFound(err) {
		return "; the new container " + name + " was left: " + err.Error()
	}
	return ""
}

// rollback removes the new container, gives the old one its name back, and
// starts it (R11). It runs even when ctx was cancelled. The old container's
// name is read again by its ID, since a call that failed or was cancelled
// may still have been carried out. Every step is tried, so the old
// container is started even when its name cannot be restored.
func rollback(ctx context.Context, e engine, r Result, name, oldID, newID string, why string) Result {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	var failed []string
	if err := e.ContainerRemove(ctx, newID, true); err != nil && !docker.IsNotFound(err) {
		failed = append(failed, "remove the new container: "+err.Error())
	}
	current := ""
	if old, err := e.ContainerInspect(ctx, oldID); err != nil {
		failed = append(failed, "inspect the old container: "+err.Error())
	} else {
		current = strings.TrimPrefix(old.Name, "/")
	}
	if current != "" && current != name {
		if err := e.ContainerRename(ctx, oldID, name); err != nil {
			failed = append(failed, "give the old container its name back: "+err.Error())
		} else {
			current = name
		}
	}
	if err := e.ContainerStart(ctx, oldID); err != nil {
		failed = append(failed, "start the old container: "+err.Error())
	}
	// Rolled back means the old container runs under its own name now.
	if len(failed) == 0 {
		switch old, err := e.ContainerInspect(ctx, oldID); {
		case err != nil:
			failed = append(failed, "inspect the old container after starting it: "+err.Error())
		case strings.TrimPrefix(old.Name, "/") != name || !old.State.Running:
			current = strings.TrimPrefix(old.Name, "/")
			failed = append(failed, fmt.Sprintf("see the old container running as %s (it is %s, running %v)", name, current, old.State.Running))
		}
	}
	r.Reason = why
	if len(failed) == 0 {
		r.Outcome = RolledBack
		return r
	}
	r.Outcome = Failed
	r.Reason += "; rollback failed to " + strings.Join(failed, "; ")
	if current != "" && current != name {
		r.Reason += "; the old container is " + current
	}
	return r
}

// watch inspects the started container until opt.Watch has passed and
// returns why it failed, or "" when it passed (R24).
func watch(ctx context.Context, e engine, id string, opt Options) string {
	start := time.Now()
	ct, err := e.ContainerInspect(ctx, id)
	if err != nil {
		return "inspect after start: " + err.Error()
	}
	// A new container has never restarted, so any restart counts, even one
	// before this first look.
	startedAt := ct.State.StartedAt
	for {
		if why := failing(ct, startedAt, time.Since(start)); why != "" {
			return why
		}
		left := opt.Watch - time.Since(start)
		if left <= 0 {
			break
		}
		if err := sleep(ctx, min(opt.Poll, left)); err != nil {
			return "watch interrupted: " + err.Error()
		}
		if ct, err = e.ContainerInspect(ctx, id); err != nil {
			return "inspect while watching: " + err.Error()
		}
	}
	if h := ct.State.Health; h != nil && h.Status != "healthy" {
		return fmt.Sprintf("not healthy %s after start: %s", human(opt.Watch), healthDetail(h))
	}
	return ""
}

// failing returns why a watched container has failed, or "".
func failing(ct docker.Container, startedAt string, after time.Duration) string {
	s := ct.State
	switch {
	case s.OOMKilled:
		return fmt.Sprintf("killed for running out of memory %s after start", human(after))
	case ct.RestartCount > 0 || s.StartedAt != startedAt || s.Restarting:
		// Docker resets the exit code when the container runs again.
		if s.Running {
			return fmt.Sprintf("restarted within %s of start", human(after))
		}
		return fmt.Sprintf("restarted within %s of start, last exit code %d", human(after), s.ExitCode)
	case !s.Running:
		why := fmt.Sprintf("exited with code %d %s after start", s.ExitCode, human(after))
		if s.Error != "" {
			why += ": " + s.Error
		}
		return why
	case s.Health != nil && s.Health.Status == "unhealthy":
		return fmt.Sprintf("unhealthy %s after start: %s", human(after), healthDetail(s.Health))
	}
	return ""
}

// healthDetail describes the last health check run.
func healthDetail(h *docker.Health) string {
	if len(h.Log) == 0 {
		return "health is " + h.Status + ", no check has finished"
	}
	last := h.Log[len(h.Log)-1]
	d := fmt.Sprintf("health check exited with %d", last.ExitCode)
	if out := firstLine(last.Output); out != "" {
		d += ": " + out
	}
	return d
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 120
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// human writes a duration the way a notification reads it: 2m, 45s, 1m30s.
func human(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	d = d.Round(time.Second)
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// message is the daemon's own message for an error it answered with.
func message(err error) string {
	if e, ok := err.(*docker.Error); ok && e.Message != "" {
		return e.Message
	}
	return err.Error()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// selfImages are the image repositories of hikifune itself (R28).
var selfImages = map[string]bool{"hikifune": true, "hikifune-agent": true}

// IsSelf reports whether an image reference names hikifune's own server or
// agent image, wherever it is hosted. Such a container is never recreated:
// an agent that stops itself mid-update cannot roll back.
func IsSelf(image string) bool {
	ref, err := registry.ParseReference(image)
	if err != nil {
		return false
	}
	return selfImages[path.Base(ref.Repository)]
}
