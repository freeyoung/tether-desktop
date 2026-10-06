#!/usr/bin/env bash
# Runs upstream tetherd headless in Docker, for the integration tests.
#
#   scripts/tetherd-container.sh build   # build the image from a tether checkout ($TETHER_SRC)
#   scripts/tetherd-container.sh up      # start it; tetherd listens on 127.0.0.1:$TETHERD_PORT
#   scripts/tetherd-container.sh down    # remove the containers and volumes
#
# Then: (cd go && TETHERD_PORT=15134 TETHERD_CONTAINER=tetherd-it go test ./core)
#
# Based on upstream scripts/test-container.sh: disposable volumes, a private
# D-Bus, no host BlueZ or Avahi, so no mDNS: the tests dial the port directly.
set -euo pipefail

image=${TETHERD_IMAGE:-tether:d2f705a}
name=${TETHERD_CONTAINER:-tetherd-it}
port=${TETHERD_PORT:-15134}
bus="$name-bus"
volumes=("$name-data" "$name-downloads" "$name-bus")

case ${1:-} in
build)
    src=${TETHER_SRC:?set TETHER_SRC to a zackb/tether checkout}
    docker build --target runtime -f "$src/packaging/container/Dockerfile" -t "$image" "$src"
    ;;
up)
    "$0" down >/dev/null 2>&1 || true
    for volume in "${volumes[@]}"; do docker volume create "$volume" >/dev/null; done
    docker run --rm --network none --user 0:0 \
        -v "$name-data:/data" -v "$name-downloads:/downloads" -v "$name-bus:/bus" \
        --entrypoint /bin/sh "$image" -ec 'chown 1000:1000 /data /downloads /bus; chmod 700 /data /downloads /bus'
    docker run -d --name "$bus" --network none --user 1000:1000 --read-only \
        --cap-drop ALL --security-opt no-new-privileges:true -e XDG_RUNTIME_DIR=/bus -v "$name-bus:/bus" \
        --entrypoint /usr/bin/dbus-daemon "$image" \
        --session --nofork --nopidfile --address=unix:path=/bus/system_bus_socket >/dev/null
    for ((i = 0; i < 50; i++)); do
        docker exec "$bus" test -S /bus/system_bus_socket && break
        sleep 0.1
    done
    docker run -d --name "$name" --read-only --user 1000:1000 \
        --cap-drop ALL --security-opt no-new-privileges:true \
        -p "127.0.0.1:$port:5134" \
        --tmpfs /run/tether-runtime:uid=1000,gid=1000,mode=0700 --tmpfs /tmp:mode=1777 \
        -v "$name-data:/data" -v "$name-downloads:/downloads" \
        -v "$name-bus:/host/run/dbus:ro" "$image" >/dev/null
    for ((i = 0; i < 100; i++)); do
        if docker exec "$name" /usr/local/libexec/tether-container-healthcheck 2>/dev/null; then
            echo "tetherd is up on 127.0.0.1:$port (container $name)"
            exit 0
        fi
        sleep 0.2
    done
    docker logs --tail 50 "$name" >&2
    echo "tetherd did not become healthy" >&2
    exit 1
    ;;
down)
    docker rm -f "$name" "$bus" >/dev/null 2>&1 || true
    for volume in "${volumes[@]}"; do docker volume rm "$volume" >/dev/null 2>&1 || true; done
    ;;
*)
    echo "usage: $0 build|up|down" >&2
    exit 2
    ;;
esac
