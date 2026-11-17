# Plan

Draft. Nothing is built yet. Each item below is marked as decided, proposed or open. Proposed items are the starting point for development and can still change.

## Why

hikifune replaces tugtainer on a few Docker hosts. The agent design worked well and stays. Three things pushed the change:

- tugtainer 2.0.0 ships amd64 and arm64 images only and needs Docker Engine 25 (API 1.44). A NAS whose vendor ships Docker 24 could no longer connect.
- A 2.x server and a 1.x agent speak different protocols, so a host whose agent was not upgraded at the same time failed before anything ran.
- A new container starts with checking and updating turned off, and each one has to be turned on in the web UI.

Sizing at the time: 85 containers on four hosts, 78 checked and 61 updated automatically, 23 updates in 30 days, one https webhook. Hooks, update delays and health watching were never used.

### Lessons from tugtainer

Over 30 scheduled update runs, tugtainer recreated containers 92 times. 72 went fine, 15 were rolled back and still unhealthy afterwards, and 5 failed to start and then failed to roll back. Nearly every failure came from how the container was recreated, not from the new image.

| Cause | Times |
| -- | -- |
| 1.x turned an exec-form health check (`["CMD", "/app", "healthcheck"]`) into a `--health-cmd` string, which runs through `/bin/sh`. Images without a shell failed every check | 14 |
| 1.x turned a GPU device request into a `--gpus` string, which failed with `capabilities=gpu: unknown device`. The rollback did the same conversion and failed too | 5 |
| The same plan stopped a service that the updated one depended on | 1 |
| Only the first network was attached before start, and the others while the app was already starting | 2 |
| The 2.x server and a 1.x agent refused each other | 1 |

All 57 Discord messages were delivered, but the reasons for failures only appeared in the logs.

## Requirements

### Decided

