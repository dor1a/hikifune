// Command hikifune-agent runs on each Docker host and talks to its Docker
// Engine for the server.
//
//	hikifune-agent inventory  print every container on the host as JSON (read only)
//	hikifune-agent check      ask each image's registry for updates and print them as JSON (read only)
//	hikifune-agent version    print the version
//
// The daemon is reached at HIKIFUNE_DOCKER_HOST (unix:///var/run/docker.sock
// by default, or tcp://host:port for a socket proxy). Registry credentials
// come from the Docker config.json at HIKIFUNE_DOCKER_CONFIG; without one,
// every registry is asked anonymously.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/registry"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "inventory":
		err = printInventory()
	case "check":
		err = printChecks()
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		err = fmt.Errorf("unknown command %q (use inventory, check or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hikifune-agent:", err)
		os.Exit(1)
	}
}

func printInventory() error {
	c, err := docker.New(os.Getenv("HIKIFUNE_DOCKER_HOST"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := inventory.Collect(ctx, c)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// checked is one container in the check output.
type checked struct {
	Name      string          `json:"name"`
	Image     string          `json:"image"`
	Protected bool            `json:"protected"`
	Check     registry.Result `json:"check"`
}

func printChecks() error {
	c, err := docker.New(os.Getenv("HIKIFUNE_DOCKER_HOST"))
	if err != nil {
		return err
	}
	keys, err := registry.LoadConfig(os.Getenv("HIKIFUNE_DOCKER_CONFIG"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := inventory.Collect(ctx, c)
	if err != nil {
		return err
	}
	targets := make([]registry.Target, len(rep.Containers))
	for i, ct := range rep.Containers {
		targets[i] = registry.Target{Image: ct.Image, RepoDigests: ct.RepoDigests, TagMoved: ct.TagMoved, ImageID: ct.ImageID}
	}
	results := registry.New(keys, nil).CheckAll(ctx, targets)
	out := struct {
		Docker     inventory.Docker `json:"docker"`
		Containers []checked        `json:"containers"`
	}{Docker: rep.Docker, Containers: make([]checked, len(rep.Containers))}
	for i, ct := range rep.Containers {
		out.Containers[i] = checked{Name: ct.Name, Image: ct.Image, Protected: ct.Protected, Check: results[i]}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
