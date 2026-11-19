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
| R3 | Works with Engine API 1.43 (Docker 24) and newer. The agent talks to each daemon at the daemon's own API version. Creating a container at an older version drops or changes settings a newer daemon has (health check start interval, per-network MAC address and gateway priority, umask, recursive read-only mounts), and inspecting at a version before 1.45 adds the container's short ID to its network aliases |
| R4 | A container with no `hikifune.*` label is checked and updated automatically. `hikifune.protected=true` turns automatic updates off; a protected container is still checked, so its available update shows in the notification and the web UI |
| R5 | The web UI lists every container on every host, protected ones too |
| R6 | Checks run daily at 00:00 and updates at 01:00 by default, in the configured time zone. Each of the two runs every day, every week on one weekday, or every month on one day; on days 29 to 31 a shorter month runs on its last day. `HIKIFUNE_CHECK_EVERY` and `HIKIFUNE_UPDATE_EVERY` take `day`, `sun` to `sat`, or `1` to `31` |
| R7 | A check compares the registry digest of the container's image reference with the local image |
| R8 | Private registries are reached with a Docker `config.json` mounted into the agent |
| R9 | A container is recreated from the configuration the Engine API reports for it. Nothing is converted: health checks, device requests and every other field are passed through as JSON |
| R10 | The new container is created under a temporary name and compared with the old one before the old one is stopped |
| R11 | The old container is renamed and kept until the new one passes. Rollback removes the new one, gives the old one its name back and starts it |
| R12 | Every network is connected before the new container starts |
| R13 | On each host, containers are updated one at a time. Other containers are never stopped |
| R14 | Rollback runs automatically for every update, scheduled or started with Update now (changed from scheduled only before M5: a failed update by hand would otherwise leave a broken new container and a renamed old one). There is no manual rollback button and old images are not kept |
| R15 | A digest whose update failed is skipped at the next run. A newer digest is tried again |
| R16 | Before updating a host, the server checks that the agent runs the same version as the server and that the Docker API is 1.43 or newer. If not, the host is skipped and the notification says why |
| R17 | Notifications go to a Discord webhook after each scheduled run, and after `hikifune run`; runs started in the web UI are seen there and send nothing. After the check run, each host gets one message listing Available. After the update run, each host gets one message listing Updated, Rolled back and Failed, plus the space that prune reclaimed. In both, Failed also lists containers whose registry check failed, with the reason. A host with nothing to say gets no message. A notification is sent once the run has ended and the next may start, so a slow webhook holds back no run. See [Notifications](#notifications) |
| R18 | Every rolled back and failed container carries a short summary of why |
| R19 | hikifune ships a default Discord template, and the template can be edited in the web UI. It is a Go `text/template` for the embed text, kept in the server state; it is saved only when it parses and renders a message of every kind. A template is bounded in size, in what it writes, in the numbers it names, in how deep its loops nest and in how long the strings it makes get, so a saved template cannot make the server spin or fill its memory. When it fails on a message, or writes nothing for one with something to say, the default writes that message. **Send test** posts a sample message with the template in the editor, saved or not |
| R20 | The old image is pruned after a successful update |
| R21 | Written in Go. Images `hikifune` and `hikifune-agent` |
| R22 | The new configuration keeps what was set on the container and drops what came from the old image. See [Building the new configuration](#building-the-new-configuration). Copying `Config` whole would pin the old image's defaults, so a new image that changes `PATH`, an entrypoint or its health check would never get the change |
| R23 | The step 3 comparison checks what hikifune sent, not the whole inspect output: every value sent is in the new container, it runs the pulled image, every network is attached, and every mount of the old container is in the new one. Defaults the new image adds are expected to differ |
| R24 | A scheduled update is rolled back when the new container exits, restarts or reports unhealthy within the watch window, or when it has a health check and is not healthy at the end of the window. The window is 2 minutes, one server setting |
| R25 | When the registry digest differs from the host's, the check also asks the registry for the host's digest (or, for an image that lost its repo digest, its image ID). If the registry does not have it, the image was built on the host (or its manifest was deleted there), and the container is reported not checkable instead of update available, so a local build is never replaced by a registry image of the same name. The same goes for a moved tag that now names a local build |
| R26 | The server dials each agent, as Prometheus does its targets. Every operation is the server asking at a time it chooses, so request and answer fit; an agent is up when it answers. The agent listens on an address set for it (a tailnet or LAN address) and accepts only requests that carry its own token. Encryption is left to the network. An agent that dials out can be added later with the same messages |
| R27 | The server keeps its state (run records, the failed digest list, settings, agents) in JSON files behind a store interface, run records trimmed to the last N runs. Beyond that the store keeps, for each host, its newest container list and the updates by hand after it, so the web UI always has one; the run history shows the last N only. Files are written to a temporary file and renamed. At this size (about 200 bytes per container per run) a month of runs is about a megabyte; SQLite can replace the store when it grows |
| R28 | hikifune never recreates its own server or agent containers, which it knows by an image repository named `hikifune` or `hikifune-agent`. It shows their available updates. An agent that stops itself mid-update cannot roll back |
| R29 | Dependencies are updated before the containers that depend on them, using the `com.docker.compose.depends_on` label within a compose project. This avoids restarting an app against a database that is about to restart |
| R30 | Configuration is through `HIKIFUNE_*` environment variables. Every key is listed in `.env.example`, even when empty. Agents and their tokens are added with `hikifune agent add`, which writes them to the server state |
| R31 | The server keeps a run history (the last N runs) with each container's result and reason. The web UI and the notification come from the same record |
| R32 | The server starts an update on an agent and gets a job number back, then asks for the job until it is done. A dropped connection loses no result, and an agent takes one job at a time, which keeps one update at a time on a host (R13) |
| R33 | `HIKIFUNE_AUTH` has no default, so the server never starts open by accident. `oidc` signs users in through any standard OIDC provider in the app, as ukai and noroshi do; `none` leaves access control to a proxy in front, such as Cloudflare Access, and the server should then listen only where that proxy reaches it. The web UI can recreate containers on every host, so an open server is not an option |
| R34 | **Check now** for a host or all hosts, and **Update now** for one container, in the web UI. Update now works on protected containers after a confirmation, and on check-only hosts; it ignores the failed digest list and rolls back like any update (R14). It sends the ID of the container the page showed, and does nothing to a container recreated since. Only one run goes at a time: Check now and Update now are refused while a run goes, and a scheduled run waits for it to end, then starts only if its time was not changed meanwhile |
| R35 | A check-only switch per host: the host is checked on schedule but not updated then (for the first days on a host, and for hosts where updates should wait). Turning it on also stops the scheduled updates still to come in a run going on |
| R36 | The schedule set in the web UI is kept in the server state and wins over `HIKIFUNE_CHECK_EVERY`, `HIKIFUNE_CHECK_AT`, `HIKIFUNE_UPDATE_EVERY` and `HIKIFUNE_UPDATE_AT`, which only give the first values. Saving it takes effect at once, counted from the moment it was saved |
| R37 | The Discord webhook URL comes from `HIKIFUNE_DISCORD_WEBHOOK_URL` only; with none set, nothing is sent. The URL holds the webhook's token, so the web UI shows it with the token hidden and it never reaches a log or an error. Errors keep only Discord's own message, with the token taken out, and a redirect is not followed. `HIKIFUNE_DISCORD_AVATAR_URL` sets the avatar; empty keeps the one set on the webhook |

### Proposed

| ID | Proposal | Why |
| -- | -- | -- |
| P7 | The Docker and registry clients are written on `net/http` without the Docker SDK | Few dependencies, and the API version stays under our control |

### Not doing

| Item | Reason |
| -- | -- |
| Manual rollback button, keeping old images | Decided (R14) |
| Pre and post update hooks, update delays, health watching outside updates | Never used in tugtainer |
| Converting any container setting into CLI flags or another format | The cause of most tugtainer failures |
| Updating by committing image digests to the compose repository and letting the agent apply them, like Argo CD for compose | Decided against before M3. On the hosts this replaces, 46 of 59 image references are `latest` or default to it, so a recreated container does not leave the repository behind. Committing would need every reference pinned and write access to the repository from the hosts, and it leaves out containers outside compose files and users without such a repository |

## Updating one container

Only running containers are updated. A container created with `--rm` is refused: stopping it would remove it and leave nothing to roll back to.

1. Ask the registry check (R25) about the image; only update available goes on. Pull the new image. If it is the image the container already runs, stop here. If it is not the digest the check saw, another pull or build moved the tag in between, and the update fails.
2. Inspect the old container and both images, and build the new configuration (R9, R22).
3. Create the new container under a temporary name with its primary network, and connect the other networks while it is still stopped. Inspect it and compare (R23). If it differs, remove the new container and report it.
4. Stop the old container and rename it.
5. Give the new container the original name.
6. Start the new container and watch it (R24).
7. If it passes, remove the old container, and the old image when nothing else uses it. If it fails, roll back (R11).

A temporary name already taken, for example by a container left from an interrupted update, fails the update before anything is stopped. hikifune never removes such a container itself, since it may be the only copy of the previous version.

Each call that changes a container (create, connect, stop, rename, start) is sent whole even when the update is cancelled, so its outcome is always known. A cancel is taken between steps: before the old container is stopped it removes the new one, after that it rolls back. A rollback counts only when the old container runs under its own name again.

### Building the new configuration

- `HostConfig` is copied as it is. The anonymous volumes the old container uses (from the image's `VOLUME` lines, a compose volume without a source, or a bind without a source) are mounted again at the same paths, so their data stays.
- In `Config`, a value is dropped when it equals the old image's: each environment variable, each label, each exposed port and volume, and `Healthcheck`, `WorkingDir`, `User`, `StopSignal` and `Shell`. Everything else is kept.
- `Entrypoint` is dropped when it equals the old image's. `Cmd` is dropped only when `Entrypoint` was dropped and `Cmd` equals the old image's too, because Docker does not apply an image's `Cmd` to a container with its own entrypoint. An empty list is not the same as none here: an empty entrypoint clears the image's and is kept.
- The default host name, which is the old container's short ID, is dropped, and so is a host name under the `host` or `container:` network mode.
- Network endpoints keep what was configured (IPAM config, aliases, links, driver options, gateway priority) but not what Docker assigned (IP and MAC addresses, endpoint and network IDs, DNS names), nor the old container's ID among the aliases. A MAC address set by hand is not kept either, since the inspect output cannot tell it from one Docker assigned.
- A `com.docker.compose.image` label that holds the old image ID gets the new image ID. Compose recreates a container whose label differs from the image it would run, so a later `docker compose up -d` would otherwise recreate it once more.

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

The message is a Discord embed sent under the hikifune name: the title is the host, the color the worst result (red for rolled back, failed or left out, green for updated, blue for available), the footer the run and its time. The template writes the embed text only. Text longer than an embed takes (4096 characters) goes in parts, cut at line ends. Container names go in code spans and other values are escaped, so Discord reads none of them as markdown, and the message pings no one. A rate limit is waited out as Discord asks, up to 5 minutes; a longer one fails the message. A server error is tried again, which could post a message twice if Discord took it before failing; Discord answers with the result, so this is rare.

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
- A container can run an image its reference no longer names: a later pull or build moved the tag and the container was not recreated. The live checks found one on a Docker 29 host. With the containerd store, a pulled image left behind keeps no repo digest, and a local image rebuilt under the same name loses its image record while the container still runs it. The inventory marks these `tag_moved`, and the check reports update available when the registry has the reference, not checkable when it does not.

Done when: unit tests pass against a fake registry for every auth type and media type, and live checks against Docker Hub and GHCR (public and private) agree with what `docker pull` reports.

### M3. Recreate and rollback

- The Docker client moves from `/v1.43` to each daemon's own API version (R3), and gains create, start, stop, rename, remove and image pull.
- The seven steps above, the config builder (R22) and the comparison (R23).
- The watch (R24) and rollback (R11).
- `hikifune-agent update <container>` runs one update and prints the result as JSON.
- The local build check in the registry check (R25). With the containerd store, a local build gets a repo digest for its own name (M2), so without it a local build whose name also exists on a registry would be reported as an update and replaced.
- Integration tests on Docker 24 and the current release, against a registry inside each test daemon:

| Case | Expected |
| -- | -- |
| Exec-form health check on an image without a shell | Healthy after the update |
| Container on three networks | All three attached before start, aliases kept, no old container ID among them |
| New image that exits at once | Rolled back with the old name, networks and state |
| New image whose health check fails | Rolled back |
| Published ports | Old container stopped before the new one starts, no port conflict |
| Named volumes, bind mounts and an anonymous volume | Same mounts after the update, data in the anonymous volume kept |
| New image that changes an environment variable and its entrypoint | Change picked up, values set on the container kept |
| Compose image label | Holds the new image ID |
| Device requests | Same `DeviceRequests` JSON in the new container |
| Container created with `--rm` | Refused before anything is stopped |

Done when: every case passes on both Docker versions, and a real GPU container keeps its GPU after an update.

### M4. Agent API and server core

- `internal/protocol` and the version handshake (R16). The server dials each agent with its token (R26).
- Agent endpoints: version, inventory, check, update one container, prune.
- Server: agent list, scheduler with time zone, run records (R31), the skip list for failed digests (R15), dependency order (R29), state storage in JSON (R27).
- `hikifune-agent` with no command serves the API on `HIKIFUNE_AGENT_LISTEN` (`:7778`) and needs `HIKIFUNE_AGENT_TOKEN`. `hikifune agent add <name> <url>` makes the token and prints it once (a name is up to 63 letters, digits, dots, underscores and hyphens, as it goes into the web UI's paths); `hikifune run check|update` runs once now.
- Registries in `HIKIFUNE_INSECURE_REGISTRIES` are asked over plain HTTP, like Docker's insecure-registries, and never get credentials, neither in the check nor in the pull.
- `hikifune-agent update` is a command for trying an update by hand. It runs outside the agent's one-job-at-a-time rule, so it is not meant for a host while a run is going.
- After an update run, a host where something was updated prunes its dangling images, and the run record keeps the space reclaimed for the notification (M6).
- Integration tests: one shared registry for both test daemons, and the server driving one agent per daemon over HTTP.

Done when: a scheduled run against two Docker-in-Docker hosts checks both, updates one container at a time, records every result, and skips a failed digest at the next run.

### M5. Web UI

- Server-rendered HTML with HTMX, like the author's other services. Sign-in (R33).
- A sidebar holds the pages and the hosts, each host with its count of pending updates. The light and dark switch floats at the bottom right.
- Hosts: agent version, Docker API version, last contact. A host's own page shows its facts, its check-only switch and its containers.
- Containers: every container on every host (R5), grouped by host or in one list, filtered by host, state, protected and update available. Update available means the newer digest is not on the container yet, whatever the last run did with it. Host names in the pages' paths are escaped as one path segment.
- Runs: history with each result and reason.
- Settings: schedule (R36), check-only hosts (R35). The notification template comes with M6.
- Actions: Check now and Update now (R34).
- Sign-in and the page frame are ported from ukai: OIDC with PKCE and a signed session cookie, cross-origin protection on every form post, and a content security policy without inline scripts. A strip at the top of the page follows a run while it goes and reloads the page when it ends.

Done when: every container on every host appears, protected ones marked, and both actions run and show their results.

### M6. Notifications

- Discord webhook with the default template ([Notifications](#notifications)).
- Template variables: host, run type (check or update), the four lists, image references, reasons and reclaimed space.
- Send test (R19).
- The schedule's period per run: every day, a weekday, or a day of the month (R6).
- Splits a host's message when it is longer than Discord allows.

Done when: a check run sends one Available message per host, an update run with updated, rolled back and failed containers sends one message per host with every reason, and an edited template is used.

### M7. Release candidate

- Prune and the reclaimed space in the notification (R20).
- Multi-arch images on GHCR.
- README: install with compose for the server and the agents, socket proxy settings, labels, every setting.
- Tag `v0.1.0-rc.1`.

Done when: a fresh install from the README works on a clean Docker-in-Docker host.

### M8. Rollout

- Run hikifune check-only (R35) next to the old updater and compare the lists of available updates.
- Turn updates on host by host.
- Remove the old updater.
- Release v0.1.0.

Done when: a week of scheduled runs with no failure left unexplained, and the old updater is gone.

## Open questions

| # | Question | Notes |
| -- | -- | -- |
| 1 | Repository layout | Decided: one repository, below |
| 2 | How the server and agents connect | Decided: the server dials each agent, like Prometheus, and authenticates with a token per agent (R26) |
| 3 | Rollback window | Decided: 2 minutes (R24) |
| 4 | Web UI sign-in | Decided: `HIKIFUNE_AUTH` is required, `oidc` or `none` (R33) |
| 5 | Where the server keeps state | Decided: JSON files behind a store interface (R27) |
| 6 | What else `protected` turns off | Decided: it is still checked and shown (R4), and Update now works on it after a confirmation (R34) |
| 7 | Credential helpers | Whether `credsStore` and `credHelpers` in `config.json` are supported, or only `auths`. M2 reads `auths` only; when a registry refuses an image whose credentials sit in a helper, the reason names the helper |
| 8 | Image platforms | amd64 and arm64 |
| 9 | Logo | Draft A (tow) is preferred, not final. The social preview drafts still say "opted in with compose labels", which no longer matches R4. Drafts are in [docs/logo](logo/) |
| 10 | Recreate containers, or commit to Git and let the agent apply it | Decided: recreate. The other way is under [Not doing](#not-doing) |

### Repository layout

Decided: the agent and the server in this one repository, laid out like the author's other Go services.

```
hikifune/
  cmd/hikifune/         server
  cmd/hikifune-agent/   agent
  internal/buildinfo/   the version both binaries carry
  internal/protocol/    messages both sides use
  internal/docker/      Engine API client, at each daemon's own API version (1.43 or newer)
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
