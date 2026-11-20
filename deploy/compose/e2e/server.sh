#!/bin/sh
# Runs inside one test daemon after update.sh and sets up the server tests:
# a compose-like pair where web depends on db, an image whose second version
# exits at once, a protected container and one that runs hikifune's own agent
# image. Every container carries hikifune.e2e=server so the inventory tests
# leave them out. Versions go to the shared registry as in update.sh.
set -eu

: "${E2E_NS:?set E2E_NS to this daemon's name in the registry}"
BASE=busybox:1.37
REG=registry:5000/$E2E_NS
REG2=registry-v2:5000/$E2E_NS
L="hikifune.e2e=server"

image() {
  if [ "$2" = 1 ]; then ref="$REG/$1:1"; else ref="$REG2/$1:1"; fi
  docker build -q -t "$ref" - >/dev/null
  docker push -q "$ref" >/dev/null
}

# SIGKILL stops these at once, so the tests do not wait out the stop timeout.
# Each image and version gets a layer of its own, so the build cache does
# not make the same image of two names, and pruning an old one after an
# update frees space the run can report (R20).
for v in 1 2; do
  for name in srvdb srvweb srvguard hikifune-agent; do
    image "$name" "$v" <<EOF
FROM $BASE
STOPSIGNAL SIGKILL
CMD ["sleep", "1d"]
RUN head -c 1000000 /dev/urandom > /$name-$v
LABEL version=$v
EOF
  done
done
image srvbad 1 <<EOF
FROM $BASE
STOPSIGNAL SIGKILL
CMD ["sleep", "1d"]
EOF
image srvbad 2 <<EOF
FROM $BASE
STOPSIGNAL SIGKILL
CMD ["sh", "-c", "exit 3"]
EOF

docker run -d --name srv-db --label "$L" \
  --label com.docker.compose.project=srv --label com.docker.compose.service=db \
  "$REG/srvdb:1" >/dev/null
docker run -d --name srv-web --label "$L" \
  --label com.docker.compose.project=srv --label com.docker.compose.service=web \
  --label com.docker.compose.depends_on=db:service_started:false \
  "$REG/srvweb:1" >/dev/null
docker run -d --name srv-bad --label "$L" "$REG/srvbad:1" >/dev/null
docker run -d --name srv-guard --label "$L" --label hikifune.protected=true "$REG/srvguard:1" >/dev/null
docker run -d --name srv-self --label "$L" "$REG/hikifune-agent:1" >/dev/null
# The web tests update this one by hand; being protected keeps the server
# tests' scheduled runs off it.
docker run -d --name ui-guard --label "$L" --label hikifune.protected=true "$REG/srvguard:1" >/dev/null

for c in srv-db srv-web srv-bad srv-guard srv-self ui-guard; do
  if [ "$(docker inspect -f '{{.State.Running}}' "$c")" != true ]; then
    echo "$c is not running" >&2
    exit 1
  fi
done

echo "$(docker version -f '{{.Server.Version}}'): server fixtures ready"
