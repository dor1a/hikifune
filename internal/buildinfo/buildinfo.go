// Package buildinfo holds the version stamped into both binaries at build
// time. The server compares it with each agent's version before it updates a
// host, so both sides must read it from here.
package buildinfo

// Version is set with
// -ldflags "-X github.com/dor1a/hikifune/internal/buildinfo.Version=...".
var Version = "dev"
