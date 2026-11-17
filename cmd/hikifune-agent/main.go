// Command hikifune-agent runs on each Docker host and talks to its Docker
// Engine for the server. For now it only prints its version.
//
//	hikifune-agent version  print the version
package main

import (
	"fmt"
	"os"

	"github.com/dor1a/hikifune/internal/buildinfo"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		fmt.Fprintf(os.Stderr, "hikifune-agent: unknown command %q (use version)\n", cmd)
		os.Exit(1)
	}
}
