#!/bin/sh
# Runs inside one test daemon and creates the containers the e2e tests expect.
# It starts from an empty daemon each time, so it can run again.
set -eu

IMAGE=busybox:1.37

ids=$(docker ps -aq)
if [ -n "$ids" ]; then
  docker rm -f $ids >/dev/null
fi
docker pull -q "$IMAGE" >/dev/null

docker run -d --name plain "$IMAGE" sleep 1d >/dev/null
docker run -d --name protected --label hikifune.protected=true "$IMAGE" sleep 1d >/dev/null
docker run -d --name unprotected --label hikifune.protected=false "$IMAGE" sleep 1d >/dev/null
docker run -d --name typo --label hikifune.protected=ture "$IMAGE" sleep 1d >/dev/null
docker run -d --name bare-label --label hikifune.protected "$IMAGE" sleep 1d >/dev/null
docker run -d --name compose-web \
  --label com.docker.compose.project=demo \
  --label com.docker.compose.service=web \
  "$IMAGE" sleep 1d >/dev/null
docker run -d --name healthy --health-cmd true --health-interval 1s "$IMAGE" sleep 1d >/dev/null
docker create --name created "$IMAGE" true >/dev/null
docker run --name exited "$IMAGE" true

printf 'FROM %s\nLABEL built=locally\n' "$IMAGE" | docker build -q -t local/built:1 - >/dev/null
docker run -d --name local-build local/built:1 sleep 1d >/dev/null

i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' healthy)" = healthy ]; do
  i=$((i + 1))
  if [ "$i" -gt 30 ]; then
    echo "healthy did not become healthy" >&2
    exit 1
  fi
  sleep 1
done

echo "$(docker version -f '{{.Server.Version}}'): $(docker ps -aq | wc -l) containers"
