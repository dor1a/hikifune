// Package inventory lists the containers on one Docker host with what
// hikifune needs to know about each of them.
package inventory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/labels"
)

const (
	composeProject = "com.docker.compose.project"
	composeService = "com.docker.compose.service"

	// inspectWorkers is how many containers are inspected at once.
	inspectWorkers = 4
)

// Report is the inventory of one host.
type Report struct {
	Docker     Docker      `json:"docker"`
	Containers []Container `json:"containers"`
}

// Docker describes the daemon the inventory came from.
type Docker struct {
	Version       string `json:"version"`
	APIVersion    string `json:"api_version"`
	MinAPIVersion string `json:"min_api_version"`
	RequestedAPI  string `json:"requested_api"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

// Container is one container on the host.
type Container struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	ComposeProject string   `json:"compose_project,omitempty"`
	ComposeService string   `json:"compose_service,omitempty"`
	Image          string   `json:"image"`
	ImageID        string   `json:"image_id"`
	RepoDigests    []string `json:"repo_digests"`
	// OS, Architecture and Variant are the platform of the image the
	// container runs, as image inspect gives them.
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
	ImageError   string `json:"image_error,omitempty"`
	// TagMoved means Image now names another image on this host than the one
	// the container runs: a later pull or build moved the tag, and the
	// container was not recreated. With the containerd image store the old
	// image is left with no repo digest at all.
	TagMoved bool `json:"tag_moved,omitempty"`
	// NamedRepoDigests are the repo digests of the image the reference names
	// now, when the tag moved. Pulling would replace that image.
	NamedRepoDigests []string          `json:"named_repo_digests,omitempty"`
	Labels           map[string]string `json:"labels"`
	Protected        bool              `json:"protected"`
	LabelError       string            `json:"label_error,omitempty"`
	State            string            `json:"state"`
	Health           string            `json:"health,omitempty"`
}

// Collect reads every container on the daemon, stopped ones too. It only
// reads. A container removed while the inventory runs is left out.
func Collect(ctx context.Context, c *docker.Client) (Report, error) {
	v, err := c.CheckAPI(ctx)
	rep := Report{Docker: Docker{
		Version:       v.Version,
		APIVersion:    v.APIVersion,
		MinAPIVersion: v.MinAPIVersion,
		RequestedAPI:  c.APIVersion(),
		OS:            v.Os,
		Arch:          v.Arch,
	}}
	if err != nil {
		return rep, err
	}
	list, err := c.ContainerList(ctx, true)
	if err != nil {
		return rep, err
	}

	found := make([]bool, len(list))
	items := make([]Container, len(list))
	errs := make([]error, len(list))
	images := &imageCache{client: c, entries: map[string]*imageEntry{}}
	sem := make(chan struct{}, inspectWorkers)
	var wg sync.WaitGroup
	for i, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			items[i], found[i], errs[i] = describe(ctx, c, images, s.ID)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return rep, err
	}

	rep.Containers = make([]Container, 0, len(list))
	for i, ok := range found {
		if ok {
			rep.Containers = append(rep.Containers, items[i])
		}
	}
	sort.Slice(rep.Containers, func(i, j int) bool {
		return rep.Containers[i].Name < rep.Containers[j].Name
	})
	return rep, nil
}

// describe inspects one container and its image. found is false when the
// container no longer exists.
func describe(ctx context.Context, c *docker.Client, images *imageCache, id string) (item Container, found bool, err error) {
	ct, err := c.ContainerInspect(ctx, id)
	if docker.IsNotFound(err) {
		return Container{}, false, nil
	}
	if err != nil {
		return Container{}, false, err
	}
	lbl := ct.Config.Labels
	if lbl == nil {
		lbl = map[string]string{}
	}
	policy, labelErr := labels.Parse(lbl)
	item = Container{
		ID:             ct.ID,
		Name:           strings.TrimPrefix(ct.Name, "/"),
		ComposeProject: lbl[composeProject],
		ComposeService: lbl[composeService],
		Image:          ct.Config.Image,
		ImageID:        ct.Image,
		RepoDigests:    []string{},
		Labels:         lbl,
		Protected:      policy.Protected,
		State:          ct.State.Status,
	}
	if labelErr != nil {
		item.LabelError = labelErr.Error()
	}
	if ct.State.Health != nil {
		item.Health = ct.State.Health.Status
	}

	img, err := images.get(ctx, ct.Image)
	switch {
	case docker.IsNotFound(err):
		item.ImageError = "image " + ct.Image + " not found"
	case err != nil:
		return Container{}, false, err
	default:
		if img.RepoDigests != nil {
			item.RepoDigests = img.RepoDigests
		}
		item.OS, item.Architecture, item.Variant = img.Os, img.Architecture, img.Variant
	}
	// Asked even when the container's image is gone: with the containerd
	// store, rebuilding a local image under the same name drops the old
	// image's record while the container still runs it.
	if item.Image != "" && item.Image != ct.Image {
		named, err := images.get(ctx, item.Image)
		switch {
		case docker.IsNotFound(err):
			// The reference names no image here any more.
		case err != nil:
			return Container{}, false, err
		default:
			item.TagMoved = named.ID != ct.Image
			if item.TagMoved {
				item.NamedRepoDigests = append([]string{}, named.RepoDigests...)
			}
		}
	}
	return item, true, nil
}

// imageCache inspects each image once, however many containers use it. It is
// keyed by what was asked for, an image ID or a reference.
type imageCache struct {
	client  *docker.Client
	mu      sync.Mutex
	entries map[string]*imageEntry
}

type imageEntry struct {
	once sync.Once
	img  docker.Image
	err  error
}

func (ic *imageCache) get(ctx context.Context, id string) (docker.Image, error) {
	ic.mu.Lock()
	e, ok := ic.entries[id]
	if !ok {
		e = &imageEntry{}
		ic.entries[id] = e
	}
	ic.mu.Unlock()
	e.once.Do(func() {
		e.img, e.err = ic.client.ImageInspect(ctx, id)
	})
	return e.img, e.err
}
