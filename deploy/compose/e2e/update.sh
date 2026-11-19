#!/bin/sh
# Runs inside one test daemon after containers.sh and sets up the update
# tests: a registry on localhost:5000, two versions of each test image in it,
# and containers on the first versions. Every container carries
# hikifune.e2e=update so the inventory tests can leave them out.
#
# Version 1 is built and pushed as localhost:5000/e2e/<name>:1, which the
# containers use. Version 2 is built as 127.0.0.1:5000/e2e/<name>:1, the same
# registry under another name, and pushed over it. The local tag the
# containers use stays on version 1 until an update pulls.
set -eu

BASE=busybox:1.37
REG=localhost:5000
L="hikifune.e2e=update"

for n in upd-n1 upd-n2 upd-n3; do
  docker network rm "$n" >/dev/null 2>&1 || true
done
docker volume prune -af >/dev/null
docker pull -q "$BASE" >/dev/null
docker pull -q registry:3 >/dev/null

docker run -d --name upd-registry --label "$L" -p 5000:5000 registry:3 >/dev/null
i=0
until wget -q -O /dev/null "http://$REG/v2/"; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "the registry did not start" >&2
    exit 1
  fi
  sleep 0.5
done

# image NAME VERSION: builds the Dockerfile on stdin as that version.
image() {
  if [ "$2" = 1 ]; then ref="$REG/e2e/$1:1"; else ref="127.0.0.1:5000/e2e/$1:1"; fi
  docker build -q -t "$ref" - >/dev/null
  docker push -q "$ref" >/dev/null
}

for v in 1 2; do
  # No shell in the image; the health check is in exec form. The musl
  # busybox is static, so it runs from scratch.
  image noshell "$v" <<EOF
FROM busybox:1.37-musl AS bb
FROM scratch
COPY --from=bb /bin/busybox /busybox
HEALTHCHECK --interval=1s --timeout=2s --retries=2 CMD ["/busybox", "true"]
CMD ["/busybox", "sleep", "1d"]
LABEL version=$v
EOF

  # Starts only when three networks are attached.
  image nets "$v" <<EOF
FROM $BASE
CMD ["sh", "-c", "n=\$(ls /sys/class/net | grep -c '^eth'); [ \"\$n\" -ge 3 ] || exit 1; exec sleep 1d"]
LABEL version=$v
EOF

  image web "$v" <<EOF
FROM $BASE
CMD ["httpd", "-f", "-p", "80"]
LABEL version=$v
EOF

  for name in gpu rm; do
    image "$name" "$v" <<EOF
FROM $BASE
CMD ["sleep", "1d"]
LABEL version=$v
EOF
  done
done

# Version 2 changes an environment variable, the entrypoint and the command.
image app 1 <<EOF
FROM $BASE
ENV IMG=v1 KEEP=img
VOLUME /anon
ENTRYPOINT ["sleep"]
CMD ["1d"]
EOF
image app 2 <<EOF
FROM $BASE
ENV IMG=v2 KEEP=img
VOLUME /anon
ENTRYPOINT ["/bin/sleep"]
CMD ["2d"]
EOF

# Version 2 exits at once.
image crash 1 <<EOF
FROM $BASE
CMD ["sleep", "1d"]
EOF
image crash 2 <<EOF
FROM $BASE
CMD ["sh", "-c", "exit 3"]
EOF

# Version 2 never passes its health check.
image unhealthy 1 <<EOF
FROM $BASE
HEALTHCHECK --interval=1s --timeout=2s --retries=1 CMD ["true"]
CMD ["sleep", "1d"]
EOF
image unhealthy 2 <<EOF
FROM $BASE
HEALTHCHECK --interval=1s --timeout=2s --retries=1 CMD ["false"]
CMD ["sleep", "1d"]
EOF

for n in upd-n1 upd-n2 upd-n3; do
  docker network create --label "$L" "$n" >/dev/null
done
mkdir -p /srv/upd-bind

docker run -d --name upd-noshell --label "$L" "$REG/e2e/noshell:1" >/dev/null

docker create --name upd-nets --label "$L" --network upd-n1 --network-alias a1 "$REG/e2e/nets:1" >/dev/null
docker network connect --alias a2 upd-n2 upd-nets
docker network connect upd-n3 upd-nets
docker start upd-nets >/dev/null

docker run -d --name upd-web --label "$L" -p 18080:80 "$REG/e2e/web:1" >/dev/null

app_id=$(docker image inspect -f '{{.Id}}' "$REG/e2e/app:1")
docker run -d --name upd-app --label "$L" --label "com.docker.compose.image=$app_id" \
  -e KEEP=mine -e EXTRA=x -v upd-data:/data -v /srv/upd-bind:/bind:ro "$REG/e2e/app:1" >/dev/null

docker run -d --name upd-crash --label "$L" --restart unless-stopped --network upd-n1 "$REG/e2e/crash:1" >/dev/null
docker run -d --name upd-unhealthy --label "$L" "$REG/e2e/unhealthy:1" >/dev/null
docker run -d --name upd-rm --label "$L" --rm "$REG/e2e/rm:1" >/dev/null
# Created, never started: there is no GPU here, so only steps 1 to 3 run on it.
docker create --name upd-gpu --label "$L" --gpus all "$REG/e2e/gpu:1" >/dev/null

for c in upd-noshell upd-unhealthy; do
  i=0
  until [ "$(docker inspect -f '{{.State.Health.Status}}' "$c")" = healthy ]; do
    i=$((i + 1))
    if [ "$i" -gt 30 ]; then
      echo "$c did not become healthy" >&2
      exit 1
    fi
    sleep 1
  done
done
for c in upd-nets upd-web upd-app upd-crash upd-rm; do
  if [ "$(docker inspect -f '{{.State.Running}}' "$c")" != true ]; then
    echo "$c is not running" >&2
    exit 1
  fi
done

echo "$(docker version -f '{{.Server.Version}}'): update fixtures ready"