| ID | Requirement |
| -- | -- |
| R1 | One agent per Docker host and one server that holds the schedule, the web UI, the notifications and the state |
| R2 | The agent reaches the Docker Engine API through a socket proxy or the socket itself. It needs `VERSION`, `PING`, `INFO`, `CONTAINERS`, `IMAGES`, `NETWORKS` and `POST` |
| R3 | Works with Engine API 1.43 (Docker 24) and newer. The agent asks for API 1.43 |
| R4 | A container with no `hikifune.*` label is checked and updated automatically. `hikifune.protected=true` turns automatic updates off |
| R5 | The web UI lists every container on every host, protected ones too |
| R6 | Checks run daily at 00:00 and updates at 01:00 by default, in the configured time zone |
| R7 | A check compares the registry digest of the container's image reference with the local image |
| R8 | Private registries are reached with a Docker `config.json` mounted into the agent |
| R9 | A container is recreated from the configuration the Engine API reports for it. Nothing is converted: health checks, device requests and every other field are passed through as JSON |
| R10 | The new container is created under a temporary name and compared with the old one before the old one is stopped |
| R11 | The old container is renamed and kept until the new one passes. Rollback removes the new one, gives the old one its name back and starts it |
| R12 | Every network is connected before the new container starts |
| R13 | On each host, containers are updated one at a time. Other containers are never stopped |
| R14 | Rollback runs automatically for scheduled updates only. There is no manual rollback button and old images are not kept |
| R15 | A digest whose update failed is skipped at the next run. A newer digest is tried again |
| R16 | Before updating a host, the server checks that the agent runs the same version as the server and that the Docker API is 1.43 or newer. If not, the host is skipped and the notification says why |
| R17 | Notifications go to a Discord webhook by default. After the check run, each host gets one message listing Available. After the update run, each host gets one message listing Updated, Rolled back and Failed, plus the space that prune reclaimed. See [Notifications](#notifications) |
| R18 | Every rolled back and failed container carries a short summary of why |
| R19 | hikifune ships a default Discord template, and the template can be edited in the web UI |
| R20 | The old image is pruned after a successful update |
| R21 | Written in Go. Images `hikifune` and `hikifune-agent` |

### Proposed

| ID | Proposal | Why |
| -- | -- | -- |
| P1 | Copy `HostConfig` and the network endpoints as they are. In `Config`, drop the values that came from the old image (environment variables, labels, `Cmd`, `Entrypoint`, `Healthcheck`, `WorkingDir`, `User`, exposed ports and volumes that equal the old image's own config) and keep everything that was set on the container | Copying `Config` whole would pin the old image's defaults. A new image that changes `PATH`, an entrypoint or its health check would never get the change |
| P2 | The step 3 comparison checks what was copied (P1), not the whole inspect output | The new image's own defaults are expected to differ |
| P3 | **Check now** for a host or all hosts, and **Update now** for one container, in the web UI. Manual updates do not roll back automatically (R14) | A fix often should not wait for 01:00 |
| P4 | hikifune never recreates its own server or agent containers. It shows their available updates | An agent that stops itself mid-update cannot roll back |
| P5 | Update dependencies before the containers that depend on them, using the `com.docker.compose.depends_on` label when it is present | Avoids restarting an app against a database that is about to restart |
| P6 | A check-only switch per host | For the first days on a host, and for hosts where updates should wait |
| P7 | The Docker and registry clients are written on `net/http` without the Docker SDK | Few dependencies, and the API version stays under our control |
| P8 | Configuration through `HIKIFUNE_*` environment variables. Every key is listed in `.env.example`, even when empty | One place to see every setting |
| P9 | The server keeps a run history (the last N runs) with each container's result and reason | The web UI and the notification come from the same record |
| P10 | A **Send test** button for the notification template | Template edits are checked without waiting for a run |
| P11 | Rollback criteria: roll back if the new container exits or restarts within a watch window. If it has a health check, it must also be healthy by the end of the window. The window defaults to 2 minutes | Covers both images with and without health checks |

### Not doing

| Item | Reason |
| -- | -- |
| Manual rollback button, keeping old images | Decided (R14) |
| Pre and post update hooks, update delays, health watching outside updates | Never used in tugtainer |
| Converting any container setting into CLI flags or another format | The cause of most tugtainer failures |

## Updating one container

1. Pull the new image.
2. Inspect the old container and its image, and build the new configuration (R9, P1).
3. Create the new container under a temporary name with the first network. Inspect it and compare it with what was copied (P2). If it differs, remove the new container and report it.
4. Stop the old container and rename it.
5. Give the new container the original name and connect every remaining network while it is still stopped.
6. Start the new container and watch it (P11).
7. If it passes, remove the old container and prune the old image. If it fails, roll back (R11).

## Notifications

The default template follows the layout of the tugtainer messages it replaces (R17 to R19):

- A header with the host name.
- One section per list. A section with nothing in it is left out.
- Each container as its name and image reference.
- Under each rolled back or failed container, the reason in one line.
- The space prune reclaimed, after the update run.
- A host skipped by the version check (R16) gets a message that says why.

Example of an update run message:

```
hikifune · host-a

Updated
• monitoring-prometheus-1  docker.io/prom/prometheus:main

Rolled back
• apps-web-1  ghcr.io/example/web:latest
  not healthy 2m after start: health check exited with 1

Failed
• apps-tautulli-1  ghcr.io/tautulli/tautulli:latest
  create refused: port 8181 is already allocated

Reclaimed 4.8 GB
```

Proposed: send the message as a Discord embed under the hikifune name and logo, and send nothing for a host when every section is empty.

## Milestones

Proposed order. Each milestone ends when its "Done when" holds. v0.1.0 is M0 to M7. Integration tests run against Docker-in-Docker containers, never the developer's own Docker.

### M0. Skeleton

- Settle the repository layout (open question 1, decided: one repository).
- `go.mod`, `cmd/hikifune`, `cmd/hikifune-agent`, `internal/`, one Dockerfile with a server target and an agent target, a Makefile that runs Go in a container, `deploy/compose/.env.example`.
- GitHub Actions: `go vet`, `go test` and both image builds on every push. Images go to GHCR on tags only.
- Before tugtainer is removed, record everything useful from it: the web UI, the Discord message format and template variables, the agent API, the database structure and its logs.

Done when: CI passes on `dev` and both images build for amd64 and arm64.

### M1. Docker client and inventory

- `internal/docker`: Engine API over a unix socket or TCP (socket proxy), pinned to `/v1.43`. Version, ping, info, container list and inspect, image inspect, network connect.
- Inventory per container: name, compose project and service, image reference, image ID, repo digests, labels, protected flag, state and health.
- `hikifune-agent inventory` prints it as JSON. Read only.

Done when: inventory runs against Docker 24 and the current Docker release, and every container shows up with the right protected flag.

### M2. Registry check

- Parse image references: Docker Hub default and `library/`, tags, references pinned by digest (reported as pinned, not checked).
- Read credentials from `config.json` `auths`.
- Registry auth: anonymous, basic and bearer token through `WWW-Authenticate`.
- `HEAD` the manifest with the OCI index, Docker manifest list and single manifest media types, and read `Docker-Content-Digest`. Nothing is pulled.
- Compare it with the local image's repo digest for the same repository. A locally built image without a repo digest is reported as not checkable.
- An empty repo digest list is not enough to spot a local build. The M1 tests showed that the containerd image store (the default on new Docker 29 installs) gives a never-pushed local build a repo digest for its own name, while Docker 24 leaves the list empty. A registry that answers 404 or 401 for the repository has to be reported as not checkable too, not as a failure.

Done when: unit tests pass against a fake registry for every auth type and media type, and live checks against Docker Hub and GHCR (public and private) agree with what `docker pull` reports.

### M3. Recreate and rollback

- The seven steps above, the config builder (P1) and the comparison (P2).
- The watch (P11) and rollback (R11).
- Integration tests on Docker 24 and the current release:

| Case | Expected |
| -- | -- |
| Exec-form health check on an image without a shell | Healthy after the update |
| Container on three networks | All three attached before start |
| New image that exits at once | Rolled back with the old name, networks and state |
| Published ports | Old container stopped before the new one starts, no port conflict |
| Named volumes and bind mounts | Same mounts after the update |
| New image that changes an environment variable and its entrypoint | Change picked up, values set on the container kept |
| Device requests | Same `DeviceRequests` JSON after the update |

Done when: every case passes on both Docker versions, and a real GPU container keeps its GPU after an update.

### M4. Agent API and server core

- `internal/protocol` and the version handshake (R16). Connection and auth (open question 2).
- Agent endpoints: version, inventory, check, update one container, prune.
- Server: agent list, scheduler with time zone, run records (P9), the skip list for failed digests (R15), dependency order (P5), state storage (open question 5).

Done when: a scheduled run against two Docker-in-Docker hosts checks both, updates one container at a time, records every result, and skips a failed digest at the next run.

### M5. Web UI

- Server-rendered HTML with HTMX, like the author's other services. Sign-in (open question 4).
- Hosts: agent version, Docker API version, last contact.
- Containers: every container on every host (R5), filtered by host, state, protected and update available.
- Runs: history with each result and reason.
- Settings: schedule, check-only hosts (P6), notification template.
- Actions: Check now and Update now (P3).

Done when: every container on every host appears, protected ones marked, and both actions run and show their results.

### M6. Notifications

- Discord webhook with the default template ([Notifications](#notifications)).
- Template variables: host, run type (check or update), the four lists, image references, reasons and reclaimed space.
- Send test (P10).
- Splits a host's message when it is longer than Discord allows.

Done when: a check run sends one Available message per host, an update run with updated, rolled back and failed containers sends one message per host with every reason, and an edited template is used.

### M7. Release candidate

- Prune and the reclaimed space in the notification (R20).
- Multi-arch images on GHCR.
- README: install with compose for the server and the agents, socket proxy settings, labels, every setting.
- Tag `v0.1.0-rc.1`.

Done when: a fresh install from the README works on a clean Docker-in-Docker host.

### M8. Rollout

- Run hikifune check-only (P6) next to the old updater and compare the lists of available updates.
- Turn updates on host by host.
- Remove the old updater.
- Release v0.1.0.

Done when: a week of scheduled runs with no failure left unexplained, and the old updater is gone.

## Open questions

| # | Question | Notes |
| -- | -- | -- |
| 1 | Repository layout | Decided: one repository, below |
| 2 | How the server and agents connect | Which side dials the other, and how they authenticate (per-agent token or mTLS). tugtainer's server dialed its agents |
| 3 | Rollback window | P11 proposes 2 minutes |
| 4 | Web UI sign-in | OIDC in the app, or left to a reverse proxy |
| 5 | Where the server keeps state | SQLite, or a JSON file trimmed to the last N runs |
| 6 | What else `protected` turns off | R4 turns off automatic updates. Still open: whether a protected container is checked (so the UI shows an available update) and whether Update now is refused for it, as tugtainer did. The 17 containers that were check-only in tugtainer were all access paths (VPN, socket proxy, tunnel, agents) |
| 7 | Credential helpers | Whether `credsStore` and `credHelpers` in `config.json` are supported, or only `auths` |
| 8 | Image platforms | amd64 and arm64 |
| 9 | Logo | Draft A (tow) is preferred, not final. The social preview drafts still say "opted in with compose labels", which no longer matches R4. Drafts are in [docs/logo](logo/) |

### Repository layout

Decided: the agent and the server in this one repository, laid out like the author's other Go services.

```
hikifune/
  cmd/hikifune/         server
  cmd/hikifune-agent/   agent
  internal/buildinfo/   the version both binaries carry
  internal/protocol/    messages both sides use
  internal/docker/      Engine API client, pinned to API 1.43
  internal/registry/    digest checks and registry auth
  internal/labels/      hikifune.* label parsing
  deploy/compose/       .env.example, and later the install, dev and e2e compose files
  docs/                 this plan, and later the architecture
  Dockerfile            two build targets, server and agent
  Makefile              test, vet, fmt and image builds in containers
```

- The two sides have to speak the same protocol, and a mismatch is what broke one tugtainer host. With one repository, one tag builds both images from the same commit, and the server can refuse an agent from another version and name both versions.
- Protocol types, the Docker client and label parsing are shared through `internal/` without versioning a module between two repositories.
- The cost is that a fix on one side bumps the version of both. At this size that is fine.

Separate repositories make sense only if agents need their own release schedule, which nothing here calls for.
