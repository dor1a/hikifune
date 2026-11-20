// Package protocol holds the messages the server and its agents exchange
// over HTTP. The server dials each agent (R26) with the agent's token in an
// Authorization: Bearer header. Both sides are built from the same commit,
// and the server refuses an agent that reports another version (R16).
package protocol

import (
	"time"

	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/update"
)

// Paths the agent serves. Every path but PathHealth needs the token.
const (
	PathHealth    = "/healthz"
	PathVersion   = "/api/v1/version"
	PathInventory = "/api/v1/inventory"
	PathCheck     = "/api/v1/check"
	// PathUpdates takes a POST that starts a job; PathUpdates + "/{id}"
	// answers a GET with the job.
	PathUpdates = "/api/v1/updates"
	PathPrune   = "/api/v1/prune"
)

// Version is what an agent reports about itself and its Docker daemon.
type Version struct {
	Agent string `json:"agent"`
	// Docker is set when the daemon answered and serves the API hikifune
	// needs; DockerError says why not otherwise.
	Docker      *inventory.Docker `json:"docker,omitempty"`
	DockerError string            `json:"docker_error,omitempty"`
	// Boot is new each time the agent starts. Jobs and their numbers live in
	// memory, so a server that sees another Boot does not send a lost
	// request again: the agent that may have taken it is gone.
	Boot string `json:"boot,omitempty"`
}

// Checked is one container in a check report.
type Checked struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Image          string `json:"image"`
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	// DependsOn are the compose services this one depends on, from the
	// com.docker.compose.depends_on label (R29).
	DependsOn  []string `json:"depends_on,omitempty"`
	State      string   `json:"state"`
	Protected  bool     `json:"protected"`
	LabelError string   `json:"label_error,omitempty"`
	// Self marks hikifune's own server and agent containers, which are
	// never recreated (R28).
	Self  bool            `json:"self,omitempty"`
	Check registry.Result `json:"check"`
}

// CheckReport is an agent's answer to a check.
type CheckReport struct {
	Docker     inventory.Docker `json:"docker"`
	Containers []Checked        `json:"containers"`
}

// UpdateRequest starts an update of one container.
type UpdateRequest struct {
	Container string `json:"container"`
	// ID is the container ID the check saw. A container recreated since
	// then under the same name is not updated.
	ID string `json:"id,omitempty"`
	// WatchSeconds is how long the new container is watched; zero means the
	// agent's default.
	WatchSeconds int `json:"watch_seconds,omitempty"`
	// Job is the job number the server chose, 16 to 64 lowercase hex
	// digits; empty lets the agent choose. With it, a server that lost the
	// answer asks for the job by that number, and a request sent again for
	// a number the agent has gets that job instead of a new one (R32).
	Job string `json:"job,omitempty"`
}

// ValidJob tells whether id may be a job number the server chose.
func ValidJob(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// JobState is where an update job is.
type JobState string

const (
	JobRunning JobState = "running"
	JobDone    JobState = "done"
)

// Job is one update an agent runs (R32). An agent runs one job at a time.
type Job struct {
	ID        string         `json:"id"`
	Container string         `json:"container"`
	State     JobState       `json:"state"`
	Started   time.Time      `json:"started"`
	Finished  *time.Time     `json:"finished,omitempty"`
	Result    *update.Result `json:"result,omitempty"`
}

// Pruned is an agent's answer to a prune.
type Pruned struct {
	Reclaimed int64 `json:"reclaimed"`
}

// Error is the body of an answer that is not a success. Busy carries the job
// that keeps the agent from taking another.
type Error struct {
	Error string `json:"error"`
	Busy  *Job   `json:"busy,omitempty"`
}
