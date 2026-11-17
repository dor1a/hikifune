// Command hikifune-agent runs on each Docker host and talks to its Docker
// Engine for the server.
//
//	hikifune-agent inventory  print every container on the host as JSON (read only)
//	hikifune-agent version    print the version
//
// The daemon is reached at HIKIFUNE_DOCKER_HOST (unix:///var/run/docker.sock
// by default, or tcp://host:port for a socket proxy).
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
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		err = fmt.Errorf("unknown command %q (use inventory or version)", cmd)
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
