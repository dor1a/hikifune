<div align="center">

<img src="docs/logo/A-tow.svg" alt="" width="96">

# hikifune

**Checks images and updates containers across Docker hosts, with one agent per host**

</div>

> [!NOTE]
> 0.1.0 is the first release. [The plan](docs/PLAN.md) has the requirements and the design decisions behind them.

## What it does

- One small agent runs on each Docker host, and one server holds the schedule, the web UI and the notifications. The server dials the agents; an agent only answers.
- Every container's image is checked for a newer digest, by default every day at 00:00, and the ones that have one are updated at 01:00.
- The web UI opens on Home: the hosts, containers and updates at a glance, the next runs, the latest runs, and a live map of the server and its agents (top down, orbit or flow) that shows which agents answer and which run is going on.
- Everything is updated unless told not to. `hikifune.protected=true` on a container keeps it from being updated automatically; it is still checked, and the web UI lists every container either way.
- A container is recreated from the configuration Docker reports for it, with only the image changed, so health checks, GPU requests, networks and mounts come back as they were. Defaults that came from the old image are dropped, so the new image's apply.
- The old container is kept under another name until the new one has run for two minutes. With a health check, the new one must not turn unhealthy meanwhile (starting is fine) and must be healthy at the end. If not, it is rolled back, and that image digest is skipped at the next run.
- After a host's updates, its dangling images (no tag, no container) are pruned: the old images the updates left, and any other dangling image on that host. The space freed is reported.
- After each scheduled run, Discord gets what is available, updated, rolled back or failed on each host with something to say, with the reason for each rollback and failure, and the space the prune freed. By default a run's hosts share a message, an embed each in the color of its worst result, going on in the next message past Discord's 10 embeds or 6,000 characters; Settings can give each host its own message instead. A host with nothing to say is left out. The message template can be edited in the web UI, next to a preview.
- Works with Docker 24 (Engine API 1.43) and newer, on amd64 and arm64.

## How it fits together

```
                      +-------------------------------+
                      | hikifune (server)       :7777 |
                      | schedule, web UI, Discord     |
                      +-------------------------------+
                         |  HTTP, one token per agent
                         |  (tailnet or LAN)
          +--------------+--------------+
          v                             v
 +------------------+          +------------------+
 | hikifune-agent   |          | hikifune-agent   |   one per host
 |            :7778 |          |            :7778 |
 +------------------+          +------------------+
          |  tcp:2375, internal network
 +---------------------+
 | docker-socket-proxy |       only the API calls hikifune needs
 +---------------------+
          |  /var/run/docker.sock, read only
       Docker
```

Each agent asks the registries itself, so a private registry's credentials stay on the host that pulls from it.

## Install

The files are in [deploy/compose](deploy/compose). The examples use the 0.1.0 images, `ghcr.io/dor1a/hikifune:0.1.0` and `ghcr.io/dor1a/hikifune-agent:0.1.0`.

### 1. The server

On the host that runs the server:

```sh
mkdir hikifune && cd hikifune
curl -fsSLO https://raw.githubusercontent.com/dor1a/hikifune/v0.1.0/deploy/compose/server/compose.yaml
curl -fsSL -o .env https://raw.githubusercontent.com/dor1a/hikifune/v0.1.0/deploy/compose/server/.env.example
```

In `.env`, set at least:

