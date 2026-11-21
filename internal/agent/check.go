package agent

import (
	"context"
	"strings"

	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/update"
)

// composeDependsOn lists the services a compose container depends on, as
// service:condition:restart entries separated by commas.
const composeDependsOn = "com.docker.compose.depends_on"

// Check reads the inventory and asks each image's registry for updates.
func Check(ctx context.Context, d *docker.Client, r *registry.Client) (protocol.CheckReport, error) {
	rep, err := inventory.Collect(ctx, d)
	if err != nil {
		return protocol.CheckReport{Docker: rep.Docker}, err
	}
	targets := make([]registry.Target, len(rep.Containers))
	for i, ct := range rep.Containers {
		targets[i] = registry.Target{Image: ct.Image, RepoDigests: ct.RepoDigests, TagMoved: ct.TagMoved,
			ImageID: ct.ImageID, NamedRepoDigests: ct.NamedRepoDigests,
			Platform: registry.Platform{OS: ct.OS, Architecture: ct.Architecture, Variant: ct.Variant}}
	}
	results := r.CheckAll(ctx, targets)
	out := protocol.CheckReport{Docker: rep.Docker, Containers: make([]protocol.Checked, len(rep.Containers))}
	for i, ct := range rep.Containers {
		out.Containers[i] = protocol.Checked{
			ID:             ct.ID,
			Name:           ct.Name,
			Image:          ct.Image,
			ComposeProject: ct.ComposeProject,
			ComposeService: ct.ComposeService,
			DependsOn:      DependsOn(ct.Labels[composeDependsOn]),
			State:          ct.State,
			Protected:      ct.Protected,
			LabelError:     ct.LabelError,
			Self:           update.IsSelf(ct.Image),
			Check:          results[i],
		}
	}
	return out, nil
}

// DependsOn reads the service names out of a com.docker.compose.depends_on
// label.
func DependsOn(label string) []string {
	var out []string
	for _, entry := range strings.Split(label, ",") {
		if svc, _, _ := strings.Cut(strings.TrimSpace(entry), ":"); svc != "" {
			out = append(out, svc)
		}
	}
	return out
}