- `HIKIFUNE_AUTH`: `oidc` to sign in through an OIDC provider, or `none` when a proxy in front (such as Cloudflare Access) does the sign-in. It has no default, so the server never starts open by accident. With `none`, publish the port only where that proxy reaches it.
- With `oidc`, also `HIKIFUNE_PUBLIC_URL`, `HIKIFUNE_OIDC_ISSUER`, `HIKIFUNE_OIDC_CLIENT_ID`, `HIKIFUNE_OIDC_CLIENT_SECRET` and `HIKIFUNE_SESSION_KEY` (see [Settings](#server)). The server does not start while one is missing, and says which.
- `HIKIFUNE_TIMEZONE`: the time zone of the schedule, such as `Asia/Seoul`. Empty is UTC in the image.
- `HIKIFUNE_DISCORD_WEBHOOK_URL`: the webhook for the messages. Empty sends none.

Then start it:

```sh
docker compose up -d
```

The web UI is on port 7777. With `oidc`, register `<HIKIFUNE_PUBLIC_URL>/oauth/callback` as the redirect URI at the provider; only members of `HIKIFUNE_OIDC_ALLOWED_GROUP` get in. The groups are read from the ID token, under the claim `HIKIFUNE_OIDC_GROUPS_CLAIM` (`groups` by default), and one of them must equal the group name exactly. Have the provider put the claim in the ID token, not only in the userinfo answer, and check the form of the names it sends: Keycloak's group mapper sends `/hikifune-users`, with a slash, unless its **Full group path** is off.

### 2. An agent on each host

Add the host on the server. The URL is where the server reaches the agent, a tailnet or LAN address:

```sh
docker compose exec hikifune hikifune agent add web1 http://100.64.0.12:7778
```

It prints `HIKIFUNE_AGENT_TOKEN=...` once. On that host:

```sh
mkdir hikifune-agent && cd hikifune-agent
curl -fsSLO https://raw.githubusercontent.com/dor1a/hikifune/v0.1.0/deploy/compose/agent/compose.yaml
curl -fsSL -o .env https://raw.githubusercontent.com/dor1a/hikifune/v0.1.0/deploy/compose/agent/.env.example
```

Put the token in `.env`, and start the agent with its socket proxy:

```sh
docker compose up -d
```

The agent's API accepts only requests with its token, and leaves encryption to the network; `/healthz` answers without one, for health checks. In `compose.yaml`, publish port 7778 on the address the server dials rather than on every interface, for example `"100.64.0.12:7778:7778"`.

### 3. The first runs

The host shows up in the web UI. For the first days on a host, turn its **check-only** switch on: it is checked on schedule but not updated, and you see what would be. Press **Check now** in the menu to check right away, or run `docker compose exec hikifune hikifune run check` on the server.

## The Docker API

The agent never needs the Docker socket itself. The example compose file puts [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) in front of it, on a network only the two share, with these sections open:

| Setting | Used for |
| -- | -- |
| `CONTAINERS=1` | Listing, inspecting, creating, starting, stopping, renaming and removing containers |
| `IMAGES=1` | Inspecting and pulling images, and pruning dangling ones |
| `NETWORKS=1` | Connecting a new container to every network of the old one |
| `SYSTEM=1` | The images' disk usage before and after a prune, to report the space freed. Without it, Docker 24 reports 0 |
| `POST=1` | Every call that changes something (pull, create, start, stop, rename, remove, prune) |

`VERSION` and `PING` are open by default. Nothing else is needed: no info, exec, build, volumes or events.

The proxy itself carries `hikifune.protected=true`: recreating it would cut the agent off from Docker in the middle of an update. A proxy you already run works too; point `HIKIFUNE_DOCKER_HOST` at it and open the same sections. Its server timeout has to outlast the longest pull (docker-socket-proxy's is 10 minutes).

## What gets updated

| Container | Checked | Updated on schedule |
| -- | -- | -- |
| Any running container | Yes | Yes |
| `hikifune.protected=true` | Yes | No. **Update now** in the web UI still works, after a confirmation |
| On a host with check-only on | Yes | No. **Update now** still works |
| hikifune's own server and agent | Yes | Never, not even by hand |
| Not running | Yes | No |
| Image named by digest (`image@sha256:...`) | Shown as pinned | No |
| Built on the host, or not on the registry | Shown as not checkable | No. A local build is never replaced by a registry image of the same name |
| Created with `--rm` | Yes | No; the update is refused before anything stops |

The value of `hikifune.protected` is read as a boolean (`true`, `false`, `1`, `0` and the like). The label with no value counts as `true`. Any other value also counts as protected, so a typo never leads to an update, and the check reports the label to fix.

An image counts as hikifune's own when the last part of its repository is `hikifune` or `hikifune-agent`, wherever it is hosted, so a mirror of hikifune's images is never recreated either.

An update that fails or is rolled back is not tried again on schedule for the same digest. A newer digest is, and **Update now** tries the same digest again; when it succeeds, the failure is forgotten.

Within a compose project, a service is updated after the services it depends on (`depends_on`), one container at a time on each host.

## Settings

Every key is in the `.env.example` files. The schedule set in the web UI, once saved, wins over the four schedule keys.

### Server

| Key | Default | |
| -- | -- | -- |
| `HIKIFUNE_LISTEN` | `:7777` | Address the web UI listens on inside the container. Leave the host part empty: `127.0.0.1:7777` would take only the container's own loopback, and the published port would not reach it. The compose file publishes container port 7777, so change its `ports` line with it, and limit who reaches it on the host side of that line |
| `HIKIFUNE_DATA` | `/data` | State directory: agents and their tokens, run history, settings. The compose file mounts the `data` volume at `/data`, so change the volume's `target` with it. The server runs as user 65532, which owns the image's `/data`; a directory anywhere else must be writable by that user |
| `HIKIFUNE_KEEP_RUNS` | `60` | Runs kept in the history |
| `HIKIFUNE_TIMEZONE` | UTC | Time zone of the schedule, an IANA name |
| `HIKIFUNE_CHECK_EVERY` | `day` | `day`, a weekday (`sun` to `sat`), a day of the month (`1` to `31`; a shorter month runs on its last day) or `last` |
| `HIKIFUNE_CHECK_AT` | `00:00` | Time of the check run |
| `HIKIFUNE_UPDATE_EVERY` | `day` | As `HIKIFUNE_CHECK_EVERY`, for the update run |
| `HIKIFUNE_UPDATE_AT` | `01:00` | Time of the update run |
| `HIKIFUNE_WATCH` | `2m` | How long a new container must keep running before the update counts, in whole seconds of at least `1s`. With a health check, it must not turn unhealthy meanwhile and must be healthy at the end |
| `HIKIFUNE_DISCORD_WEBHOOK_URL` | | Discord webhook. The web UI shows it with the token hidden |
| `HIKIFUNE_DISCORD_AVATAR_URL` | | Avatar of the messages; empty keeps the webhook's own |
| `HIKIFUNE_AUTH` | required | `oidc` or `none` |
| `HIKIFUNE_PUBLIC_URL` | | With `oidc`: the address users open, with no path, such as `https://hikifune.example.com` |
| `HIKIFUNE_OIDC_ISSUER` | | With `oidc`: the provider's issuer URL |
| `HIKIFUNE_OIDC_CLIENT_ID` | | With `oidc` |
| `HIKIFUNE_OIDC_CLIENT_SECRET` | | With `oidc` |
| `HIKIFUNE_OIDC_ALLOWED_GROUP` | `hikifune-users` | Group whose members may sign in, exactly as the provider names it in the claim |
| `HIKIFUNE_OIDC_GROUPS_CLAIM` | `groups` | Claim of the ID token that lists the user's groups |
| `HIKIFUNE_OIDC_SCOPES` | `openid profile email` | Scopes asked for; must include `openid` |
| `HIKIFUNE_SESSION_KEY` | | With `oidc`: at least 32 random bytes, such as the output of `openssl rand -hex 32` |

### Agent

| Key | Default | |
| -- | -- | -- |
| `HIKIFUNE_AGENT_LISTEN` | `:7778` | Address the agent listens on inside the container. Leave the host part empty, as for `HIKIFUNE_LISTEN`. The compose file publishes container port 7778, so change its `ports` line with it, and limit who reaches it on the host side of that line |
| `HIKIFUNE_AGENT_TOKEN` | required | The token `hikifune agent add` printed |
| `HIKIFUNE_DOCKER_HOST` | `unix:///var/run/docker.sock` | The Docker API: `tcp://host:port` for a socket proxy (the example), or a socket. TLS is not supported |
| `HIKIFUNE_DOCKER_CONFIG` | | A Docker `config.json` with credentials for private registries |
| `HIKIFUNE_INSECURE_REGISTRIES` | | Registries the agent asks over plain HTTP, comma separated, such as `registry.lan:5000`. They never get credentials. Docker pulls the images itself, so its daemon needs the same registries in its own `insecure-registries` |

## Private registries

The agent runs as user 65532, and a `config.json` you logged in with is usually readable by its owner only. Give the agent its own copy, readable by that user and no one else:

```sh
sudo install -m 0400 -o 65532 -g 65532 /root/.docker/config.json ./registry-auth.json
```

Mount it into the agent and point `HIKIFUNE_DOCKER_CONFIG` at it:

```yaml
    volumes:
      - type: bind
        source: ./registry-auth.json
        target: /config/config.json
        read_only: true
```

```sh
HIKIFUNE_DOCKER_CONFIG=/config/config.json
```

The agent reads the file when it starts. After `docker login` changes the credentials, copy it again with the same command and recreate the agent:

```sh
docker compose up -d --force-recreate hikifune-agent
```

Only the `auths` section is read. Credentials kept by a credential helper (`credsStore`, `credHelpers`) are not; the check names the helper when a registry refuses an image for that reason.

## Updating a container by hand

**Update now** in the web UI updates one container, protected or not, and rolls it back like any update. On the host, the agent's command does the same and prints the result. Both prune the host's dangling images after a successful update, as a run does:

```sh
docker compose exec hikifune-agent hikifune-agent update <container>
```

Run it in the agent's container as above: it shares a lock with the agent there, so the two never update at once.

## Upgrading hikifune

hikifune never recreates its own containers. Change the image tag in both compose files and run `docker compose up -d` on the server and on every agent. The server leaves out a host whose agent runs another version, and says so in the web UI and the message, so upgrade them together.

## Backup

The server's state is the `data` volume. It holds the agents' tokens, so keep a copy where only you can read it. Without it, add the agents again; containers and images on the hosts are not affected.

## Name

Hikifune (曳船) is Japanese for a tugboat. The agent tows each container back into its place.

## License

[MIT](LICENSE)
